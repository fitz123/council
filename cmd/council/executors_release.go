//go:build !testbinary

package main

// Side-effect imports — register the v3 executors for the release
// binary. The build tag `!testbinary` ensures the smoke test binary
// (built with `-tags testbinary`) substitutes in the mock under the
// "claude-code" key instead. codex and gemini stay registered under
// their canonical keys for both builds; smoke tests targeting those
// vendors would route through the testbinary mock if and when one is
// added.
import (
	_ "github.com/fitz123/council/pkg/executor/claudecode"
	_ "github.com/fitz123/council/pkg/executor/codex"
	_ "github.com/fitz123/council/pkg/executor/gemini"
)
