package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// piBackend implements Backend by spawning the Pi CLI in non-interactive
// JSON mode (`pi -p --mode json`) and parsing its event stream on stdout.
//
// It also backs the "omp" (oh-my-pi) provider — omp is a separate CLI
// (https://omp.sh) that is a drop-in fork of pi and speaks the same JSON
// event protocol. The daemon probes a separate `omp` binary and registers
// it under the "omp" key; piBackend uses defaultExecutable so the fallback
// binary name matches the provider key (pi → "pi", omp → "omp") when
// cfg.ExecutablePath is empty.
//
// Fresh sessions take one of two shapes depending on the CLI generation
// (see the session selection in Execute): legacy CLIs append to a
// daemon-named --session file, resume-only CLIs (omp ≥ 18.7) create their
// own transcript inside --session-dir and the adopted path becomes the
// opaque session id. Resume runs pass the transcript path to --session in
// both generations.
type piBackend struct {
	cfg               Config
	defaultExecutable string
	// providerLabel is the human-facing name used in log messages and error
	// strings ("pi" or "omp"). Defaults to "pi" when empty so existing callers
	// that construct piBackend directly (tests) keep their original output.
	providerLabel string
	// turnErrorGrace overrides the silence window after a turn-level provider
	// error. Production uses defaultPiTurnErrorGrace; tests shorten it without
	// changing concurrent executions through package-global state.
	turnErrorGrace time.Duration
}

var (
	piControlTokenRE = regexp.MustCompile(`<\|[A-Za-z0-9_-]+>[A-Za-z0-9_-]*|<[A-Za-z0-9_-]+\|>`)
)

// defaultPiTurnErrorGrace is the recovery window Pi gets after reporting a
// provider error without exiting. Pi reports the same turn_end stopReason
// before automatic retries, so the error cannot be treated as terminal at
// once. Ten minutes matches the existing provider-specific semantic-idle
// precedent while remaining far below the daemon's two-hour fallback.
const defaultPiTurnErrorGrace = 10 * time.Minute

// piTurnErrorGuard owns the only timer associated with a pending Pi turn
// error. The stream goroutine records protocol activity; the timer callback
// invokes the execution's bounded stop path after re-checking the state. It
// never emits a Message, so observing the provider error cannot refresh the
// daemon's independent lastActivityAt watchdog clock.
type piTurnErrorGuard struct {
	mu            sync.Mutex
	grace         time.Duration
	expireRun     func()
	timer         *time.Timer
	generation    uint64
	lastError     string
	inFlightTools int
	graceExpired  bool
	terminal      bool
	stopped       bool
}

func newPiTurnErrorGuard(grace time.Duration, expireRun func()) *piTurnErrorGuard {
	return &piTurnErrorGuard{grace: grace, expireRun: expireRun}
}

// closePiReadPipe interrupts a scanner/copy blocked in Read before releasing
// the adapter's descriptor. The deadline matters on Unix, where Close from a
// different goroutine need not interrupt a syscall already in the kernel.
func closePiReadPipe(pipe io.ReadCloser) {
	if deadlinePipe, ok := pipe.(interface{ SetReadDeadline(time.Time) error }); ok {
		_ = deadlinePipe.SetReadDeadline(time.Now())
	}
	_ = pipe.Close()
}

// observeEvent records a parseable Pi protocol event. Recovery events clear a
// stale provider error; every other event gives an unresolved error a fresh
// grace window. Tool accounting is kept inside the adapter so an error timer
// can never terminate a tool that is still in flight.
func (g *piTurnErrorGuard) observeEvent(eventType string) {
	if eventType == "" {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return
	}

	switch eventType {
	case "turn_start", "auto_retry_start":
		g.clearErrorLocked()
		return
	case "tool_execution_start":
		g.inFlightTools++
	case "tool_execution_end":
		if g.inFlightTools > 0 {
			g.inFlightTools--
		}
	}
	g.rearmLocked()
}

func (g *piTurnErrorGuard) record(errText string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return
	}
	g.lastError = errText
	g.rearmLocked()
}

func (g *piTurnErrorGuard) clear() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.stopped {
		return
	}
	g.clearErrorLocked()
}

func (g *piTurnErrorGuard) clearErrorLocked() {
	g.lastError = ""
	g.generation++
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
}

func (g *piTurnErrorGuard) rearmLocked() {
	if g.lastError == "" || g.stopped {
		return
	}
	g.generation++
	generation := g.generation
	if g.timer != nil {
		g.timer.Stop()
	}
	g.timer = time.AfterFunc(g.grace, func() {
		g.expire(generation)
	})
}

func (g *piTurnErrorGuard) expire(generation uint64) {
	g.mu.Lock()
	if g.stopped || generation != g.generation || g.lastError == "" {
		g.mu.Unlock()
		return
	}
	g.timer = nil
	if g.inFlightTools > 0 {
		g.mu.Unlock()
		return
	}
	g.graceExpired = true
	g.stopped = true
	expireRun := g.expireRun
	g.mu.Unlock()

	// The callback owns both process-tree cancellation and the adapter's local
	// pipes. terminal remains false until the result goroutine has escaped its
	// scanner/Wait path and can publish a Result, so a liveness watcher can never
	// mistake "we decided to stop" for "finalization is guaranteed to finish".
	expireRun()
}

func (g *piTurnErrorGuard) finish() (lastError string, graceExpired bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.stopped = true
	g.generation++
	if g.timer != nil {
		g.timer.Stop()
		g.timer = nil
	}
	return g.lastError, g.graceExpired
}

func (g *piTurnErrorGuard) markTerminal() {
	g.mu.Lock()
	g.terminal = true
	g.mu.Unlock()
}

func (g *piTurnErrorGuard) terminalObserved() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.terminal
}

