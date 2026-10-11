package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"
)

// zcodeBackend drives ZCode's headless stream-json protocol:
// `zcode --output-format stream-json --mode yolo -p <prompt>`.
//
// Wire shape (verified against ZCode 0.16.9): stdout carries NDJSON session
// events — {type, eventId, payload, seq, sessionId, turnId, ...} frames such
// as turn.started / model.streaming / tool.updated / turn.completed — and the
// run ends with one terminal line {"type":"result","sessionId":...,
// "response":...,"usage":...,"projection":{"status":...}}. Exit code is 0 on
// success and 1 on failure, with "Error: ..." on stderr. This is NOT Claude
// Code's stream-json shape; do not reuse the claude parser.
//
// Two protocol facts shape this backend. ZCode reads no prompt from stdin, so
// the prompt rides in argv (see buildZcodeArgs); and headless -p mode denies
// every tool unless --mode yolo is set, so that flag is daemon-owned.
type zcodeBackend struct {
	cfg Config
}

// zcodeBlockedArgs are owned by Multica. The prompt, the stream protocol, the
// session pointer, and the working directory are how the daemon drives the
// run, so custom args must not replace any of them. --mode yolo is the
// headless auto-approval switch; letting custom args lower it would turn every
// run into an approval-denied no-op, and raising it past yolo is impossible.
var zcodeBlockedArgs = map[string]blockedArgMode{
	"-p":              blockedWithValue,
	"--prompt":        blockedWithValue,
	"--output-format": blockedWithValue,
	"--json":          blockedStandalone,
	"--resume":        blockedWithValue,
	"-c":              blockedStandalone,
	"--continue":      blockedStandalone,
	"--mode":          blockedWithValue,
	"--cwd":           blockedWithValue,
}

// buildZcodeArgs assembles the argv for a one-shot zcode invocation.
//
// The prompt is delivered via -p because ZCode's headless mode reads nothing
// from stdin. That is the one deliberate divergence from the qwen/claude
// stdin convention (#6082): there is no stdin channel to move the prompt to,
// so very large task contexts are bounded by the OS argv limit instead. The
// known-limitation note in docs/providers.mdx documents this.
func buildZcodeArgs(prompt string, opts ExecOptions, logger *slog.Logger) []string {
	args := []string{"--output-format", "stream-json"}
	// --mode yolo is daemon-owned: ZCode's headless permission broker
	// auto-approves only workflow tooling and denies everything else with
	// "No permission client configured", so without it a run that touches any
	// real tool fails. All other adapters use an equivalent daemon-owned
	// bypass (qwen's --yolo, MUL-5134).
	args = append(args, "--mode", "yolo")
	args = append(args, filterCustomArgs(opts.ExtraArgs, zcodeBlockedArgs, logger)...)
	args = append(args, filterCustomArgs(opts.CustomArgs, zcodeBlockedArgs, logger)...)
	if opts.ResumeSessionID != "" {
		args = append(args, "--resume", opts.ResumeSessionID)
	}
	args = append(args, "-p", prompt)
	return args
}

