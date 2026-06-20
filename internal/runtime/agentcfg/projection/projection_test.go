package projection_test

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	"github.com/hurtener/Harbor/internal/skills"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

const (
	projTenant = "tenant-proj"
	projUser   = "user-proj"
	projSess   = "sess-proj"
	projAgent  = "agent-proj"
)

func projID() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: projTenant, UserID: projUser, SessionID: projSess}}
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

func views(names ...string) []skills.SkillView {
	out := make([]skills.SkillView, 0, len(names))
	for _, n := range names {
		out = append(out, skills.SkillView{Name: n})
	}
	return out
}

func names(vs []skills.SkillView) []string {
	out := make([]string, 0, len(vs))
	for _, v := range vs {
		out = append(out, v.Name)
	}
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestFilterSkillViewsByMembership_KeepsMembersDropsRest is the pure
// set-intersection contract.
func TestFilterSkillViewsByMembership_KeepsMembersDropsRest(t *testing.T) {
	got := names(projection.FilterSkillViewsByMembership(views("a", "b", "c"), []string{"a", "c", "z"}))
	if !eq(got, []string{"a", "c"}) {
		t.Fatalf("got %v, want [a c]", got)
	}
	// Empty membership keeps nothing (the rollback-to-empty case).
	if got := projection.FilterSkillViewsByMembership(views("a", "b"), nil); len(got) != 0 {
		t.Fatalf("empty membership kept %v, want none", names(got))
	}
}

// TestActiveSkillViews_NilRegistryOrEmptyAgentID_PassThrough proves the
// backward-compatible ungated path: with no registry or no agent id the
// views are returned unchanged.
func TestActiveSkillViews_NilRegistryOrEmptyAgentID_PassThrough(t *testing.T) {
	in := views("a", "b")
	got, err := projection.ActiveSkillViews(context.Background(), nil, projAgent, projID(), in)
	if err != nil {
		t.Fatalf("nil registry: %v", err)
	}
	if !eq(names(got), []string{"a", "b"}) {
		t.Fatalf("nil registry filtered: %v", names(got))
	}
	reg := newRegistry(t)
	got, err = projection.ActiveSkillViews(context.Background(), reg, "", projID(), in)
	if err != nil {
		t.Fatalf("empty agent id: %v", err)
	}
	if !eq(names(got), []string{"a", "b"}) {
		t.Fatalf("empty agent id filtered: %v", names(got))
	}
}

// TestActiveSkillViews_NoActiveRevision_PassThrough proves an agent with no
// config revision sees the unfiltered directory (backward compatible).
func TestActiveSkillViews_NoActiveRevision_PassThrough(t *testing.T) {
	reg := newRegistry(t)
	got, err := projection.ActiveSkillViews(context.Background(), reg, projAgent, projID(), views("a", "b"))
	if err != nil {
		t.Fatalf("no active revision: %v", err)
	}
	if !eq(names(got), []string{"a", "b"}) {
		t.Fatalf("no active revision filtered: %v", names(got))
	}
}

// TestActiveSkillViews_ActiveRevisionFilters proves a real active revision's
// skills membership narrows the directory views — through the REAL registry.
func TestActiveSkillViews_ActiveRevisionFilters(t *testing.T) {
	reg := newRegistry(t)
	ctx := context.Background()
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"a", "c"}},
	}); err != nil {
		t.Fatalf("set revision: %v", err)
	}
	got, err := projection.ActiveSkillViews(ctx, reg, projAgent, projID(), views("a", "b", "c"))
	if err != nil {
		t.Fatalf("active filter: %v", err)
	}
	if !eq(names(got), []string{"a", "c"}) {
		t.Fatalf("active revision projection = %v, want [a c]", names(got))
	}
}

// TestActiveSkillViews_RollbackChangesProjection proves the next-turn effect
// of a rollback: rollback repoints the active revision, and the projection
// reflects the rolled-back membership — the §13 consumer behaviour over the
// real registry.
func TestActiveSkillViews_RollbackChangesProjection(t *testing.T) {
	reg := newRegistry(t)
	ctx := context.Background()
	r1, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"a"}},
	})
	if err != nil {
		t.Fatalf("set r1: %v", err)
	}
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"a", "b", "c"}},
	}); err != nil {
		t.Fatalf("set r2: %v", err)
	}
	// Active now is r2 → projection keeps a,b,c.
	got, err := projection.ActiveSkillViews(ctx, reg, projAgent, projID(), views("a", "b", "c"))
	if err != nil {
		t.Fatalf("active r2: %v", err)
	}
	if !eq(names(got), []string{"a", "b", "c"}) {
		t.Fatalf("r2 projection = %v, want [a b c]", names(got))
	}
	// Roll back to r1 → projection narrows to just a (next-turn effect).
	if _, err := reg.Rollback(ctx, projID(), projAgent, r1.RevisionID); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	got, err = projection.ActiveSkillViews(ctx, reg, projAgent, projID(), views("a", "b", "c"))
	if err != nil {
		t.Fatalf("active after rollback: %v", err)
	}
	if !eq(names(got), []string{"a"}) {
		t.Fatalf("post-rollback projection = %v, want [a]", names(got))
	}
}
