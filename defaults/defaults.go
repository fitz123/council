// Package defaults exposes the compiled-in default profile and prompt seeds.
//
// The files live at the repo root (docs/design/v1.md §14 "Repository layout"),
// so the //go:embed directive has to sit here rather than inside pkg/config —
// go:embed cannot traverse up out of the package's own directory tree.
// pkg/config/embed.go reads from this package's FS when no on-disk profile
// exists.
package defaults

import "embed"

//go:embed default.yaml prompts/*.md
var FS embed.FS

// DefaultYAMLPath is the path, within FS, of the default profile YAML.
const DefaultYAMLPath = "default.yaml"
