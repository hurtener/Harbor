package protocol_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/skills"
	localdb "github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func newStateStore(t *testing.T) (state.StateStore, error) {
	t.Helper()
	st, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		return nil, err
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return st, nil
}

const testAgentID = "agent-x"

func scope() prototypes.IdentityScope {
	return prototypes.IdentityScope{Tenant: "t", User: "u", Session: "s"}
}

func newRegistry(t *testing.T) agentcfg.Registry {
	reg, _ := newRegistryWithState(t)
	return reg
}

func newRegistryWithState(t *testing.T) (agentcfg.Registry, state.StateStore) {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32,
		IdleTimeout: 30 * time.Second, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	st, err := newStateStore(t)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()); _ = bus.Close(context.Background()) })
	return reg, st
}

func newSkills(t *testing.T) skills.SkillStore {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32,
		IdleTimeout: 30 * time.Second, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("skills bus: %v", err)
	}
	store, err := localdb.New(skills.ConfigSnapshot{Driver: "localdb", DSN: ":memory:"}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("localdb: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()); _ = bus.Close(context.Background()) })
	return store
}

func svc(t *testing.T, withSkills bool) *agentcfgprotocol.Service {
	t.Helper()
	opts := []agentcfgprotocol.Option{}
	if withSkills {
		opts = append(opts, agentcfgprotocol.WithSkillStore(newSkills(t)))
	}
	s, err := agentcfgprotocol.NewService(newRegistry(t), opts...)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return s
}

func TestNewService_NilRegistryFailsLoud(t *testing.T) {
	if _, err := agentcfgprotocol.NewService(nil); !errors.Is(err, agentcfgprotocol.ErrMisconfigured) {
		t.Fatalf("nil registry should fail with ErrMisconfigured, got %v", err)
	}
}

func TestSetGetDiffRollback_RoundTrip(t *testing.T) {
	ctx := context.Background()
	s := svc(t, false)
	rev1, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{Skills: &prototypes.AgentConfigSkillsSelection{Names: []string{"a"}}},
	})
	if err != nil {
		t.Fatalf("set rev1: %v", err)
	}
	rev2, err := s.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{Skills: &prototypes.AgentConfigSkillsSelection{Names: []string{"a", "b"}}},
	})
	if err != nil {
		t.Fatalf("set rev2: %v", err)
	}
	get, err := s.Get(ctx, prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil || !get.Set || get.Revision == nil {
		t.Fatalf("get: %+v err=%v", get, err)
	}
	if get.Revision.RevisionID != rev2.Revision.RevisionID {
		t.Fatalf("active=%q want %q", get.Revision.RevisionID, rev2.Revision.RevisionID)
	}
	if get.Revision.AuthorTenant != "t" || get.Revision.AuthorUser != "u" {
		t.Fatalf("author not projected: %+v", get.Revision)
	}
	list, err := s.ListRevisions(ctx, prototypes.AgentConfigListRevisionsRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil || len(list.Revisions) != 2 {
		t.Fatalf("list: %d err=%v", len(list.Revisions), err)
	}
	diff, err := s.Diff(ctx, prototypes.AgentConfigDiffRequest{
		Identity: scope(), AgentID: testAgentID, FromRevision: rev1.Revision.RevisionID, ToRevision: rev2.Revision.RevisionID,
	})
	if err != nil {
		t.Fatalf("diff: %v", err)
	}
	if len(diff.Diff.Skills.Added) != 1 || diff.Diff.Skills.Added[0] != "b" {
		t.Fatalf("diff added=%v", diff.Diff.Skills.Added)
	}
	rb, err := s.Rollback(ctx, prototypes.AgentConfigRollbackRequest{Identity: scope(), AgentID: testAgentID, RevisionID: rev1.Revision.RevisionID})
	if err != nil || rb.Revision.RevisionID != rev1.Revision.RevisionID {
		t.Fatalf("rollback: %+v err=%v", rb, err)
	}
}

func TestRollbackMissing_FailsLoud(t *testing.T) {
	s := svc(t, false)
	_, err := s.Rollback(context.Background(), prototypes.AgentConfigRollbackRequest{Identity: scope(), AgentID: testAgentID, RevisionID: "01ZZZNONE"})
	if !errors.Is(err, agentcfg.ErrRevisionNotFound) {
		t.Fatalf("rollback missing should be ErrRevisionNotFound, got %v", err)
	}
}

func TestIdentityRequired(t *testing.T) {
	s := svc(t, false)
	_, err := s.Get(context.Background(), prototypes.AgentConfigGetRequest{Identity: prototypes.IdentityScope{Tenant: "t"}, AgentID: testAgentID})
	if !errors.Is(err, agentcfgprotocol.ErrIdentityRequired) {
		t.Fatalf("incomplete identity should fail, got %v", err)
	}
	_, err = s.Get(context.Background(), prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: ""})
	if !errors.Is(err, agentcfgprotocol.ErrIdentityRequired) {
		t.Fatalf("empty agent id should fail, got %v", err)
	}
}

