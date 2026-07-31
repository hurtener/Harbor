package statestore_test

import (
	"context"
	"errors"
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
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// reserved sentinel + record kinds mirrored from the driver (an unexported
// constant; the test asserts the pinned keying scheme directly against the
// StateStore records).
const (
	reservedUser        = "__agentcfg__"
	kindActive          = "agentcfg.active"
	kindUserActive      = "agentcfg.user.active"
	kindUserRevisionPfx = "agentcfg.user.revision."
)

// newRegistryWithStore builds a registry AND returns the underlying
// StateStore so a test can inspect the persisted record keying/kinds (the
// PINNED keying scheme is asserted directly against the store).
func newRegistryWithStore(t *testing.T) (agentcfg.Registry, state.StateStore) {
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
	return reg, st
}

const tenantT = "t"

func agentQuad(agentID string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: tenantT, UserID: reservedUser, SessionID: agentID}}
}

func userQuad(user, agentID string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: tenantT, UserID: user, SessionID: agentID}}
}

// TestStateStore_UserScope_KeyingAndKinds asserts a ConfigScopeUser write
// persists under the caller's REAL (tenant, user) + agent-in-session AND the
// distinct agentcfg.user.* kinds — and writes NOTHING onto the agent-level
// synthetic chain.
func TestStateStore_UserScope_KeyingAndKinds(t *testing.T) {
	ctx := context.Background()
	r, st := newRegistryWithStore(t)
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "alice", SessionID: "s"}}
	const agent = "a1"
	if _, err := r.SetRevision(ctx, id, agent, agentcfg.ConfigScopeUser, skills("u1"), agentcfg.SetOptions{}); err != nil {
		t.Fatalf("user set: %v", err)
	}
	// The user active pointer lives under the REAL (tenant, user)+agent slot
	// and the agentcfg.user.active kind.
	if _, err := st.Load(ctx, userQuad("alice", agent), kindUserActive); err != nil {
		t.Fatalf("user active pointer not at the user key/kind: %v", err)
	}
	// The agent-level synthetic chain is untouched — no agentcfg.active record.
	if _, err := st.Load(ctx, agentQuad(agent), kindActive); err == nil {
		t.Fatalf("user write leaked onto the agent-level synthetic chain")
	}
}

// TestStateStore_AgentScope_Golden_KindsUnchanged pins that the
// ConfigScopeAgent keying + kinds are byte-identical to before (the synthetic
// slot + agentcfg.active / agentcfg.revision.), and that an agent write puts
// NOTHING onto the user chain.
func TestStateStore_AgentScope_Golden_KindsUnchanged(t *testing.T) {
	ctx := context.Background()
	r, st := newRegistryWithStore(t)
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "alice", SessionID: "s"}}
	const agent = "a1"
	rev, err := r.SetRevision(ctx, id, agent, agentcfg.ConfigScopeAgent, skills("g1"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("agent set: %v", err)
	}
	// active pointer + the revision record persist at the synthetic identity
	// and the existing kinds.
	if _, err := st.Load(ctx, agentQuad(agent), kindActive); err != nil {
		t.Fatalf("agent active pointer not at the synthetic key/kind: %v", err)
	}
	if _, err := st.Load(ctx, agentQuad(agent), "agentcfg.revision."+rev.RevisionID); err != nil {
		t.Fatalf("agent revision not at the synthetic key/kind: %v", err)
	}
	// Nothing on the user chain.
	if _, err := st.Load(ctx, userQuad("alice", agent), kindUserActive); err == nil {
		t.Fatalf("agent write leaked onto the user chain")
	}
}

// TestStateStore_CrossUserIsolation proves user A's variant is invisible to
// user B: B's Active is empty, and B's Diff/Rollback of A's revision id fail
// loud with ErrRevisionNotFound (the id lives under A's key).
func TestStateStore_CrossUserIsolation(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	const agent = "a1"
	a := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "alice", SessionID: "sa"}}
	b := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "bob", SessionID: "sb"}}
	aRev, err := r.SetRevision(ctx, a, agent, agentcfg.ConfigScopeUser, skills("alice-only"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("alice set: %v", err)
	}
	if _, set, err := r.Active(ctx, b, agent, agentcfg.ConfigScopeUser); err != nil || set {
		t.Fatalf("bob sees alice's variant: set=%v err=%v", set, err)
	}
	if _, err := r.Rollback(ctx, b, agent, aRev.RevisionID, agentcfg.ConfigScopeUser, agentcfg.SetOptions{}); !errors.Is(err, agentcfg.ErrRevisionNotFound) {
		t.Fatalf("bob rolled back alice's revision: %v", err)
	}
	if _, err := r.Diff(ctx, b, agent, aRev.RevisionID, aRev.RevisionID, agentcfg.ConfigScopeUser); !errors.Is(err, agentcfg.ErrRevisionNotFound) {
		t.Fatalf("bob diffed alice's revision: %v", err)
	}
	// Direct Get of alice's revision id under bob's identity must also
	// fail closed (the id lives under alice's key, not bob's) — no
	// existence leak, even by id.
	if _, err := r.Get(ctx, b, agent, aRev.RevisionID, agentcfg.ConfigScopeUser); !errors.Is(err, agentcfg.ErrRevisionNotFound) {
		t.Fatalf("bob got alice's revision by id: %v", err)
	}
}

