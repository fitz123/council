//go:build testbinary

package main

// Side-effect import — registers the mock executor under the same
// "claude-code" name as the real one, so smoke tests using the default
// profile transparently route to a stub. Behavior is selected at runtime
// via the COUNCIL_MOCK_EXECUTOR environment variable.
import _ "github.com/fitz123/council/pkg/executor/mock"
