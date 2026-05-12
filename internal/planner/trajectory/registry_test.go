package trajectory_test

import (
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

// TestRegistry_SetGetRoundTrip — installing and retrieving a value
// returns the same reference.
func TestRegistry_SetGetRoundTrip(t *testing.T) {
	r := trajectory.NewProcessLocalRegistry()
	r.Set("h-1", "the-value")
	got, err := r.Get("h-1")
	if err != nil {
		t.Fatalf("Get err = %v want nil", err)
	}
	if got != "the-value" {
		t.Errorf("Get = %v want \"the-value\"", got)
	}
}

// TestRegistry_GetMiss_FailsLoudly — Get on an unset HandleID returns
// (nil, ErrToolContextLost{Handle: id}). Never (nil, nil). This is
// the fail-loudly contract that closes the predecessor's silent-
// tool-context-loss bug (brief 02 §4).
func TestRegistry_GetMiss_FailsLoudly(t *testing.T) {
	r := trajectory.NewProcessLocalRegistry()
	got, err := r.Get("never-set")
	if err == nil {
		t.Fatalf("Get(missing) returned nil error — fail-loudly contract violated")
	}
	if got != nil {
		t.Errorf("Get(missing) returned non-nil value %v — must be nil on miss", got)
	}

	var lost trajectory.ErrToolContextLost
	if !errors.As(err, &lost) {
		t.Fatalf("err = %v want errors.As(ErrToolContextLost)", err)
	}
	if lost.Handle != "never-set" {
		t.Errorf("ErrToolContextLost.Handle = %q want %q", lost.Handle, "never-set")
	}
}

// TestRegistry_Delete_RemovesHandle — Delete makes a previously
// installed handle Get return ErrToolContextLost.
func TestRegistry_Delete_RemovesHandle(t *testing.T) {
	r := trajectory.NewProcessLocalRegistry()
	r.Set("h-x", 42)
	r.Delete("h-x")
	_, err := r.Get("h-x")
	var lost trajectory.ErrToolContextLost
	if !errors.As(err, &lost) {
		t.Fatalf("after Delete, Get err = %v want ErrToolContextLost", err)
	}
}

// TestRegistry_Delete_Idempotent — Deleting a non-existent handle is
// a no-op (no panic, no error path).
func TestRegistry_Delete_Idempotent(t *testing.T) {
	r := trajectory.NewProcessLocalRegistry()
	r.Delete("never-existed") // must not panic
}

// TestRegistry_CrossHandle_Isolation — installing handle A does not
// surface under handle B's Get.
func TestRegistry_CrossHandle_Isolation(t *testing.T) {
	r := trajectory.NewProcessLocalRegistry()
	r.Set("a", "value-a")
	r.Set("b", "value-b")

	gotA, err := r.Get("a")
	if err != nil || gotA != "value-a" {
		t.Errorf("Get(a) = (%v, %v) want (\"value-a\", nil)", gotA, err)
	}
	gotB, err := r.Get("b")
	if err != nil || gotB != "value-b" {
		t.Errorf("Get(b) = (%v, %v) want (\"value-b\", nil)", gotB, err)
	}
}

// TestRegistry_OverwriteSemantics — Set re-installs silently
// (standard map semantics). The previous value is replaced.
func TestRegistry_OverwriteSemantics(t *testing.T) {
	r := trajectory.NewProcessLocalRegistry()
	r.Set("h", "v1")
	r.Set("h", "v2")
	got, err := r.Get("h")
	if err != nil {
		t.Fatalf("Get err = %v", err)
	}
	if got != "v2" {
		t.Errorf("Get after overwrite = %v want \"v2\"", got)
	}
}

// TestRegistry_HandlesLiveValues — the registry holds live values
// (channels, functions, closures — the brief 02 §4 list). They
// round-trip by reference.
func TestRegistry_HandlesLiveValues(t *testing.T) {
	r := trajectory.NewProcessLocalRegistry()

	// Channel
	ch := make(chan int, 1)
	r.Set("ch", ch)
	gotCh, err := r.Get("ch")
	if err != nil {
		t.Fatalf("Get(ch) err = %v", err)
	}
	if gotCh != any(ch) { // compare interface-wrapped equal
		t.Errorf("channel did not round-trip by reference")
	}

	// Closure
	called := false
	fn := func() { called = true }
	r.Set("fn", fn)
	gotFn, err := r.Get("fn")
	if err != nil {
		t.Fatalf("Get(fn) err = %v", err)
	}
	cb, ok := gotFn.(func())
	if !ok {
		t.Fatalf("retrieved value is not a func()")
	}
	cb()
	if !called {
		t.Errorf("closure did not execute through the registry round-trip")
	}
}

// TestErrToolContextLost_ErrorMessage — the canonical Error() message
// names the missing HandleID.
func TestErrToolContextLost_ErrorMessage(t *testing.T) {
	err := trajectory.ErrToolContextLost{Handle: "ghost-handle"}
	got := err.Error()
	if !contains(got, "ghost-handle") {
		t.Errorf("Error() = %q does not name the missing handle", got)
	}
	if !contains(got, "trajectory") {
		t.Errorf("Error() = %q should include the 'trajectory' subsystem prefix", got)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		indexOf(haystack, needle) >= 0
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
