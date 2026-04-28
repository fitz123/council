// This file implements the JSON-tail extractor for published answers.
// Per ADR-0014 (json-extraction-published-answer), every R2 response must
// end with a fenced JSON code block carrying a clean `answer` field. The
// orchestrator extracts that field and writes it to output.md instead of
// the raw R2 body, which contains debate meta-commentary referring to
// other experts. When extraction fails, the caller falls back to writing
// the raw R2 unchanged — strictly today's behavior, no regression.
//
// ExtractAnswer is intentionally a pure function: no logging, no errors,
// no I/O. The caller decides what to do with each fail-closed status.

package debate

import (
	"encoding/json"
	"strings"
)

// ExtractStatus reports the outcome of ExtractAnswer. Every value other
// than ExtractOK means the caller should fall back to writing the raw R2
// body so that no answer is ever lost to a parser bug.
type ExtractStatus int

const (
	// ExtractOK indicates a clean, non-empty answer string was found.
	ExtractOK ExtractStatus = iota
	// ExtractNoJSONBlock indicates no fenced ```...``` block was found.
	ExtractNoJSONBlock
	// ExtractInvalidJSON indicates the fenced block did not parse as JSON.
	ExtractInvalidJSON
	// ExtractMissingAnswer indicates the JSON parsed but `answer` is
	// absent, null, or not a string.
	ExtractMissingAnswer
	// ExtractEmptyAnswer indicates `answer` is a string but empty after
	// trimming surrounding whitespace.
	ExtractEmptyAnswer
)

// VerdictStatus returns the wire-format string for verdict.json's
// answer_extraction.status field (ADR-0014). ExtractOK maps to "ok"; every
// fail-closed value maps to "fallback_<reason>" so a single jq probe
// (`.answer_extraction.status == "ok" or
// (.answer_extraction.status | startswith("fallback_"))`) can verify the
// invariant across every session.
//
// An unknown ExtractStatus (defensive — should never occur) maps to
// "fallback_unknown" so the field is always serializable rather than
// panicking on a future enum extension that forgot to update this map.
func (s ExtractStatus) VerdictStatus() string {
	switch s {
	case ExtractOK:
		return "ok"
	case ExtractNoJSONBlock:
		return "fallback_no_json"
	case ExtractInvalidJSON:
		return "fallback_invalid_json"
	case ExtractMissingAnswer:
		return "fallback_missing_answer"
	case ExtractEmptyAnswer:
		return "fallback_empty_answer"
	default:
		return "fallback_unknown"
	}
}

// ExtractAnswer scans raw for a fenced JSON-candidate block at the very
// tail of the response and returns the value of its top-level `answer`
// field. Pure function: no logging, no errors. Callers inspect the
// returned status and fall back to writing the raw R2 body whenever
// status != ExtractOK.
//
// A JSON-candidate block is either an explicitly tagged ```json fence,
// or an untagged ``` fence whose trimmed body begins with `{`. Earlier
// JSON blocks in the response are NOT considered: peer-aware.md
// requires the JSON block to be the last content (nothing after the
// closing fence). If the LAST fenced block is a non-JSON language
// (```bash, ```python, ...), or if any non-whitespace content follows
// the closing fence, the result is ExtractNoJSONBlock — accurately
// attributing the absence of a JSON tail rather than silently
// publishing a stale draft from earlier in the body.
//
// The scan requires explicit code fences. Bare `{...}` at the end of
// prose is rejected (ExtractNoJSONBlock) so unstructured trailing JSON
// in a peer's argument cannot be mistaken for the published answer.
func ExtractAnswer(raw string) (answer string, status ExtractStatus) {
	lines := splitLines(raw)
	blocks := findFencedBlocks(lines)
	if len(blocks) == 0 {
		return "", ExtractNoJSONBlock
	}
	last := blocks[len(blocks)-1]
	// Tail enforcement: the JSON block must be the LAST content. Any
	// non-whitespace line after the closing fence disqualifies the
	// block — we won't extract a stale draft followed by prose or by
	// another (non-JSON) code fence.
	for _, line := range lines[last.closeIdx+1:] {
		if strings.TrimSpace(line) != "" {
			return "", ExtractNoJSONBlock
		}
	}
	body := strings.TrimSpace(last.body)
	switch last.lang {
	case "json":
		// candidate
	case "":
		if !strings.HasPrefix(body, "{") {
			return "", ExtractNoJSONBlock
		}
	default:
		return "", ExtractNoJSONBlock
	}

	var parsed map[string]any
	if err := json.Unmarshal([]byte(body), &parsed); err != nil {
		return "", ExtractInvalidJSON
	}
	v, ok := parsed["answer"]
	if !ok || v == nil {
		return "", ExtractMissingAnswer
	}
	s, ok := v.(string)
	if !ok {
		return "", ExtractMissingAnswer
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", ExtractEmptyAnswer
	}
	return s, ExtractOK
}

// fencedBlock holds one fenced code block: its info-string (language
// tag, e.g. "json", "bash", or "" for an untagged fence), the raw body
// between the open and close fences, and the index of the closing
// fence line in the source `lines` slice (used by the tail-enforcement
// check above).
type fencedBlock struct {
	lang     string
	body     string
	closeIdx int
}

// splitLines splits raw on "\n" and trims trailing "\r" from each line
// so CRLF inputs behave the same as LF.
func splitLines(raw string) []string {
	lines := strings.Split(raw, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], "\r")
	}
	return lines
}

// findFencedBlocks returns all fenced code blocks in lines, in document
// order. A fence is three backticks at the start of a line (optionally
// with an info-string like `json`), and the closing fence is three
// backticks at the start of a line. The fence-open line and the
// fence-close line are excluded from the body; the close-line index is
// preserved so callers can verify what (if anything) follows it.
//
// Implemented by hand instead of regexp so the line-anchoring is precise
// across CRLF and trailing-whitespace edge cases. Backticks appearing
// mid-line (e.g. inside a quoted string) are correctly ignored — they
// never start with column 0 of a line, so they cannot match a fence.
func findFencedBlocks(lines []string) []fencedBlock {
	var blocks []fencedBlock
	i := 0
	for i < len(lines) {
		lang, ok := fenceOpenLang(lines[i])
		if !ok {
			i++
			continue
		}
		start := i + 1
		j := start
		for j < len(lines) {
			if strings.TrimSpace(lines[j]) == "```" {
				blocks = append(blocks, fencedBlock{
					lang:     lang,
					body:     strings.Join(lines[start:j], "\n"),
					closeIdx: j,
				})
				i = j + 1
				break
			}
			j++
		}
		if j >= len(lines) {
			// unterminated fence — ignore; caller falls back to raw R2
			return blocks
		}
	}
	return blocks
}

// fenceOpenLang reports whether line is an opening fence and, if so,
// returns its info-string (the optional language tag, e.g. "json", or
// "" for an untagged fence). A valid opener is exactly three backticks
// optionally followed by an info-string of [a-zA-Z0-9_-] characters and
// nothing else. Leading and trailing whitespace on the line is tolerated
// (LLMs and markdown renderers commonly emit indented or trailing-space
// fences) — the line is trimmed before the strict-shape check. The strict
// info-string charset rejects unusual variants like "``` json" with an
// embedded space.
func fenceOpenLang(line string) (string, bool) {
	trimmed := strings.TrimSpace(line)
	if !strings.HasPrefix(trimmed, "```") {
		return "", false
	}
	rest := trimmed[3:]
	for _, r := range rest {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '_' || r == '-':
		default:
			return "", false
		}
	}
	return rest, true
}
