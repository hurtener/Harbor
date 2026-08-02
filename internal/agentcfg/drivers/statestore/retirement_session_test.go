package statestore_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	skillpkg "github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

type retirementSessionCASBarrier struct {
	state.StateStore
	entered chan struct{}
	release chan struct{}
}

func (s *retirementSessionCASBarrier) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if strings.HasPrefix(next.Kind, "agentcfg.session_personal.v1.") {
		s.entered <- struct{}{}
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.StateStore.SaveIf(ctx, expectations, next)
}

func retirementPersonalSkill(name string) skillpkg.Skill {
	return skillpkg.Skill{Name: name, Trigger: "when needed", Steps: []string{"do it"}, Origin: skillpkg.OriginGenerated, Scope: skillpkg.ScopeSession}
}

func TestRetirement_Phase233aManifestExactAndFourSlotCleanup(t *testing.T) {
	ctx := context.Background()
	registry, st := newRegistryWithStore(t)
	personal, err := sessionoverlay.NewDurableStore(st, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	overlays, err := sessionoverlay.NewStore(st, time.Now)
	if err != nil {
		t.Fatal(err)
	}

	target := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "alice", SessionID: "session-a"}}
	siblingSession := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "bob", SessionID: "session-b"}}
	otherTenant := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-b", UserID: "alice", SessionID: "session-a"}}
	admin := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-a", UserID: "admin", SessionID: "control"}}
	otherAdmin := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-b", UserID: "admin", SessionID: "control"}}

	var targetRevision agentcfg.Revision
	for _, seed := range []struct {
		id    identity.Quadruple
		agent string
	}{
		{id: admin, agent: "a"},
		{id: admin, agent: "ab"},
		{id: otherAdmin, agent: "a"},
	} {
		rev, setErr := registry.SetRevision(ctx, seed.id, seed.agent, agentcfg.ConfigScopeAgent, skillsPayload(seed.agent), agentcfg.SetOptions{})
		if setErr != nil {
			t.Fatalf("seed lifecycle %s/%s: %v", seed.id.TenantID, seed.agent, setErr)
		}
		if seed.id.TenantID == admin.TenantID && seed.agent == "a" {
			targetRevision = rev
		}
	}
	for _, seed := range []struct {
		id    identity.Quadruple
		agent string
		name  string
	}{
		{id: target, agent: "a", name: "alpha"},
		{id: siblingSession, agent: "a", name: "beta"},
		{id: target, agent: "ab", name: "ab-skill"},
		{id: otherTenant, agent: "a", name: "other-tenant"},
	} {
		if _, saveErr := personal.SavePersonal(ctx, seed.id, seed.agent, retirementPersonalSkill(seed.name), "", ""); saveErr != nil {
			t.Fatalf("seed personal %s/%s/%s: %v", seed.id.TenantID, seed.agent, seed.name, saveErr)
		}
		if _, setErr := overlays.SetUserPrompt(ctx, seed.id, seed.agent, "prompt-"+seed.name); setErr != nil {
			t.Fatalf("seed overlay %s/%s/%s: %v", seed.id.TenantID, seed.agent, seed.name, setErr)
		}
	}

	retirer := registry.(agentcfg.RetirementRegistry)
	status, err := retirer.Retire(ctx, admin, "a", agentcfg.RetirementRequest{OperationID: "phase233a-op", ExpectedContentHash: targetRevision.ContentHash})
	if err != nil {
		t.Fatalf("retire: %v", err)
	}
	counts := map[string]int{}
	for !status.Completed {
		if len(status.Cleanup) != 1 {
			t.Fatalf("bounded pending status=%+v", status)
		}
		step := status.Cleanup[0]
		counts[step.Class]++
		status, err = retirer.CompleteRetirementStep(ctx, admin, "a", status.OperationID, step.Class, step.Resource)
		if err != nil {
			t.Fatalf("complete %s: %v", step.Class, err)
		}
	}
	if counts["session_personal"] != 2 || counts["legacy_session_overlay"] != 2 || !status.Completed {
		t.Fatalf("manifest counts=%v completed=%v", counts, status.Completed)
	}
	if _, _, err := personal.LoadPersonal(ctx, target, "a", "alpha"); !errors.Is(err, agentcfg.ErrAgentRetired) {
		t.Fatalf("retired personal read=%v", err)
	}
	if _, _, err := overlays.Get(ctx, siblingSession, "a"); !errors.Is(err, agentcfg.ErrAgentRetired) {
		t.Fatalf("retired overlay read=%v", err)
	}
	for _, survivor := range []struct {
		id    identity.Quadruple
		agent string
		name  string
	}{
		{id: target, agent: "ab", name: "ab-skill"},
		{id: otherTenant, agent: "a", name: "other-tenant"},
	} {
		if got, found, loadErr := personal.LoadPersonal(ctx, survivor.id, survivor.agent, survivor.name); loadErr != nil || !found || got.Deleted {
			t.Fatalf("survivor %s/%s=(%+v,%v,%v)", survivor.id.TenantID, survivor.agent, got, found, loadErr)
		}
	}
	kind, _ := sessionoverlay.PersonalSkillKind("a", "alpha")
	physical, err := st.Load(ctx, target, kind)
	if tombstoneErr := assertPersonalTombstone(physical.Bytes); err != nil || tombstoneErr != nil {
		t.Fatalf("logical tombstone=(%s,%v)", physical.Bytes, err)
	}
	if _, err := st.Load(ctx, target, sessionoverlay.LegacyOverlayKind("a")); err != nil {
		t.Fatalf("legacy compatibility row was physically removed: %v", err)
	}
}

