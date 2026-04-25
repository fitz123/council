package executor

import (
	"context"
	"strings"
	"testing"
	"time"
)

// stubExec is a minimal Executor used only in registry tests. Execute is
// a no-op because we never call it from the registry tests — the tests
// only exercise lookup behavior.
type stubExec struct{ name string }

func (s stubExec) Name() string       { return s.name }
func (s stubExec) BinaryName() string { return s.name }
func (s stubExec) Execute(ctx context.Context, req Request) (Response, error) {
	return Response{}, nil
}

func TestRegisterAndGet(t *testing.T) {
	ResetForTest()
	want := stubExec{name: "stub-a"}
	Register(want)
	got, err := Get("stub-a")
	if err != nil {
		t.Fatalf("Get(stub-a): %v", err)
	}
	if got.Name() != "stub-a" {
		t.Errorf("got Name() = %q, want stub-a", got.Name())
	}
}

func TestGetUnknownNamesAvailable(t *testing.T) {
	ResetForTest()
	Register(stubExec{name: "stub-a"})
	Register(stubExec{name: "stub-b"})
	_, err := Get("nope")
	if err == nil {
		t.Fatal("Get(nope): want error, got nil")
	}
	msg := err.Error()
	// the error must name BOTH the requested name and the available
	// ones — that is the affordance that lets users notice a typo
	// without a separate --list-executors flag.
	if !strings.Contains(msg, `"nope"`) {
		t.Errorf("error %q missing requested name", msg)
	}
	if !strings.Contains(msg, "stub-a") || !strings.Contains(msg, "stub-b") {
		t.Errorf("error %q missing one of the registered names", msg)
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	ResetForTest()
	Register(stubExec{name: "dup"})
	defer func() {
		r := recover()
		if r == nil {
			t.Fatal("second Register of same name did not panic")
		}
		if !strings.Contains(asString(r), "duplicate") {
			t.Errorf("panic value %v missing 'duplicate' marker", r)
		}
	}()
	Register(stubExec{name: "dup"})
}

func TestRegisterNilPanics(t *testing.T) {
	ResetForTest()
	defer func() {
		if recover() == nil {
			t.Fatal("Register(nil) did not panic")
		}
	}()
	Register(nil)
}

func TestRegisterEmptyNamePanics(t *testing.T) {
	ResetForTest()
	defer func() {
		if recover() == nil {
			t.Fatal("Register with empty Name did not panic")
		}
	}()
	Register(stubExec{name: ""})
}

// asString is a tiny helper to extract a panic value as a string for
// substring checks. The panic source uses fmt.Sprintf so the value is a
// string; the type assertion just makes that explicit.
func asString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// TestExecuteSignatureCompiles is a compile-time check that the stub
// satisfies the interface — if Executor's signature ever drifts, this
// file stops compiling and we catch it before runtime. The runtime body
// is trivial.
func TestExecuteSignatureCompiles(t *testing.T) {
	var _ Executor = stubExec{name: "x"}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, err := stubExec{name: "x"}.Execute(ctx, Request{})
	if err != nil {
		t.Fatalf("stub Execute: %v", err)
	}
}
