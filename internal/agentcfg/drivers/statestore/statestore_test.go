package statestore_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/conformance"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func newRegistry(t *testing.T) agentcfg.Registry {
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
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = st.Close(context.Background())
	})
	return reg
}

// TestStateStore_Conformance runs the shared driver conformance suite.
func TestStateStore_Conformance(t *testing.T) {
	conformance.Run(t, newRegistry)
}

// TestStateStore_TenantIsolation asserts agent_id is a key, not an
// isolation filter: two tenants' configs for the SAME agent id stay
// isolated.
func TestStateStore_TenantIsolation_SameAgentID(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	a := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "u", SessionID: "s"}}
	b := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-b", UserID: "u", SessionID: "s"}}
	const agent = "shared-agent-id"
	if _, err := r.SetRevision(ctx, a, agent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"alpha"}}}); err != nil {
		t.Fatalf("set a: %v", err)
	}
	if _, err := r.SetRevision(ctx, b, agent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"beta"}}}); err != nil {
		t.Fatalf("set b: %v", err)
	}
	ra, _, _ := r.Active(ctx, a, agent)
	rb, _, _ := r.Active(ctx, b, agent)
	if got := ra.Payload.SkillNames(); len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("tenant-a leaked: %v", got)
	}
	if got := rb.Payload.SkillNames(); len(got) != 1 || got[0] != "beta" {
		t.Fatalf("tenant-b leaked: %v", got)
	}
	// ListRevisions must not cross tenants.
	la, _ := r.ListRevisions(ctx, a, agent, 0)
	if len(la) != 1 {
		t.Fatalf("tenant-a list crossed tenant boundary: %d", len(la))
	}
}

// TestStateStore_ConcurrentReuse exercises N concurrent SetRevision +
// Active + Diff against ONE shared Registry (D-025). Run with -race.
func TestStateStore_ConcurrentReuse(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	base := runtime.NumGoroutine()

	const n = 120
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make(chan error, n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			// Each goroutine owns a distinct agent id + tenant so there is
			// no cross-run bleed and the assertion is exact.
			id := identity.Quadruple{Identity: identity.Identity{
				TenantID: fmt.Sprintf("tenant-%d", i), UserID: "u", SessionID: "s",
			}}
			agent := fmt.Sprintf("agent-%d", i)
			rev1, err := r.SetRevision(ctx, id, agent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{fmt.Sprintf("skill-%d-a", i)}}})
			if err != nil {
				errs <- fmt.Errorf("set1 %d: %w", i, err)
				return
			}
			rev2, err := r.SetRevision(ctx, id, agent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{fmt.Sprintf("skill-%d-a", i), fmt.Sprintf("skill-%d-b", i)}}})
			if err != nil {
				errs <- fmt.Errorf("set2 %d: %w", i, err)
				return
			}
			active, ok, err := r.Active(ctx, id, agent)
			if err != nil || !ok {
				errs <- fmt.Errorf("active %d ok=%v: %w", i, ok, err)
				return
			}
			// No context bleed: this run's active must be its own rev2.
			if active.RevisionID != rev2.RevisionID {
				errs <- fmt.Errorf("context bleed at %d: active=%q want %q", i, active.RevisionID, rev2.RevisionID)
				return
			}
			d, err := r.Diff(ctx, id, agent, rev1.RevisionID, rev2.RevisionID)
			if err != nil {
				errs <- fmt.Errorf("diff %d: %w", i, err)
				return
			}
			if len(d.Skills.Added) != 1 {
				errs <- fmt.Errorf("diff %d added=%v", i, d.Skills.Added)
				return
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	// Goroutine baseline restored (allow brief settle).
	for range 20 {
		if runtime.NumGoroutine() <= base+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - base; delta > 5 {
		t.Errorf("goroutine leak: baseline=%d now=%d (delta=%d)", base, runtime.NumGoroutine(), delta)
	}
}

// TestStateStore_RevisionsSurviveRollback asserts content-addressing +
// the parent chain after a rollback-then-new-revision.
func TestStateStore_NewRevisionAfterRollbackChainsFromActive(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	const agent = "a"
	rev1, _ := r.SetRevision(ctx, id, agent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"a"}}})
	_, _ = r.SetRevision(ctx, id, agent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"a", "b"}}})
	if _, err := r.Rollback(ctx, id, agent, rev1.RevisionID); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	// A new revision after rollback chains from rev1 (the now-active).
	rev3, err := r.SetRevision(ctx, id, agent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"a", "z"}}})
	if err != nil {
		t.Fatalf("set rev3: %v", err)
	}
	if rev3.ParentRevisionID != rev1.RevisionID {
		t.Fatalf("rev3 parent=%q want %q (active after rollback)", rev3.ParentRevisionID, rev1.RevisionID)
	}
}
