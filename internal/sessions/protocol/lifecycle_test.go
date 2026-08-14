package protocol

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions"
)

// catalogSnapshots builds `count` session snapshots for (tenant, user),
// newest-OpenedAt last — the raw catalog the identity-scoped lister feeds
// the projector. idPrefix keeps session ids unique across mixed-tenant
// fixtures (the real registry never mints the same id under two tenants,
// and a fixture that did would let a cross-tenant lookup succeed by
// accident instead of by reach).
func catalogSnapshots(count int, tenant, user, idPrefix string) []sessions.SessionSnapshot {
	base := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	snapshots := make([]sessions.SessionSnapshot, 0, count)
	for i := range count {
		sessionID := fmt.Sprintf("%s-%03d", idPrefix, i)
		snapshots = append(snapshots, sessions.SessionSnapshot{
			Session: sessions.Session{
				ID:       sessionID,
				Identity: identity.Identity{TenantID: tenant, UserID: user, SessionID: sessionID},
				OpenedAt: base.Add(time.Duration(i) * time.Minute),
				LastSeen: base.Add(time.Duration(i) * time.Minute),
			},
			Running: true,
		})
	}
	return snapshots
}

// lifecycleCaller is the caller triple the lifecycle tests run under.
func lifecycleCaller() identity.Identity {
	return identity.Identity{TenantID: "tenant-1", UserID: "user-1", SessionID: "caller-session"}
}

// partialEnricher reports a PARTIAL counter rollup — every count is an
// HONEST LOWER BOUND (SessionCounters.Partial).
type partialEnricher struct{}

func (partialEnricher) Counters(context.Context, identity.Identity, string) SessionCounters {
	return SessionCounters{Partial: true}
}

// subscribeAdminAudit opens a subscription for the canonical
// audit.admin_scope_used event anchored on the actor triple.
func subscribeAdminAudit(t *testing.T, bus events.EventBus, actor identity.Identity) events.Subscription {
	t.Helper()
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant:  actor.TenantID,
		User:    actor.UserID,
		Session: actor.SessionID,
		Types:   []events.EventType{events.EventTypeAdminScopeUsed},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)
	return sub
}