// TestStateStore_SentinelCollisionRejected proves a ConfigScopeUser call
// whose verified user id equals the reserved sentinel is rejected loud AND
// neither reads nor clobbers the agent-level chain.
func TestStateStore_SentinelCollisionRejected(t *testing.T) {
	ctx := context.Background()
	r, st := newRegistryWithStore(t)
	const agent = "a1"
	// Seed an agent-level revision (the chain the sentinel would alias).
	admin := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "admin", SessionID: "s"}}
	if _, err := r.SetRevision(ctx, admin, agent, agentcfg.ConfigScopeAgent, skills("operator-secret"), agentcfg.SetOptions{}); err != nil {
		t.Fatalf("seed agent chain: %v", err)
	}
	before, err := st.Load(ctx, agentQuad(agent), kindActive)
	if err != nil {
		t.Fatalf("load seeded active: %v", err)
	}
	// A user-scope caller whose user id IS the reserved sentinel is rejected
	// before any read or write.
	sentinel := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: reservedUser, SessionID: "s"}}
	if _, _, err := r.Active(ctx, sentinel, agent, agentcfg.ConfigScopeUser); !errors.Is(err, agentcfg.ErrReservedUser) {
		t.Fatalf("sentinel Active not rejected: %v", err)
	}
	if _, err := r.SetRevision(ctx, sentinel, agent, agentcfg.ConfigScopeUser, skills("attacker"), agentcfg.SetOptions{}); !errors.Is(err, agentcfg.ErrReservedUser) {
		t.Fatalf("sentinel SetRevision not rejected: %v", err)
	}
	// The agent-level active pointer's bytes are unchanged (no read, no clobber).
	after, err := st.Load(ctx, agentQuad(agent), kindActive)
	if err != nil {
		t.Fatalf("reload active: %v", err)
	}
	if string(before.Bytes) != string(after.Bytes) {
		t.Fatalf("agent-level active pointer clobbered by a rejected sentinel call")
	}
}

// TestStateStore_UserScope_IdempotentReset proves the idempotent re-set
// parity holds on the user arm.
func TestStateStore_UserScope_IdempotentReset(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "alice", SessionID: "s"}}
	const agent = "a1"
	r1, err := r.SetRevision(ctx, id, agent, agentcfg.ConfigScopeUser, skills("a", "b"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("set1: %v", err)
	}
	r2, err := r.SetRevision(ctx, id, agent, agentcfg.ConfigScopeUser, skills("b", "a"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("set2: %v", err)
	}
	if r1.RevisionID != r2.RevisionID {
		t.Fatalf("idempotent re-set minted a new revision: %q != %q", r1.RevisionID, r2.RevisionID)
	}
	revs, err := r.ListRevisions(ctx, id, agent, agentcfg.ConfigScopeUser, 0)
	if err != nil || len(revs) != 1 {
		t.Fatalf("user list after idempotent re-set: n=%d err=%v", len(revs), err)
	}
}

// TestStateStore_UserScope_ListRevisionsFilter proves ListRevisions on the
// user arm returns only the caller's own revisions (filtered to the user slot
// + the agentcfg.user.revision. prefix), never the agent chain or another
// user's.
func TestStateStore_UserScope_ListRevisionsFilter(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	const agent = "a1"
	a := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "alice", SessionID: "s"}}
	b := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "bob", SessionID: "s"}}
	admin := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "admin", SessionID: "s"}}
	_, _ = r.SetRevision(ctx, admin, agent, agentcfg.ConfigScopeAgent, skills("agent"), agentcfg.SetOptions{})
	_, _ = r.SetRevision(ctx, a, agent, agentcfg.ConfigScopeUser, skills("a1"), agentcfg.SetOptions{})
	_, _ = r.SetRevision(ctx, a, agent, agentcfg.ConfigScopeUser, skills("a1", "a2"), agentcfg.SetOptions{})
	_, _ = r.SetRevision(ctx, b, agent, agentcfg.ConfigScopeUser, skills("b1"), agentcfg.SetOptions{})
	aList, err := r.ListRevisions(ctx, a, agent, agentcfg.ConfigScopeUser, 0)
	if err != nil {
		t.Fatalf("alice list: %v", err)
	}
	if len(aList) != 2 {
		t.Fatalf("alice user list should have exactly 2 (not agent or bob): got %d", len(aList))
	}
}

