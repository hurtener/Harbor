package protocol

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions"
)

// ListerProjector is the V1 production Projector — a thin read-only
// projection over a `sessions.SessionLister` (the Registry's
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
// # Enrichment seam (CLAUDE.md §4.4)
//
// The SessionLister owns the lifecycle fields (status, timestamps, title,
// identity); it does NOT model the per-session cost / token / task / event
// counters or a session→agent binding. ListerProjector reads the counters
// through the optional Enricher seam (enricher.go), mirroring the shipped
// tasks.Projector enricher. When no Enricher is wired, the counters stay
// ZERO — honest ("we don't have this data"), not silent degradation — and
// the Service loud-rejects a facet/sort over them (WARN-3) so an unwired
// build never returns a false-empty counter page. Every row's explicit
// counter availability is marked via SessionRow.CounterStatus
// (current / partial / not_requested / unavailable) so a zero counter never
// reads as a measured zero.
//
// # Lifecycle-only projection
//
// `projection=lifecycle` requests (pageLifecycleOnly / inspectLifecycleOnly)
// are served from the catalog projection / page path BEFORE any Enricher
// call: the counters are never read, stay zero, and are marked
// CounterStatus=not_requested. The filter / sort / cursor / truncation and
// the identity-scope / admin-widening semantics are exactly the full
// projection's — only the counter payload is absent.
//
// # Concurrent reuse
//
// A constructed *ListerProjector is immutable after NewListerProjector
// and safe to share across N concurrent goroutines — it holds only the
// SessionLister + optional Enricher references (both themselves safe for
// concurrent reuse); every method's per-call state lives in the call's
// arguments and locals.
type ListerProjector struct {
	lister   sessions.SessionLister
	enricher Enricher
}

// maxCounterEnrichmentWorkers bounds the counter reads for requests whose
// filter or sort needs every candidate enriched. Lifecycle-only requests take
// pageBeforeEnrichment instead; the remaining counter-dependent path must see
// the full candidate set for exact Protocol semantics, but it must not spawn a
// goroutine per catalog row.
const maxCounterEnrichmentWorkers = 8

// ListerProjectorOption configures NewListerProjector.
type ListerProjectorOption func(*ListerProjector)

// WithEnricher wires the read-time counter-rollup backend. A nil enricher
// is treated as "WithEnricher not supplied" — the projector ships honest
// ZERO counters and reports CountersAvailable()==false, so the Service
// loud-rejects a facet/sort over the counters rather than returning a
// false-empty page (WARN-3).
func WithEnricher(e Enricher) ListerProjectorOption {
	return func(p *ListerProjector) {
		if e != nil {
			p.enricher = e
		}
	}
}

