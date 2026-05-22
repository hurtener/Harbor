package steering

import (
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
)

func TestRegistry_OpenLookupRetire_Lifecycle(t *testing.T) {
	reg := NewRegistry(WithClock(newFakeClock()))
	if reg.Len() != 0 {
		t.Fatalf("fresh Registry Len() = %d, want 0", reg.Len())
	}

	in, err := reg.Open(runA)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if reg.Len() != 1 {
		t.Errorf("Len() after Open = %d, want 1", reg.Len())
	}

	got, err := reg.Lookup(runA)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got != in {
		t.Error("Lookup returned a different *Inbox than Open")
	}

	if err := reg.Retire(runA); err != nil {
		t.Fatalf("Retire: %v", err)
	}
	if reg.Len() != 0 {
		t.Errorf("Len() after Retire = %d, want 0", reg.Len())
	}
	if _, err := reg.Lookup(runA); !errors.Is(err, ErrInboxNotFound) {
		t.Errorf("Lookup after Retire = %v, want ErrInboxNotFound", err)
	}
}

func TestRegistry_Open_RejectsDuplicate(t *testing.T) {
	reg := NewRegistry(WithClock(newFakeClock()))
	if _, err := reg.Open(runA); err != nil {
		t.Fatalf("first Open: %v", err)
	}
	_, err := reg.Open(runA)
	if !errors.Is(err, ErrInboxExists) {
		t.Errorf("second Open(same run) = %v, want ErrInboxExists", err)
	}
}

func TestRegistry_Open_RejectsIncompleteIdentity(t *testing.T) {
	reg := NewRegistry()
	cases := []identity.Quadruple{
		{Identity: identity.Identity{UserID: "u", SessionID: "s"}, RunID: "r"},
		{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}, // no run
		{}, // wholly empty
	}
	for i, q := range cases {
		if _, err := reg.Open(q); !errors.Is(err, ErrIdentityRequired) {
			t.Errorf("case %d: Open(incomplete) = %v, want ErrIdentityRequired", i, err)
		}
	}
}

func TestRegistry_Lookup_RejectsIncompleteIdentity(t *testing.T) {
	reg := NewRegistry()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	if _, err := reg.Lookup(q); !errors.Is(err, ErrIdentityRequired) {
		t.Errorf("Lookup(no run) = %v, want ErrIdentityRequired", err)
	}
}

func TestRegistry_Retire_NotFoundAndDoubleRetire(t *testing.T) {
	reg := NewRegistry(WithClock(newFakeClock()))
	if err := reg.Retire(runA); !errors.Is(err, ErrInboxNotFound) {
		t.Errorf("Retire(never opened) = %v, want ErrInboxNotFound", err)
	}
	if _, err := reg.Open(runA); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := reg.Retire(runA); err != nil {
		t.Fatalf("first Retire: %v", err)
	}
	if err := reg.Retire(runA); !errors.Is(err, ErrInboxNotFound) {
		t.Errorf("second Retire = %v, want ErrInboxNotFound", err)
	}
}

// TestRegistry_PerRunIsolation proves two runs' inboxes never share
// state — an event enqueued on run A's inbox is invisible to run B's
// (CLAUDE.md §6 multi-isolation).
func TestRegistry_PerRunIsolation(t *testing.T) {
	reg := NewRegistry(WithClock(newFakeClock()))
	inA, err := reg.Open(runA)
	if err != nil {
		t.Fatalf("Open runA: %v", err)
	}
	inB, err := reg.Open(runB)
	if err != nil {
		t.Fatalf("Open runB: %v", err)
	}

	for range 5 {
		if err = inA.Enqueue(validEvent(runA)); err != nil {
			t.Fatalf("Enqueue runA: %v", err)
		}
	}
	if inA.Len() != 5 {
		t.Errorf("inA.Len() = %d, want 5", inA.Len())
	}
	if inB.Len() != 0 {
		t.Errorf("inB.Len() = %d, want 0 — cross-run bleed", inB.Len())
	}

	drainedB, err := inB.Drain()
	if err != nil {
		t.Fatalf("Drain runB: %v", err)
	}
	if len(drainedB) != 0 {
		t.Errorf("runB drained %d events, want 0 — cross-run bleed", len(drainedB))
	}
	// runA still holds its 5.
	if inA.Len() != 5 {
		t.Errorf("inA.Len() after runB drain = %d, want 5", inA.Len())
	}
}

// TestRegistry_SameTripleDifferentRun proves the run component is
// part of the inbox key — two runs in the SAME session get distinct
// inboxes.
func TestRegistry_SameTripleDifferentRun(t *testing.T) {
	reg := NewRegistry(WithClock(newFakeClock()))
	base := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	q1 := identity.Quadruple{Identity: base, RunID: "run-1"}
	q2 := identity.Quadruple{Identity: base, RunID: "run-2"}

	in1, err := reg.Open(q1)
	if err != nil {
		t.Fatalf("Open q1: %v", err)
	}
	in2, err := reg.Open(q2)
	if err != nil {
		t.Fatalf("Open q2: %v", err)
	}
	if in1 == in2 {
		t.Fatal("two runs in the same session share an *Inbox — run is not part of the key")
	}
	if reg.Len() != 2 {
		t.Errorf("Len() = %d, want 2", reg.Len())
	}
}
