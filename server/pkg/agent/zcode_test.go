package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNewReturnsZcodeBackend(t *testing.T) {
	t.Parallel()
	backend, err := New("zcode", Config{ExecutablePath: "/nonexistent/zcode"})
	if err != nil {
		t.Fatalf("New(zcode): %v", err)
	}
	if _, ok := backend.(*zcodeBackend); !ok {
		t.Fatalf("New(zcode) = %T, want *zcodeBackend", backend)
	}
}

func TestBuildZcodeArgsKeepsProtocolManaged(t *testing.T) {
	t.Parallel()
	args := buildZcodeArgs("the task prompt", ExecOptions{
		ResumeSessionID: "sess-keep",
		ExtraArgs:       []string{"--verbose", "--no-color"},
		CustomArgs: []string{
			"--prompt=replace", "-p", "injected", "--output-format", "text", "--json",
			"--resume", "sess-other", "--continue", "--mode", "plan", "--cwd", "/elsewhere",
		},
	}, slog.Default())
	joined := strings.Join(args, " ")
	for _, forbidden := range []string{"replace", "injected", "text", "sess-other", "--continue", "plan", "/elsewhere", "--json"} {
		if strings.Contains(joined, forbidden) {
			t.Fatalf("managed argument %q leaked into %v", forbidden, args)
		}
	}
	wantPrefix := []string{"--output-format", "stream-json", "--mode", "yolo"}
	for i, want := range wantPrefix {
		if args[i] != want {
			t.Fatalf("args[%d] = %q, want %q; all=%v", i, args[i], want, args)
		}
	}
	if !strings.Contains(joined, "--verbose") || !strings.Contains(joined, "--no-color") {
		t.Fatalf("non-managed custom args missing from %v", args)
	}
	// ZCode reads no prompt from stdin, so -p with the task prompt is the one
	// legitimate -p in argv — but it must carry OUR prompt, exactly once, and
	// the daemon-owned --mode yolo must survive whatever custom args contain.
	if count := strings.Count(joined, "--mode"); count != 1 {
		t.Fatalf("--mode count = %d in %v, want exactly 1 (daemon-owned)", count, args)
	}
	if args[len(args)-2] != "-p" || args[len(args)-1] != "the task prompt" {
		t.Fatalf("prompt must be delivered last via -p, got tail %v", args[len(args)-3:])
	}
}

func fakeZcodeScript() string {
	return `#!/bin/sh
if [ -n "$ZCODE_ARGS_FILE" ]; then printf '%s\n' "$@" > "$ZCODE_ARGS_FILE"; fi
case "$ZCODE_MODE" in
  error)
    printf '%s\n' '{"type":"turn.failed","payload":{"error":"synthetic ZCode provider failure"},"sessionId":"sess-zcode-err"}'
    printf '%s\n' '{"type":"result","sessionId":"sess-zcode-err","projection":{"status":"error"}}'
    ;;
  exit)
    echo 'synthetic zcode stderr' >&2
    exit 7
    ;;
  resume-missing)
    cat "$ZCODE_STDERR_FIXTURE" >&2
    exit 1
    ;;
  spin)
    while :; do :; done
    ;;
  *)
    printf '%s\n' '{"type":"session.created","payload":{},"sessionId":"sess-zcode-1","seq":0}'
    printf '%s\n' '{"type":"turn.started","payload":{},"sessionId":"sess-zcode-1","seq":1}'
    printf '%s\n' '{"type":"tool.updated","payload":{"kind":"scheduled","toolCallId":"call-1","toolName":"Bash","input":{"command":"ls","description":"List files"}},"sessionId":"sess-zcode-1","seq":2}'
    printf '%s\n' '{"type":"tool.updated","payload":{"kind":"result","toolCallId":"call-1","output":"file1\nfile2"},"sessionId":"sess-zcode-1","seq":3}'
    printf '%s\n' '{"type":"turn.completed","payload":{"response":"PONG","resultType":"success","usage":{"inputTokens":10,"outputTokens":2,"cacheReadTokens":3}},"sessionId":"sess-zcode-1","seq":4}'
    printf '%s\n' '{"type":"result","sessionId":"sess-zcode-1","response":"PONG","usage":{"inputTokens":20,"outputTokens":4,"cacheReadTokens":6},"eventCount":5,"projection":{"status":"completed","turnCount":1,"totalTokenCount":24}}'
    ;;
esac
`
}

func newFakeZcodeBackend(t *testing.T, env map[string]string) *zcodeBackend {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is POSIX-only")
	}
	path := filepath.Join(t.TempDir(), "zcode")
	writeTestExecutable(t, path, []byte(fakeZcodeScript()))
	return &zcodeBackend{cfg: Config{ExecutablePath: path, Logger: slog.Default(), Env: env}}
}

func awaitZcodeResult(t *testing.T, session *Session) ([]Message, Result) {
	t.Helper()
	var messages []Message
	for message := range session.Messages {
		messages = append(messages, message)
	}
	select {
	case result, ok := <-session.Result:
		if !ok {
			t.Fatal("result channel closed without a result")
		}
		return messages, result
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for zcode result")
		return nil, Result{}
	}
}