// TestStateStore_UserScope_GuardBranches covers the cheap-to-reach guard
// branches on the user arm: a cancelled context fails fast on every method,
// and an empty revision id / empty diff arg fails loud.
func TestStateStore_UserScope_GuardBranches(t *testing.T) {
	r := newRegistry(t)
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "alice", SessionID: "s"}}
	const agent = "a1"

	// Empty revision id / empty diff args fail loud (not-found), independent
	// of ctx.
	bg := context.Background()
	if _, err := r.Get(bg, id, agent, "", agentcfg.ConfigScopeUser); !errors.Is(err, agentcfg.ErrRevisionNotFound) {
		t.Errorf("empty-id Get should be ErrRevisionNotFound: %v", err)
	}
	if _, err := r.Rollback(bg, id, agent, "", agentcfg.ConfigScopeUser, agentcfg.SetOptions{}); !errors.Is(err, agentcfg.ErrRevisionNotFound) {
		t.Errorf("empty-id Rollback should be ErrRevisionNotFound: %v", err)
	}
	if _, err := r.Diff(bg, id, agent, "", "x", agentcfg.ConfigScopeUser); !errors.Is(err, agentcfg.ErrRevisionNotFound) {
		t.Errorf("empty-from Diff should be ErrRevisionNotFound: %v", err)
	}

	// A cancelled context fails fast on every method (after the cheap guards).
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.SetRevision(cctx, id, agent, agentcfg.ConfigScopeUser, skills("x"), agentcfg.SetOptions{}); err == nil {
		t.Error("SetRevision ignored a cancelled ctx")
	}
	if _, _, err := r.Active(cctx, id, agent, agentcfg.ConfigScopeUser); err == nil {
		t.Error("Active ignored a cancelled ctx")
	}
	if _, err := r.Get(cctx, id, agent, "rev", agentcfg.ConfigScopeUser); err == nil {
		t.Error("Get ignored a cancelled ctx")
	}
	if _, err := r.ListRevisions(cctx, id, agent, agentcfg.ConfigScopeUser, 0); err == nil {
		t.Error("ListRevisions ignored a cancelled ctx")
	}
	if _, err := r.Rollback(cctx, id, agent, "rev", agentcfg.ConfigScopeUser, agentcfg.SetOptions{}); err == nil {
		t.Error("Rollback ignored a cancelled ctx")
	}
	if _, err := r.Diff(cctx, id, agent, "a", "b", agentcfg.ConfigScopeUser); err == nil {
		t.Error("Diff ignored a cancelled ctx")
	}
}

func skills(names ...string) agentcfg.ConfigPayload {
	return agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: names}}
}

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
	conformance.Run(t, newRegistry, newFaultyRegistry)
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
	if _, err := r.SetRevision(ctx, a, agent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"alpha"}}}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("set a: %v", err)
	}
	if _, err := r.SetRevision(ctx, b, agent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"beta"}}}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("set b: %v", err)
	}
	ra, _, _ := r.Active(ctx, a, agent, agentcfg.ConfigScopeAgent)
	rb, _, _ := r.Active(ctx, b, agent, agentcfg.ConfigScopeAgent)
	if got := ra.Payload.SkillNames(); len(got) != 1 || got[0] != "alpha" {
		t.Fatalf("tenant-a leaked: %v", got)
	}
	if got := rb.Payload.SkillNames(); len(got) != 1 || got[0] != "beta" {
		t.Fatalf("tenant-b leaked: %v", got)
	}
	// ListRevisions must not cross tenants.
	la, _ := r.ListRevisions(ctx, a, agent, agentcfg.ConfigScopeAgent, 0)
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
			rev1, err := r.SetRevision(ctx, id, agent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{fmt.Sprintf("skill-%d-a", i)}}}, agentcfg.SetOptions{})
			if err != nil {
				errs <- fmt.Errorf("set1 %d: %w", i, err)
				return
			}
			rev2, err := r.SetRevision(ctx, id, agent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{fmt.Sprintf("skill-%d-a", i), fmt.Sprintf("skill-%d-b", i)}}}, agentcfg.SetOptions{})
			if err != nil {
				errs <- fmt.Errorf("set2 %d: %w", i, err)
				return
			}
			active, ok, err := r.Active(ctx, id, agent, agentcfg.ConfigScopeAgent)
			if err != nil || !ok {
				errs <- fmt.Errorf("active %d ok=%v: %w", i, ok, err)
				return
			}
			// No context bleed: this run's active must be its own rev2.
			if active.RevisionID != rev2.RevisionID {
				errs <- fmt.Errorf("context bleed at %d: active=%q want %q", i, active.RevisionID, rev2.RevisionID)
				return
			}
			d, err := r.Diff(ctx, id, agent, rev1.RevisionID, rev2.RevisionID, agentcfg.ConfigScopeAgent)
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
	rev1, _ := r.SetRevision(ctx, id, agent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"a"}}}, agentcfg.SetOptions{})
	_, _ = r.SetRevision(ctx, id, agent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"a", "b"}}}, agentcfg.SetOptions{})
	if _, err := r.Rollback(ctx, id, agent, rev1.RevisionID, agentcfg.ConfigScopeAgent, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	// A new revision after rollback chains from rev1 (the now-active).
	rev3, err := r.SetRevision(ctx, id, agent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"a", "z"}}}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("set rev3: %v", err)
	}
	if rev3.ParentRevisionID != rev1.RevisionID {
		t.Fatalf("rev3 parent=%q want %q (active after rollback)", rev3.ParentRevisionID, rev1.RevisionID)
	}
}