// TestService_List_Lifecycle_ZeroEnricherCalls pins the core lifecycle
// contract: a projection=lifecycle list performs ZERO counter / history /
// task / pause enrichment reads even though an Enricher IS wired. The page
// is served entirely from the catalog projection / page path.
func TestService_List_Lifecycle_ZeroEnricherCalls(t *testing.T) {
	snapshots := catalogSnapshots(60, "tenant-1", "user-1", "lc-session")
	enricher := &countingEnricher{}
	projector, err := NewListerProjector(catalogLister{snapshots: snapshots}, WithEnricher(enricher))
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	svc, err := NewService(projector)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	caller := lifecycleCaller()
	req := prototypes.SessionsListRequest{
		Identity:   prototypes.IdentityScope{Tenant: caller.TenantID, User: caller.UserID, Session: caller.SessionID},
		Projection: prototypes.SessionProjectionLifecycle,
		Filter:     prototypes.SessionFilter{Statuses: []prototypes.SessionStatus{prototypes.SessionStatusRunning}},
		Sort:       prototypes.SessionSortLastActivityDesc,
		Limit:      10,
	}
	resp, err := svc.List(context.Background(), req, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := enricher.calls.Load(); got != 0 {
		t.Fatalf("counter enrichments = %d, want 0 — a lifecycle-only projection must never touch the Enricher", got)
	}
	if len(resp.Rows) != 10 {
		t.Fatalf("lifecycle page returned %d rows, want 10", len(resp.Rows))
	}
	for _, r := range resp.Rows {
		if r.CounterStatus != prototypes.CounterStatusNotRequested {
			t.Errorf("row %q CounterStatus = %q, want not_requested", r.SessionID, r.CounterStatus)
		}
		if r.TasksCount != 0 || r.EventsCount != 0 || r.TotalCostCents != 0 || r.TotalTokens != 0 ||
			r.HasPendingIntervention || r.HasFailedTask {
			t.Errorf("row %q carried counter data on a lifecycle projection: %+v", r.SessionID, r)
		}
	}
}

// TestService_List_Lifecycle_PageAndCursorParityWithFull pins that the
// lifecycle projection produces IDENTICAL row ordering, cursor, and
// truncation semantics to the full projection — the only difference is the
// counter payload. The full-projection calculation is retained as the
// oracle so the fast path cannot silently change ordering / cursor /
// truncation while skipping enrichment.
func TestService_List_Lifecycle_PageAndCursorParityWithFull(t *testing.T) {
	snapshots := catalogSnapshots(60, "tenant-1", "user-1", "lc-session")
	caller := lifecycleCaller()
	req := prototypes.SessionsListRequest{
		Identity:   prototypes.IdentityScope{Tenant: caller.TenantID, User: caller.UserID, Session: caller.SessionID},
		Projection: prototypes.SessionProjectionLifecycle,
		Filter:     prototypes.SessionFilter{Statuses: []prototypes.SessionStatus{prototypes.SessionStatusRunning}},
		Sort:       prototypes.SessionSortLastActivityDesc,
		Limit:      25,
	}

	// Oracle: the full projection over the same catalog, filtered, sorted,
	// and paged with the shared machinery.
	legacy, err := NewListerProjector(catalogLister{snapshots: snapshots}, WithEnricher(zeroEnricher{}))
	if err != nil {
		t.Fatalf("NewListerProjector legacy: %v", err)
	}
	allRows, err := legacy.ListSessions(context.Background(), caller, req.Filter, false)
	if err != nil {
		t.Fatalf("legacy ListSessions: %v", err)
	}
	filtered := make([]prototypes.SessionRow, 0, len(allRows))
	for _, row := range allRows {
		if filterMatches(req.Filter, row) {
			filtered = append(filtered, row)
		}
	}
	sortRows(filtered, req.Sort)
	wantPage := func(cursor *pageCursor) (rows []prototypes.SessionRow, truncated bool, next string) {
		start, end, truncated, next := pageBounds(filtered, cursor, req.Sort, req.Limit)
		return filtered[start:end], truncated, next
	}
	want1, wantTruncated1, wantCursor1 := wantPage(nil)

	enricher := &countingEnricher{}
	projector, err := NewListerProjector(catalogLister{snapshots: snapshots}, WithEnricher(enricher))
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	svc, err := NewService(projector)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	resp, err := svc.List(context.Background(), req, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := enricher.calls.Load(); got != 0 {
		t.Fatalf("lifecycle list enriched %d rows, want 0", got)
	}
	assertPageParity(t, resp, want1, wantTruncated1, wantCursor1)

	// Page 2 through the lifecycle cursor — identical cursor semantics.
	req.Cursor = resp.NextCursor
	decoded, err := decodeCursor(req.Cursor)
	if err != nil {
		t.Fatalf("decodeCursor: %v", err)
	}
	want2, wantTruncated2, wantCursor2 := wantPage(decoded)
	resp2, err := svc.List(context.Background(), req, false)
	if err != nil {
		t.Fatalf("page 2 List: %v", err)
	}
	assertPageParity(t, resp2, want2, wantTruncated2, wantCursor2)
	if got := enricher.calls.Load(); got != 0 {
		t.Fatalf("lifecycle page 2 enriched %d rows, want 0", got)
	}
}

// assertPageParity compares a lifecycle page against the full-projection
// oracle: identical row order (session ids), truncation, and next cursor.
func assertPageParity(t *testing.T, got prototypes.SessionsListResponse, wantRows []prototypes.SessionRow, wantTruncated bool, wantCursor string) {
	t.Helper()
	if got.Truncated != wantTruncated || got.NextCursor != wantCursor || len(got.Rows) != len(wantRows) {
		t.Fatalf("page metadata = rows=%d truncated=%v cursor=%q; oracle = rows=%d truncated=%v cursor=%q",
			len(got.Rows), got.Truncated, got.NextCursor, len(wantRows), wantTruncated, wantCursor)
	}
	for i := range wantRows {
		if got.Rows[i].SessionID != wantRows[i].SessionID {
			t.Fatalf("row %d session_id = %q, want oracle %q", i, got.Rows[i].SessionID, wantRows[i].SessionID)
		}
	}
}

// TestService_List_Lifecycle_CounterDependentFacetsAndSort_Rejected pins
// the lifecycle rejection matrix: a projection=lifecycle row carries no
// counters, so every counter-dependent filter and the cost_desc sort must be
// rejected invalid_request REGARDLESS of whether an Enricher is wired (the
// full path rejects only when the counters are unavailable — the lifecycle
// path has nothing to narrow or order by, ever).
func TestService_List_Lifecycle_CounterDependentFacetsAndSort_Rejected(t *testing.T) {
	caller := lifecycleCaller()
	req := func(f prototypes.SessionFilter, srt prototypes.SessionSort) prototypes.SessionsListRequest {
		return prototypes.SessionsListRequest{
			Identity:   prototypes.IdentityScope{Tenant: caller.TenantID, User: caller.UserID, Session: caller.SessionID},
			Projection: prototypes.SessionProjectionLifecycle,
			Filter:     f,
			Sort:       srt,
		}
	}
	above := int64(80)
	yes := true
	cases := map[string]prototypes.SessionsListRequest{
		"cost_above_cents": req(prototypes.SessionFilter{CostAboveCents: &above}, ""),
		"has_failed_task":  req(prototypes.SessionFilter{HasFailedTask: &yes}, ""),
		"has_intervention": req(prototypes.SessionFilter{HasIntervention: &yes}, ""),
		"cost_desc_sort":   req(prototypes.SessionFilter{}, prototypes.SessionSortCostDesc),
	}

	for _, wired := range []bool{false, true} {
		t.Run(map[bool]string{false: "unwired", true: "wired"}[wired], func(t *testing.T) {
			var projector *ListerProjector
			var err error
			if wired {
				projector, err = NewListerProjector(catalogLister{}, WithEnricher(zeroEnricher{}))
			} else {
				projector, err = NewListerProjector(catalogLister{})
			}
			if err != nil {
				t.Fatalf("NewListerProjector: %v", err)
			}
			svc, err := NewService(projector)
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			for name, lcReq := range cases {
				t.Run(name, func(t *testing.T) {
					resp, err := svc.List(context.Background(), lcReq, false)
					if !errors.Is(err, ErrInvalidRequest) {
						t.Fatalf("lifecycle %s returned (%d rows, err=%v), want ErrInvalidRequest — a lifecycle row has no counters to narrow/order by", name, len(resp.Rows), err)
					}
				})
			}
			// Contrast on the WIRED build: the SAME axes on the full
			// projection are served (the counters exist) — the lifecycle
			// rejection is about absent counters, not absent enrichment.
			if wired {
				fullReq := cases["cost_above_cents"]
				fullReq.Projection = prototypes.SessionProjectionFull
				if _, err := svc.List(context.Background(), fullReq, false); err != nil {
					t.Fatalf("full projection must serve a counter facet on a wired build: %v", err)
				}
			}
		})
	}
}

// TestService_UnknownProjection_Rejected pins the typed invalid-argument
// posture for an unrecognized projection value on both read methods.
func TestService_UnknownProjection_Rejected(t *testing.T) {
	projector, err := NewListerProjector(catalogLister{snapshots: catalogSnapshots(3, "tenant-1", "user-1", "lc-session")})
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	svc, err := NewService(projector)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	caller := lifecycleCaller()
	req := prototypes.SessionsListRequest{
		Identity:   prototypes.IdentityScope{Tenant: caller.TenantID, User: caller.UserID, Session: caller.SessionID},
		Projection: "complete",
	}
	if _, err := svc.List(context.Background(), req, false); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("List with unknown projection error = %v, want ErrInvalidRequest", err)
	}
	inspect := prototypes.SessionsInspectRequest{
		Identity:   prototypes.IdentityScope{Tenant: caller.TenantID, User: caller.UserID, Session: caller.SessionID},
		SessionID:  "lc-session-000",
		Projection: "complete",
	}
	if _, err := svc.Inspect(context.Background(), inspect, false); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("Inspect with unknown projection error = %v, want ErrInvalidRequest", err)
	}
}