// NewListerProjector builds the V1 Projector over a SessionLister. The
// lister is mandatory — a nil fails loud rather than building a
// projector that nil-panics on the first request (CLAUDE.md §5).
func NewListerProjector(lister sessions.SessionLister, opts ...ListerProjectorOption) (*ListerProjector, error) {
	if lister == nil {
		return nil, fmt.Errorf("%w: SessionLister is nil", ErrMisconfigured)
	}
	p := &ListerProjector{lister: lister}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// CountersAvailable reports whether the projector populates the numeric /
// boolean counters (cost / tokens / tasks / events / intervention /
// failed-task) — i.e. whether an Enricher is wired. False on a partial
// build: the Service then loud-rejects a facet / sort over those counters
// rather than returning a false-empty page (WARN-3).
func (p *ListerProjector) CountersAvailable() bool { return p.enricher != nil }

// enrich overlays the enricher's counter rollup onto a projected row and
// marks the row's explicit counter availability. A nil enricher leaves the
// honest zeros in place and marks the counters UNAVAILABLE (the zeros mean
// "this build cannot provide them", never "measured as zero" — an
// explicitly represented absence). It scopes the enricher read to the ROW's
// own identity so an admin listing another tenant's session reads exactly
// that session's counters (no cross-session bleed).
func (p *ListerProjector) enrich(ctx context.Context, snap sessions.SessionSnapshot, row *prototypes.SessionRow) {
	if p.enricher == nil {
		row.CounterStatus = prototypes.CounterStatusUnavailable
		return
	}
	sid := identity.Identity{
		TenantID:  snap.Identity.TenantID,
		UserID:    snap.Identity.UserID,
		SessionID: snap.Identity.SessionID,
	}
	c := p.enricher.Counters(ctx, sid, snap.ID)
	row.TasksCount = c.TasksCount
	row.EventsCount = c.EventsCount
	row.TotalCostCents = c.TotalCostCents
	row.TotalTokens = c.TotalTokens
	row.HasPendingIntervention = c.HasPendingIntervention
	row.HasFailedTask = c.HasFailedTask
	row.CountersPartial = c.Partial
	if c.Partial {
		row.CounterStatus = prototypes.CounterStatusPartial
	} else {
		row.CounterStatus = prototypes.CounterStatusCurrent
	}
}

// ListSessions implements Projector.ListSessions. It builds the
// identity-scoped registry filter, lists the snapshots, and projects
// each onto a SessionRow.
func (p *ListerProjector) ListSessions(ctx context.Context, id identity.Identity, f prototypes.SessionFilter, adminScoped bool) ([]prototypes.SessionRow, error) {
	snaps, err := p.listSnapshots(ctx, id, f, adminScoped)
	if err != nil {
		return nil, err
	}
	rows := make([]prototypes.SessionRow, 0, len(snaps))
	for _, snap := range snaps {
		rows = append(rows, projectRow(snap))
	}
	if err := p.enrichRows(ctx, snaps, rows); err != nil {
		return nil, fmt.Errorf("sessions/protocol: enrich session rows: %w", err)
	}
	return rows, nil
}

// enrichRows applies counter rollups with bounded parallelism. Each worker
// writes one distinct row index, preserving the catalog order for the Service
// filter/sort layer while preventing a large counter-dependent list from
// opening one event/task/pause scan per goroutine. On a projector with NO
// Enricher wired, no reads are possible — every row is marked explicitly
// CounterStatus=unavailable so the honest zeros never read as measured zeros.
func (p *ListerProjector) enrichRows(ctx context.Context, snaps []sessions.SessionSnapshot, rows []prototypes.SessionRow) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(rows) == 0 {
		return nil
	}
	if p.enricher == nil {
		for i := range rows {
			rows[i].CounterStatus = prototypes.CounterStatusUnavailable
		}
		return nil
	}
	workers := min(maxCounterEnrichmentWorkers, len(rows))
	jobs := make(chan int)
	var wg sync.WaitGroup
	wg.Add(workers)
	for range workers {
		go func() {
			defer wg.Done()
			for {
				select {
				case <-ctx.Done():
					return
				case i, ok := <-jobs:
					if !ok {
						return
					}
					if ctx.Err() != nil {
						return
					}
					p.enrich(ctx, snaps[i], &rows[i])
				}
			}
		}()
	}
	dispatchCanceled := false
	for i := range rows {
		select {
		case <-ctx.Done():
			dispatchCanceled = true
		case jobs <- i:
		}
		if dispatchCanceled {
			break
		}
	}
	close(jobs)
	wg.Wait()
	return ctx.Err()
}

// listSnapshots resolves the identity-scoped lifecycle catalog shared by the
// full projection and the lifecycle-only paging path.
func (p *ListerProjector) listSnapshots(ctx context.Context, id identity.Identity, f prototypes.SessionFilter, adminScoped bool) ([]sessions.SessionSnapshot, error) {
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
		// cross-tenant filter without the admin claim. Naming the
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
	return snaps, nil
}

