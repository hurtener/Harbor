package projection_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// reconcile_owner_test.go — the reconcile detach leg carries the reconciling
// owner all the way to the concrete, which is what lets the MCP registry's
// removal be owner-scoped at its own resolution choke point.

// TestReconcileConnections_PassesReconcilingOwnerToDetach pins the owner
// threading the registry's owner-scoped removal depends on: Detach must carry
// the SAME (tenant, agent) owner the AttachedSources view was taken under, so
// the concrete's registry deregister resolves the entry that view produced
// rather than any registration sharing the bare name.
func TestReconcileConnections_PassesReconcilingOwnerToDetach(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	// No declared connections — "drop" is attached and undeclared, so it detaches.
	det := newFakeDetacher("drop")

	if _, _, err := projection.ReconcileConnections(ctx, reg, projAgent, projID(), det, nil, nil); err != nil {
		t.Fatalf("ReconcileConnections: %v", err)
	}
	owners := det.detachOwnersSeen()
	want := auth.Owner{Tenant: projTenant, Agent: projAgent}
	if len(owners) != 1 || owners[0] != want {
		t.Fatalf("Detach owners = %v, want exactly [%+v]", owners, want)
	}
}
