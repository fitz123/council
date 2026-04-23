package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Snapshot serializes the resolved profile to dstPath as YAML. Prompt bodies
// are inlined verbatim under prompt_body / ballot_prompt_body so the snapshot
// is self-contained: reload depends on neither the original prompt files
// (which the user may edit later) nor the embedded FS (whose relative paths
// only resolve from inside the binary). PromptFile is written alongside,
// purely as a record of where the body was originally loaded from.
//
// sessionNonce is the per-session 16-hex nonce from pkg/debate; it is
// recorded under the top-level session_nonce key so resume (D14) can re-read
// it without re-generating. Empty string means omit (non-session callers
// like config snapshot tests do not carry a nonce).
func Snapshot(p *Profile, sessionNonce, dstPath string) error {
	y := yamlProfile{
		Version:          p.Version,
		Name:             p.Name,
		Experts:          make([]yamlRole, len(p.Experts)),
		Quorum:           p.Quorum,
		MaxRetries:       p.MaxRetries,
		Rounds:           p.Rounds,
		Round2PromptFile: p.Round2Prompt.File,
		Round2PromptBody: p.Round2Prompt.Body,
		Voting:           votingToYaml(&p.Voting),
		SessionNonce:     sessionNonce,
	}
	for i := range p.Experts {
		y.Experts[i] = roleToYaml(&p.Experts[i])
	}
	buf, err := yaml.Marshal(&y)
	if err != nil {
		return fmt.Errorf("marshal profile: %w", err)
	}
	if err := os.WriteFile(dstPath, buf, 0o644); err != nil {
		return fmt.Errorf("write snapshot %s: %w", dstPath, err)
	}
	return nil
}

func roleToYaml(r *RoleConfig) yamlRole {
	return yamlRole{
		Name:       r.Name,
		Executor:   r.Executor,
		Model:      r.Model,
		PromptFile: r.PromptFile,
		PromptBody: r.PromptBody,
		Timeout:    r.Timeout.String(),
	}
}

func votingToYaml(v *VotingConfig) yamlVoting {
	out := yamlVoting{
		BallotPromptFile: v.BallotPromptFile,
		BallotPromptBody: v.BallotPromptBody,
	}
	if v.Timeout > 0 {
		out.Timeout = v.Timeout.String()
	}
	return out
}