// pagedCatalog resolves the identity-scoped lifecycle catalog, applies the
// facet filter + sort, and slices the requested page — the machinery shared
// by the pre-enrichment page path (which enriches the page afterwards) and
// the lifecycle-only page path (which never touches the Enricher). It
// returns the page rows, the page's source snapshots (for the optional
// enrichment pass), and the cursor / truncation metadata.
func (p *ListerProjector) pagedCatalog(
	ctx context.Context,
	id identity.Identity,
	f prototypes.SessionFilter,
	adminScoped bool,
	srt prototypes.SessionSort,
	cursor *pageCursor,
	limit int,
) (page []prototypes.SessionRow, pageSnapshots []sessions.SessionSnapshot, truncated bool, next string, err error) {
	snaps, err := p.listSnapshots(ctx, id, f, adminScoped)
	if err != nil {
		return nil, nil, false, "", err
	}
	type candidate struct {
		snapshot sessions.SessionSnapshot
		row      prototypes.SessionRow
	}
	candidates := make([]candidate, 0, len(snaps))
	for _, snap := range snaps {
		row := projectRow(snap)
		if filterMatches(f, row) {
			candidates = append(candidates, candidate{snapshot: snap, row: row})
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return lessForSort(candidates[i].row, candidates[j].row, srt)
	})
	rows := make([]prototypes.SessionRow, len(candidates))
	for i := range candidates {
		rows[i] = candidates[i].row
	}
	start, end, truncated, next := pageBounds(rows, cursor, srt, limit)
	page = make([]prototypes.SessionRow, end-start)
	pageSnapshots = make([]sessions.SessionSnapshot, end-start)
	for i := start; i < end; i++ {
		page[i-start] = candidates[i].row
		pageSnapshots[i-start] = candidates[i].snapshot
	}
	return page, pageSnapshots, truncated, next, nil
}

// pageBeforeEnrichment applies only lifecycle-backed filter/sort/cursor work
// before enriching the returned page. Service calls it exclusively for
// requests without counter predicates or a cost sort; applying it to those
// requests would be wrong because their values are supplied by Enricher.
func (p *ListerProjector) pageBeforeEnrichment(
	ctx context.Context,
	id identity.Identity,
	f prototypes.SessionFilter,
	adminScoped bool,
	srt prototypes.SessionSort,
	cursor *pageCursor,
	limit int,
) (prototypes.SessionsListResponse, error) {
	if requiresCounters(f, srt) {
		return prototypes.SessionsListResponse{}, fmt.Errorf("%w: counter filter or cost sort cannot page before enrichment", ErrInvalidRequest)
	}
	page, pageSnapshots, truncated, next, err := p.pagedCatalog(ctx, id, f, adminScoped, srt, cursor, limit)
	if err != nil {
		return prototypes.SessionsListResponse{}, err
	}
	if err := p.enrichRows(ctx, pageSnapshots, page); err != nil {
		return prototypes.SessionsListResponse{}, fmt.Errorf("sessions/protocol: enrich session page: %w", err)
	}
	return prototypes.SessionsListResponse{Rows: page, NextCursor: next, Truncated: truncated}, nil
}

// pageLifecycleOnly serves a `projection=lifecycle` list request: the same
// identity-scoped filter / sort / cursor / truncation machinery as the
// full projection, but it returns from the catalog page path BEFORE any
// Enricher call — the counters are never read, stay zero, and are marked
// CounterStatus=not_requested so a zero never reads as a measured zero.
func (p *ListerProjector) pageLifecycleOnly(
	ctx context.Context,
	id identity.Identity,
	f prototypes.SessionFilter,
	adminScoped bool,
	srt prototypes.SessionSort,
	cursor *pageCursor,
	limit int,
) (prototypes.SessionsListResponse, error) {
	if requiresCounters(f, srt) {
		return prototypes.SessionsListResponse{}, fmt.Errorf("%w: counter filter or cost sort cannot page a lifecycle-only projection", ErrInvalidRequest)
	}
	page, _, truncated, next, err := p.pagedCatalog(ctx, id, f, adminScoped, srt, cursor, limit)
	if err != nil {
		return prototypes.SessionsListResponse{}, err
	}
	for i := range page {
		page[i].CounterStatus = prototypes.CounterStatusNotRequested
	}
	return prototypes.SessionsListResponse{Rows: page, NextCursor: next, Truncated: truncated}, nil
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
		// scope by the caller's full (tenant, user) so the
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
			row := projectRow(snap)
			p.enrich(ctx, snap, &row)
			return prototypes.SessionsInspectResponse{
				Row:                 row,
				RecentInterventions: []prototypes.InterventionSummary{},
				RecentArtifacts:     []prototypes.ArtifactRefSummary{},
			}, nil
		}
	}
	return prototypes.SessionsInspectResponse{}, fmt.Errorf("%w: session %q", ErrSessionNotFound, sessionID)
}

