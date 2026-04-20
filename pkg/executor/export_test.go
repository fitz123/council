package executor

// Reset clears the global registry. Available only to tests inside
// package executor. Tests in other packages (e.g. pkg/orchestrator)
// that need to swap the registry should use ResetForTest below, which
// is exported — the two exist side-by-side because Go's _test.go
// convention only exposes this file to the same package.
func Reset() { reset() }
