package executor

// Reset clears the global registry. Test-only hook (filename has the
// _test suffix, so it is not built into the production binary). Tests
// that want to assert behavior in isolation call Reset before
// registering a stub.
func Reset() { reset() }
