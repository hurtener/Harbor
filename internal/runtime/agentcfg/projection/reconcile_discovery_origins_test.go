package projection_test

import (
	"context"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// reconcile_discovery_origins_test.go — unit coverage for
// ReconcileDiscoveryOrigins (the ALLOWANCE-reconcile / rollback leg): the
// full-idempotent re-derive-and-reapply, the owner-scoped view, and the
// rollback-past-grant live revoke.

// fakeOriginReconciler reports a fixed owner-scoped attached set and records the
// LAST allow-list re-applied per source (the live registry stand-in). Safe for
// concurrent use.
type fakeOriginReconciler struct {
	attached []string
	mu       sync.Mutex
	live     map[string][]string
	calls    int
	// owners records the owner presented on each apply — the reconcile passes
	// the reconciling owner through so the live write is owner-scoped.
	owners []auth.Owner
}

// seenOwners returns a copy of the owners presented on the apply calls.
func (f *fakeOriginReconciler) seenOwners() []auth.Owner {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]auth.Owner(nil), f.owners...)
}

func newFakeOriginReconciler(live map[string][]string, attached ...string) *fakeOriginReconciler {
	if live == nil {
		live = map[string][]string{}
	}
	return &fakeOriginReconciler{attached: attached, live: live}
}

func (f *fakeOriginReconciler) AttachedSources(_ context.Context, _ auth.Owner) []string {
	return append([]string(nil), f.attached...)
}

func (f *fakeOriginReconciler) SetOAuthDiscoveryOrigins(_ context.Context, owner auth.Owner, name string, origins []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	f.owners = append(f.owners, owner)
	prev := append([]string(nil), f.live[name]...)
	f.live[name] = append([]string(nil), origins...)
	return prev, nil
}

func (f *fakeOriginReconciler) get(name string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.live[name]...)
}

// seedConnectionsWithOrigins writes an active revision declaring one connection
// with the given allow-list.
//
//nolint:unparam // test helper: the connection name is fixed across these cases.
func seedConnectionsWithOrigins(t *testing.T, reg agentcfg.Registry, name string, origins []string) {
	t.Helper()
	payload := agentcfg.ConfigPayload{Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{
		{Name: name, Transport: agentcfg.MCPTransportHTTP, URL: "https://example.invalid/" + name, OAuthDiscoveryAllowedOrigins: origins},
	}}}
	if _, err := reg.SetRevision(context.Background(), projID(), projAgent, agentcfg.ConfigScopeAgent, payload, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// TestReconcile_PassesReconcilingOwnerToTheLiveApply proves the allowance
// re-apply carries the reconciling (tenant, agent) owner — the same owner the
// attached-set view was taken under — so the live write stays scoped to that
// owner's own runtime-added registrations.
func TestReconcile_PassesReconcilingOwnerToTheLiveApply(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	seedConnectionsWithOrigins(t, reg, "srv", []string{"https://as.example.net"})
	rec := newFakeOriginReconciler(nil, "srv")

	if _, err := projection.ReconcileDiscoveryOrigins(ctx, reg, projAgent, projID(), rec); err != nil {
		t.Fatalf("ReconcileDiscoveryOrigins: %v", err)
	}
	want := auth.Owner{Tenant: projTenant, Agent: projAgent}
	seen := rec.seenOwners()
	if len(seen) != 1 || seen[0] != want {
		t.Fatalf("owners presented to the live apply = %v, want [%v]", seen, want)
	}
}

func TestReconcile_RollbackPastGrant_RevokesOriginLive(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	// The CURRENT active revision (post-rollback) declares an EMPTY allow-list.
	seedConnectionsWithOrigins(t, reg, "srv", nil)
	// The live registry still carries a stale grant from before the rollback.
	rec := newFakeOriginReconciler(map[string][]string{"srv": {"https://as.example.net"}}, "srv")

	n, err := projection.ReconcileDiscoveryOrigins(ctx, reg, projAgent, projID(), rec)
	if err != nil {
		t.Fatalf("ReconcileDiscoveryOrigins: %v", err)
	}
	if n != 1 {
		t.Fatalf("reapplied count = %d, want 1", n)
	}
	if live := rec.get("srv"); len(live) != 0 {
		t.Fatalf("live allow-list = %v after rollback reconcile, want empty (revoked)", live)
	}
}

func TestReconcile_FullIdempotentReprune_HealsStaleRequirement(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	seedConnectionsWithOrigins(t, reg, "srv", []string{"https://as.example.net"})
	// A stale live state (an extra origin from a race) — the re-derive corrects it.
	rec := newFakeOriginReconciler(map[string][]string{"srv": {"https://as.example.net", "https://stale.example.net"}}, "srv")

	// Two reconciles must converge idempotently to the DECLARED set.
	for i := range 2 {
		if _, err := projection.ReconcileDiscoveryOrigins(ctx, reg, projAgent, projID(), rec); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
	live := rec.get("srv")
	if len(live) != 1 || live[0] != "https://as.example.net" {
		t.Fatalf("live allow-list = %v, want the declared [https://as.example.net] (stale pruned)", live)
	}
}

func TestReconcile_DiscoveryOrigins_UndeclaredSourceSkipped(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	seedConnectionsWithOrigins(t, reg, "srv", []string{"https://as.example.net"})
	// "gone" is attached in the owner view but NOT declared — detach territory,
	// never re-applied here.
	rec := newFakeOriginReconciler(map[string][]string{"gone": {"https://x.example.net"}}, "srv", "gone")

	n, err := projection.ReconcileDiscoveryOrigins(ctx, reg, projAgent, projID(), rec)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if n != 1 {
		t.Fatalf("reapplied = %d, want 1 (srv only)", n)
	}
	if got := rec.get("gone"); len(got) != 1 {
		t.Fatalf("undeclared 'gone' was touched: %v", got)
	}
}