func (b *zcodeBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	execName := b.cfg.ExecutablePath
	if execName == "" {
		execName = "zcode"
	}
	lookedUp, err := exec.LookPath(execName)
	if err != nil {
		return nil, fmt.Errorf("zcode executable not found at %q: %w", execName, err)
	}
	// The desktop app ships ZCode as a Node bundle (zcode.cjs), not a native
	// binary. App-bundle probe fallbacks hand us that path directly, so a
	// resolved .cjs/.mjs runs under `node`.
	if isNodeScriptPath(lookedUp) {
		if _, err := exec.LookPath("node"); err != nil {
			return nil, fmt.Errorf("zcode runtime at %q is a Node script but node was not found in PATH: %w", lookedUp, err)
		}
	}
	timeout := opts.Timeout
	runCtx, cancel := runContext(ctx, timeout)
	args := buildZcodeArgs(prompt, opts, b.cfg.Logger)

	cmd, _, _ := b.cfg.commandAt(execName).execVia(runCtx, chooseZcodeInvocation, lookedUp, args, b.cfg.Logger)
	hideAgentWindow(cmd)
	b.cfg.logAgentCommand(cmd, newAgentCommandLogArgs(args))
	cmd.WaitDelay = 10 * time.Second
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	cmd.Env = buildEnv(b.cfg.Env)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("zcode stdout pipe: %w", err)
	}
	// Stdin stays nil: the child inherits a closed stdin, which is what ZCode
	// expects — it reads no prompt from there.
	stderrBuf := newStderrTail(newLogWriter(b.cfg.Logger, "[zcode:stderr] "), agentStderrTailBytes)
	cmd.Stderr = stderrBuf
	if err := startOwnedProcessTree(cmd, b.cfg.Logger); err != nil {
		cancel()
		return nil, fmt.Errorf("start zcode: %w", err)
	}
	b.cfg.Logger.Info("zcode started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "resumed", opts.ResumeSessionID != "")

	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)
	go func() {
		defer cancel()
		defer close(msgCh)
		defer close(resCh)

		started := time.Now()
		state := zcodeStreamState{usage: make(map[string]TokenUsage)}
		go func() {
			<-runCtx.Done()
			_ = stdout.Close()
		}()

		scanner := newAgentStreamScanner(stdout)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var event zcodeStreamEvent
			if err := json.Unmarshal([]byte(line), &event); err != nil {
				state.invalidEventCount++
				continue
			}
			state.eventCount++
			state.lastEventType = event.Type
			handleZcodeEvent(event, msgCh, &state)
		}
		scanErr := scanner.Err()
		if scanErr != nil {
			_ = stdout.Close()
		}
		exitErr := cmd.Wait()
		releaseProcessGroup(cmd)
		duration := time.Since(started)

		status, output, errMsg := finalizeStreamResult("zcode", timeout, runCtx.Err(), nil, exitErr, state.sessionID, streamTerminalState{
			lastAssistantText: state.lastAssistantText,
			finalResultText:   state.finalResultText,
			sawResult:         state.sawResult,
			resultIsError:     state.resultIsError,
			scanErr:           scanErr,
		}, "")
		if errMsg != "" {
			errMsg = withAgentStderr(errMsg, "zcode", stderrBuf.Tail())
		}
		logStreamProtocolObservation(b.cfg.Logger, streamProtocolObservation{
			provider: "zcode", cliVersion: b.cfg.CLIVersion, model: "",
			exitCode: streamProcessExitCode(exitErr), eventCount: state.eventCount,
			invalidEventCount: state.invalidEventCount,
			toolUseCount:      state.toolUseCount, sawResult: state.sawResult, resultIsError: state.resultIsError,
			resultBytes: len(state.finalResultText), lastAssistantBytes: len(state.lastAssistantText),
			scannerError: scanErr != nil, lastEventType: state.lastEventType,
		})
		b.cfg.Logger.Info("zcode finished", "pid", cmd.Process.Pid, "status", status, "duration", duration.Round(time.Millisecond).String())
		resCh <- Result{
			Status: status, Output: output, Error: errMsg, DurationMs: duration.Milliseconds(),
			SessionID: resolveSessionID(opts.ResumeSessionID, state.sessionID, status == "failed", errMsg, stderrBuf.Tail()),
			Usage:     state.usage,
			ResumeRejected: resumeWasRejected(opts.ResumeSessionID, state.sessionID, status == "failed", errMsg, stderrBuf.Tail()),
		}
	}()
	return &Session{Messages: msgCh, Result: resCh}, nil
}

// zcodeStreamEvent is one NDJSON frame. Session events carry a payload object
// and envelope metadata; the terminal line carries response/usage/projection
// at the top level instead.
type zcodeStreamEvent struct {
	Type       string           `json:"type"`
	SessionID  string           `json:"sessionId,omitempty"`
	Payload    json.RawMessage  `json:"payload,omitempty"`
	Response   string           `json:"response,omitempty"`
	Usage      *zcodeUsage      `json:"usage,omitempty"`
	Projection *zcodeProjection `json:"projection,omitempty"`
}

type zcodeProjection struct {
	Status string `json:"status"`
}

// zcodeUsage is the run-total token count. ZCode reports cache reads inside
// inputTokens (its own totalTokens equals input+output, so input subsumes the
// cached share), which is the same convention qwenTokenUsage adjusts for.
type zcodeUsage struct {
	InputTokens         int64 `json:"inputTokens"`
	OutputTokens        int64 `json:"outputTokens"`
	CacheReadTokens     int64 `json:"cacheReadTokens"`
	CacheCreationTokens int64 `json:"cacheCreationTokens"`
}