// TestService_List_DefaultProjection_IsFull pins the default-compatibility
// contract: an omitted Projection is byte-for-byte an explicit "full" — the
// same rows, ordering, cursor, truncation, AND the same enrichment (the
// page is enriched because the full projection asks for counters).
func TestService_List_DefaultProjection_IsFull(t *testing.T) {
	snapshots := catalogSnapshots(20, "tenant-1", "user-1", "lc-session")
	enricher := &countingEnricher{}
	projector, err := NewListerProjector(catalogLister{snapshots: snapshots}, WithEnricher(enricher))
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	svc, err := NewService(projector)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	caller := lifecycleCaller()
	base := prototypes.SessionsListRequest{
		Identity: prototypes.IdentityScope{Tenant: caller.TenantID, User: caller.UserID, Session: caller.SessionID},
		Sort:     prototypes.SessionSortLastActivityDesc,
		Limit:    10,
	}
	omitted, err := svc.List(context.Background(), base, false)
	if err != nil {
		t.Fatalf("List (omitted): %v", err)
	}
	explicit := base
	explicit.Projection = prototypes.SessionProjectionFull
	full, err := svc.List(context.Background(), explicit, false)
	if err != nil {
		t.Fatalf("List (full): %v", err)
	}
	assertPageParity(t, omitted, full.Rows, full.Truncated, full.NextCursor)
	if got := enricher.calls.Load(); got != int64(2*10) {
		t.Fatalf("counter enrichments = %d, want %d — the full projection enriches exactly its returned pages", got, 2*10)
	}
	for _, r := range omitted.Rows {
		if r.CounterStatus != prototypes.CounterStatusCurrent {
			t.Errorf("omitted-projection row %q CounterStatus = %q, want current (enriched)", r.SessionID, r.CounterStatus)
		}
	}
}

