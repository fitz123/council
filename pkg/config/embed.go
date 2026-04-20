package config

import (
	"bytes"
	"fmt"
	"io/fs"
	"path"
	"path/filepath"

	"github.com/fitz123/council/defaults"
)

// loadFromEmbedded parses the embedded default.yaml + prompt seeds shipped
// with the binary. It is the third precedence rung of Load (after the cwd-
// local and user-global config paths) per docs/design/v1.md §5.
//
// Prompt-file paths remain relative ("prompts/judge.md") because they are
// looked up inside the embedded FS, not the host filesystem. Consumers that
// need an absolute path should prefer a materialised config on disk.
func loadFromEmbedded() (*Profile, error) {
	data, err := fs.ReadFile(defaults.FS, defaults.DefaultYAMLPath)
	if err != nil {
		return nil, fmt.Errorf("read embedded %s: %w", defaults.DefaultYAMLPath, err)
	}
	var y yamlProfile
	if err := decodeStrict(bytes.NewReader(data), &y); err != nil {
		return nil, fmt.Errorf("parse embedded %s: %w", defaults.DefaultYAMLPath, err)
	}
	read := func(p string) ([]byte, error) {
		// embed.FS uses forward-slash paths. buildProfile hands us OS-specific
		// separators via filepath.Join; normalise before dispatching.
		return fs.ReadFile(defaults.FS, path.Clean(filepath.ToSlash(p)))
	}
	return buildProfile(&y, ".", read, ".")
}
