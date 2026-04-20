package config

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

// Snapshot serializes the resolved profile to dstPath as YAML. PromptFile
// entries are written as-is (already absolute after Load); prompt bodies are
// not inlined. Re-loading the snapshot reads the prompt files at their
// absolute paths and yields a structurally equivalent Profile, provided the
// referenced files have not changed.
func Snapshot(p *Profile, dstPath string) error {
	y := yamlProfile{
		Version:    p.Version,
		Name:       p.Name,
		Judge:      roleToYaml(&p.Judge),
		Experts:    make([]yamlRole, len(p.Experts)),
		Quorum:     p.Quorum,
		MaxRetries: p.MaxRetries,
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
		Timeout:    r.Timeout.String(),
	}
}
