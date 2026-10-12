package agent

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestPiResumeOnlySessionGate pins which configurations take the dir-based
// fresh-session flow. Only the built-in omp runtime at or above the first
// resume-only release switches; everything else — older omp, pi 0.x, custom
// runtime profiles, undetectable versions — stays on the legacy --session
// flow each of those CLIs is verified against.
func TestPiResumeOnlySessionGate(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		version string
		builtin bool
		want    bool
	}{
		{version: "omp/18.7.0", builtin: true, want: true},
		{version: "omp/18.8.7", builtin: true, want: true},
		{version: "omp/18.9.1", builtin: true, want: true},
		{version: "omp/19.0.0", builtin: true, want: true},
		{version: "18.7.0", builtin: true, want: true},
		{version: "omp/18.6.3", builtin: true, want: false},
		{version: "18.6.3", builtin: true, want: false},
		{version: "omp/17.2.12", builtin: true, want: false},
		{version: "pi 0.83.0", builtin: true, want: false},
		// An undetectable version must not move an unknown CLI off the
		// legacy flow: detectCLIVersion salvages banner output on a broken
		// install, and that text proves nothing about flag semantics.
		{version: "", builtin: true, want: false},
		{version: "initializing plugins", builtin: true, want: false},
		// A custom runtime profile keeps its protocol family as the provider,
		// so BuiltinRuntime is what scopes the exception to the vendor binary
		// whose behavior was actually verified.
		{version: "omp/18.8.7", builtin: false, want: false},
	} {
		got := piResumeOnlySession(Config{CLIVersion: tc.version, BuiltinRuntime: tc.builtin})
		if got != tc.want {
			t.Errorf("piResumeOnlySession({CLIVersion: %q, BuiltinRuntime: %v}) = %v, want %v",
				tc.version, tc.builtin, got, tc.want)
		}
	}
}

// TestBuildPiArgsSessionDirMode pins the dir-mode argv: --session-dir instead
// of --session, nothing else moved.
func TestBuildPiArgsSessionDirMode(t *testing.T) {
	t.Parallel()
	args := buildPiArgs("", "/tmp/pi-sessions", ExecOptions{
		Model:         "anthropic/claude-sonnet-4",
		ThinkingLevel: "high",
	}, slog.Default())

	joined := strings.Join(args, " ")
	for _, want := range []string{"-p", "--mode json", "--session-dir /tmp/pi-sessions", "--model anthropic/claude-sonnet-4", "--thinking high"} {
		if !strings.Contains(joined, want) {
			t.Errorf("expected %q in args, got: %v", want, args)
		}
	}
	for i, arg := range args {
		if arg == "--session" {
			t.Errorf("dir mode must not pass --session (arg %d of %v)", i, args)
		}
	}
}

// TestBuildPiArgsBlocksCustomSessionLocation pins that user custom_args can
// neither override the daemon-owned --session-dir nor reintroduce --session
// in either flow: the daemon owns where transcripts live.
func TestBuildPiArgsBlocksCustomSessionLocation(t *testing.T) {
	t.Parallel()
	args := buildPiArgs("", "/tmp/owned", ExecOptions{
		CustomArgs: []string{"--session-dir", "/tmp/evil", "--session", "/tmp/evil.jsonl"},
	}, slog.Default())

	joined := strings.Join(args, "\x00")
	if strings.Contains(joined, "/tmp/evil") {
		t.Errorf("custom session location leaked into args: %v", args)
	}
	if strings.Count(joined, "--session-dir") != 1 {
		t.Errorf("expected exactly the daemon-owned --session-dir, got: %v", args)
	}
}

