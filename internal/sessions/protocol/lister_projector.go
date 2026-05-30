package protocol

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions"
)

// ListerProjector is the V1 production Projector — a thin read-only
// projection over a `sessions.SessionLister` (the Phase 08 Registry's
// `ListSnapshots` surface). It maps the runtime `sessions.SessionSnapshot`
// onto the flat Protocol `SessionRow` wire shape (RFC §5.1 single-source
// rule: the Console never reads `sessions.Session`).
//
// # Identity scoping (CLAUDE.md §6)
//
// ListSessions builds the `sessions.SessionListFilter` so the registry
// scopes by tenant: a non-admin caller is restricted to its own
// `(tenant, user)`; an admin caller MAY widen via the request's
// `TenantIDs`. The registry's `ListSnapshots` does NOT re-check scope —
// the gate is the Service's `ErrCrossTenantScope` check; this projector
// only translates the gate decision into the filter shape.
//
// # Concurrent reuse (D-025)
//
// A constructed *ListerProjector is immutable after NewListerProjector
// and safe to share across N concurrent goroutines — it holds only the
// SessionLister reference; every method's per-call state lives in the
// call's arguments and locals.
type ListerProjector struct {
	lister sessions.SessionLister
}

// NewListerProjector builds the V1 Projector over a SessionLister. The
// lister is mandatory — a nil fails loud rather than building a
// projector that nil-panics on the first request (CLAUDE.md §5).
func NewListerProjector(lister sessions.SessionLister) (*ListerProjector, error) {
	if lister == nil {
		return nil, fmt.Errorf("%w: SessionLister is nil", ErrMisconfigured)
	}
	return &ListerProjector{lister: lister}, nil
}

// ListSessions implements Projector.ListSessions. It builds the
// identity-scoped registry filter, lists the snapshots, and projects
// each onto a SessionRow.
func (p *ListerProjector) ListSessions(ctx context.Context, id identity.Identity, f prototypes.SessionFilter, adminScoped bool) ([]prototypes.SessionRow, error) {
	regFilter := sessions.SessionListFilter{
		// IncludeClosed is always true — the Sessions page is the
		// past-and-active record; the Service's status facet narrows
		// to Completed / Failed when the operator asks.
		IncludeClosed: true,
	}
	if adminScoped && len(f.TenantIDs) > 0 {
		// Admin caller with an explicit cross-tenant filter — honour it.
		regFilter.TenantIDs = append(regFilter.TenantIDs, f.TenantIDs...)
	} else {
		// Non-admin (or admin with no explicit tenant filter): restrict
		// to the caller's own (tenant, user). CLAUDE.md §6 — the registry
		// WHERE-clauses by the triple; the Service already rejected a
		// cross-tenant filter without the admin claim. D-171: naming the
		// caller's UserID (not just the tenant) lets the registry hydrate
		// the per-(tenant, user) session catalog so sessions a prior
		// process created are listable after a restart, and keeps one
		// user's sessions out of another user's listing under one tenant.
		regFilter.TenantIDs = []string{id.TenantID}
		regFilter.UserIDs = []string{id.UserID}
	}
	snaps, err := p.lister.ListSnapshots(ctx, regFilter)
	if err != nil {
		return nil, fmt.Errorf("sessions/protocol: list snapshots: %w", err)
	}
	rows := make([]prototypes.SessionRow, 0, len(snaps))
	for _, snap := range snaps {
		rows = append(rows, projectRow(snap))
	}
	return rows, nil
}

// InspectSession implements Projector.InspectSession. It lists the one
// session id and projects the snapshot plus the (currently empty)
// recent-interventions / recent-artifacts slices.
//
// V1 scope note: the recent-interventions / recent-artifacts cards are
// fed by the Console's own event-stream subscription on the detail
// route (the page consumes `pause.*` / `artifacts.*` events filtered to
// the session — page spec §5). `sessions.inspect` ships the Row
// projection + empty capped slices; the cards populate from the live
// event stream client-side. A future StateStore-backed enrichment can
// pre-fill the slices without a wire-shape break (the fields are
// already on the response).
func (p *ListerProjector) InspectSession(ctx context.Context, id identity.Identity, sessionID string, adminScoped bool) (prototypes.SessionsInspectResponse, error) {
	regFilter := sessions.SessionListFilter{
		SessionIDs:    []string{sessionID},
		IncludeClosed: true,
	}
	if !adminScoped {
		// D-171: scope by the caller's full (tenant, user) so the
		// registry hydrates the catalog and a closed/past session created
		// by a prior process is inspectable after a restart.
		regFilter.TenantIDs = []string{id.TenantID}
		regFilter.UserIDs = []string{id.UserID}
	}
	snaps, err := p.lister.ListSnapshots(ctx, regFilter)
	if err != nil {
		return prototypes.SessionsInspectResponse{}, fmt.Errorf("sessions/protocol: inspect snapshots: %w", err)
	}
	for _, snap := range snaps {
		if snap.ID == sessionID {
			return prototypes.SessionsInspectResponse{
				Row:                 projectRow(snap),
				RecentInterventions: []prototypes.InterventionSummary{},
				RecentArtifacts:     []prototypes.ArtifactRefSummary{},
			}, nil
		}
	}
	return prototypes.SessionsInspectResponse{}, fmt.Errorf("%w: session %q", ErrSessionNotFound, sessionID)
}

// projectRow maps a runtime SessionSnapshot onto the flat Protocol
// SessionRow wire shape.
//
// Cost / token / task / event counters and the agent binding are NOT
// modelled on the Phase 08 Session record — the Sessions page surfaces
// them from the live event stream (the `llm.cost.recorded` aggregation
// the page spec §3 describes is Console-local). projectRow ships the
// lifecycle fields the registry owns; the count fields are zero and the
// Console enriches them from its event subscription. This keeps
// `sessions.list` a pure registry projection (no shadow aggregation
// store — D-061) and is a documented D-122 deviation.
func projectRow(snap sessions.SessionSnapshot) prototypes.SessionRow {
	status := prototypes.SessionStatusCompleted
	switch {
	case snap.Running:
		status = prototypes.SessionStatusRunning
	case !snap.Closed:
		// An open session with no running run is "running" from the
		// operator's lens (it can still receive runs).
		status = prototypes.SessionStatusRunning
	case snap.Closed && snap.ClosedReason == "failed":
		status = prototypes.SessionStatusFailed
	}
	lastActivity := snap.LastSeen
	if snap.Closed && !snap.ClosedAt.IsZero() {
		lastActivity = snap.ClosedAt
	}
	duration := lastActivity.Sub(snap.OpenedAt)
	if duration < 0 {
		duration = 0
	}
	return prototypes.SessionRow{
		SessionID:      snap.ID,
		Status:         status,
		UserID:         snap.Identity.UserID,
		TenantID:       snap.Identity.TenantID,
		StartedAt:      snap.OpenedAt,
		LastActivityAt: lastActivity,
		Duration:       duration,
		Identity: prototypes.IdentityScope{
			Tenant:  snap.Identity.TenantID,
			User:    snap.Identity.UserID,
			Session: snap.Identity.SessionID,
		},
	}
}

// Compile-time assertion: *ListerProjector satisfies Projector.
var _ Projector = (*ListerProjector)(nil)
