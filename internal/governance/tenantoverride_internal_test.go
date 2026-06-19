package governance

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// These are package-internal tests: they seed the StateStore directly
// under the synthetic governance identity + kind to exercise the
// corrupt-record / forward-schema fail-loud branches of lazyLoad, which
// the public API cannot reach.

func internalHarness(t *testing.T) (*TenantOverridePolicy, state.StateStore) {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}
	pol, err := NewTenantOverridePolicy(st, bus, []string{"model-a"}, nil)
	if err != nil {
		t.Fatalf("NewTenantOverridePolicy: %v", err)
	}
	t.Cleanup(func() {
		_ = pol.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = st.Close(context.Background())
	})
	return pol, st
}

// TestTenantOverride_CorruptRecord_FailsLoud asserts a malformed persisted
// record surfaces ErrStateUnavailable rather than silently resetting.
func TestTenantOverride_CorruptRecord_FailsLoud(t *testing.T) {
	pol, st := internalHarness(t)
	if err := st.Save(context.Background(), state.StateRecord{
		ID:       state.NewEventID(),
		Identity: govTenantQuad("t1"),
		Kind:     kindTenantOverrides,
		Bytes:    []byte("{not valid json"),
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := pol.Get(context.Background(), "t1"); !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("corrupt record: want ErrStateUnavailable, got %v", err)
	}
}

// TestTenantOverride_ForwardSchema_FailsLoud asserts a record from a future
// schema fails loud rather than being partially parsed.
func TestTenantOverride_ForwardSchema_FailsLoud(t *testing.T) {
	pol, st := internalHarness(t)
	future := tenantOverrideRecord{Schema: tenantOverrideSchema + 99}
	buf, _ := json.Marshal(future)
	if err := st.Save(context.Background(), state.StateRecord{
		ID:       state.NewEventID(),
		Identity: govTenantQuad("t1"),
		Kind:     kindTenantOverrides,
		Bytes:    buf,
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, _, err := pol.Get(context.Background(), "t1"); !errors.Is(err, ErrStateUnavailable) {
		t.Fatalf("forward-schema record: want ErrStateUnavailable, got %v", err)
	}
}

// TestTenantOverride_BoundaryValues_Accepted asserts the inclusive
// boundaries validate: temperature 0 and 2, max-tokens 1.
func TestTenantOverride_BoundaryValues_Accepted(t *testing.T) {
	pol, _ := internalHarness(t)
	zero, two, one := 0.0, 2.0, 1
	if err := pol.Set(context.Background(), govTenantQuad("t1"), "t1", TenantOverrideSpec{
		Temperature: &zero, MaxTokens: &one,
	}); err != nil {
		t.Errorf("temp=0, maxtokens=1: want accepted, got %v", err)
	}
	if err := pol.Set(context.Background(), govTenantQuad("t2"), "t2", TenantOverrideSpec{
		Temperature: &two,
	}); err != nil {
		t.Errorf("temp=2: want accepted, got %v", err)
	}
}

// TestTenantOverride_GetSnapshot_ImmuneToLaterSet asserts a spec returned
// by Get is a value snapshot — a later Set (fresh pointers via validate's
// defensive copy) does not mutate the earlier result. This is the policy-
// level guarantee underpinning the run-start D-025 snapshot: an in-flight
// run's pinned override never changes under a concurrent admin set.
func TestTenantOverride_GetSnapshot_ImmuneToLaterSet(t *testing.T) {
	pol, _ := internalHarness(t)
	if err := pol.Set(context.Background(), govTenantQuad("t1"), "t1",
		TenantOverrideSpec{Model: strp("model-a")}); err != nil {
		t.Fatalf("Set 1: %v", err)
	}
	snap, set, err := pol.Get(context.Background(), "t1")
	if err != nil || !set || snap.Model == nil {
		t.Fatalf("Get 1: set=%v err=%v model=%v", set, err, snap.Model)
	}
	// Re-set to a different model.
	if err := pol.Set(context.Background(), govTenantQuad("t1"), "t1",
		TenantOverrideSpec{Model: strp("model-a"), Temperature: f64p(0.9)}); err != nil {
		t.Fatalf("Set 2: %v", err)
	}
	// The earlier snapshot's fields are untouched (it captured the prior
	// value; the re-set installed FRESH pointers into the cache).
	if *snap.Model != "model-a" || snap.Temperature != nil {
		t.Errorf("earlier Get snapshot mutated by a later Set: model=%v temp=%v", *snap.Model, snap.Temperature)
	}
}

func strp(s string) *string   { return &s }
func f64p(f float64) *float64 { return &f }
