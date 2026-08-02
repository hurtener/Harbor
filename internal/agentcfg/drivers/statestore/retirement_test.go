package statestore_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// retirementCommitAckFaultStore models one ambiguous conditional write: the
// StateStore commits the exact bytes, but its acknowledgement is lost. It is
// deliberately one-shot so the retry/convergence path runs against the real
// store rather than a permanently unavailable one.
type retirementCommitAckFaultStore struct {
	state.StateStore
	failAt int64
	calls  atomic.Int64
}

func (s *retirementCommitAckFaultStore) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if isActivePointerKind(next.Kind) && s.calls.Add(1) == s.failAt {
		if err := s.StateStore.SaveIf(ctx, expectations, next); err != nil {
			return err
		}
		return errPointerWrite
	}
	return s.StateStore.SaveIf(ctx, expectations, next)
}

// retirementFailOnceBus refuses exactly one armed lifecycle publication while
// delegating all subscription and close behaviour to the real in-memory bus.
type retirementFailOnceBus struct {
	events.EventBus
	armed atomic.Bool
}

func (b *retirementFailOnceBus) Publish(ctx context.Context, event events.Event) error {
	if b.armed.CompareAndSwap(true, false) {
		return errPointerWrite
	}
	return b.EventBus.Publish(ctx, event)
}

// TestRetirement_TerminalHistoryAndReplay proves the lifecycle transition is
// terminal for every active/mutation door while immutable revision history is
// still usable for audit and same-operation replay is exact.
func TestRetirement_TerminalHistoryAndReplay(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	retirer, ok := r.(agentcfg.RetirementRegistry)
	if !ok {
		t.Fatal("shipped StateStore registry lacks RetirementRegistry")
	}
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "admin", SessionID: "control"}}
	const agent = "agent-a"
	first, err := r.SetRevision(ctx, id, agent, agentcfg.ConfigScopeAgent, skills("before"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed revision: %v", err)
	}
	status, err := retirer.Retire(ctx, id, agent, agentcfg.RetirementRequest{OperationID: "retire-a", ExpectedContentHash: first.ContentHash})
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if status.PriorRevisionID != first.RevisionID || status.PriorContentHash != first.ContentHash {
		t.Fatalf("prior identity = (%q,%q), want (%q,%q)", status.PriorRevisionID, status.PriorContentHash, first.RevisionID, first.ContentHash)
	}
	if _, _, err := r.Active(ctx, id, agent, agentcfg.ConfigScopeAgent); !errors.Is(err, agentcfg.ErrAgentRetired) {
		t.Fatalf("active after retire = %v, want ErrAgentRetired", err)
	}
	if _, err := r.SetRevision(ctx, id, agent, agentcfg.ConfigScopeAgent, skills("after"), agentcfg.SetOptions{}); !errors.Is(err, agentcfg.ErrAgentRetired) {
		t.Fatalf("agent write after retire = %v, want ErrAgentRetired", err)
	}
	if _, err := r.SetRevision(ctx, id, agent, agentcfg.ConfigScopeUser, skills("user-after"), agentcfg.SetOptions{}); !errors.Is(err, agentcfg.ErrAgentRetired) {
		t.Fatalf("user write after retire = %v, want ErrAgentRetired", err)
	}
	if _, err := r.Rollback(ctx, id, agent, first.RevisionID, agentcfg.ConfigScopeAgent, agentcfg.SetOptions{}); !errors.Is(err, agentcfg.ErrAgentRetired) {
		t.Fatalf("rollback after retire = %v, want ErrAgentRetired", err)
	}
	if got, err := r.Get(ctx, id, agent, first.RevisionID, agentcfg.ConfigScopeAgent); err != nil || got.ContentHash != first.ContentHash {
		t.Fatalf("historical get = (%+v,%v), want retained revision", got, err)
	}
	if got, err := r.ListRevisions(ctx, id, agent, agentcfg.ConfigScopeAgent, 0); err != nil || len(got) != 1 || got[0].RevisionID != first.RevisionID {
		t.Fatalf("historical list = (%+v,%v), want retained first revision", got, err)
	}
	if _, err := r.Diff(ctx, id, agent, first.RevisionID, first.RevisionID, agentcfg.ConfigScopeAgent); err != nil {
		t.Fatalf("historical diff after retire: %v", err)
	}
	if replay, err := retirer.Retire(ctx, id, agent, agentcfg.RetirementRequest{OperationID: "retire-a", ExpectedContentHash: first.ContentHash}); err != nil || replay.Generation != status.Generation {
		t.Fatalf("same-operation replay = (%+v,%v), want stored status", replay, err)
	}
	if _, err := retirer.Retire(ctx, id, agent, agentcfg.RetirementRequest{OperationID: "retire-b", ExpectedContentHash: first.ContentHash}); !errors.Is(err, agentcfg.ErrRetirementConflict) {
		t.Fatalf("different operation = %v, want ErrRetirementConflict", err)
	}
}