func TestSkillsUnavailable_WhenNoStore(t *testing.T) {
	s := svc(t, false)
	_, err := s.SkillsList(context.Background(), prototypes.AgentConfigSkillsListRequest{Identity: scope(), AgentID: testAgentID})
	if !errors.Is(err, agentcfgprotocol.ErrSkillsUnavailable) {
		t.Fatalf("skills without store should be ErrSkillsUnavailable, got %v", err)
	}
}

func TestSkillsUpsertRecordsRevisionAndDelete(t *testing.T) {
	ctx := context.Background()
	s := svc(t, true)
	up, err := s.SkillsUpsert(ctx, prototypes.AgentConfigSkillsUpsertRequest{
		Identity: scope(), AgentID: testAgentID,
		Skill: prototypes.AgentConfigSkillInput{
			Name: "deploy", Trigger: "when deploying", Steps: []string{"step1"},
			Origin: "generated", Scope: "session",
		},
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// A revision was recorded with the skill in membership.
	if names := up.Revision.Payload.Skills; names == nil || len(names.Names) != 1 || names.Names[0] != "deploy" {
		t.Fatalf("upsert revision membership=%+v", up.Revision.Payload.Skills)
	}
	if up.Skill.Name != "deploy" || up.Skill.ContentHash == "" {
		t.Fatalf("upsert skill summary missing: %+v", up.Skill)
	}
	// List returns it.
	list, err := s.SkillsList(ctx, prototypes.AgentConfigSkillsListRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil || len(list.Skills) != 1 {
		t.Fatalf("skills list: %d err=%v", len(list.Skills), err)
	}
	// Delete records a revision with empty membership.
	del, err := s.SkillsDelete(ctx, prototypes.AgentConfigSkillsDeleteRequest{Identity: scope(), AgentID: testAgentID, Name: "deploy"})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if del.Revision.Payload.Skills == nil || len(del.Revision.Payload.Skills.Names) != 0 {
		t.Fatalf("delete revision should have empty membership, got %+v", del.Revision.Payload.Skills)
	}
}

func TestSkillsUpsert_PackOverwriteRefused(t *testing.T) {
	ctx := context.Background()
	s := svc(t, true)
	// Seed a pack-origin skill.
	if _, err := s.SkillsUpsert(ctx, prototypes.AgentConfigSkillsUpsertRequest{
		Identity: scope(), AgentID: testAgentID,
		Skill: prototypes.AgentConfigSkillInput{Name: "p", Trigger: "t", Steps: []string{"s"}, Origin: "pack", Scope: "session"},
	}); err != nil {
		t.Fatalf("seed pack: %v", err)
	}
	// A non-pack overwrite must surface the typed refusal — never silent.
	_, err := s.SkillsUpsert(ctx, prototypes.AgentConfigSkillsUpsertRequest{
		Identity: scope(), AgentID: testAgentID,
		Skill: prototypes.AgentConfigSkillInput{Name: "p", Trigger: "t", Steps: []string{"s2"}, Origin: "generated", Scope: "session"},
	})
	if !errors.Is(err, skills.ErrPackOverwriteRefused) {
		t.Fatalf("pack overwrite should be refused with typed error, got %v", err)
	}
}

func TestSkillsDelete_MissingFailsLoud(t *testing.T) {
	s := svc(t, true)
	_, err := s.SkillsDelete(context.Background(), prototypes.AgentConfigSkillsDeleteRequest{Identity: scope(), AgentID: testAgentID, Name: "nope"})
	if !errors.Is(err, skills.ErrSkillNotFound) {
		t.Fatalf("delete missing should be ErrSkillNotFound, got %v", err)
	}
}