func stripPiToolCallMarkup(s string) string {
	s = stripPiStructuredToolMarkup(s)
	return piControlTokenRE.ReplaceAllString(s, "")
}

func drainPiTextBuffer(buf *strings.Builder, delta string) string {
	buf.WriteString(delta)
	emit, pending := drainPiSanitizedText(buf.String())
	buf.Reset()
	buf.WriteString(pending)
	return emit
}

func flushPiTextBuffer(buf *strings.Builder) string {
	s := buf.String()
	buf.Reset()
	emit, pending := drainPiSanitizedText(s)
	emit += piControlTokenRE.ReplaceAllString(pending, "")
	return emit
}

func drainPiSanitizedText(s string) (string, string) {
	var out strings.Builder
	for i := 0; i < len(s); {
		start, prefixLen := nextPiToolMarkupPrefix(s, i)
		if start == -1 {
			safeLen := safePiTextEmitLen(s[i:])
			out.WriteString(s[i : i+safeLen])
			return piControlTokenRE.ReplaceAllString(out.String(), ""), s[i+safeLen:]
		}
		out.WriteString(s[i:start])
		end, ok := scanPiToolMarkupEnd(s, start+prefixLen)
		if !ok {
			return piControlTokenRE.ReplaceAllString(out.String(), ""), s[start:]
		}
		i = end
	}
	return piControlTokenRE.ReplaceAllString(out.String(), ""), ""
}

func stripPiStructuredToolMarkup(s string) string {
	var out strings.Builder
	for i := 0; i < len(s); {
		start, prefixLen := nextPiToolMarkupPrefix(s, i)
		if start == -1 {
			out.WriteString(s[i:])
			break
		}
		out.WriteString(s[i:start])
		end, ok := scanPiToolMarkupEnd(s, start+prefixLen)
		if !ok {
			out.WriteString(s[start:])
			break
		}
		i = end
	}
	return out.String()
}

func safePiTextEmitLen(s string) int {
	hold := 0
	for _, prefix := range []string{"call:", "response:"} {
		for n := 1; n < len(prefix) && n <= len(s); n++ {
			if strings.HasSuffix(s, prefix[:n]) && n > hold {
				hold = n
			}
		}
	}
	if i := strings.LastIndexByte(s, '<'); i >= 0 && looksLikePiControlTokenPrefix(s[i:]) {
		if len(s)-i > hold {
			hold = len(s) - i
		}
	}
	return len(s) - hold
}

func looksLikePiControlTokenPrefix(s string) bool {
	if len(s) == 0 || s[0] != '<' || len(s) > 64 {
		return false
	}
	for i := 1; i < len(s); i++ {
		b := s[i]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b == '-' || b == '|' || b == '>' {
			continue
		}
		return false
	}
	return true
}

func nextPiToolMarkupPrefix(s string, from int) (int, int) {
	best := -1
	bestLen := 0
	for _, prefix := range []string{"call:", "response:"} {
		if i := strings.Index(s[from:], prefix); i >= 0 {
			abs := from + i
			if best == -1 || abs < best {
				best = abs
				bestLen = len(prefix)
			}
		}
	}
	return best, bestLen
}

func scanPiToolMarkupEnd(s string, i int) (int, bool) {
	nameStart := i
	for i < len(s) && isPiToolNameByte(s[i]) {
		i++
	}
	if i == nameStart || i >= len(s) || s[i] != '{' {
		return 0, false
	}

	const quoteMarker = `<|"|>`
	depth := 0
	inQuote := false
	for i < len(s) {
		if strings.HasPrefix(s[i:], quoteMarker) {
			inQuote = !inQuote
			i += len(quoteMarker)
			continue
		}

		if !inQuote {
			switch s[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					i++
					if strings.HasPrefix(s[i:], "<tool_call|>") {
						i += len("<tool_call|>")
					}
					return i, true
				}
			}
		}
		i++
	}
	return 0, false
}

func isPiToolNameByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b == '-'
}

