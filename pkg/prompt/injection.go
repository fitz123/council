package prompt

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Sentinel errors for forgery and injection detection (ADR-0008). Callers
// should use errors.Is for matching; the wrapped errors carry context
// (matched delimiter line, nonce literal) for operator diagnostics.
var (
	// ErrNonceLeakage is returned when an LLM-sourced output contains the
	// session nonce as a substring. Treat as a hard reject: either the
	// expert echoed the nonce back (prompt echo), or it attempted to forge
	// a matching fence.
	ErrNonceLeakage = errors.New("prompt: session nonce leaked in LLM output")

	// ErrForgedFence is returned when an LLM-sourced output contains any
	// line-anchored delimiter of the form `^=== .* ===$`. This catches
	// forged open fences, close fences, and global section markers
	// regardless of whether the LLM also emitted the correct nonce.
	ErrForgedFence = errors.New("prompt: forged delimiter fence in LLM output")

	// ErrInjectionSuspected is returned by ScanQuestionForInjection when
	// the operator-supplied question contains a delimiter-shaped line.
	// Surfaced as verdict.status = "injection_suspected_in_question".
	ErrInjectionSuspected = errors.New("prompt: injection suspected in operator question")
)

// delimiterLineRE matches any line that looks like a prompt-protocol fence:
// starts at beginning of a line with "=== ", ends with " ===" at end of line.
// The (?m) flag makes ^ / $ match per-line. We also accept the whole input
// as a single unterminated line (no trailing newline) — that is what `$` in
// multi-line mode already matches against end-of-string.
//
// The trailing `[ \t\r]*` tolerates CRLF line endings (Windows-pasted
// questions, LLMs that emit `\r\n`) and trailing horizontal whitespace.
// Without this, a line like `=== END USER QUESTION ===\r\n` would slip past
// the scan because Go's `$` in multi-line mode anchors before `\n`, leaving
// `\r` between ` ===` and the anchor — reopening the D11 boundary.
var delimiterLineRE = regexp.MustCompile(`(?m)^=== .* ===[ \t\r]*$`)

// Wrap fences an LLM-sourced content blob with an open/close pair tagged
// with the current session nonce. The exact byte layout is:
//
//	=== EXPERT: <label> [nonce-<hex>] ===\n<content>\n=== END EXPERT: <label> [nonce-<hex>] ===
//
// No trailing newline — aggregates add separators explicitly. Callers must
// pre-validate content with CheckForgery; Wrap does not.
func Wrap(label, content, nonce string) string {
	var b strings.Builder
	b.Grow(len(content) + len(label)*2 + len(nonce)*2 + 64)
	b.WriteString("=== EXPERT: ")
	b.WriteString(label)
	b.WriteString(" [nonce-")
	b.WriteString(nonce)
	b.WriteString("] ===\n")
	b.WriteString(content)
	b.WriteString("\n=== END EXPERT: ")
	b.WriteString(label)
	b.WriteString(" [nonce-")
	b.WriteString(nonce)
	b.WriteString("] ===")
	return b.String()
}

// CheckForgery rejects an LLM-sourced output if it (a) contains the session
// nonce as a substring or (b) contains any line-anchored delimiter matching
// `^=== .* ===$`. Both checks are required by ADR-0008: the nonce check
// catches echoed-back prompt fragments, while the broad delimiter regex
// catches close fences and global section markers that an attacker can
// forge without knowing the nonce.
func CheckForgery(output, nonce string) error {
	if nonce != "" && strings.Contains(output, nonce) {
		return fmt.Errorf("%w: nonce %q found in output", ErrNonceLeakage, nonce)
	}
	if loc := delimiterLineRE.FindStringIndex(output); loc != nil {
		return fmt.Errorf("%w: matched line %q", ErrForgedFence, output[loc[0]:loc[1]])
	}
	return nil
}

// ScanQuestionForInjection rejects an operator-supplied question that
// contains a line-anchored delimiter. The operator is trusted (per the
// threat model), but a pasted question that accidentally holds a fence
// would corrupt every prompt downstream; better to fail at load time with
// a clear status than to debug a malformed aggregate.
func ScanQuestionForInjection(question string) error {
	if loc := delimiterLineRE.FindStringIndex(question); loc != nil {
		return fmt.Errorf("%w: matched line %q", ErrInjectionSuspected, question[loc[0]:loc[1]])
	}
	return nil
}
