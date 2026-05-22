package protocol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	memprotocol "github.com/hurtener/Harbor/internal/memory/protocol"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

func TestHealth_TotalRecordCount(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, 100000)
	id := testIdentity()
	seedTurns(t, h, id, 7)

	resp, err := memprotocol.Health(context.Background(),
		memprotocol.HealthDeps{Store: h.store, DriverName: "inmem"}, id)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if resp.Aggregate.Total != 7 {
		t.Errorf("Health Total = %d, want 7", resp.Aggregate.Total)
	}
}

func TestHealth_FailsLoudlyOnIncompleteIdentity(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	_, err := memprotocol.Health(context.Background(),
		memprotocol.HealthDeps{Store: h.store, DriverName: "inmem"},
		identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u"}})
	if !errors.Is(err, memory.ErrIdentityRequired) {
		t.Fatalf("Health with incomplete identity: err = %v, want ErrIdentityRequired", err)
	}
}

func TestHealth_DriverByScopeDefaultsToSession(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	resp, err := memprotocol.Health(context.Background(),
		memprotocol.HealthDeps{Store: h.store, DriverName: "sqlite"}, testIdentity())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	got := resp.Aggregate.DriverByScope[string(prototypes.MemoryScopeSession)]
	if got != "sqlite" {
		t.Errorf("DriverByScope[session] = %q, want sqlite", got)
	}
}

func TestHealth_DriverByScopeExplicitMapping(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	explicit := map[string]string{
		string(prototypes.MemoryScopeSession): "inmem",
		string(prototypes.MemoryScopeUser):    "sqlite",
		string(prototypes.MemoryScopeTenant):  "postgres",
	}
	resp, err := memprotocol.Health(context.Background(),
		memprotocol.HealthDeps{Store: h.store, DriverByScope: explicit, DriverName: "inmem"},
		testIdentity())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	for scope, want := range explicit {
		if resp.Aggregate.DriverByScope[scope] != want {
			t.Errorf("DriverByScope[%s] = %q, want %q", scope, resp.Aggregate.DriverByScope[scope], want)
		}
	}
}

func TestHealth_CounterArithmetic(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	id := testIdentity()
	agg := newAggregator(t, h)

	// Emit N memory.identity_rejected events by calling AddTurn with
	// an incomplete triple — same tenant so the tenant-scoped 24h
	// counter picks them up. The driver fails closed AND emits the
	// event (D-033).
	const n = 4
	for range n {
		_ = h.store.AddTurn(context.Background(),
			identity.Quadruple{Identity: identity.Identity{TenantID: id.TenantID, UserID: id.UserID}},
			memory.ConversationTurn{})
	}

	resp, err := memprotocol.Health(context.Background(),
		memprotocol.HealthDeps{Store: h.store, Aggregator: agg, DriverName: "inmem"}, id)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if resp.Aggregate.IdentityRejected24h != int64(n) {
		t.Errorf("IdentityRejected24h = %d, want %d", resp.Aggregate.IdentityRejected24h, n)
	}
}

func TestHealth_RecoveryDroppedCounter(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	id := testIdentity()
	agg := newAggregator(t, h)

	// Emit M memory.recovery_dropped events directly onto the bus
	// (D-035). EmitRecoveryDropped is the shipped helper.
	const m = 3
	for i := range m {
		if err := memory.EmitRecoveryDropped(context.Background(), h.bus, id, "backlog_overflow"); err != nil {
			t.Fatalf("EmitRecoveryDropped[%d]: %v", i, err)
		}
	}

	resp, err := memprotocol.Health(context.Background(),
		memprotocol.HealthDeps{Store: h.store, Aggregator: agg, DriverName: "inmem"}, id)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if resp.Aggregate.RecoveryDropped24h != int64(m) {
		t.Errorf("RecoveryDropped24h = %d, want %d", resp.Aggregate.RecoveryDropped24h, m)
	}
}

func TestHealth_NilAggregatorReportsZeroCounters(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	resp, err := memprotocol.Health(context.Background(),
		memprotocol.HealthDeps{Store: h.store, DriverName: "inmem"}, testIdentity())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if resp.Aggregate.IdentityRejected24h != 0 || resp.Aggregate.RecoveryDropped24h != 0 {
		t.Errorf("nil aggregator: counters = %d/%d, want 0/0 (page still renders)",
			resp.Aggregate.IdentityRejected24h, resp.Aggregate.RecoveryDropped24h)
	}
}

func TestHealth_FailsLoudlyOnClosedStore(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	if err := h.store.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_, err := memprotocol.Health(context.Background(),
		memprotocol.HealthDeps{Store: h.store, DriverName: "inmem"}, testIdentity())
	if !errors.Is(err, memory.ErrStoreClosed) {
		t.Fatalf("Health on closed store: err = %v, want ErrStoreClosed", err)
	}
}
