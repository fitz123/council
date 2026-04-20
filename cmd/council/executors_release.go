//go:build !testbinary

package main

// Side-effect import — registers the "claude-code" executor for the
// release binary. The build tag `!testbinary` ensures the smoke test
// binary (built with `-tags testbinary`) substitutes in mocks instead.
import _ "github.com/fitz123/council/pkg/executor/claudecode"
