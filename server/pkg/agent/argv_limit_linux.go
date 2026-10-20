//go:build linux

package agent

// maxInlinePromptArgBytes is Linux's MAX_ARG_STRLEN: a single argv string may
// not exceed 32 * PAGE_SIZE = 131072 bytes, so 131071 is the largest string
// that ever forks successfully (measured on an x86_64 host; 131072 fails with
// E2BIG regardless of the total ARG_MAX headroom).
const maxInlinePromptArgBytes = 131071
