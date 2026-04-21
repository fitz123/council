package config

import "time"

// Profile is the resolved, validated council profile loaded from a YAML file
// or the embedded defaults. RoleConfig.PromptBody is populated with the
// prompt contents at Load time. RoleConfig.PromptFile may be an absolute
// path for filesystem-loaded profiles, a relative path for embedded
// profiles (resolved against the embedded FS), or empty when the prompt is
// provided inline via the YAML `prompt_body` field.
type Profile struct {
	Version    int
	Name       string
	Judge      RoleConfig
	Experts    []RoleConfig
	Quorum     int
	MaxRetries int
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
