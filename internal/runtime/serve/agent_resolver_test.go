package serve

// The caller-named-agent resolver adapter: the two-check rule, over a
// REAL StateStore-backed agent-config registry (no mock at the seam).

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
)

// TestAgentResolverAdapter_TwoCheckRule covers every branch of the rule
// and both of its deliberate exclusions.
func TestAgentResolverAdapter_TwoCheckRule(t *testing.T) {
	ctx := context.Background()
	reg := acTestRegistry(t)

	caller := identity.Identity{TenantID: "t-a", UserID: "u", SessionID: "s"}
	foreign := identity.Identity{TenantID: "t-b", UserID: "u", SessionID: "s"}

	// A revision under the FOREIGN tenant only.
	if _, err := reg.SetRevision(ctx, identity.Quadruple{Identity: foreign},
		"tenant-b-agent", agentcfg.ConfigScopeAgent,
		agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"x"}}}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("SetRevision(foreign): %v", err)
	}
	// A revision under the CALLER's tenant.
	if _, err := reg.SetRevision(ctx, identity.Quadruple{Identity: caller},
		"tenant-a-agent", agentcfg.ConfigScopeAgent,
		agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"x"}}}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("SetRevision(caller): %v", err)
	}

	r := NewAgentResolverAdapter(reg, "boot-agent")

	cases := []struct {
		name    string
		id      identity.Identity
		agentID string
		want    bool
	}{
		// Check (i): the configured default is accepted with NO revision
		// written for it — the case registry membership would refuse,
		// since the boot agent is never registered as a fleet entity.
		{"configured default without a revision", caller, "boot-agent", true},
		// Check (ii): a config revision under the CALLER's tenant.
		{"agent with a revision under the caller's tenant", caller, "tenant-a-agent", true},
		// A revision under ANOTHER tenant is not the caller's to name.
		{"agent registered under another tenant", caller, "tenant-b-agent", false},
		// ...and the never-existing id answers identically. Both take the
		// same (false, nil) path, structurally: the config key puts the
		// caller's own tenant in the tenant slot.
		{"never-existing agent", caller, "no-such-agent", false},
		// The owning tenant CAN name its own agent — proving the two
		// negatives above were tenant scoping, not a broken revision.
		{"the owning tenant names its own agent", foreign, "tenant-b-agent", true},
		// An empty id never resolves here; the ControlSurface short-
		// circuits it before ever calling the resolver.
		{"empty agent id", caller, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := r.ResolveAgent(ctx, tc.id, tc.agentID)
			if err != nil {
				t.Fatalf("ResolveAgent: unexpected error %v", err)
			}
			if got != tc.want {
				t.Fatalf("ResolveAgent(%q under %q) = %v, want %v", tc.agentID, tc.id.TenantID, got, tc.want)
			}
		})
	}
}

// TestAgentResolverAdapter_NoRegistryRefusesRatherThanAccepts — an
// assembly with no agent-config registry answers false for every
// non-default id. It never accepts, and the ControlSurface turns the
// false into the standard refusal (fail-closed, CLAUDE.md §13).
func TestAgentResolverAdapter_NoRegistryRefusesRatherThanAccepts(t *testing.T) {
	ctx := context.Background()
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}

	r := NewAgentResolverAdapter(nil, "boot-agent")
	if got, err := r.ResolveAgent(ctx, id, "anything"); err != nil || got {
		t.Fatalf("ResolveAgent with no registry = (%v, %v), want (false, nil)", got, err)
	}
	// The configured default still resolves — check (i) needs no store.
	if got, err := r.ResolveAgent(ctx, id, "boot-agent"); err != nil || !got {
		t.Fatalf("ResolveAgent(default) with no registry = (%v, %v), want (true, nil)", got, err)
	}

	// An assembly with NEITHER a registry NOR a configured default id
	// resolves nothing at all — it cannot accidentally accept "".
	bare := NewAgentResolverAdapter(nil, "")
	if got, err := bare.ResolveAgent(ctx, id, ""); err != nil || got {
		t.Fatalf("bare ResolveAgent(\"\") = (%v, %v), want (false, nil)", got, err)
	}
}

// TestAgentResolverAdapter_StoreErrorIsReturnedNotFolded — a store
// failure must surface as an error, never as a plain false that the edge
// could not distinguish from "unknown agent".
func TestAgentResolverAdapter_StoreErrorIsReturnedNotFolded(t *testing.T) {
	ctx := context.Background()
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}

	reg := acTestRegistry(t)
	// Close the registry underneath the adapter so the lookup errors.
	if err := reg.Close(ctx); err != nil {
		t.Fatalf("registry.Close: %v", err)
	}
	// defaultID empty so check (i) cannot short-circuit the store read.
	r := NewAgentResolverAdapter(reg, "")
	got, err := r.ResolveAgent(ctx, id, "some-agent")
	if got {
		t.Fatal("a failing store answered TRUE")
	}
	if err == nil {
		t.Fatal("a failing store answered (false, nil) — the edge cannot distinguish that from 'unknown agent'; the error must be returned")
	}
}