// TestService_List_Lifecycle_AuthorityParity pins the identity-scope and
// admin-widening semantics of the lifecycle projection: own-tenant scoping,
// the cross-tenant gate, and the admin widening are exactly the full
// projection's.
func TestService_List_Lifecycle_AuthorityParity(t *testing.T) {
	mixed := append(catalogSnapshots(10, "tenant-1", "user-1", "t1-session"), catalogSnapshots(5, "tenant-other", "user-other", "tother-session")...)
	enricher := &countingEnricher{}
	projector, err := NewListerProjector(catalogLister{snapshots: mixed}, WithEnricher(enricher))
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	svc, err := NewService(projector)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	caller := lifecycleCaller()
	base := prototypes.SessionsListRequest{
		Identity:   prototypes.IdentityScope{Tenant: caller.TenantID, User: caller.UserID, Session: caller.SessionID},
		Projection: prototypes.SessionProjectionLifecycle,
	}

	// 1. Own-tenant scoping: only the caller's (tenant, user) sessions.
	own, err := svc.List(context.Background(), base, false)
	if err != nil {
		t.Fatalf("own-tenant lifecycle List: %v", err)
	}
	if len(own.Rows) != 10 {
		t.Fatalf("own-tenant lifecycle list returned %d rows, want 10", len(own.Rows))
	}
	for _, r := range own.Rows {
		if r.TenantID != "tenant-1" || r.UserID != "user-1" {
			t.Errorf("non-admin lifecycle list leaked %q/%q outside the caller's (tenant, user) — CLAUDE.md §6 isolation breach", r.TenantID, r.UserID)
		}
	}

	// 2. Cross-tenant filter without the admin claim — the Service gate
	// fires exactly as on the full projection.
	cross := base
	cross.Filter = prototypes.SessionFilter{TenantIDs: []string{"tenant-other"}}
	if _, err := svc.List(context.Background(), cross, false); !errors.Is(err, ErrCrossTenantScope) {
		t.Fatalf("cross-tenant lifecycle list without admin error = %v, want ErrCrossTenantScope", err)
	}

	// 3. Cross-tenant filter under the admin claim — widened to the named
	// tenant, exactly like the full projection.
	admin, err := svc.List(context.Background(), cross, true)
	if err != nil {
		t.Fatalf("admin-widened lifecycle List: %v", err)
	}
	if len(admin.Rows) != 5 {
		t.Fatalf("admin-widened lifecycle list returned %d rows, want 5 (tenant-other)", len(admin.Rows))
	}
	for _, r := range admin.Rows {
		if r.TenantID != "tenant-other" {
			t.Errorf("admin-widened lifecycle list leaked tenant %q, want tenant-other", r.TenantID)
		}
	}
	// The lifecycle projection never touched the Enricher across any of
	// the three shapes.
	if got := enricher.calls.Load(); got != 0 {
		t.Fatalf("lifecycle authority-parity lists enriched %d rows, want 0", got)
	}
}