func (b *piBackend) Execute(ctx context.Context, prompt string, opts ExecOptions) (*Session, error) {
	label := b.providerLabel
	if label == "" {
		label = "pi"
	}
	// Pi trims piped stdin before building its initial message. Reject an empty
	// task here so whitespace-only input cannot turn into a successful process
	// with no turn, no output, and an empty session.
	if strings.TrimSpace(prompt) == "" {
		return nil, fmt.Errorf("%s prompt must not be empty", label)
	}

	execName := b.cfg.ExecutablePath
	if execName == "" {
		execName = b.defaultExecutable
	}
	if execName == "" {
		execName = "pi"
	}
	lookedUp, err := exec.LookPath(execName)
	if err != nil {
		return nil, fmt.Errorf("%s executable not found at %q: %w", label, execName, err)
	}

	timeout := opts.Timeout

	// The Pi family spans two --session generations, and a fresh session is
	// where they disagree:
	//
	//   - pi 0.x and omp ≤ 18.6 treat `--session <path>` as create-or-append
	//     but refuse to start when the path is missing, so the daemon picks
	//     the name and pre-creates the (empty) file.
	//   - omp 18.7 turned --session into an alias of --resume: a missing path
	//     fails with "Session not found" and the pre-created empty file with
	//     "Cannot resume session ... holds no entries", so every fresh run
	//     died within a second of launch. Those CLIs create the transcript
	//     inside --session-dir under their own name and report the session id
	//     in the stream's first JSON event; we let them and adopt the path.
	//
	// In both generations the transcript path doubles as our opaque session
	// identifier: we return it as SessionID and expect it back as
	// ResumeSessionID on the next turn.
	sessionPath := opts.ResumeSessionID
	sessionDirMode := false
	if sessionPath == "" && piResumeOnlySession(b.cfg) {
		sessionDirMode = true
	} else if sessionPath == "" {
		p, err := newPiSessionPath()
		if err != nil {
			return nil, fmt.Errorf("%s session path: %w", label, err)
		}
		sessionPath = p
	}
	var sessionDir string
	if sessionDirMode {
		dir, err := piSessionDir()
		if err != nil {
			return nil, fmt.Errorf("%s session dir: %w", label, err)
		}
		sessionDir = dir
	}
	var sessionLock *os.File
	if !sessionDirMode {
		if err := ensurePiSessionFile(sessionPath); err != nil {
			return nil, fmt.Errorf("%s session file: %w", label, err)
		}
		lock, locked, err := tryLockPiSessionFile(sessionPath)
		if err != nil {
			return nil, fmt.Errorf("%s session lock: %w", label, err)
		}
		sessionLock = lock
		if !locked {
			if opts.ResumeSessionID != "" {
				return piSessionBusyResult(label, sessionPath), nil
			}
			return nil, fmt.Errorf("%s session file %q is already in use", label, sessionPath)
		}
	}

	runCtx, cancel := runContext(ctx, timeout)
	processCtx, cancelProcess := context.WithCancel(runCtx)

	args := buildPiArgs(sessionPath, sessionDir, opts, b.cfg.Logger)
	cmd, _, _ := b.cfg.commandAt(execName).execVia(processCtx, choosePiInvocation, lookedUp, args, b.cfg.Logger)
	hideAgentWindow(cmd)
	b.cfg.logAgentCommand(cmd, newAgentCommandLogArgs(args))
	cmd.WaitDelay = 10 * time.Second
	if opts.Cwd != "" {
		cmd.Dir = opts.Cwd
	}
	cmd.Env = buildEnv(b.cfg.Env)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		releasePiSessionFileLock(sessionLock)
		cancelProcess()
		cancel()
		return nil, fmt.Errorf("%s stdout pipe: %w", label, err)
	}
	// Pi reads piped stdin to EOF as its initial prompt in print/JSON mode.
	// Keeping user-controlled text off argv prevents the npm PowerShell shim
	// from re-tokenising embedded quotes into CLI flags on Windows (#6457).
	// The explicit close remains part of the #2188 contract too: under systemd,
	// Pi has been observed to wait indefinitely when stdin never reaches EOF.
	stdin, err := cmd.StdinPipe()
	if err != nil {
		closePiReadPipe(stdout)
		releasePiSessionFileLock(sessionLock)
		cancelProcess()
		cancel()
		return nil, fmt.Errorf("%s stdin pipe: %w", label, err)
	}
	var closeStdinOnce sync.Once
	closeStdin := func() { closeStdinOnce.Do(func() { _ = stdin.Close() }) }
	// Watch stderr as well as log it. When Pi refuses a resume it exits before
	// emitting a single JSON event, so stderr is the only place the reason
	// exists and Result.ResumeRejected has nothing else to be built from. Own the
	// pipe directly instead of letting os/exec hide it behind a copy goroutine:
	// an escaped descendant can inherit stderr too, and the error-grace path must
	// be able to release every read that could hold Result finalization open.
	stderrRead, err := cmd.StderrPipe()
	if err != nil {
		closeStdin()
		closePiReadPipe(stdout)
		releasePiSessionFileLock(sessionLock)
		cancelProcess()
		cancel()
		return nil, fmt.Errorf("%s stderr pipe: %w", label, err)
	}
	stderrWatch := newPiStderrWatcher(newLogWriter(b.cfg.Logger, "["+label+":stderr] "))

	if err := startOwnedProcessTree(cmd, b.cfg.Logger); err != nil {
		closeStdin()
		closePiReadPipe(stdout)
		closePiReadPipe(stderrRead)
		releasePiSessionFileLock(sessionLock)
		cancelProcess()
		cancel()
		return nil, fmt.Errorf("start %s: %w", label, err)
	}
	stderrDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(stderrWatch, stderrRead)
		close(stderrDone)
	}()

	b.cfg.Logger.Info(label+" started", "pid", cmd.Process.Pid, "cwd", opts.Cwd, "model", opts.Model)

	msgCh := make(chan Message, 256)
	resCh := make(chan Result, 1)
	turnErrorGrace := b.turnErrorGrace
	if turnErrorGrace <= 0 {
		turnErrorGrace = defaultPiTurnErrorGrace
	}
	turnErrors := newPiTurnErrorGuard(turnErrorGrace, func() {
		b.cfg.Logger.Info(label + " turn error recovery grace expired; stopping runtime")
		// CommandContext owns process-tree cancellation for every runtime
		// command. An escaped descendant can nevertheless retain stdout/stderr
		// write ends after that group dies, so close the adapter-owned pipes too:
		// finalization must not depend on an unowned process eventually exiting.
		cancelProcess()
		closeStdin()
		closePiReadPipe(stdout)
		closePiReadPipe(stderrRead)
	})

	// Write concurrently with stdout consumption. A large prompt can fill the
	// stdin pipe while the child fills stdout; serialising those operations can
	// deadlock both processes. Closing stdin signals the end of Pi's prompt.
	writeErrCh := make(chan error, 1)
	go func() {
		_, err := io.WriteString(stdin, prompt)
		closeStdin()
		writeErrCh <- err
	}()

	// Close every adapter-owned pipe when the context is cancelled. Closing stdin
	// releases a writer blocked on a child that stopped reading; closing the read
	// pipes releases the stdout scanner and stderr copier.
	go func() {
		<-runCtx.Done()
		closeStdin()
		closePiReadPipe(stdout)
		closePiReadPipe(stderrRead)
	}()

	go func() {
		defer func() { releasePiSessionFileLock(sessionLock) }()
		defer cancelProcess()
		defer cancel()
		defer close(msgCh)
		defer close(resCh)

		startTime := time.Now()
		var output strings.Builder
		finalStatus := "completed"
		var finalError string
		usage := make(map[string]TokenUsage)

		// Pi message_update events can be large (they embed the full message
		// partial on each delta); the shared stream bound covers that.
		scanner := newAgentStreamScanner(stdout)
		var textBuffer strings.Builder
		// The transcript header event the CLI emits at run start carries its
		// session id — the handle that ties a dir-mode fresh run to the file
		// the CLI created for it.
		var reportedSessionID string

		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line == "" {
				continue
			}
			var evt piStreamEvent
			if err := json.Unmarshal([]byte(line), &evt); err != nil {
				continue
			}
			turnErrors.observeEvent(evt.Type)

			switch evt.Type {
			case "session":
				reportedSessionID = evt.ID

			case "agent_start":
				trySend(msgCh, Message{Type: MessageStatus, Status: "running"})

			case "turn_start":
				output.Reset()
				textBuffer.Reset()

			case "message_update":
				if evt.AssistantMessageEvent == nil {
					continue
				}
				switch evt.AssistantMessageEvent.Type {
				case "text_delta":
					if d := drainPiTextBuffer(&textBuffer, evt.AssistantMessageEvent.Delta); d != "" {
						output.WriteString(d)
						trySend(msgCh, Message{Type: MessageText, Content: d})
					}
				case "thinking_delta":
					if d := evt.AssistantMessageEvent.Delta; d != "" {
						trySend(msgCh, Message{Type: MessageThinking, Content: d})
					}
				}

			case "tool_execution_start":
				var params map[string]any
				if len(evt.Args) > 0 {
					_ = json.Unmarshal(evt.Args, &params)
				}
				trySend(msgCh, Message{
					Type:   MessageToolUse,
					Tool:   evt.ToolName,
					CallID: evt.ToolCallID,
					Input:  params,
				})

			case "tool_execution_end":
				trySend(msgCh, Message{
					Type:   MessageToolResult,
					CallID: evt.ToolCallID,
					Output: decodePiResult(evt.Result),
				})

			case "turn_end":
				msg := decodePiMessage(evt.Message)
				if msg == nil {
					continue
				}
				if msg.Usage != nil {
					model := msg.Model
					if model == "" {
						model = opts.Model
					}
					if model == "" {
						model = "unknown"
					}
					u := usage[model]
					u.InputTokens += msg.Usage.Input
					u.OutputTokens += msg.Usage.Output
					u.CacheReadTokens += msg.Usage.CacheRead
					u.CacheWriteTokens += msg.Usage.CacheWrite
					usage[model] = u
				}
				// A turn Pi ends on an error is only terminal when nothing
				// follows it. Pi emits the same stopReason before an automatic
				// retry, and turn_start clears this, so a later successful turn
				// leaves nothing behind.
				if msg.StopReason == "error" {
					turnError := msg.ErrorMessage
					if turnError == "" {
						turnError = label + " ended the turn with an error"
					}
					turnErrors.record(turnError)
				} else {
					// A successful terminal turn is positive recovery evidence even
					// if a future Pi version omits the expected turn_start.
					turnErrors.clear()
				}

			case "agent_end":
				// agent_end closes the run, and its last assistant message holds
				// the run's terminal state. Pi 0.74.0 and earlier report a
				// failure raised outside a model stream, such as an API key they
				// cannot resolve, only here: no turn_end, no error event, and
				// print mode still exits 0. Record it like a turn_end error, so
				// an automatic retry or a later successful turn still clears it.
				msg := lastPiAssistantMessage(evt.Messages)
				if msg == nil || msg.StopReason != "error" {
					continue
				}
				runError := msg.ErrorMessage
				if runError == "" {
					runError = label + " ended the run with an error"
				}
				turnErrors.record(runError)

			case "error":
				errText := decodePiString(evt.Message)
				trySend(msgCh, Message{Type: MessageError, Content: errText})
				if finalStatus == "completed" {
					finalStatus = "failed"
					finalError = errText
				}

			case "auto_retry_end":
				if evt.Success {
					turnErrors.clear()
				} else if finalStatus == "completed" {
					finalStatus = "failed"
					if evt.FinalError != "" {
						finalError = evt.FinalError
					} else {
						finalError = label + " exhausted automatic retries"
					}
				}
			}
		}
		scanErr := scanner.Err()
		if scanErr != nil {
			// Scanner stopped consuming stdout. Close the pipe before Wait so Pi,
			// still writing an oversized event, cannot deadlock on the full OS
			// pipe; the scanner error remains the primary failure.
			closePiReadPipe(stdout)
		}
		if d := flushPiTextBuffer(&textBuffer); d != "" {
			output.WriteString(d)
			trySend(msgCh, Message{Type: MessageText, Content: d})
		}

		// Finish the user-owned stderr read before Wait closes StderrPipe. Normal
		// exit gets the same 10s backstop cmd.WaitDelay used to provide when
		// os/exec owned the copier. Cancellation and error-grace expiry close
		// stderrRead above, so their finalization does not pay it.
		stderrTimer := time.NewTimer(cmd.WaitDelay)
		select {
		case <-stderrDone:
			stderrTimer.Stop()
		case <-stderrTimer.C:
			closePiReadPipe(stderrRead)
			<-stderrDone
		}

		waitErr := cmd.Wait()
		releaseProcessGroup(cmd)
		duration := time.Since(startTime)
		lastTurnError, turnErrorGraceExpired := turnErrors.finish()

		// Wait closes the process pipes, so a prompt write still blocked when the
		// child exited has returned by now. The writer sends exactly once.
		writeErr := <-writeErrCh
		authoritativeTerminal := false

		if runCtx.Err() == context.DeadlineExceeded {
			finalStatus = "timeout"
			finalError = fmt.Sprintf("%s timed out after %s", label, timeout)
		} else if runCtx.Err() == context.Canceled {
			if lastTurnError != "" {
				// A daemon watchdog or user cancellation must not replace a
				// provider failure Pi had already reported with a generic local
				// cancellation. The daemon can then persist and classify the
				// original provider message.
				finalStatus = "failed"
				finalError = lastTurnError
				authoritativeTerminal = true
			} else {
				finalStatus = "aborted"
				finalError = "execution cancelled"
			}
		} else if turnErrorGraceExpired {
			finalStatus = "failed"
			finalError = lastTurnError
			authoritativeTerminal = true
		} else if scanErr != nil && finalStatus == "completed" {
			// The unread rest of the stream may hold the run's terminal state
			// (agent_end repeats every message of the run, so it is the line
			// most likely to overflow), so the run cannot count as a success.
			// Ranked above waitErr: closing stdout is what made Pi exit.
			authoritativeTerminal = true
			finalStatus = "failed"
			finalError = fmt.Sprintf("%s stdout read error: %v", label, scanErr)
			if lastTurnError != "" {
				finalError = lastTurnError + "; " + finalError
			}
		} else if waitErr != nil && finalStatus == "completed" {
			authoritativeTerminal = true
			finalStatus = "failed"
			// Prefer the turn's provider message over the process exit code.
			// Pi (and pi-print-clean-exit) exits 1 after stopReason=error, so
			// Wait() wins this branch and used to drop lastTurnError. The
			// classifier then saw only "exit status 1" and filed the run as
			// non-retryable process_failure — even when the turn was a
			// transient LiteLLM/OpenAI "Connection error." / "Request timed
			// out." Keep the exit status as a suffix so a genuine crash is
			// still visible (same shape as the OpenCode empty-step+exit
			// composite).
			if lastTurnError != "" {
				finalError = fmt.Sprintf("%s; %s exited with error: %v", lastTurnError, label, waitErr)
			} else {
				finalError = fmt.Sprintf("%s exited with error: %v", label, waitErr)
			}
		} else if writeErr != nil && finalStatus == "completed" {
			authoritativeTerminal = true
			finalStatus = "failed"
			finalError = fmt.Sprintf("%s prompt write failed: %v", label, writeErr)
		} else if lastTurnError != "" && finalStatus == "completed" {
			authoritativeTerminal = true
			// Pi exits 0 after a turn it could not complete and did not retry,
			// and emits neither an `error` event nor `auto_retry_end`. Without
			// this the run reports success with no output.
			finalStatus = "failed"
			finalError = lastTurnError
		} else if runCtx.Err() == nil {
			authoritativeTerminal = true
		}

		b.cfg.Logger.Info(label+" finished", "pid", cmd.Process.Pid, "status", finalStatus, "duration", duration.Round(time.Millisecond).String())
		// Publish the authoritative terminal boundary before Result. The daemon
		// uses this ordering to let an already-observed provider failure outrank
		// a watchdog cancellation that raced with adapter finalization.
		if authoritativeTerminal {
			turnErrors.markTerminal()
		}

		// Publish the terminal result only after the transcript is available to
		// a follow-up run. The result channel is buffered, so relying on a defer
		// would let the receiver race ahead and spuriously treat the session as
		// still busy after Pi had already exited.
		releasePiSessionFileLock(sessionLock)
		sessionLock = nil
		// In the legacy flow this is the path we passed in. In dir mode the
		// CLI named the file itself; the header event's id is what maps the
		// run back to its transcript. An undiscoverable transcript still
		// delivers the turn's output — only the continuation is lost, and
		// the next turn starts fresh.
		resultSessionID := sessionPath
		if sessionDirMode {
			resultSessionID = discoveredPiSessionFile(reportedSessionID)
			if resultSessionID == "" {
				b.cfg.Logger.Warn(label+" transcript not found after run; this turn will not be resumable",
					"session_id", reportedSessionID)
			}
		}
		resCh <- Result{
			Status:     finalStatus,
			Output:     output.String(),
			Error:      finalError,
			DurationMs: duration.Milliseconds(),
			SessionID:  resultSessionID,
			Usage:      usage,
			// Only a run that asked to resume can have had a resume refused.
			// On a cold run the phrase cannot appear anyway, but stating the
			// precondition keeps this honest against Result.ResumeRejected's
			// contract rather than relying on Pi never saying it.
			ResumeRejected: opts.ResumeSessionID != "" && stderrWatch.resumeRefused(),
		}
	}()

	return &Session{
		Messages:         msgCh,
		Result:           resCh,
		TerminalObserved: turnErrors.terminalObserved,
	}, nil
}

