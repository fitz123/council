package config

// embeddedBytes holds the bytes of the compiled-in defaults/default.yaml. It
// is nil until Task 8 wires up //go:embed, at which point loadFromEmbedded
// becomes the third precedence rung in Load.
var embeddedBytes []byte

// loadFromEmbedded parses the embedded default.yaml. In Task 1 it is a stub
// that returns ErrNoConfig because embeddedBytes is nil; Task 8 overrides it.
func loadFromEmbedded() (*Profile, error) {
	if embeddedBytes == nil {
		return nil, ErrNoConfig
	}
	return nil, ErrNoConfig
}
