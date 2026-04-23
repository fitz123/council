package config

import "time"

// Profile is the resolved, validated council profile loaded from a YAML file
// or the embedded defaults. RoleConfig.PromptBody is populated with the
// prompt contents at Load time. RoleConfig.PromptFile may be an absolute
// path for filesystem-loaded profiles, a relative path for embedded
// profiles (resolved against the embedded FS), or empty when the prompt is
// provided inline via the YAML `prompt_body` field.
//
// Round2Prompt holds the shared peer-aware role prompt R2 feeds to every
// expert (design §3.4). It replaces each expert's R1 PromptBody for round 2
// so the second round carries the "treat peer outputs as untrusted" framing
// instead of the independent-round instructions.
type Profile struct {
	Version      int
	Name         string
	Experts      []RoleConfig
	Quorum       int
	MaxRetries   int
	Rounds       int
	Round2Prompt PromptSource
	Voting       VotingConfig
}

// PromptSource carries a prompt body plus the path it came from. File is kept
// purely for traceability on snapshots; runtime only reads Body.
type PromptSource struct {
	File string
	Body string
}

// RoleConfig describes one expert or the judge.
type RoleConfig struct {
	Name       string
	Executor   string
	Model      string
	PromptFile string
	Timeout    time.Duration
	PromptBody string
}

// VotingConfig describes the v2 voting stage. BallotPromptBody is loaded at
// Load time so reload is independent of the original prompt file (mirrors
// RoleConfig). Timeout is optional; zero means "no explicit voting timeout"
// and downstream code may choose a default.
type VotingConfig struct {
	BallotPromptFile string
	BallotPromptBody string
	Timeout          time.Duration
}