// TestService_List_Lifecycle_AdminWidening_AuditEmitted pins the admin
// audit contract on the lifecycle projection: a successful cross-tenant
// lifecycle list emits exactly one audit.admin_scope_used.
func TestService_List_Lifecycle_AdminWidening_AuditEmitted(t *testing.T) {
	bus := newReplayBus(t, 64)
	mixed := append(catalogSnapshots(3, "tenant-1", "user-1", "t1-session"), catalogSnapshots(3, "tenant-other", "user-other", "tother-session")...)
	projector, err := NewListerProjector(catalogLister{snapshots: mixed})
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	svc, err := NewService(projector, WithBus(bus), WithRedactor(passRedactor{}))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	caller := lifecycleCaller()
	sub := subscribeAdminAudit(t, bus, caller)
	req := prototypes.SessionsListRequest{
		Identity:   prototypes.IdentityScope{Tenant: caller.TenantID, User: caller.UserID, Session: caller.SessionID},
		Projection: prototypes.SessionProjectionLifecycle,
		Filter:     prototypes.SessionFilter{TenantIDs: []string{"tenant-other"}},
	}
	resp, err := svc.List(context.Background(), req, true)
	if err != nil {
		t.Fatalf("admin-widened lifecycle List: %v", err)
	}
	if len(resp.Rows) != 3 {
		t.Fatalf("admin-widened lifecycle list returned %d rows, want 3", len(resp.Rows))
	}
	select {
	case ev := <-sub.Events():
		if ev.Type != events.EventTypeAdminScopeUsed {
			t.Fatalf("audit event type = %q, want audit.admin_scope_used", ev.Type)
		}
		if payload, ok := ev.Payload.(SessionsAdminQueryPayload); !ok || payload.Method != "sessions.list" {
			t.Fatalf("audit payload = %T (%+v), want SessionsAdminQueryPayload{Method: sessions.list}", ev.Payload, ev.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no audit.admin_scope_used within 2s of an admin-widened lifecycle list")
	}
}

// TestService_Inspect_Lifecycle_ZeroEnricherAndNotFoundParity pins the
// lifecycle inspect contract: zero Enricher calls, a not_requested marker,
// and the SAME non-oracular not-found posture as the full projection.
func TestService_Inspect_Lifecycle_ZeroEnricherAndNotFoundParity(t *testing.T) {
	mixed := append(catalogSnapshots(3, "tenant-1", "user-1", "t1-session"), catalogSnapshots(3, "tenant-other", "user-other", "tother-session")...)
	enricher := &countingEnricher{}
	projector, err := NewListerProjector(catalogLister{snapshots: mixed}, WithEnricher(enricher))
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	svc, err := NewService(projector)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	caller := lifecycleCaller()
	scope := prototypes.IdentityScope{Tenant: caller.TenantID, User: caller.UserID, Session: caller.SessionID}

	// 1. Lifecycle inspect of an own-tenant session: no enrichment, the
	// lifecycle fields, counters zero, marker not_requested.
	resp, err := svc.Inspect(context.Background(), prototypes.SessionsInspectRequest{
		Identity: scope, SessionID: "t1-session-001", Projection: prototypes.SessionProjectionLifecycle,
	}, false)
	if err != nil {
		t.Fatalf("lifecycle Inspect: %v", err)
	}
	if resp.Row.SessionID != "t1-session-001" || resp.Row.TenantID != "tenant-1" {
		t.Fatalf("lifecycle inspect row = %+v, want t1-session-001 in tenant-1", resp.Row)
	}
	if resp.Row.CounterStatus != prototypes.CounterStatusNotRequested {
		t.Errorf("lifecycle inspect row CounterStatus = %q, want not_requested", resp.Row.CounterStatus)
	}
	if resp.Row.TasksCount != 0 || resp.Row.TotalCostCents != 0 || resp.Row.HasFailedTask {
		t.Errorf("lifecycle inspect row carried counter data: %+v", resp.Row)
	}
	if got := enricher.calls.Load(); got != 0 {
		t.Fatalf("lifecycle inspect enriched %d rows, want 0", got)
	}

	// 2. Foreign-tenant session without admin — non-oracular not-found,
	// exactly the full projection's posture.
	if _, err := svc.Inspect(context.Background(), prototypes.SessionsInspectRequest{
		Identity: scope, SessionID: "tother-session-000", Projection: prototypes.SessionProjectionLifecycle,
	}, false); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("cross-tenant lifecycle inspect without admin error = %v, want ErrSessionNotFound (existence is never revealed)", err)
	}

	// 3. Unknown session id — non-oracular not-found.
	if _, err := svc.Inspect(context.Background(), prototypes.SessionsInspectRequest{
		Identity: scope, SessionID: "no-such-session", Projection: prototypes.SessionProjectionLifecycle,
	}, false); !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("unknown-session lifecycle inspect error = %v, want ErrSessionNotFound", err)
	}

	// 4. Contrast: the FULL inspect of the same session DOES enrich exactly
	// one row and marks it current (the countingEnricher reports exact
	// zero-count counters).
	full, err := svc.Inspect(context.Background(), prototypes.SessionsInspectRequest{
		Identity: scope, SessionID: "t1-session-001",
	}, false)
	if err != nil {
		t.Fatalf("full Inspect: %v", err)
	}
	if full.Row.CounterStatus != prototypes.CounterStatusCurrent {
		t.Errorf("full inspect row CounterStatus = %q, want current (enriched)", full.Row.CounterStatus)
	}
	if got := enricher.calls.Load(); got != 1 {
		t.Fatalf("counter enrichments = %d, want exactly 1 (the single full inspect)", got)
	}
}

