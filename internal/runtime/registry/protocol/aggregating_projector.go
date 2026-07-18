package protocol

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// ListTenantAgents implements Projector.ListTenantAgents — the
// admin-widened FLEET read. It enumerates every registered agent across
// ALL sessions of the named tenants through the Agent Registry's explicit
// tenant-scoped enumeration seam (registry.AgentRegistry.ListTenant) and
// projects each onto the flat Agent wire row, carrying full
// per-(tenant, user, session) identity attribution.
//
// # Scope + gate
//
// This is the widened counterpart to ListAgents. It is invoked by
// Service.List ONLY after the cross-tenant admin-scope gate passes; the
// RegistryProjector honours whatever tenant set the Service hands it.
// The registry's ListTenant reads the whole per-runtime agent set for a
// tenant (all users, all sessions) via the StateStore maintenance-scan
// surface — cross-RUNTIME federation stays the coordinator's job over
// per-runtime reads (the same division as sessions / events).
//
// # Identity under which config joins run
//
// Each record is projected under its OWN registration identity (never the
// caller's): the ConfigSource joins that hydrate planner/model/tool counts
// are scoped to the agent's own (tenant, user, session), matching the
// "act on each record under its own identity" contract the maintenance
// scan carries. agent_id is never an isolation key.
func (p *RegistryProjector) ListTenantAgents(ctx context.Context, tenantIDs []string) ([]prototypes.Agent, error) {
	seen := make(map[string]struct{}, len(tenantIDs))
	out := make([]prototypes.Agent, 0, 16)
	for _, tid := range tenantIDs {
		if tid == "" {
			continue
		}
		if _, dup := seen[tid]; dup {
			continue
		}
		seen[tid] = struct{}{}
		records, err := p.reg.ListTenant(ctx, tid)
		if err != nil {
			return nil, fmt.Errorf("registry/protocol: fleet list tenant %q: %w", tid, mapRegistryErr(err))
		}
		collides := false
		for _, rec := range records {
			if p.defaultAgent.isSet() && rec.AgentID == p.defaultAgent.ID {
				collides = true
			}
			// Project under the record's OWN registration identity so any
			// ConfigSource join scopes to the agent's own scope.
			ownIdentity := identity.Identity{
				TenantID:  rec.Identity.TenantID,
				UserID:    rec.Identity.UserID,
				SessionID: rec.Identity.SessionID,
			}
			out = append(out, p.projectRecord(ctx, ownIdentity, rec))
		}
		// The synthetic default agent surfaces once per named tenant this
		// runtime serves, unless a real registration under the well-known
		// id shadows it in that tenant (collision is per-tenant). Its
		// per-row Identity carries only the named tenant — the default
		// agent serves every session on the runtime, not a single owning
		// (user, session), so those axes are left empty as representable
		// absence, never a fabricated session id.
		if p.defaultAgent.isSet() && !collides {
			out = append(out, p.synthDefaultRow(identity.Identity{TenantID: tid}))
		}
	}
	return out, nil
}
