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
	// line-anchored delimiter of the form `^=== .*[nonce-<16hex>] ===$`.
	// Per ADR-0011, every structural fence the orchestrator emits carries
	// the session nonce in this exact shape, so any line that matches the
	// shape is necessarily a forgery attempt — the regex no longer needs
	// to bear-trap benign `=== Section ===` lines that web content may
	// legitimately contain.
	ErrForgedFence = errors.New("prompt: forged delimiter fence in LLM output")

	// ErrInjectionSuspected is returned by ScanQuestionForInjection when
	// the operator-supplied question contains a nonce-bearing delimiter
	// line. Surfaced as verdict.status = "injection_suspected_in_question".
	ErrInjectionSuspected = errors.New("prompt: injection suspected in operator question")
)

// delimiterLineRE matches any line that looks like a nonce-bearing
// prompt-protocol fence: starts at beginning of a line with "=== ", ends
// with "[nonce-<16hex>] ===" at end of line. The (?m) flag makes ^ / $
// match per-line. We also accept the whole input as a single unterminated
// line (no trailing newline) — that is what `$` in multi-line mode already
// matches against end-of-string.
//
// Per ADR-0011, every structural fence the orchestrator emits carries the
// session nonce in `[nonce-<16hex>] ===` shape — `EXPERT:`, `END EXPERT:`,
// `USER QUESTION`, `END USER QUESTION`, `CANDIDATES`, `END CANDIDATES`.
// The shape requirement narrows the bear trap: benign markdown like
// `=== Section ===` or `=== Table of Contents ===` (common in web tool
// fetch results that experts may quote) no longer trips the scan, while
// any fence-shaped attempt to break out of a wrapped region still does
// because the attacker doesn't know the per-session nonce. A wrong-valued
// but well-formed 16-hex nonce still matches the regex and is rejected;
// only the genuine session-issued fence (which the orchestrator never
// puts in a place CheckForgery would scan) is considered legitimate.
//
// The trailing `[ \t\r]*` tolerates CRLF line endings (Windows-pasted
// questions, LLMs that emit `\r\n`) and trailing horizontal whitespace.
// Without this, a line like `=== END USER QUESTION [nonce-…] ===\r\n`
// would slip past the scan because Go's `$` in multi-line mode anchors
// before `\n`, leaving `\r` between ` ===` and the anchor — reopening the
// D11 boundary.
var delimiterLineRE = regexp.MustCompile(`(?m)^=== .*\[nonce-[0-9a-f]{16}\] ===[ \t\r]*$`)

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
// the nonce-bearing fence shape `^=== .*[nonce-<16hex>] ===$`. Both checks
// are required by ADR-0008 as amended by ADR-0011: the nonce check catches
// echoed-back prompt fragments, while the shape check catches any line
// that mimics an orchestrator-emitted fence (the session nonce is unknown
// to the LLM, so any well-formed fence in its output is necessarily a
// forgery — even if the 16-hex literal happens not to be the live nonce).
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
// contains a nonce-bearing delimiter line (shape `^=== .*[nonce-<16hex>]
// ===$`). The operator is trusted (per the threat model), but a pasted
// question that accidentally holds a fence in this exact shape would
// corrupt every prompt downstream; better to fail at load time with a
// clear status than to debug a malformed aggregate. Per ADR-0011, benign
// markdown-style `=== Section ===` lines no longer trip the scan — the
// shape requirement keeps the bear trap narrow enough that operators can
// paste real-world content without constant false-positives.
func ScanQuestionForInjection(question string) error {
	if loc := delimiterLineRE.FindStringIndex(question); loc != nil {
		return fmt.Errorf("%w: matched line %q", ErrInjectionSuspected, question[loc[0]:loc[1]])
	}
	return nil
}