func piSessionBusyResult(label, sessionPath string) *Session {
	msgCh := make(chan Message)
	close(msgCh)
	resCh := make(chan Result, 1)
	resCh <- Result{
		Status:                  "failed",
		Error:                   fmt.Sprintf("%s session file %q is already in use by another execution", label, sessionPath),
		ResumeRejectedTransient: true,
	}
	close(resCh)
	return &Session{Messages: msgCh, Result: resCh}
}

// piResumeRefusedMarker is Pi's message when a resumed transcript names a
// working directory that no longer exists. Pi re-anchors a resumed run to the
// cwd recorded in the session header; when that directory is gone it prints
// this to stderr and exits 1 immediately, before any JSON event, any tool call
// and any output (GH #8082).
const piResumeRefusedMarker = "Stored session working directory does not exist"

// piResumeRefusedMarkers are every stderr phrase the Pi family is known to
// print when it refuses to open the transcript a resume pointed at, each
// exiting 1 before any JSON event, any tool call, and any output. The
// daemon's resume gate already declines to hand the CLI such a session, so in
// normal operation none of these fire. They are the second layer, and their
// reach is exactly a refusal arriving on a run the gate let through.
//
// Without them such a run fails as a generic non-retryable process_failure and
// the same stale pointer is served again on the next claim — the permanent
// loop #8082 reported. Each entry is a fixed refusal shape and does not
// generalise to a refusal worded differently.
var piResumeRefusedMarkers = piResumeRefusedSignatures{
	{phrases: []string{piResumeRefusedMarker}},
	// omp ≥ 18.7 refusing an empty transcript. That is the shape of the empty
	// file the pre-18.7 flow pre-creates, so a daemon upgrade landing
	// mid-conversation turns the stored pointer into exactly this refusal.
	{phrases: []string{"Cannot resume session"}},
	// omp ≥ 18.7 refusing a transcript path that no longer exists. The path
	// sits between the two phrases (`Session "<path>" not found.`), so both
	// must appear: matching `" not found.` alone would also flag a model or
	// provider "not found" error, which is not a session problem.
	{phrases: []string{`Session "`, `" not found.`}},
}