// TestRetirement_ConcurrentSameOperationAndTenantIsolation runs the registry
// through the concurrent-reuse contract: one shared artifact, 100 callers,
// one durable operation identity, and no bleed into another tenant.
func TestRetirement_ConcurrentSameOperationAndTenantIsolation(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	retirer := r.(agentcfg.RetirementRegistry)
	idA := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "admin", SessionID: "control"}}
	idB := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-b", UserID: "admin", SessionID: "control"}}
	const agent = "same-id"
	rev, err := r.SetRevision(ctx, idA, agent, agentcfg.ConfigScopeAgent, skills("a"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed tenant a: %v", err)
	}
	if _, err := r.SetRevision(ctx, idB, agent, agentcfg.ConfigScopeAgent, skills("b"), agentcfg.SetOptions{}); err != nil {
		t.Fatalf("seed tenant b: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for range 100 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := retirer.Retire(ctx, idA, agent, agentcfg.RetirementRequest{OperationID: "same-op", ExpectedContentHash: rev.ContentHash})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("same operation concurrent retire: %v", err)
		}
	}
	if _, ok, err := r.Active(ctx, idB, agent, agentcfg.ConfigScopeAgent); err != nil || !ok {
		t.Fatalf("tenant b bled from tenant a retirement: active=%v err=%v", ok, err)
	}
}

// TestRetirement_SharedSQLiteTwoRegistries_N100 proves the durable active-slot
// CAS, rather than either Runtime's local lock, linearises a terminal replay.
func TestRetirement_SharedSQLiteTwoRegistries_N100(t *testing.T) {
	ctx := context.Background()
	left, right := newSharedStores(t)
	a := newRegistryOnStore(t, left)
	b := newRegistryOnStore(t, right)
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-sqlite", UserID: "admin", SessionID: "control"}}
	const agent = "agent-shared"
	rev, err := a.SetRevision(ctx, id, agent, agentcfg.ConfigScopeAgent, skills("seed"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			r := a
			if i%2 == 1 {
				r = b
			}
			_, err := r.(agentcfg.RetirementRegistry).Retire(ctx, id, agent, agentcfg.RetirementRequest{OperationID: "shared-op", ExpectedContentHash: rev.ContentHash})
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("same-operation race: %v", err)
		}
	}
	status, found, err := a.(agentcfg.RetirementRegistry).RetirementStatus(ctx, id, agent)
	if err != nil || !found || status.OperationID != "shared-op" || !status.Completed {
		t.Fatalf("durable status=(%+v,%v,%v)", status, found, err)
	}
}

// TestRetirement_ProgressIsFrozenCASState proves cleanup acknowledgement is
// durable, exactly names a manifest item, and cannot be replaced by an
// invented cleanup target after the tombstone wins.
func TestRetirement_ProgressIsFrozenCASState(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	retirer := r.(agentcfg.RetirementRegistry)
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "admin", SessionID: "control"}}
	payload := agentcfg.ConfigPayload{Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{{Name: "owned"}}}}
	rev, err := r.SetRevision(ctx, id, "agent-a", agentcfg.ConfigScopeAgent, payload, agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed revision: %v", err)
	}
	status, err := retirer.Retire(ctx, id, "agent-a", agentcfg.RetirementRequest{OperationID: "op-a", ExpectedContentHash: rev.ContentHash})
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	if status.Completed || len(status.Cleanup) != 1 || status.Cleanup[0].Resource != "owned" {
		t.Fatalf("initial status = %+v, want one pending owned connection", status)
	}
	if _, err := retirer.CompleteRetirementStep(ctx, id, "agent-a", "op-a", "mcp_connection", "foreign"); !errors.Is(err, agentcfg.ErrRetirementConflict) {
		t.Fatalf("invented cleanup item = %v, want retirement conflict", err)
	}
	status, err = retirer.CompleteRetirementStep(ctx, id, "agent-a", "op-a", "mcp_connection", "owned")
	if err != nil {
		t.Fatalf("complete frozen item: %v", err)
	}
	if !status.Completed || !status.Cleanup[0].Completed || status.Generation < 2 {
		t.Fatalf("completed status = %+v, want durable completed generation", status)
	}
	replay, found, err := retirer.RetirementStatus(ctx, id, "agent-a")
	if err != nil || !found || !replay.Completed || !replay.Cleanup[0].Completed {
		t.Fatalf("reread durable progress = (%+v,%v,%v)", replay, found, err)
	}
}

