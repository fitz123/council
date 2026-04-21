// Package prompt builds the stdin payloads sent to expert and judge
// subprocesses. Formatting is bytes-exact with docs/design/v1.md §9; the
// snapshot tests in expert_test.go and judge_test.go gate that contract.
//
// The builders are pure string formatters: they take the role body (already
// loaded from disk by pkg/config), the user question, and — for the judge —
// the collected expert outputs. They do not read files, spawn processes, or
// log. Side effects belong to pkg/orchestrator.
//
// BuildExpert and BuildJudge both accept a priorRounds parameter. In v1 the
// slice is always empty; it is a forward-compat slot for v2 multi-round
// debate. Callers pass nil (or an empty slice — the two are treated
// identically) to preserve the signature across the v1→v2 boundary.
package prompt

// ExpertOutput names one expert's completed stdout for a single round. Name
// is the profile role name (e.g. "independent", "critic"); Body is the raw
// subprocess stdout verbatim — no trimming, no escaping.
type ExpertOutput struct {
	Name string
	Body string
}

// RoundOutput bundles every expert output from a completed round. In v1 the
// orchestrator never passes a populated RoundOutput to the builders; the
// type exists so that v2's multi-round debate can feed prior rounds into the
// next round's prompts without breaking the v1 function signature.
type RoundOutput struct {
	Experts []ExpertOutput
}
