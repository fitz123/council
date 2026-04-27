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