type zcodeToolUpdate struct {
	Kind      string          `json:"kind,omitempty"`
	ToolCallID string         `json:"toolCallId,omitempty"`
	ToolName  string          `json:"toolName,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	Output    json.RawMessage `json:"output,omitempty"`
	Error     json.RawMessage `json:"error,omitempty"`
}

type zcodeTurnCompleted struct {
	Response   string      `json:"response,omitempty"`
	ResultType string      `json:"resultType,omitempty"`
	Usage      *zcodeUsage `json:"usage,omitempty"`
}

type zcodeStreamState struct {
	sessionID, lastAssistantText, finalResultText, lastEventType string
	sawResult, resultIsError                                     bool
	usage                                                        map[string]TokenUsage
	eventCount, invalidEventCount, toolUseCount                  int
}

func handleZcodeEvent(event zcodeStreamEvent, ch chan<- Message, state *zcodeStreamState) {
	if event.SessionID != "" {
		state.sessionID = event.SessionID
	}
	switch event.Type {
	case "turn.started":
		trySend(ch, Message{Type: MessageStatus, Status: "running", SessionID: state.sessionID})
	case "tool.updated":
		handleZcodeToolUpdate(event.Payload, ch, state)
	case "turn.completed":
		var turn zcodeTurnCompleted
		if json.Unmarshal(event.Payload, &turn) != nil {
			return
		}
		if turn.Response != "" {
			state.lastAssistantText = turn.Response
			trySend(ch, Message{Type: MessageText, Content: turn.Response})
		}
	case "turn.failed":
		// Be fail-closed if the terminal result line never arrives after a
		// failed turn: adopt the failure now so a crash on the way out still
		// reports why.
		state.resultIsError = true
		state.finalResultText = zcodePayloadErrorText(event.Payload)
	case "result":
		state.sawResult = true
		if event.Response != "" {
			state.finalResultText = event.Response
		}
		if event.Projection != nil && event.Projection.Status == "error" {
			state.resultIsError = true
			// A turn.failed may already have deposited its message; only
			// fill in a placeholder when the whole run stayed silent about
			// why. An empty response must never clobber that text.
			if state.finalResultText == "" {
				state.finalResultText = "zcode returned an error result without details"
			}
		}
		if usage := zcodeResultUsage(event.Usage); len(usage) > 0 {
			state.usage = usage
		}
	}
}

func handleZcodeToolUpdate(raw json.RawMessage, ch chan<- Message, state *zcodeStreamState) {
	var update zcodeToolUpdate
	if json.Unmarshal(raw, &update) != nil {
		return
	}
	switch update.Kind {
	case "scheduled", "started":
		state.toolUseCount++
		var input map[string]any
		if len(update.Input) > 0 {
			_ = json.Unmarshal(update.Input, &input)
		}
		trySend(ch, Message{Type: MessageToolUse, Tool: update.ToolName, CallID: update.ToolCallID, Input: input})
	case "result":
		trySend(ch, Message{Type: MessageToolResult, CallID: update.ToolCallID, Output: zcodeRawText(update.Output)})
	case "error":
		trySend(ch, Message{Type: MessageToolResult, CallID: update.ToolCallID, Output: zcodeRawText(update.Error)})
	}
}

// zcodeResultUsage maps the terminal usage onto the shared buckets. The map
// key is "unknown" because ZCode accepts no model flag — the model lives in
// the CLI's own provider config (mcode precedent). The total is this
// invocation's run summary, not a session total, so resumed runs may use it
// directly.
func zcodeResultUsage(usage *zcodeUsage) map[string]TokenUsage {
	if usage == nil || (usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.CacheReadTokens == 0 && usage.CacheCreationTokens == 0) {
		return nil
	}
	return map[string]TokenUsage{
		"unknown": {
			InputTokens:      max(0, usage.InputTokens-usage.CacheReadTokens),
			OutputTokens:     usage.OutputTokens,
			CacheReadTokens:  usage.CacheReadTokens,
			CacheWriteTokens: usage.CacheCreationTokens,
		},
	}
}

func zcodeRawText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	return string(raw)
}

func zcodePayloadErrorText(raw json.RawMessage) string {
	if text := zcodeRawText(raw); text != "" && text != "null" {
		var body struct {
			Message string `json:"message"`
			Error   string `json:"error"`
		}
		if json.Unmarshal(raw, &body) == nil && (body.Message != "" || body.Error != "") {
			if body.Message != "" {
				return body.Message
			}
			return body.Error
		}
		return text
	}
	return "zcode turn failed without details"
}

// isNodeScriptPath reports whether a resolved executable is a Node script the
// daemon must run under node rather than exec directly.
func isNodeScriptPath(path string) bool {
	return strings.HasSuffix(path, ".cjs") || strings.HasSuffix(path, ".mjs")
}