// resumeRefused reports whether the retained stderr tail carries evidence of
// one of the known refusals.
func (m piResumeRefusedSignatures) resumeRefused(tail []byte) bool {
	for _, signature := range m {
		found := true
		for _, phrase := range signature.phrases {
			if !bytes.Contains(tail, []byte(phrase)) {
				found = false
				break
			}
		}
		if found {
			return true
		}
	}
	return false
}

type piResumeRefusedSignatures []struct {
	phrases []string
}

// piStderrTailLimit bounds the retained stderr tail. The marker arrives in a
// single write at startup, so this only has to be large enough that a partial
// flush cannot split it apart.
const piStderrTailLimit = 8 << 10

// piStderrWatcher tees Pi's stderr to the debug log while retaining a bounded
// tail to test for a resume refusal. Writes arrive from the child process
// goroutine and resumeRefused is read from the result goroutine after Wait, so
// the buffer is mutex-guarded rather than relying on that ordering.
type piStderrWatcher struct {
	log io.Writer

	mu   sync.Mutex
	tail []byte
}

func newPiStderrWatcher(log io.Writer) *piStderrWatcher {
	return &piStderrWatcher{log: log}
}

func (w *piStderrWatcher) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.tail = append(w.tail, p...)
	if len(w.tail) > piStderrTailLimit {
		w.tail = w.tail[len(w.tail)-piStderrTailLimit:]
	}
	w.mu.Unlock()
	// Never fail the child's stderr write on a logging problem: a short write
	// here makes Pi's own writer error out mid-run.
	_, _ = w.log.Write(p)
	return len(p), nil
}

