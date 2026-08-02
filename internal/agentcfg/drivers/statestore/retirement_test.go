package statestore_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
)

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
