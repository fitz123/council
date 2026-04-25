package main

import (
	"fmt"
	"os/exec"

	"github.com/fitz123/council/pkg/config"
	"github.com/fitz123/council/pkg/executor"
)

// lookPath is the os/exec.LookPath indirection used by Preflight. Var-
// indirect so tests can stub it without provisioning real binaries on
// $PATH (the production path is always exec.LookPath; only test seams
// override it).
var lookPath = exec.LookPath

// Preflight verifies every expert's executor is registered and that the
// CLI binary it points at resolves on PATH before the orchestrator is
// allowed to fan out. Without this check, a missing CLI surfaces as a
// per-expert spawn error deep inside debate.Run — three near-identical
// "exec: ... not found" lines instead of one up-front failure naming
// the binary, the executor, and the offending profile line.
//
// orchestrator.Validate already proves every executor name resolves in
// the registry, but it does not touch PATH. Preflight handles the
// runtime side and runs immediately after Validate in cmd/council so
// the two checks together cover both halves of "this profile is
// usable".
func Preflight(profile *config.Profile) error {
	for i, e := range profile.Experts {
		ex, err := executor.Get(e.Executor)
		if err != nil {
			return fmt.Errorf("preflight: experts[%d] %q: %w", i, e.Name, err)
		}
		bin := ex.BinaryName()
		if _, err := lookPath(bin); err != nil {
			return fmt.Errorf("preflight: experts[%d] %q (executor %q): binary %q not on PATH: %w",
				i, e.Name, e.Executor, bin, err)
		}
	}
	return nil
}