func (w *piStderrWatcher) resumeRefused() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return piResumeRefusedMarkers.resumeRefused(w.tail)
}

// ── Pi event types ──

// piStreamEvent is the union of fields we consume from Pi's JSON event
// stream. Fields that can be either string or object across event types
// (e.g. `message`, `result`) are held as json.RawMessage and decoded on
// demand by the switch arms.
type piStreamEvent struct {
	Type string `json:"type"`

	// session: the transcript header emitted at run start. Its id ties a
	// dir-mode fresh run to the transcript file the CLI created for it.
	ID string `json:"id,omitempty"`

	// message_update
	AssistantMessageEvent *piAssistantMessageEvent `json:"assistantMessageEvent,omitempty"`

	// tool_execution_start / tool_execution_end
	ToolCallID string          `json:"toolCallId,omitempty"`
	ToolName   string          `json:"toolName,omitempty"`
	Args       json.RawMessage `json:"args,omitempty"`
	Result     json.RawMessage `json:"result,omitempty"`
	IsError    bool            `json:"isError,omitempty"`

	// error: Message is a string. turn_end: Message is an object.
	Message json.RawMessage `json:"message,omitempty"`

	// agent_end: the messages the agent run produced, oldest first.
	Messages json.RawMessage `json:"messages,omitempty"`

	// auto_retry_end
	Success    bool   `json:"success,omitempty"`
	FinalError string `json:"finalError,omitempty"`
}

type piAssistantMessageEvent struct {
	Type  string `json:"type"`
	Delta string `json:"delta,omitempty"`
}

type piMessage struct {
	Role  string   `json:"role,omitempty"`
	Model string   `json:"model,omitempty"`
	Usage *piUsage `json:"usage,omitempty"`

	// turn_end carries the terminal state of the turn, and agent_end's last
	// assistant message that of the run. Pi sets StopReason to "error" for a
	// provider call it could not complete, whether or not it goes on to retry,
	// and puts the provider's message in ErrorMessage.
	StopReason   string `json:"stopReason,omitempty"`
	ErrorMessage string `json:"errorMessage,omitempty"`
}

type piUsage struct {
	Input       int64 `json:"input"`
	Output      int64 `json:"output"`
	CacheRead   int64 `json:"cacheRead"`
	CacheWrite  int64 `json:"cacheWrite"`
	TotalTokens int64 `json:"totalTokens"`
}

func decodePiMessage(raw json.RawMessage) *piMessage {
	if len(raw) == 0 {
		return nil
	}
	var m piMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	return &m
}

// lastPiAssistantMessage returns the newest assistant message in agent_end's
// messages, or nil when there is none. Messages are decoded one at a time so
// an unexpected shape elsewhere in the run cannot hide the last one.
func lastPiAssistantMessage(raw json.RawMessage) *piMessage {
	var messages []json.RawMessage
	if err := json.Unmarshal(raw, &messages); err != nil {
		return nil
	}
	for i := len(messages) - 1; i >= 0; i-- {
		if m := decodePiMessage(messages[i]); m != nil && m.Role == "assistant" {
			return m
		}
	}
	return nil
}

func decodePiString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}

