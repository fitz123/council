package executor

import (
	"fmt"
	"sync"
)

// registry holds every Executor known to the process. Implementations
// register themselves from their subpackage's init() function, so simply
// importing a backend (anonymously, with `_ "..."`) makes it available
// to Get without any wiring on the caller side.
//
// The map is guarded by a sync.RWMutex purely as defensive hygiene;
// real-world registration happens during init() (single-threaded), and
// real-world Get happens during orchestrator setup (also single
// goroutine), so contention is not a concern. The lock keeps the data
// race detector quiet if a future test does something exotic.
var (
	regMu sync.RWMutex
	reg   = map[string]Executor{}
)

// Register adds e to the global registry under e.Name(). Registering the
// same name twice panics — duplicate registration is a programmer error
// (likely two init() functions racing for the same name) and silently
// shadowing the earlier binding would make the bug invisible.
//
// Register is safe to call from package init().
func Register(e Executor) {
	if e == nil {
		panic("executor.Register: nil executor")
	}
	name := e.Name()
	if name == "" {
		panic("executor.Register: empty Name()")
	}
	regMu.Lock()
	defer regMu.Unlock()
	if _, exists := reg[name]; exists {
		panic(fmt.Sprintf("executor.Register: duplicate name %q", name))
	}
	reg[name] = e
}

// Get returns the registered Executor for name, or an error naming both
// the requested and the available executors. The error text is what the
// orchestrator surfaces when a profile references an unknown executor;
// listing the known names makes typos obvious without needing a separate
// `--list-executors` flag.
func Get(name string) (Executor, error) {
	regMu.RLock()
	defer regMu.RUnlock()
	if e, ok := reg[name]; ok {
		return e, nil
	}
	known := make([]string, 0, len(reg))
	for n := range reg {
		known = append(known, n)
	}
	return nil, fmt.Errorf("executor.Get: unknown executor %q (registered: %v)", name, known)
}

// reset wipes the registry. Test-only — exported via export_test.go so
// individual test files can isolate themselves from each other and from
// the production init() registrations.
func reset() {
	regMu.Lock()
	defer regMu.Unlock()
	reg = map[string]Executor{}
}

// ResetForTest wipes the registry from code outside package executor.
// It is the cross-package equivalent of Reset() in export_test.go:
// tests in pkg/orchestrator and elsewhere register their stubs with
// this, run their assertions, and rely on t.Cleanup to restore a clean
// slate. Name begins with "ForTest" so linters (and humans) know it is
// not production surface; linking this in a non-test path is a bug.
//
// It is not in export_test.go because _test.go files are only visible
// to the package they live in — external tests cannot use them.
func ResetForTest() { reset() }
