package inmem_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/conformancetest"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// TestInMem_Conformance drives the canonical conformance suite
// against the inmem driver. This is the gate Phase 15 (SQLite) and
// Phase 16 (Postgres) drivers will inherit verbatim.
func TestInMem_Conformance(t *testing.T) {
	conformancetest.Run(t, func() (state.StateStore, func()) {
		s, err := inmem.New(config.StateConfig{Driver: "inmem"})
		if err != nil {
			t.Fatalf("inmem.New: %v", err)
		}
		return s, func() { _ = s.Close(context.Background()) }
	})
}

// TestInMem_DefendsAgainstCallerMutation pins the deep-copy contract
// noted in the inmem package godoc — callers that mutate a slice
// they passed in (or got back) MUST NOT see the change reflected in
// the store.
func TestInMem_DefendsAgainstCallerMutation(t *testing.T) {
	s, err := inmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	q := identity.Quadruple{
		Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"},
	}
	rec := state.StateRecord{
		ID:       "01HABXX-mut",
		Identity: q,
		Kind:     "task.checkpoint",
		Bytes:    []byte("original"),
	}
	if err := s.Save(context.Background(), rec); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Mutate the slice the caller passed in.
	rec.Bytes[0] = 'X'

	got, err := s.Load(context.Background(), q, "task.checkpoint")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if string(got.Bytes) != "original" {
		t.Errorf("InMem did not deep-copy on Save: %q", got.Bytes)
	}

	// Mutate the loaded slice; a second Load must still see the
	// pristine value.
	got.Bytes[0] = 'Y'
	got2, err := s.Load(context.Background(), q, "task.checkpoint")
	if err != nil {
		t.Fatal(err)
	}
	if string(got2.Bytes) != "original" {
		t.Errorf("InMem did not deep-copy on Load: %q", got2.Bytes)
	}
}

// TestInMem_DriverRegistered verifies the init() side-effect —
// the driver self-registers under "inmem" so OpenDriver can resolve.
func TestInMem_DriverRegistered(t *testing.T) {
	cfg := config.StateConfig{Driver: "inmem"}
	s, err := state.OpenDriver("inmem", cfg)
	if err != nil {
		t.Fatalf("OpenDriver: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
}