func decodePiResult(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// ── Arg builder ──

// piBlockedArgs are flags hardcoded by the daemon that must not be
// overridden by user-configured custom_args. Overriding these would
// break the daemon↔Pi communication protocol.
var piBlockedArgs = map[string]blockedArgMode{
	"-p":           blockedStandalone, // non-interactive mode
	"--print":      blockedStandalone, // alias for -p
	"--mode":       blockedWithValue,  // "json" event stream protocol
	"--session":    blockedWithValue,  // daemon manages the session path
	"--session-dir": blockedWithValue, // daemon manages where transcripts live
	"--thinking":   blockedWithValue,  // owned by agent.thinking_level
}

// piCustomArgModes mirrors Pi 0.83's built-in parser closely enough to
// distinguish option values from positional messages. Unknown long flags are
// extension flags and may take one optional value; unknown short flags are left
// intact so Pi can report them as it did before.
var piCustomArgModes = map[string]blockedArgMode{
	"--help":                 blockedStandalone,
	"-h":                     blockedStandalone,
	"--version":              blockedStandalone,
	"-v":                     blockedStandalone,
	"--continue":             blockedStandalone,
	"-c":                     blockedStandalone,
	"--resume":               blockedStandalone,
	"-r":                     blockedStandalone,
	"--provider":             blockedWithValue,
	"--model":                blockedWithValue,
	"--api-key":              blockedWithValue,
	"--system-prompt":        blockedWithValue,
	"--append-system-prompt": blockedWithValue,
	"--name":                 blockedWithValue,
	"-n":                     blockedWithValue,
	"--no-session":           blockedStandalone,
	"--session-id":           blockedWithValue,
	"--fork":                 blockedWithValue,
	"--session-dir":          blockedWithValue,
	"--models":               blockedWithValue,
	"--no-tools":             blockedStandalone,
	"-nt":                    blockedStandalone,
	"--no-builtin-tools":     blockedStandalone,
	"-nbt":                   blockedStandalone,
	"--tools":                blockedWithValue,
	"-t":                     blockedWithValue,
	"--exclude-tools":        blockedWithValue,
	"-xt":                    blockedWithValue,
	"--thinking":             blockedWithValue,
	"--export":               blockedWithValue,
	"--extension":            blockedWithValue,
	"-e":                     blockedWithValue,
	"--no-extensions":        blockedStandalone,
	"-ne":                    blockedStandalone,
	"--skill":                blockedWithValue,
	"--prompt-template":      blockedWithValue,
	"--theme":                blockedWithValue,
	"--no-skills":            blockedStandalone,
	"-ns":                    blockedStandalone,
	"--no-prompt-templates":  blockedStandalone,
	"-np":                    blockedStandalone,
	"--no-themes":            blockedStandalone,
	"--no-context-files":     blockedStandalone,
	"-nc":                    blockedStandalone,
	"--list-models":          blockedOptionalValue,
	"--verbose":              blockedStandalone,
	"--approve":              blockedStandalone,
	"-a":                     blockedStandalone,
	"--no-approve":           blockedStandalone,
	"-na":                    blockedStandalone,
	"--offline":              blockedStandalone,
}

// buildPiArgs assembles the argv for a one-shot Pi invocation.
//
// Flags:
//
//	-p                          non-interactive mode (prompt arrives on stdin)
//	--mode json                 emit one JSON event per line on stdout
//	--session <path>            session log file (created upfront, reused on resume)
//	--session-dir <dir>         dir the CLI creates its own transcript in, when
//	                            it owns the naming (omp ≥ 18.7); mutually
//	                            exclusive with --session
//	--model <selector>          model selector, passed through verbatim
//	--thinking <level>          per-agent reasoning level override
//
// The prompt is deliberately absent from argv. Pi reads a non-TTY stdin in
// print/JSON mode; using that supported path prevents Windows PowerShell's npm
// shim from re-tokenising prompt content into options.
func buildPiArgs(sessionPath, sessionDir string, opts ExecOptions, logger *slog.Logger) []string {
	args := []string{
		"-p",
		"--mode", "json",
	}
	if sessionPath != "" {
		args = append(args, "--session", sessionPath)
	} else if sessionDir != "" {
		args = append(args, "--session-dir", sessionDir)
	}
	// The selector goes to --model whole, and --provider is never synthesized.
	// Pi's own resolver already accepts every shape we hold: a canonical
	// `provider/id`, a bare id, and — crucially — an id that itself contains a
	// slash, which is the normal case for gateway-style providers whose model
	// ids look like `claude/claude-opus-5`. Splitting on the first slash to
	// fill --provider turns that id into a provider name Pi has never heard of,
	// and an unknown --provider is a hard error ("Unknown provider ...") rather
	// than something Pi can recover from — whereas --model alone falls back to
	// matching the full string as a raw model id. Passing less is strictly more
	// capable here (GH #7300).
	if model := strings.TrimSpace(opts.Model); model != "" {
		args = append(args, "--model", model)
	}
	if opts.ThinkingLevel != "" {
		args = append(args, "--thinking", opts.ThinkingLevel)
	}
	// Note: we intentionally do NOT pass --tools here. Omitting it lets
	// Pi use its full tool registry, including user-installed extension
	// tools. Passing --tools acts as a restrictive allowlist that
	// silently filters out extension-registered tools (#2379).
	// Users who want to restrict tools can do so via custom_args.
	//
	// SystemPrompt is intentionally not forwarded as --append-system-prompt:
	// Pi loads the per-task AGENTS.md the daemon writes into the workdir, so
	// inlining the same runtime brief would duplicate it on every turn.
	// Verified against Pi 0.67.2 (MUL-5392).
	args = append(args, filterPiCustomArgs(opts.CustomArgs, logger)...)
	return args
}

// filterPiCustomArgs removes @file and positional message inputs from
// custom_args. Once the task prompt is delivered on stdin, Pi concatenates the
// first positional message to stdin with no separator. Keeping those tokens
// would silently mutate or replace the task prompt. Option values remain
// supported, including values for extension-provided --long flags.
func filterPiCustomArgs(args []string, logger *slog.Logger) []string {
	args = filterCustomArgs(args, piBlockedArgs, logger)
	filtered := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if strings.HasPrefix(arg, "@") {
			if logger != nil {
				logger.Warn("custom_args: Pi file input would alter the stdin task prompt, skipping")
			}
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			if logger != nil {
				logger.Warn("custom_args: Pi positional input would alter the stdin task prompt, skipping")
			}
			continue
		}

		flag := arg
		hasInlineValue := false
		if idx := strings.Index(arg, "="); idx > 0 {
			flag = arg[:idx]
			hasInlineValue = true
		}
		filtered = append(filtered, arg)
		if hasInlineValue {
			continue
		}

		mode, known := piCustomArgModes[flag]
		if !known {
			if strings.HasPrefix(flag, "--") {
				mode = blockedOptionalValue
			} else {
				continue
			}
		}
		switch mode {
		case blockedWithValue:
			if i+1 < len(args) {
				filtered = append(filtered, args[i+1])
				i++
			}
		case blockedOptionalValue:
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") && !strings.HasPrefix(args[i+1], "@") {
				filtered = append(filtered, args[i+1])
				i++
			}
		}
	}
	return filtered
}