// inspectLifecycleOnly serves a `projection=lifecycle` inspect request: the
// same identity-scoped lookup and not-found semantics as InspectSession, but
// returning BEFORE any Enricher call. The row carries the lifecycle catalog
// fields with CounterStatus=not_requested and its counters stay zero; the
// recent-interventions / recent-artifacts capped slices stay empty exactly
// as InspectSession ships them.
func (p *ListerProjector) inspectLifecycleOnly(ctx context.Context, id identity.Identity, sessionID string, adminScoped bool) (prototypes.SessionsInspectResponse, error) {
	regFilter := sessions.SessionListFilter{
		SessionIDs:    []string{sessionID},
		IncludeClosed: true,
	}
	if !adminScoped {
		// scope by the caller's full (tenant, user) so the
		// registry hydrates the catalog and a closed/past session created
		// by a prior process is inspectable after a restart — exactly the
		// InspectSession lookup.
		regFilter.TenantIDs = []string{id.TenantID}
		regFilter.UserIDs = []string{id.UserID}
	}
	snaps, err := p.lister.ListSnapshots(ctx, regFilter)
	if err != nil {
		return prototypes.SessionsInspectResponse{}, fmt.Errorf("sessions/protocol: inspect snapshots: %w", err)
	}
	for _, snap := range snaps {
		if snap.ID == sessionID {
			row := projectRow(snap)
			row.CounterStatus = prototypes.CounterStatusNotRequested
			return prototypes.SessionsInspectResponse{
				Row:                 row,
				RecentInterventions: []prototypes.InterventionSummary{},
				RecentArtifacts:     []prototypes.ArtifactRefSummary{},
			}, nil
		}
	}
	return prototypes.SessionsInspectResponse{}, fmt.Errorf("%w: session %q", ErrSessionNotFound, sessionID)
}

// projectRow maps a runtime SessionSnapshot onto the flat Protocol
// SessionRow wire shape — the LIFECYCLE fields the registry owns (status,
// timestamps, title, identity).
//
// The per-session cost / token / task / event counters are NOT on the
// Session record; they are overlaid at read time by the Enricher seam (see
// enrich). projectRow leaves them ZERO — an honest "not yet enriched"
// default the overlay replaces with real values when an Enricher is wired.
// The session→agent binding fields (AgentID / AgentName) stay NIL: no
// single-valued session→agent binding exists in V1 (a session may run
// several agents), so their absence is REPRESENTABLE (nil, omitted on the
// wire) rather than a fabricated value — a `filter.agent_ids` facet over
// them fails loud. projectRow NEVER assigns the counters or the agent
// fields, so the projector-field-set pin test can hold it to exactly the
// lifecycle field-set (§17.8).
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
		Title:       snap.Title,
		TitleSource: string(snap.TitleSource),
	}
}

// Compile-time assertion: *ListerProjector satisfies Projector.
var _ Projector = (*ListerProjector)(nil)

// Compile-time assertion: *ListerProjector satisfies the private
// lifecycle-only projection seam the Service dispatches
// `projection=lifecycle` requests to.
var _ lifecycleProjector = (*ListerProjector)(nil)