// TestStateStore_Hooks_RoundTrip_SectionMerge_Diff_Rollback exercises the
// run-completion-hook section end-to-end through the driver: a set_revision
// carrying the hooks section alongside a sibling section round-trips both
// (section preservation); a second revision changing only the hook produces a
// distinct revision whose Diff reports the hooks arm; a rollback repoints the
// active pointer back to the first hook (D-280).
func TestStateStore_Hooks_RoundTrip_SectionMerge_Diff_Rollback(t *testing.T) {
	ctx := context.Background()
	r := newRegistry(t)
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	const agent = "hook-agent"

	// Revision 1: skills + hooks together — both must round-trip.
	rev1, err := r.SetRevision(ctx, id, agent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"s1"}},
		Hooks:  &agentcfg.HooksSection{RunCompletion: &agentcfg.RunCompletionHook{Tool: "sink-a", TimeoutMS: 5000}},
	}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("set rev1: %v", err)
	}
	active, ok, err := r.Active(ctx, id, agent, agentcfg.ConfigScopeAgent)
	if err != nil || !ok {
		t.Fatalf("active after rev1: ok=%v err=%v", ok, err)
	}
	rc, set := active.Payload.RunCompletionHookView()
	if !set || rc.Tool != "sink-a" || rc.TimeoutMS != 5000 {
		t.Fatalf("hooks section did not round-trip: %+v (set=%v)", rc, set)
	}
	if names := active.Payload.SkillNames(); len(names) != 1 || names[0] != "s1" {
		t.Fatalf("sibling skills section not preserved alongside hooks: %v", names)
	}

	// Revision 2: change only the hook tool.
	rev2, err := r.SetRevision(ctx, id, agent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"s1"}},
		Hooks:  &agentcfg.HooksSection{RunCompletion: &agentcfg.RunCompletionHook{Tool: "sink-b", TimeoutMS: 5000}},
	}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("set rev2: %v", err)
	}
	if rev2.RevisionID == rev1.RevisionID {
		t.Fatal("a hook-only change did not produce a distinct revision")
	}

	// Diff surfaces the hooks arm (tool changed, timeout unchanged).
	d, err := r.Diff(ctx, id, agent, rev1.RevisionID, rev2.RevisionID, agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if !d.Hooks.Changed() || !d.Hooks.RunCompletionToolChanged ||
		d.Hooks.RunCompletionToolFrom != "sink-a" || d.Hooks.RunCompletionToolTo != "sink-b" {
		t.Errorf("hooks diff arm = %+v, want tool sink-a→sink-b", d.Hooks)
	}
	if d.Hooks.RunCompletionTimeoutChanged {
		t.Errorf("timeout should be unchanged in the diff: %+v", d.Hooks)
	}
	if d.Skills.Changed() {
		t.Errorf("skills should be unchanged (a hook-only edit): %+v", d.Skills)
	}

	// Rollback to rev1 → active hook is sink-a again.
	if _, err := r.Rollback(ctx, id, agent, rev1.RevisionID, agentcfg.ConfigScopeAgent, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	active2, _, err := r.Active(ctx, id, agent, agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("active after rollback: %v", err)
	}
	rc2, _ := active2.Payload.RunCompletionHookView()
	if rc2 == nil || rc2.Tool != "sink-a" {
		t.Fatalf("rollback did not restore the sink-a hook: %+v", rc2)
	}
}