// ── Session path ──

// piSessionDir returns the directory where Pi session JSONL files live.
// Exported via a helper so the usage scanner (package usage) can point at
// the same location without duplicating the path construction.
func piSessionDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".multica", "pi-sessions"), nil
}

func newPiSessionPath() (string, error) {
	dir, err := piSessionDir()
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s.jsonl", time.Now().UTC().Format("20060102T150405.000000000"))
	return filepath.Join(dir, name), nil
}

// ensurePiSessionFile creates an empty session file if one does not yet
// exist at path. Legacy CLIs refuse to start when --session points at a
// missing file; paths that already exist (a resumed session) are left
// untouched. Resume-only CLIs (omp ≥ 18.7) never take this path on a fresh
// run — they create the transcript themselves inside --session-dir.
func ensurePiSessionFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	return f.Close()
}

// ompResumeOnlySession is the first omp release where `--session <path>`
// stopped creating transcripts and became an alias of `--resume`: a missing
// file fails with "Session not found" and the legacy flow's pre-created empty
// file with "Cannot resume session ... holds no entries". Empirically
// bisected: 18.6.3 create-or-append; 18.7.0, 18.8.6, 18.8.7 and 18.9.1
// resume-only.
var ompResumeOnlySession = [3]int{18, 7, 0}

// piResumeOnlySession reports whether the configured CLI needs dir-based
// session creation: the built-in omp runtime at or above ompResumeOnlySession.
// Everything else — pi 0.x, omp ≤ 18.6, a custom runtime profile whose command
// merely resembles omp, and any CLI whose version could not be detected —
// keeps the legacy --session flow, which is the behavior each of those is
// verified against. Failing closed on an undetectable version keeps an
// unknown wrapper on today's proven path rather than on an experiment.
func piResumeOnlySession(cfg Config) bool {
	if !cfg.BuiltinRuntime {
		return false
	}
	version, ok := parseSemverTriple(cfg.CLIVersion)
	if !ok {
		return false
	}
	return semverAtLeast(version, ompResumeOnlySession)
}

// parseSemverTriple extracts the first major.minor[.patch] triple from a CLI
// version string ("omp/18.8.7", "18.8.7", "pi 0.83.0"). A missing patch
// defaults to zero.
func parseSemverTriple(s string) ([3]int, bool) {
	var triple [3]int
	m := semverTripleRE.FindStringSubmatch(s)
	if m == nil {
		return triple, false
	}
	triple[0], _ = strconv.Atoi(m[1])
	triple[1], _ = strconv.Atoi(m[2])
	if m[3] != "" {
		triple[2], _ = strconv.Atoi(m[3])
	}
	return triple, true
}

// semverTripleRE anchors each side on a non-digit (or the string ends) so a
// token inside a longer number ("1.2.345") is not read as "1.2.345"→(1,2,34).
var semverTripleRE = regexp.MustCompile(`(?:^|[^\d])(\d{1,3})\.(\d{1,3})(?:\.(\d{1,3}))?(?:[^\d]|$)`)

func semverAtLeast(v, min [3]int) bool {
	for i := range v {
		if v[i] != min[i] {
			return v[i] > min[i]
		}
	}
	return true
}

// discoveredPiSessionFile locates the transcript the CLI created for a
// dir-mode fresh run. omp writes `<timestamp>_<session id>.jsonl` directly
// inside --session-dir (verified on 18.6.3 and 18.9.1), so the session
// header event's id pins the run's file even while other fresh runs are
// creating transcripts in the same directory. The run's artifacts live in a
// sibling directory that shares the transcript's name, so a match only
// counts when it is a regular file. No regular match — a CLI that changed
// its layout, or a header event it never emitted — returns "", and the
// caller degrades to a non-resumable turn rather than failing the run.
func discoveredPiSessionFile(headerID string) string {
	if headerID == "" {
		return ""
	}
	dir, err := piSessionDir()
	if err != nil {
		return ""
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*_"+headerID+".jsonl"))
	if err != nil {
		return ""
	}
	newest := ""
	var newestMod time.Time
	for _, match := range matches {
		info, err := os.Stat(match)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if newest == "" || info.ModTime().After(newestMod) {
			newest, newestMod = match, info.ModTime()
		}
	}
	return newest
}

// PiSessionDir exposes piSessionDir to other packages in this module.
func PiSessionDir() (string, error) {
	return piSessionDir()
}
