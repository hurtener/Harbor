package projection_test

import (
	"context"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
)

type membershipCountingRegistry struct {
	agentcfg.Registry
	mu     sync.Mutex
	counts map[agentcfg.ConfigScope]int
}

func (r *membershipCountingRegistry) Active(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope) (agentcfg.Revision, bool, error) {
	r.mu.Lock()
	r.counts[scope]++
	r.mu.Unlock()
	return r.Registry.Active(ctx, id, agentID, scope)
}

func (r *membershipCountingRegistry) count(scope agentcfg.ConfigScope) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.counts[scope]
}

func TestActiveSessionSkillMembership_PresenceAndExactScopes(t *testing.T) {
	ctx := t.Context()
	tests := []struct {
		name      string
		agent     *agentcfg.SkillsSelection
		user      *agentcfg.SkillsSelection
		wantSet   bool
		wantAgent []string
		wantUser  []string
	}{
		{name: "absent revisions leave admin selection absent"},
		{name: "explicit empty agent selection remains present", agent: &agentcfg.SkillsSelection{}, wantSet: true},
		{
			name:      "agent and exact user names are captured",
			agent:     &agentcfg.SkillsSelection{Names: []string{"admin-a", "admin-b"}},
			user:      &agentcfg.SkillsSelection{Names: []string{"user-a"}},
			wantSet:   true,
			wantAgent: []string{"admin-a", "admin-b"},
			wantUser:  []string{"user-a"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := newRegistry(t)
			if tt.agent != nil {
				if _, err := base.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Skills: tt.agent}, agentcfg.SetOptions{}); err != nil {
					t.Fatalf("set agent revision: %v", err)
				}
			}
			if tt.user != nil {
				if _, err := base.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeUser, agentcfg.ConfigPayload{Skills: tt.user}, agentcfg.SetOptions{}); err != nil {
					t.Fatalf("set user revision: %v", err)
				}
			}
			reg := &membershipCountingRegistry{Registry: base, counts: make(map[agentcfg.ConfigScope]int)}
			got, err := projection.ActiveSessionSkillMembership(ctx, reg, projAgent, projID())
			if err != nil {
				t.Fatalf("ActiveSessionSkillMembership: %v", err)
			}
			if got.AdminMembershipSet != tt.wantSet || !eq(got.AdminNames, tt.wantAgent) || !eq(got.UserPersonalNames, tt.wantUser) {
				t.Fatalf("membership = %#v, want set=%t agent=%v user=%v", got, tt.wantSet, tt.wantAgent, tt.wantUser)
			}
			if got := reg.count(agentcfg.ConfigScopeAgent); got != 1 {
				t.Fatalf("agent Active reads = %d, want 1", got)
			}
			if got := reg.count(agentcfg.ConfigScopeUser); got != 1 {
				t.Fatalf("user Active reads = %d, want 1", got)
			}
		})
	}
}

func TestActiveSessionSkillMembership_ReturnsIndependentSlices(t *testing.T) {
	ctx := t.Context()
	reg := newRegistry(t)
	if _, err := reg.SetRevision(ctx, projID(), projAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		Skills: &agentcfg.SkillsSelection{Names: []string{"admin-a"}},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("set agent revision: %v", err)
	}

	first, err := projection.ActiveSessionSkillMembership(ctx, reg, projAgent, projID())
	if err != nil {
		t.Fatalf("first membership: %v", err)
	}
	first.AdminNames[0] = "mutated"
	second, err := projection.ActiveSessionSkillMembership(ctx, reg, projAgent, projID())
	if err != nil {
		t.Fatalf("second membership: %v", err)
	}
	if !eq(second.AdminNames, []string{"admin-a"}) {
		t.Fatalf("registry-backed membership was mutated through returned slice: %v", second.AdminNames)
	}
}
