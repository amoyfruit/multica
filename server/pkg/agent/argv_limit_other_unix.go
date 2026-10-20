//go:build unix && !linux

package agent

// maxInlinePromptArgBytes is 0 on the other unixes: they cap the TOTAL size
// of argv+env (ARG_MAX) rather than any single string, and where that total is
// exceeded fork/exec still answers E2BIG — which wrapArgvLimitError already
// names. Refusing a 200 KB prompt up front on a platform with a 1 MB total
// budget would block runs that work.
const maxInlinePromptArgBytes = 0