// TestRetirement_Phase233aN100StalePersonalCAS exercises one shared compiled
// registry/store with 100 personal writers that all captured the old lifecycle
// generation. The retirement CAS wins first; every four-slot write then fails
// and no stale session record appears outside the frozen scan.
func TestRetirement_Phase233aN100StalePersonalCAS(t *testing.T) {
	ctx := context.Background()
	base, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	barrier := &retirementSessionCASBarrier{StateStore: base, entered: make(chan struct{}, 100), release: make(chan struct{})}
	registry := newRegistryOnStore(t, barrier)
	personal, err := sessionoverlay.NewDurableStore(barrier, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	admin := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant-race", UserID: "admin", SessionID: "control"}}
	const agent = "agent-race"
	revision, err := registry.SetRevision(ctx, admin, agent, agentcfg.ConfigScopeAgent, skills("seed"), agentcfg.SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	errCh := make(chan error, 100)
	var wg sync.WaitGroup
	for i := range 100 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			session := identity.Quadruple{Identity: identity.Identity{TenantID: admin.TenantID, UserID: fmt.Sprintf("user-%03d", i), SessionID: "session"}}
			_, saveErr := personal.SavePersonal(ctx, session, agent, retirementPersonalSkill(fmt.Sprintf("skill-%03d", i)), "", "")
			errCh <- saveErr
		}(i)
	}
	for range 100 {
		<-barrier.entered
	}
	status, err := registry.(agentcfg.RetirementRegistry).Retire(ctx, admin, agent, agentcfg.RetirementRequest{OperationID: "session-race-op", ExpectedContentHash: revision.ContentHash})
	if err != nil || !status.Completed {
		t.Fatalf("retire=(%+v,%v)", status, err)
	}
	close(barrier.release)
	wg.Wait()
	close(errCh)
	for saveErr := range errCh {
		if !errors.Is(saveErr, state.ErrConditionFailed) {
			t.Fatalf("stale personal save=%v, want condition failure", saveErr)
		}
	}
	prefix, _ := sessionoverlay.PersonalSkillPrefix(agent)
	page, err := base.ScanKindForTenant(ctx, state.ListScope{MaintenanceScoped: true}, admin.TenantID, prefix, state.MaxStateScanLimit, "")
	if err != nil || len(page.Records) != 0 {
		t.Fatalf("stale records=(%d,%v)", len(page.Records), err)
	}
}

func skillsPayload(name string) agentcfg.ConfigPayload { return skills(name) }

func assertPersonalTombstone(data []byte) error {
	if len(data) == 0 || !containsAll(string(data), `"agent_id":"a"`, `"canonical_name":"alpha"`, `"deleted":true`) {
		return errors.New("not an exact personal tombstone")
	}
	return nil
}

func containsAll(value string, fragments ...string) bool {
	for _, fragment := range fragments {
		if !strings.Contains(value, fragment) {
			return false
		}
	}
	return true
}
