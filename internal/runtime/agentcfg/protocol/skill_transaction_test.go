package protocol_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/skills"
)

type observedSkillStore struct {
	skills.SkillStore
	mu      sync.Mutex
	upserts int
	deletes int
}

func (s *observedSkillStore) Upsert(ctx context.Context, id identity.Quadruple, skill skills.Skill) error {
	s.mu.Lock()
	s.upserts++
	s.mu.Unlock()
	return s.SkillStore.Upsert(ctx, id, skill)
}

func (s *observedSkillStore) Delete(ctx context.Context, id identity.Quadruple, name string, scope skills.Scope) error {
	s.mu.Lock()
	s.deletes++
	s.mu.Unlock()
	return s.SkillStore.Delete(ctx, id, name, scope)
}

func (s *observedSkillStore) DeleteAgent(ctx context.Context, id identity.Quadruple, agentID, name string, scope skills.Scope) error {
	s.mu.Lock()
	s.deletes++
	s.mu.Unlock()
	return s.SkillStore.DeleteAgent(ctx, id, agentID, name, scope)
}

func (s *observedSkillStore) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upserts, s.deletes
}

func (s *observedSkillStore) reset() {
	s.mu.Lock()
	s.upserts, s.deletes = 0, 0
	s.mu.Unlock()
}

type setFailureRegistry struct {
	agentcfg.Registry
	err error
}

func (r setFailureRegistry) SetRevision(context.Context, identity.Quadruple, string, agentcfg.ConfigScope, agentcfg.ConfigPayload, agentcfg.SetOptions) (agentcfg.Revision, error) {
	return agentcfg.Revision{}, r.err
}

func TestSkillMutationDoors_StaleExpectationHasNoBodySideEffect(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		run  func(*agentcfgprotocol.Service, string) error
	}{
		{"admin_upsert", func(s *agentcfgprotocol.Service, token string) error {
			_, err := s.SkillsUpsert(ctx, prototypes.AgentConfigSkillsUpsertRequest{Identity: scope(), AgentID: testAgentID, Skill: condSkillInput("guarded"), ExpectedContentHash: token})
			return err
		}},
		{"admin_delete", func(s *agentcfgprotocol.Service, token string) error {
			_, err := s.SkillsDelete(ctx, prototypes.AgentConfigSkillsDeleteRequest{Identity: scope(), AgentID: testAgentID, Name: "guarded", ExpectedContentHash: token})
			return err
		}},
		{"user_upsert", func(s *agentcfgprotocol.Service, token string) error {
			_, err := s.UserSkillsUpsert(ctx, prototypes.AgentConfigUserSkillsUpsertRequest{Identity: scope(), AgentID: testAgentID, Skill: condSkillInput("guarded"), ExpectedContentHash: token})
			return err
		}},
		{"user_delete", func(s *agentcfgprotocol.Service, token string) error {
			_, err := s.UserSkillsDelete(ctx, prototypes.AgentConfigUserSkillsDeleteRequest{Identity: scope(), AgentID: testAgentID, Name: "guarded", ExpectedContentHash: token})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg := newRegistry(t)
			stale := staleTokenFor(t, reg, map[bool]agentcfg.ConfigScope{true: agentcfg.ConfigScopeUser}[tc.name[:4] == "user"])
			store := &observedSkillStore{SkillStore: newSkills(t)}
			svc, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithSkillStore(store))
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			if err := tc.run(svc, stale); !errors.Is(err, agentcfg.ErrRevisionConflict) {
				t.Fatalf("got %v, want ErrRevisionConflict", err)
			}
			if upserts, deletes := store.counts(); upserts != 0 || deletes != 0 {
				t.Fatalf("stale refusal touched SkillStore: upserts=%d deletes=%d", upserts, deletes)
			}
		})
	}
}

func TestSkillMutationDoors_RevisionFailureCompensatesBody(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("injected revision failure")
	for _, user := range []bool{false, true} {
		name := "admin"
		if user {
			name = "user"
		}
		t.Run(name+"_upsert", func(t *testing.T) {
			reg := newRegistry(t)
			store := &observedSkillStore{SkillStore: newSkills(t)}
			svc, err := agentcfgprotocol.NewService(setFailureRegistry{Registry: reg, err: boom}, agentcfgprotocol.WithSkillStore(store))
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			if user {
				_, err = svc.UserSkillsUpsert(ctx, prototypes.AgentConfigUserSkillsUpsertRequest{Identity: scope(), AgentID: testAgentID, Skill: condSkillInput("rollback")})
			} else {
				_, err = svc.SkillsUpsert(ctx, prototypes.AgentConfigSkillsUpsertRequest{Identity: scope(), AgentID: testAgentID, Skill: condSkillInput("rollback")})
			}
			if !errors.Is(err, boom) {
				t.Fatalf("got %v, want injected error", err)
			}
			if upserts, deletes := store.counts(); upserts != 1 || deletes != 1 {
				t.Fatalf("compensation calls: upserts=%d deletes=%d, want 1/1", upserts, deletes)
			}
			if _, err := store.Get(ctx, identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}, "rollback"); !errors.Is(err, skills.ErrSkillNotFound) {
				t.Fatalf("stranded body: %v", err)
			}
		})
		t.Run(name+"_delete", func(t *testing.T) {
			reg := newRegistry(t)
			store := &observedSkillStore{SkillStore: newSkills(t)}
			seed, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithSkillStore(store))
			if err != nil {
				t.Fatalf("NewService(seed): %v", err)
			}
			if user {
				_, err = seed.UserSkillsUpsert(ctx, prototypes.AgentConfigUserSkillsUpsertRequest{Identity: scope(), AgentID: testAgentID, Skill: condSkillInput("restore")})
			} else {
				_, err = seed.SkillsUpsert(ctx, prototypes.AgentConfigSkillsUpsertRequest{Identity: scope(), AgentID: testAgentID, Skill: condSkillInput("restore")})
			}
			if err != nil {
				t.Fatalf("seed body: %v", err)
			}
			store.reset()
			svc, err := agentcfgprotocol.NewService(setFailureRegistry{Registry: reg, err: boom}, agentcfgprotocol.WithSkillStore(store))
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			if user {
				_, err = svc.UserSkillsDelete(ctx, prototypes.AgentConfigUserSkillsDeleteRequest{Identity: scope(), AgentID: testAgentID, Name: "restore"})
			} else {
				_, err = svc.SkillsDelete(ctx, prototypes.AgentConfigSkillsDeleteRequest{Identity: scope(), AgentID: testAgentID, Name: "restore"})
			}
			if !errors.Is(err, boom) {
				t.Fatalf("got %v, want injected error", err)
			}
			if upserts, deletes := store.counts(); upserts != 1 || deletes != 1 {
				t.Fatalf("compensation calls: upserts=%d deletes=%d, want 1/1", upserts, deletes)
			}
			scope := skills.ScopeSession
			if user {
				scope = skills.ScopeUser
			}
			if _, err := store.GetScopeAgent(ctx, identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}, testAgentID, "restore", scope); err != nil {
				t.Fatalf("deleted body was not restored: %v", err)
			}
		})
	}
}
