package config

import "time"

// Profile is the resolved, validated council profile loaded from a YAML file
// or the embedded defaults. All RoleConfig.PromptFile values are absolute
// paths, and RoleConfig.PromptBody is populated with the file contents as
// read at Load time.
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