func TestZcodeBackendStreamsNativeEvents(t *testing.T) {
	t.Parallel()
	backend := newFakeZcodeBackend(t, nil)
	session, err := backend.Execute(context.Background(), "reply PONG", ExecOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	messages, result := awaitZcodeResult(t, session)
	if result.Status != "completed" || result.Output != "PONG" || result.SessionID != "sess-zcode-1" {
		t.Fatalf("unexpected result: %+v", result)
	}
	// ZCode has no model flag, so the run total lands under the "unknown" key
	// (mcode precedent) with cache reads split out of input.
	usage := result.Usage["unknown"]
	if usage.InputTokens != 14 || usage.OutputTokens != 4 || usage.CacheReadTokens != 6 {
		t.Fatalf("unexpected final usage: %+v", usage)
	}
	var status, toolUse, toolResult, text bool
	for _, message := range messages {
		switch message.Type {
		case MessageStatus:
			status = message.Status == "running" && message.SessionID == "sess-zcode-1"
		case MessageToolUse:
			toolUse = message.Tool == "Bash" && message.CallID == "call-1" && message.Input["command"] == "ls"
		case MessageToolResult:
			toolResult = message.CallID == "call-1" && message.Output == "file1\nfile2"
		case MessageText:
			text = message.Content == "PONG"
		}
	}
	if !status || !toolUse || !toolResult || !text {
		t.Fatalf("missing native events status=%v toolUse=%v toolResult=%v text=%v; messages=%+v", status, toolUse, toolResult, text, messages)
	}
}

func TestZcodeBackendPreservesSuccessfulResumeSession(t *testing.T) {
	t.Parallel()
	backend := newFakeZcodeBackend(t, nil)
	session, err := backend.Execute(context.Background(), "continue task", ExecOptions{
		ResumeSessionID: "sess-zcode-1", Timeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_, result := awaitZcodeResult(t, session)
	if result.Status != "completed" || result.Output != "PONG" || result.SessionID != "sess-zcode-1" {
		t.Fatalf("resumed result = %+v", result)
	}
}

// TestZcodeBackendDeliversPromptViaArgv guards the zcode-specific delivery
// contract: the CLI reads no stdin, so the prompt rides in argv and must
// arrive intact — embedded quotes, em dash, shell metacharacters — because
// Go's exec passes each argv element verbatim. The fake script dumps its argv
// one token per line; a round-trip through a shell here would be the exact
// corruption this test exists to catch.
func TestZcodeBackendDeliversPromptViaArgv(t *testing.T) {
	t.Parallel()
	argsPath := filepath.Join(t.TempDir(), "zcode.args")
	backend := newFakeZcodeBackend(t, map[string]string{"ZCODE_ARGS_FILE": argsPath})
	prompt := `go build -ldflags "-X main.version=foo" — mind the em dash & the quotes"`
	session, err := backend.Execute(context.Background(), prompt, ExecOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	_, result := awaitZcodeResult(t, session)
	if result.Status != "completed" {
		t.Fatalf("result = %+v", result)
	}
	data, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read captured zcode args: %v", err)
	}
	args := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(args) < 2 || args[len(args)-2] != "-p" || args[len(args)-1] != prompt {
		t.Fatalf("prompt not delivered intact via -p: %q", args)
	}
}

func TestZcodeBackendFailureTimeoutAndCancellation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture is POSIX-only")
	}
	resumeFixture, err := filepath.Abs(filepath.Join("testdata", "zcode-0.16.9-resume-not-found.stderr.txt"))
	if err != nil {
		t.Fatalf("resolve resume fixture: %v", err)
	}
	for _, tc := range []struct {
		name               string
		env                map[string]string
		opts               ExecOptions
		status             string
		needle             string
		wantSession        string
		wantResumeRejected bool
	}{
		{"turn failed", map[string]string{"ZCODE_MODE": "error"}, ExecOptions{}, "failed", "synthetic ZCode provider failure", "sess-zcode-err", false},
		{"process error", map[string]string{"ZCODE_MODE": "exit"}, ExecOptions{}, "failed", "synthetic zcode stderr", "", false},
		{"missing resume", map[string]string{"ZCODE_MODE": "resume-missing", "ZCODE_STDERR_FIXTURE": resumeFixture}, ExecOptions{ResumeSessionID: "sess-redacted"}, "failed", "Session not found", "", true},
		{"timeout", map[string]string{"ZCODE_MODE": "spin"}, ExecOptions{Timeout: 20 * time.Millisecond}, "timeout", "timed out", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			backend := newFakeZcodeBackend(t, tc.env)
			session, err := backend.Execute(context.Background(), "task", tc.opts)
			if err != nil {
				t.Fatalf("Execute: %v", err)
			}
			_, result := awaitZcodeResult(t, session)
			if result.Status != tc.status || !strings.Contains(result.Error, tc.needle) || result.SessionID != tc.wantSession {
				t.Fatalf("result = %+v, want status=%q error containing %q session=%q", result, tc.status, tc.needle, tc.wantSession)
			}
			if result.ResumeRejected != tc.wantResumeRejected {
				t.Fatalf("ResumeRejected = %v, want %v (error=%q)", result.ResumeRejected, tc.wantResumeRejected, result.Error)
			}
		})
	}
}

func TestZcodeCode0169FixtureParses(t *testing.T) {
	t.Parallel()
	data, err := os.ReadFile(filepath.Join("testdata", "zcode-0.16.9-stream-json.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	state := zcodeStreamState{usage: make(map[string]TokenUsage)}
	messages := make(chan Message, 16)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var event zcodeStreamEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("fixture event: %v\n%s", err, line)
		}
		handleZcodeEvent(event, messages, &state)
	}
	if !state.sawResult || state.resultIsError || state.sessionID != "sess_3bb1304f-redacted" || state.finalResultText != "DONE" {
		t.Fatalf("unexpected fixture state: %+v", state)
	}
	if state.usage["unknown"] != (TokenUsage{InputTokens: 600, OutputTokens: 120, CacheReadTokens: 900}) {
		t.Fatalf("fixture usage = %+v", state.usage)
	}
}
