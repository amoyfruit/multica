package agent

import "log/slog"

// chooseZcodeInvocation selects the actual program (argv[0]) and the full argv
// to spawn a ZCode run.
//
// ZCode ships as a Node bundle (zcode.cjs) inside the desktop app rather than
// as a native binary, so a resolved .cjs/.mjs path runs under `node`. Execute
// has already verified node exists by the time this runs. Shims and native
// builds pass through unchanged.
func chooseZcodeInvocation(execName, lookedUp string, args []string, _ *slog.Logger) (string, []string) {
	if isNodeScriptPath(lookedUp) {
		return "node", append([]string{lookedUp}, args...)
	}
	return execName, args
}
