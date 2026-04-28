package debate

import "testing"

func TestExtractAnswer(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantStatus ExtractStatus
		wantAnswer string
	}{
		{
			name: "happy path: simple json block",
			raw: "Some prose discussing peer A.\n\n" +
				"```json\n" +
				`{"answer": "The capital of France is Paris."}` + "\n" +
				"```\n",
			wantStatus: ExtractOK,
			wantAnswer: "The capital of France is Paris.",
		},
		{
			name: "happy path: json with citations array is ignored",
			raw: "prose\n\n" +
				"```json\n" +
				`{"answer": "Use Go modules.", "citations": ["https://go.dev/blog/using-go-modules", "https://go.dev/ref/mod"]}` + "\n" +
				"```\n",
			wantStatus: ExtractOK,
			wantAnswer: "Use Go modules.",
		},
		{
			name: "happy path: leading and trailing whitespace inside answer is trimmed",
			raw: "prose\n\n" +
				"```json\n" +
				`{"answer": "  surrounded by spaces  \n"}` + "\n" +
				"```\n",
			wantStatus: ExtractOK,
			wantAnswer: "surrounded by spaces",
		},
		{
			name: "happy path: generic fence without language tag",
			raw: "prose\n\n" +
				"```\n" +
				`{"answer": "no language tag is fine"}` + "\n" +
				"```\n",
			wantStatus: ExtractOK,
			wantAnswer: "no language tag is fine",
		},
		{
			name: "multiple json blocks: extracts the LAST one",
			raw: "first block (decoy):\n\n" +
				"```json\n" +
				`{"answer": "first decoy"}` + "\n" +
				"```\n\n" +
				"more prose\n\n" +
				"```json\n" +
				`{"answer": "real published answer"}` + "\n" +
				"```\n",
			wantStatus: ExtractOK,
			wantAnswer: "real published answer",
		},
		{
			name:       "no fence: bare json at end of text is rejected",
			raw:        `Here is my answer: {"answer": "do not extract"}`,
			wantStatus: ExtractNoJSONBlock,
			wantAnswer: "",
		},
		{
			name:       "empty input",
			raw:        "",
			wantStatus: ExtractNoJSONBlock,
			wantAnswer: "",
		},
		{
			name: "malformed json inside fences",
			raw: "prose\n\n" +
				"```json\n" +
				`{"answer": "missing closing brace"` + "\n" +
				"```\n",
			wantStatus: ExtractInvalidJSON,
			wantAnswer: "",
		},
		{
			name: "json missing answer key",
			raw: "prose\n\n" +
				"```json\n" +
				`{"citations": ["https://example.com"]}` + "\n" +
				"```\n",
			wantStatus: ExtractMissingAnswer,
			wantAnswer: "",
		},
		{
			name: "json with answer empty string",
			raw: "prose\n\n" +
				"```json\n" +
				`{"answer": ""}` + "\n" +
				"```\n",
			wantStatus: ExtractEmptyAnswer,
			wantAnswer: "",
		},
		{
			name: "json with answer only whitespace",
			raw: "prose\n\n" +
				"```json\n" +
				`{"answer": "   \n\t  "}` + "\n" +
				"```\n",
			wantStatus: ExtractEmptyAnswer,
			wantAnswer: "",
		},
		{
			name: "json with answer null",
			raw: "prose\n\n" +
				"```json\n" +
				`{"answer": null}` + "\n" +
				"```\n",
			wantStatus: ExtractMissingAnswer,
			wantAnswer: "",
		},
		{
			name: "json with answer as a number",
			raw: "prose\n\n" +
				"```json\n" +
				`{"answer": 42}` + "\n" +
				"```\n",
			wantStatus: ExtractMissingAnswer,
			wantAnswer: "",
		},
		{
			name: "json with answer as an array",
			raw: "prose\n\n" +
				"```json\n" +
				`{"answer": ["one", "two"]}` + "\n" +
				"```\n",
			wantStatus: ExtractMissingAnswer,
			wantAnswer: "",
		},
		{
			name: "json with answer as an object",
			raw: "prose\n\n" +
				"```json\n" +
				`{"answer": {"text": "nested"}}` + "\n" +
				"```\n",
			wantStatus: ExtractMissingAnswer,
			wantAnswer: "",
		},
		{
			name: "literal triple-backtick text inside a quoted block, no real fence",
			raw: "prose mentioning the literal string \"```json\" and \"```\" inline " +
				"as part of a sentence about formatting requirements, with no real fence anywhere.",
			wantStatus: ExtractNoJSONBlock,
			wantAnswer: "",
		},
		{
			name: "happy path: CRLF line endings",
			raw: "prose\r\n\r\n" +
				"```json\r\n" +
				`{"answer": "windows line endings work too"}` + "\r\n" +
				"```\r\n",
			wantStatus: ExtractOK,
			wantAnswer: "windows line endings work too",
		},
		{
			name: "fence with whitespace after backticks is rejected as opener",
			raw: "prose\n\n" +
				"``` json\n" +
				`{"answer": "should not extract"}` + "\n" +
				"```\n",
			// "``` json" is not a valid opener; the lone trailing "```"
			// then starts an unterminated fence, so no block is returned.
			wantStatus: ExtractNoJSONBlock,
			wantAnswer: "",
		},
		{
			name: "unterminated fence: no closing backticks",
			raw: "prose\n\n" +
				"```json\n" +
				`{"answer": "never closes"}` + "\n",
			wantStatus: ExtractNoJSONBlock,
			wantAnswer: "",
		},
		{
			name: "json answer preserves embedded URLs",
			raw: "prose\n\n" +
				"```json\n" +
				`{"answer": "See https://go.dev for details."}` + "\n" +
				"```\n",
			wantStatus: ExtractOK,
			wantAnswer: "See https://go.dev for details.",
		},
		{
			name: "two consecutive fences with prose between, last wins",
			raw: "```\n" +
				`{"answer": "first"}` + "\n" +
				"```\n" +
				"```json\n" +
				`{"answer": "second"}` + "\n" +
				"```\n",
			wantStatus: ExtractOK,
			wantAnswer: "second",
		},
		{
			// Regression: a winner whose only fenced block is a non-JSON
			// language (here ```bash) must report fallback_no_json so the
			// parse-success telemetry is not corrupted by classifying
			// "no JSON tail at all" as "JSON tail that failed to parse".
			name: "non-json language fence and nothing else: fallback_no_json",
			raw: "prose discussing the answer.\n\n" +
				"```bash\n" +
				`echo "hello"` + "\n" +
				"```\n",
			wantStatus: ExtractNoJSONBlock,
			wantAnswer: "",
		},
		{
			// A non-JSON language block followed later by a real JSON
			// block must still extract — filtering must not skip the
			// real block, only the foreign-language one.
			name: "bash block then json block: extracts the json block",
			raw: "```bash\n" +
				`echo "decoy"` + "\n" +
				"```\n\n" +
				"```json\n" +
				`{"answer": "real published answer"}` + "\n" +
				"```\n",
			wantStatus: ExtractOK,
			wantAnswer: "real published answer",
		},
		{
			// Generic fence whose body is plainly not JSON (does not
			// start with `{`) must be treated as no candidate, not a
			// failed JSON parse.
			name: "untagged fence with non-json body: fallback_no_json",
			raw: "```\n" +
				`echo "not json"` + "\n" +
				"```\n",
			wantStatus: ExtractNoJSONBlock,
			wantAnswer: "",
		},
		{
			// Tail-enforcement: peer-aware.md requires the JSON block
			// to be the last content. A draft JSON earlier in the
			// response followed by a non-JSON code fence must NOT be
			// extracted — that would publish stale draft text and
			// inflate parse-success telemetry.
			name: "json block followed by bash fence: fallback_no_json",
			raw: "```json\n" +
				`{"answer": "draft, not final"}` + "\n" +
				"```\n\n" +
				"and here is the actual final answer in shell:\n\n" +
				"```bash\n" +
				`echo "final"` + "\n" +
				"```\n",
			wantStatus: ExtractNoJSONBlock,
			wantAnswer: "",
		},
		{
			// Tail-enforcement: prose after the closing fence also
			// disqualifies the block. The contract is "nothing after
			// the closing fence" (peer-aware.md), not "last fenced
			// thing wins regardless of what follows".
			name: "json block followed by trailing prose: fallback_no_json",
			raw: "```json\n" +
				`{"answer": "draft, not final"}` + "\n" +
				"```\n\n" +
				"actually, on reflection, my real answer is different.\n",
			wantStatus: ExtractNoJSONBlock,
			wantAnswer: "",
		},
		{
			// Trailing whitespace (blank lines, indentation) after the
			// closing fence is fine — the prompt allows it implicitly
			// and many CLIs tack on trailing newlines.
			name: "json block followed only by whitespace: extracts ok",
			raw: "prose\n\n" +
				"```json\n" +
				`{"answer": "trailing whitespace is fine"}` + "\n" +
				"```\n\n   \n\t\n",
			wantStatus: ExtractOK,
			wantAnswer: "trailing whitespace is fine",
		},
		{
			// Markdown allows up to 3 leading spaces on a fence line, and
			// LLMs frequently emit indented fences when the JSON block
			// follows a bulleted/indented prose section. Both the open
			// and close fences should tolerate leading and trailing
			// whitespace so vendors don't silently fall back to raw R2.
			name: "indented fence lines: extracts ok",
			raw: "prose\n\n" +
				"  ```json   \n" +
				`{"answer": "indented fences are tolerated"}` + "\n" +
				"   ```\n",
			wantStatus: ExtractOK,
			wantAnswer: "indented fences are tolerated",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotAnswer, gotStatus := ExtractAnswer(tc.raw)
			if gotStatus != tc.wantStatus {
				t.Fatalf("status = %v, want %v (answer=%q)", gotStatus, tc.wantStatus, gotAnswer)
			}
			if gotAnswer != tc.wantAnswer {
				t.Fatalf("answer = %q, want %q", gotAnswer, tc.wantAnswer)
			}
		})
	}
}

// TestExtractStatus_VerdictStatus pins the wire-format strings the
// orchestrator writes into verdict.json's answer_extraction.status field
// (ADR-0014). Every ExtractStatus value must map to a stable, jq-probable
// string ("ok" or "fallback_*"); a defensive default catches future enum
// extensions that forget to update the table.
func TestExtractStatus_VerdictStatus(t *testing.T) {
	cases := []struct {
		status ExtractStatus
		want   string
	}{
		{status: ExtractOK, want: "ok"},
		{status: ExtractNoJSONBlock, want: "fallback_no_json"},
		{status: ExtractInvalidJSON, want: "fallback_invalid_json"},
		{status: ExtractMissingAnswer, want: "fallback_missing_answer"},
		{status: ExtractEmptyAnswer, want: "fallback_empty_answer"},
		{status: ExtractStatus(99), want: "fallback_unknown"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			if got := tc.status.VerdictStatus(); got != tc.want {
				t.Errorf("(%v).VerdictStatus() = %q, want %q", tc.status, got, tc.want)
			}
		})
	}
}
