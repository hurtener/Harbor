package protocol

import (
	"context"
	"fmt"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
)

// ListTenantTasks implements Projector.ListTenantTasks — the
// admin-widened FLEET read. It enumerates every task across ALL sessions
// of the named tenants through the registry's explicit tenant-scoped
// enumeration seam (tasks.TaskRegistry.ListTenant) and projects each onto
// the flat TaskRow wire shape, carrying full per-(tenant, user, session)
// identity attribution.
//
// # Scope + gate
//
// This method is the widened counterpart to ListTasks. It is invoked by
// Service.List ONLY after the cross-tenant admin-scope gate passes; the
// RegistryProjector honours whatever tenant set the Service hands it. The
// registry's ListTenant reads the whole per-runtime task set for a tenant
// (all users, all sessions) — cross-RUNTIME federation stays the
// coordinator's job over per-runtime reads (the same division as
// sessions / events).
//
// # Group enrichment
//
// The narrow ListTasks path enriches each row's GroupID from the caller's
// identity-scoped ListGroups. The fleet read deliberately does NOT resolve
// GroupID across every session of the tenant: the per-job "Related
// Sessions" group drill-in is a per-session Background Jobs feature, not a
// fleet-board column, and a tenant-wide ListGroups fan-out is not worth
// the cost at V1 fleet cardinality. A widened row therefore carries an
// empty GroupID — the honest "group membership not resolved in the fleet
// view", never a silent degradation of a known value.
func (p *RegistryProjector) ListTenantTasks(ctx context.Context, tenantIDs []string) ([]prototypes.TaskRow, error) {
	seen := make(map[string]struct{}, len(tenantIDs))
	rows := make([]prototypes.TaskRow, 0, 16)
	for _, tid := range tenantIDs {
		if tid == "" {
			continue
		}
		if _, dup := seen[tid]; dup {
			continue
		}
		seen[tid] = struct{}{}
		fleet, err := p.registry.ListTenant(ctx, tid, tasks.TaskFilter{})
		if err != nil {
			return nil, fmt.Errorf("tasks/protocol: fleet list tenant %q: %w", tid, err)
		}
		for _, t := range fleet {
			row := projectRow(t)
			p.applyApproval(ctx, t, &row)
			rows = append(rows, row)
		}
	}
	return rows, nil
}