// TestDiscoveredPiSessionFilePrefersRegularFile exercises the discovery glob
// against the two decoys a real session directory holds: the artifacts
// directory that shares the transcript's name, and subagent child transcripts
// nested inside it. Only the transcript itself may win.
func TestDiscoveredPiSessionFilePrefersRegularFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("HOME-based path isolation is POSIX-only")
	}
	t.Setenv("HOME", t.TempDir())

	dir, err := piSessionDir()
	if err != nil {
		t.Fatalf("piSessionDir: %v", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir session dir: %v", err)
	}

	const id = "01testsession000000000000000"
	artifacts := filepath.Join(dir, "2026-01-01T00-00-00-000Z_"+id+".jsonl")
	if err := os.MkdirAll(artifacts, 0o755); err != nil {
		t.Fatalf("mkdir artifacts dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(artifacts, "child.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write child transcript: %v", err)
	}
	want := filepath.Join(dir, "2026-01-02T00-00-00-000Z_"+id+".jsonl")
	if err := os.WriteFile(want, []byte("{}\n"), 0o644); err != nil {
		t.Fatalf("write transcript: %v", err)
	}

	if got := discoveredPiSessionFile(id); got != want {
		t.Errorf("discoveredPiSessionFile(%q) = %q, want %q", id, got, want)
	}
	if got := discoveredPiSessionFile(""); got != "" {
		t.Errorf("discoveredPiSessionFile(\"\") = %q, want empty", got)
	}
	if got := discoveredPiSessionFile("01missing"); got != "" {
		t.Errorf("discoveredPiSessionFile(unknown id) = %q, want empty", got)
	}
}

// ompDirSessionFixture is a fake omp ≥ 18.7: it refuses --session exactly as
// the real CLI would, requires --session-dir, emits the transcript header
// event the real CLI emits, and creates the transcript file named the way the
// real CLI names it.
func ompDirSessionFixture(withHeader bool) string {
	var b strings.Builder
	b.WriteString("#!/bin/sh\n")
	b.WriteString("cat > /dev/null\n")
	b.WriteString("dir=\n")
	b.WriteString("for a in \"$@\"; do\n")
	b.WriteString("  case \"$a\" in\n")
	b.WriteString("    --session) echo 'unexpected --session flag' >&2; exit 3;;\n")
	b.WriteString("    --session-dir=*) dir=${a#--session-dir=};;\n")
	b.WriteString("  esac\n")
	b.WriteString("done\n")
	// The daemon passes the value as a separate argv token.
	b.WriteString("prev=\n")
	b.WriteString("for a in \"$@\"; do\n")
	b.WriteString("  [ \"$prev\" = '--session-dir' ] && dir=\"$a\"\n")
	b.WriteString("  prev=$a\n")
	b.WriteString("done\n")
	b.WriteString("if [ -z \"$dir\" ]; then echo 'missing --session-dir' >&2; exit 3; fi\n")
	// The real omp creates the session dir itself (ensureDirSync).
	b.WriteString("mkdir -p \"$dir\"\n")
	if withHeader {
		b.WriteString("printf '%s\\n' '{\"type\":\"session\",\"version\":3,\"id\":\"01dirsession00000000000000\",\"cwd\":\"/tmp\"}'\n")
	}
	b.WriteString("printf '%s\\n' '{\"type\":\"agent_start\"}'\n")
	b.WriteString("printf '%s\\n' '{\"type\":\"turn_end\",\"message\":{\"role\":\"assistant\",\"model\":\"test\",\"usage\":{\"input\":1,\"output\":1}}}'\n")
	b.WriteString(": > \"$dir/2026-10-12T00-00-00-000Z_01dirsession00000000000000.jsonl\"\n")
	b.WriteString("exit 0\n")
	return b.String()
}

// TestOmpExecuteFreshDirSessionAdoptsTranscript is the end-to-end contract of
// the omp ≥ 18.7 fix: a fresh run goes out with --session-dir (never
// --session), and the transcript the CLI created — located through the
// session header event's id — comes back as the opaque SessionID the daemon
// stores and hands back as ResumeSessionID on the next turn.
func TestOmpExecuteFreshDirSessionAdoptsTranscript(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	t.Setenv("HOME", t.TempDir())

	fakePath := filepath.Join(t.TempDir(), "omp")
	writeTestExecutable(t, fakePath, []byte(ompDirSessionFixture(true)))

	backend, err := ResolveBackend("omp", Config{
		ExecutablePath: fakePath,
		CLIVersion:     "omp/18.8.7",
		BuiltinRuntime: true,
		Logger:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("ResolveBackend(omp): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "test prompt", ExecOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()

	var result Result
	select {
	case result = <-session.Result:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for result")
	}
	if result.Status != "completed" {
		t.Fatalf("status = %q (error=%q), want completed", result.Status, result.Error)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	want := filepath.Join(home, ".multica", "pi-sessions", "2026-10-12T00-00-00-000Z_01dirsession00000000000000.jsonl")
	if result.SessionID != want {
		t.Errorf("SessionID = %q, want adopted transcript %q", result.SessionID, want)
	}
}

// TestOmpExecuteFreshDirSessionWithoutHeaderStaysUnresumable pins the
// degradation when the CLI never reports its session id: the turn's output is
// still delivered, and the empty SessionID tells the daemon this turn has no
// continuation instead of inventing a path.
func TestOmpExecuteFreshDirSessionWithoutHeaderStaysUnresumable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	t.Setenv("HOME", t.TempDir())

	fakePath := filepath.Join(t.TempDir(), "omp")
	writeTestExecutable(t, fakePath, []byte(ompDirSessionFixture(false)))

	backend, err := ResolveBackend("omp", Config{
		ExecutablePath: fakePath,
		CLIVersion:     "omp/18.8.7",
		BuiltinRuntime: true,
		Logger:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("ResolveBackend(omp): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "test prompt", ExecOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()

	var result Result
	select {
	case result = <-session.Result:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for result")
	}
	if result.Status != "completed" {
		t.Fatalf("status = %q (error=%q), want completed", result.Status, result.Error)
	}
	if result.SessionID != "" {
		t.Errorf("SessionID = %q, want empty when the CLI reported no session id", result.SessionID)
	}
}

// TestOmpExecuteLegacyFreshSessionKeepsNamedFile pins that a pre-18.7 omp
// keeps today's flow end to end: the daemon still names the file, still
// passes --session, and still echoes that path as SessionID.
func TestOmpExecuteLegacyFreshSessionKeepsNamedFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fixture is POSIX-only")
	}
	t.Setenv("HOME", t.TempDir())

	// A legacy fake that refuses to run without --session, mirroring the
	// documented legacy refusal at a missing session file.
	script := "#!/bin/sh\n" +
		"cat > /dev/null\n" +
		"has=0\n" +
		"for a in \"$@\"; do [ \"$a\" = '--session' ] && has=1; done\n" +
		"if [ \"$has\" = '0' ]; then echo 'missing --session flag' >&2; exit 3; fi\n" +
		"printf '%s\\n' '{\"type\":\"agent_start\"}'\n" +
		"printf '%s\\n' '{\"type\":\"turn_end\",\"message\":{\"role\":\"assistant\",\"model\":\"test\",\"usage\":{\"input\":1,\"output\":1}}}'\n" +
		"exit 0\n"
	fakePath := filepath.Join(t.TempDir(), "omp")
	writeTestExecutable(t, fakePath, []byte(script))

	backend, err := ResolveBackend("omp", Config{
		ExecutablePath: fakePath,
		CLIVersion:     "omp/18.6.3",
		BuiltinRuntime: true,
		Logger:         slog.Default(),
	})
	if err != nil {
		t.Fatalf("ResolveBackend(omp): %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := backend.Execute(ctx, "test prompt", ExecOptions{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	go func() {
		for range session.Messages {
		}
	}()

	var result Result
	select {
	case result = <-session.Result:
	case <-time.After(10 * time.Second):
		t.Fatal("timeout waiting for result")
	}
	if result.Status != "completed" {
		t.Fatalf("status = %q (error=%q), want completed", result.Status, result.Error)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("UserHomeDir: %v", err)
	}
	if dir := filepath.Dir(result.SessionID); dir != filepath.Join(home, ".multica", "pi-sessions") {
		t.Errorf("SessionID dir = %q, want %q", dir, filepath.Join(home, ".multica", "pi-sessions"))
	}
	if base := filepath.Base(result.SessionID); !strings.HasSuffix(base, ".jsonl") || strings.Contains(base, "_01dirsession") {
		t.Errorf("SessionID base = %q, want a daemon-named transcript", base)
	}
}