// TestRetirement_CommitThenAckLossConverges proves every retirement-owned
// SaveIf boundary treats a returned store error as ambiguous. The durable
// lifecycle record is reread exactly: if the intended tombstone/progress
// landed, the call resumes the pending event transition instead of requiring
// an operator to guess whether retirement took effect.
func TestRetirement_CommitThenAckLossConverges(t *testing.T) {
	ctx := context.Background()
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-fault", UserID: "admin", SessionID: "control"}}

	t.Run("tombstone", func(t *testing.T) {
		base := newSharedStore(t)
		seed := newRegistryOnStore(t, base)
		rev, err := seed.SetRevision(ctx, id, "agent-tombstone", agentcfg.ConfigScopeAgent, skills("seed"), agentcfg.SetOptions{})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		reg := newRegistryOnStore(t, &retirementCommitAckFaultStore{StateStore: base, failAt: 1})
		status, err := reg.(agentcfg.RetirementRegistry).Retire(ctx, id, "agent-tombstone", agentcfg.RetirementRequest{OperationID: "op-tombstone", ExpectedContentHash: rev.ContentHash})
		if err != nil {
			t.Fatalf("retire after committed-but-errored tombstone: %v", err)
		}
		if !status.Completed || status.OperationID != "op-tombstone" {
			t.Fatalf("tombstone status=%+v, want completed same operation", status)
		}
	})

	t.Run("progress", func(t *testing.T) {
		base := newSharedStore(t)
		seed := newRegistryOnStore(t, base)
		payload := agentcfg.ConfigPayload{Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{{Name: "owned"}}}}
		rev, err := seed.SetRevision(ctx, id, "agent-progress", agentcfg.ConfigScopeAgent, payload, agentcfg.SetOptions{})
		if err != nil {
			t.Fatalf("seed: %v", err)
		}
		// The first two writes are tombstone install and started-event ack;
		// the third is the durable cleanup-progress transition.
		reg := newRegistryOnStore(t, &retirementCommitAckFaultStore{StateStore: base, failAt: 3})
		retirer := reg.(agentcfg.RetirementRegistry)
		status, err := retirer.Retire(ctx, id, "agent-progress", agentcfg.RetirementRequest{OperationID: "op-progress", ExpectedContentHash: rev.ContentHash})
		if err != nil {
			t.Fatalf("retire: %v", err)
		}
		if status.Completed || len(status.Cleanup) != 1 {
			t.Fatalf("initial status=%+v, want one pending cleanup", status)
		}
		status, err = retirer.CompleteRetirementStep(ctx, id, "agent-progress", "op-progress", "mcp_connection", "owned")
		if err != nil {
			t.Fatalf("complete after committed-but-errored progress: %v", err)
		}
		if !status.Completed || !status.Cleanup[0].Completed {
			t.Fatalf("progress status=%+v, want completed frozen cleanup", status)
		}
	})
}

// TestRetirement_EventPublishFailureStaysCheckpointed proves a failed
// lifecycle event does not turn into an unobserved completed tombstone. The
// pending checkpoint remains durable, blocks success, and an exact
// same-operation retry redelivers the transition before completing.
func TestRetirement_EventPublishFailureStaysCheckpointed(t *testing.T) {
	ctx := context.Background()
	bus := &retirementFailOnceBus{EventBus: newBus(t)}
	reg := newRegistryOnBus(t, bus)
	retirer := reg.(agentcfg.RetirementRegistry)
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-event", UserID: "admin", SessionID: "control"}}
	const agent = "agent-event"
	payload := agentcfg.ConfigPayload{Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{{Name: "owned"}}}}
	rev, err := reg.SetRevision(ctx, id, agent, agentcfg.ConfigScopeAgent, payload, agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	bus.armed.Store(true)
	if _, err := retirer.Retire(ctx, id, agent, agentcfg.RetirementRequest{OperationID: "op-event", ExpectedContentHash: rev.ContentHash}); !errors.Is(err, agentcfg.ErrStateUnavailable) {
		t.Fatalf("retire with failed started event = %v, want state unavailable", err)
	}
	status, found, err := retirer.RetirementStatus(ctx, id, agent)
	if err != nil || !found || status.Completed {
		t.Fatalf("status after failed event = (%+v,%v,%v), want durable incomplete tombstone", status, found, err)
	}
	status, err = retirer.Retire(ctx, id, agent, agentcfg.RetirementRequest{OperationID: "op-event", ExpectedContentHash: rev.ContentHash})
	if err != nil {
		t.Fatalf("same-operation retry after failed event: %v", err)
	}
	if status.Completed || status.OperationID != "op-event" || len(status.Cleanup) != 1 {
		t.Fatalf("retry status=%+v, want pending stored operation", status)
	}
	status, err = retirer.CompleteRetirementStep(ctx, id, agent, "op-event", "mcp_connection", "owned")
	if err != nil {
		t.Fatalf("complete after replayed started event: %v", err)
	}
	if !status.Completed || !status.Cleanup[0].Completed {
		t.Fatalf("completed retry status=%+v, want acknowledged cleanup", status)
	}
}