// TestService_Inspect_Lifecycle_AdminWidening_AuditEmitted pins the admin
// audit contract on the lifecycle inspect projection: an admin-widened
// lifecycle inspect of a foreign-tenant session emits one
// audit.admin_scope_used.
func TestService_Inspect_Lifecycle_AdminWidening_AuditEmitted(t *testing.T) {
	bus := newReplayBus(t, 64)
	mixed := append(catalogSnapshots(3, "tenant-1", "user-1", "t1-session"), catalogSnapshots(3, "tenant-other", "user-other", "tother-session")...)
	projector, err := NewListerProjector(catalogLister{snapshots: mixed})
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	svc, err := NewService(projector, WithBus(bus), WithRedactor(passRedactor{}))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	caller := lifecycleCaller()
	sub := subscribeAdminAudit(t, bus, caller)
	resp, err := svc.Inspect(context.Background(), prototypes.SessionsInspectRequest{
		Identity:   prototypes.IdentityScope{Tenant: caller.TenantID, User: caller.UserID, Session: caller.SessionID},
		SessionID:  "tother-session-000",
		Projection: prototypes.SessionProjectionLifecycle,
	}, true)
	if err != nil {
		t.Fatalf("admin-widened lifecycle Inspect: %v", err)
	}
	if resp.Row.TenantID != "tenant-other" {
		t.Fatalf("admin-widened lifecycle inspect row = %+v, want tenant-other", resp.Row)
	}
	select {
	case ev := <-sub.Events():
		if ev.Type != events.EventTypeAdminScopeUsed {
			t.Fatalf("audit event type = %q, want audit.admin_scope_used", ev.Type)
		}
		if payload, ok := ev.Payload.(SessionsAdminQueryPayload); !ok || payload.Method != "sessions.inspect" {
			t.Fatalf("audit payload = %T (%+v), want SessionsAdminQueryPayload{Method: sessions.inspect}", ev.Payload, ev.Payload)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no audit.admin_scope_used within 2s of an admin-widened lifecycle inspect")
	}
}

// TestService_CounterStatus_AvailabilityStates pins the explicit
// counter-availability contract across all four states:
//
//   - full + wired + exact   → current
//   - full + wired + partial → partial (and CountersPartial=true)
//   - full + unwired         → unavailable
//   - lifecycle (either)     → not_requested
//
// A zero counter must never read as a measured zero without the row saying
// which state produced it.
func TestService_CounterStatus_AvailabilityStates(t *testing.T) {
	snapshots := catalogSnapshots(4, "tenant-1", "user-1", "lc-session")
	caller := lifecycleCaller()
	scope := prototypes.IdentityScope{Tenant: caller.TenantID, User: caller.UserID, Session: caller.SessionID}
	listReq := func(proj prototypes.SessionProjection) prototypes.SessionsListRequest {
		return prototypes.SessionsListRequest{Identity: scope, Projection: proj, Limit: 10}
	}

	t.Run("full_wired_exact_is_current", func(t *testing.T) {
		projector, err := NewListerProjector(catalogLister{snapshots: snapshots}, WithEnricher(zeroEnricher{}))
		if err != nil {
			t.Fatalf("NewListerProjector: %v", err)
		}
		svc, err := NewService(projector)
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		resp, err := svc.List(context.Background(), listReq(prototypes.SessionProjectionFull), false)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, r := range resp.Rows {
			if r.CounterStatus != prototypes.CounterStatusCurrent || r.CountersPartial {
				t.Errorf("row %q = status %q partial %v, want current/false", r.SessionID, r.CounterStatus, r.CountersPartial)
			}
		}
	})

	t.Run("full_wired_partial_is_partial", func(t *testing.T) {
		projector, err := NewListerProjector(catalogLister{snapshots: snapshots}, WithEnricher(partialEnricher{}))
		if err != nil {
			t.Fatalf("NewListerProjector: %v", err)
		}
		svc, err := NewService(projector)
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		resp, err := svc.List(context.Background(), listReq(prototypes.SessionProjectionFull), false)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, r := range resp.Rows {
			if r.CounterStatus != prototypes.CounterStatusPartial || !r.CountersPartial {
				t.Errorf("row %q = status %q partial %v, want partial/true (an honest lower bound)", r.SessionID, r.CounterStatus, r.CountersPartial)
			}
		}
	})

	t.Run("full_unwired_is_unavailable", func(t *testing.T) {
		projector, err := NewListerProjector(catalogLister{snapshots: snapshots})
		if err != nil {
			t.Fatalf("NewListerProjector: %v", err)
		}
		svc, err := NewService(projector)
		if err != nil {
			t.Fatalf("NewService: %v", err)
		}
		resp, err := svc.List(context.Background(), listReq(prototypes.SessionProjectionFull), false)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, r := range resp.Rows {
			if r.CounterStatus != prototypes.CounterStatusUnavailable {
				t.Errorf("row %q CounterStatus = %q, want unavailable (no Enricher wired)", r.SessionID, r.CounterStatus)
			}
		}
	})

	t.Run("lifecycle_is_not_requested_regardless_of_wiring", func(t *testing.T) {
		for _, wired := range []bool{false, true} {
			name := map[bool]string{false: "unwired", true: "wired"}[wired]
			t.Run(name, func(t *testing.T) {
				var projector *ListerProjector
				var err error
				if wired {
					projector, err = NewListerProjector(catalogLister{snapshots: snapshots}, WithEnricher(zeroEnricher{}))
				} else {
					projector, err = NewListerProjector(catalogLister{snapshots: snapshots})
				}
				if err != nil {
					t.Fatalf("NewListerProjector: %v", err)
				}
				svc, err := NewService(projector)
				if err != nil {
					t.Fatalf("NewService: %v", err)
				}
				resp, err := svc.List(context.Background(), listReq(prototypes.SessionProjectionLifecycle), false)
				if err != nil {
					t.Fatalf("List: %v", err)
				}
				for _, r := range resp.Rows {
					// The request chose to skip the counters, so the marker
					// is "not requested" even on an unwired build — never
					// "unavailable" (the counters were not asked for) and
					// never "measured as zero".
					if r.CounterStatus != prototypes.CounterStatusNotRequested {
						t.Errorf("row %q CounterStatus = %q, want not_requested", r.SessionID, r.CounterStatus)
					}
				}
			})
		}
	})
}

// bareProjector implements Projector but NOT the private lifecycle-only
// seam — the stand-in for a hypothetical projector that cannot serve a
// lifecycle-only request.
type bareProjector struct{}

func (bareProjector) ListSessions(context.Context, identity.Identity, prototypes.SessionFilter, bool) ([]prototypes.SessionRow, error) {
	return nil, nil
}

func (bareProjector) InspectSession(context.Context, identity.Identity, string, bool) (prototypes.SessionsInspectResponse, error) {
	return prototypes.SessionsInspectResponse{}, ErrSessionNotFound
}

func (bareProjector) CountersAvailable() bool { return true }

// TestService_Lifecycle_UnsupportedProjector_FailsLoud pins the honest
// fallback: a projector that cannot serve a lifecycle-only request is
// rejected invalid_request — never silently enriched (a contract break)
// and never silently stripped (a believable-but-false page). The full
// projection on the same projector keeps working.
func TestService_Lifecycle_UnsupportedProjector_FailsLoud(t *testing.T) {
	svc, err := NewService(bareProjector{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	caller := lifecycleCaller()
	scope := prototypes.IdentityScope{Tenant: caller.TenantID, User: caller.UserID, Session: caller.SessionID}
	listReq := prototypes.SessionsListRequest{
		Identity: scope, Projection: prototypes.SessionProjectionLifecycle,
	}
	if _, err := svc.List(context.Background(), listReq, false); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("lifecycle List on a non-lifecycle projector error = %v, want ErrInvalidRequest", err)
	}
	inspectReq := prototypes.SessionsInspectRequest{
		Identity: scope, SessionID: "s1", Projection: prototypes.SessionProjectionLifecycle,
	}
	if _, err := svc.Inspect(context.Background(), inspectReq, false); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("lifecycle Inspect on a non-lifecycle projector error = %v, want ErrInvalidRequest", err)
	}
	// The full projection on the same projector is untouched.
	if resp, err := svc.List(context.Background(), prototypes.SessionsListRequest{Identity: scope}, false); err != nil || len(resp.Rows) != 0 {
		t.Fatalf("full List on a non-lifecycle projector = (%d rows, %v), want (0, nil)", len(resp.Rows), err)
	}
}

// TestService_ConcurrentReuse_LifecycleAndFull_MixedIdentities is the
// D-025 concurrent-reuse gate extension for the lifecycle projection: N≥100
// goroutines mix lifecycle and full list requests across two tenants against
// ONE shared Service (real ListerProjector + countingEnricher) under -race,
// asserting no cross-tenant bleed, lifecycle rows never carry counter data,
// full rows always carry the current marker, the Enricher sees exactly the
// full requests' page rows (zero calls from lifecycle), and the goroutine
// count returns to baseline.
func TestService_ConcurrentReuse_LifecycleAndFull_MixedIdentities(t *testing.T) {
	mixed := append(catalogSnapshots(10, "tenant-1", "user-1", "t1-session"), catalogSnapshots(10, "tenant-other", "user-other", "tother-session")...)
	enricher := &countingEnricher{}
	projector, err := NewListerProjector(catalogLister{snapshots: mixed}, WithEnricher(enricher))
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	svc, err := NewService(projector)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	baseline := settledGoroutineCount()

	const N = 200
	var wg sync.WaitGroup
	wg.Add(N)
	errCh := make(chan string, N)
	for i := range N {
		go func(n int) {
			defer wg.Done()
			tenant, user := "tenant-1", "user-1"
			if n%2 == 1 {
				tenant, user = "tenant-other", "user-other"
			}
			proj := prototypes.SessionProjectionLifecycle
			if n%4 >= 2 {
				proj = prototypes.SessionProjectionFull
			}
			req := prototypes.SessionsListRequest{
				Identity:   prototypes.IdentityScope{Tenant: tenant, User: user, Session: "caller-session"},
				Projection: proj,
				Limit:      20,
			}
			resp, lerr := svc.List(context.Background(), req, false)
			if lerr != nil {
				errCh <- fmt.Sprintf("List(%s): %v", tenant, lerr)
				return
			}
			for _, r := range resp.Rows {
				if r.TenantID != tenant {
					errCh <- fmt.Sprintf("context bleed: row tenant %q leaked into a query for %q", r.TenantID, tenant)
					return
				}
				if proj == prototypes.SessionProjectionLifecycle {
					if r.CounterStatus != prototypes.CounterStatusNotRequested {
						errCh <- fmt.Sprintf("lifecycle row %q status = %q, want not_requested", r.SessionID, r.CounterStatus)
						return
					}
					if r.TotalCostCents != 0 || r.TasksCount != 0 {
						errCh <- fmt.Sprintf("lifecycle row %q carried counter data", r.SessionID)
						return
					}
				} else if r.CounterStatus != prototypes.CounterStatusCurrent {
					errCh <- fmt.Sprintf("full row %q status = %q, want current", r.SessionID, r.CounterStatus)
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}

	// Each tenant has 10 rows, so each FULL request enriches its 10-row
	// page; lifecycle requests enrich nothing.
	fullRequests := N / 2
	if got := enricher.calls.Load(); got != int64(fullRequests*10) {
		t.Errorf("counter enrichments = %d, want %d (full requests' page rows only — lifecycle never touches the Enricher)", got, fullRequests*10)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 2 {
		t.Errorf("goroutine leak: %d goroutines above baseline %d after %d mixed lifecycle/full List calls", leaked, baseline, N)
	}
}
