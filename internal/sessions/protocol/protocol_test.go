package protocol_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
)

// fakeProjector is an in-test Projector backed by a fixed row slice.
// It enforces the identity-scope contract: when adminScoped is false it
// restricts to the caller's own tenant; when true it honours the
// filter's TenantIDs. It is NOT a re-implementation of subsystem
// behaviour (CLAUDE.md §17.4) — it is the deterministic stand-in for a
// SessionLister the Service-level unit tests need; the integration
// test uses the real registry.
type fakeProjector struct {
	rows    []prototypes.SessionRow
	listErr error
	// noCounters simulates a projector with NO Enricher wired — the WARN-3
	// partial-build posture. When true, CountersAvailable reports false and
	// the Service loud-rejects a facet/sort over the counters (D-309).
	noCounters bool
}

// CountersAvailable reports whether the fake projector "has an enricher".
// Default (false noCounters) => true: the Service tests exercise truthful
// cost/failed/intervention facets over rows this fake legitimately
// populates (it stands in for a WIRED projector).
func (f *fakeProjector) CountersAvailable() bool { return !f.noCounters }

func (f *fakeProjector) ListSessions(_ context.Context, id identity.Identity, filter prototypes.SessionFilter, adminScoped bool) ([]prototypes.SessionRow, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	out := make([]prototypes.SessionRow, 0, len(f.rows))
	for _, r := range f.rows {
		if !adminScoped {
			if r.TenantID != id.TenantID {
				continue
			}
		} else if len(filter.TenantIDs) > 0 {
			match := false
			for _, t := range filter.TenantIDs {
				if t == r.TenantID {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		out = append(out, r)
	}
	return out, nil
}

func (f *fakeProjector) InspectSession(_ context.Context, id identity.Identity, sessionID string, adminScoped bool) (prototypes.SessionsInspectResponse, error) {
	for _, r := range f.rows {
		if r.SessionID != sessionID {
			continue
		}
		if !adminScoped && r.TenantID != id.TenantID {
			continue
		}
		return prototypes.SessionsInspectResponse{
			Row:                 r,
			RecentInterventions: []prototypes.InterventionSummary{},
			RecentArtifacts:     []prototypes.ArtifactRefSummary{},
		}, nil
	}
	return prototypes.SessionsInspectResponse{}, sessionsprotocol.ErrSessionNotFound
}

// sampleRows builds a deterministic two-tenant row set. These rows stand
// in for the output of a projector WITH an enricher wired: the counters
// (cost / has_failed_task) are populated exactly as the runtime would
// populate them at the source. The agent fields stay NIL — no
// single-valued session→agent binding exists (D-309), so a fixture that
// set them would be RICHER than the real producer (§17.8). The Service
// applies its facets / sort over whatever the Projector returns.
func sampleRows() []prototypes.SessionRow {
	base := time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC)
	mk := func(sid, tenant string, n int, status prototypes.SessionStatus, cost int64, failed bool) prototypes.SessionRow {
		return prototypes.SessionRow{
			SessionID:      sid,
			Status:         status,
			UserID:         "u1",
			TenantID:       tenant,
			StartedAt:      base.Add(time.Duration(n) * time.Minute),
			LastActivityAt: base.Add(time.Duration(n+10) * time.Minute),
			Duration:       10 * time.Minute,
			TotalCostCents: cost,
			HasFailedTask:  failed,
			Identity:       prototypes.IdentityScope{Tenant: tenant, User: "u1", Session: sid},
		}
	}
	return []prototypes.SessionRow{
		mk("s-a1", "t1", 1, prototypes.SessionStatusRunning, 100, false),
		mk("s-a2", "t1", 2, prototypes.SessionStatusFailed, 500, true),
		mk("s-a3", "t1", 3, prototypes.SessionStatusCompleted, 50, false),
		mk("s-b1", "t2", 4, prototypes.SessionStatusRunning, 900, false),
	}
}

func newService(t *testing.T, rows []prototypes.SessionRow) *sessionsprotocol.Service {
	t.Helper()
	svc, err := sessionsprotocol.NewService(&fakeProjector{rows: rows})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func t1Identity() prototypes.IdentityScope {
	return prototypes.IdentityScope{Tenant: "t1", User: "u1", Session: "u1-sess"}
}

func TestNewService_NilProjector_FailsLoud(t *testing.T) {
	t.Parallel()
	if _, err := sessionsprotocol.NewService(nil); !errors.Is(err, sessionsprotocol.ErrMisconfigured) {
		t.Fatalf("NewService(nil) error = %v, want ErrMisconfigured", err)
	}
}

func TestList_IdentityMandatory(t *testing.T) {
	t.Parallel()
	svc := newService(t, sampleRows())
	_, err := svc.List(context.Background(), prototypes.SessionsListRequest{}, false)
	if !errors.Is(err, sessionsprotocol.ErrIdentityRequired) {
		t.Fatalf("List with empty identity error = %v, want ErrIdentityRequired", err)
	}
}

func TestList_OwnTenant_HappyPath(t *testing.T) {
	t.Parallel()
	svc := newService(t, sampleRows())
	resp, err := svc.List(context.Background(), prototypes.SessionsListRequest{Identity: t1Identity()}, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Rows) != 3 {
		t.Fatalf("own-tenant list returned %d rows, want 3 (t1 sessions only)", len(resp.Rows))
	}
	for _, r := range resp.Rows {
		if r.TenantID != "t1" {
			t.Errorf("non-admin list leaked tenant %q — CLAUDE.md §6 isolation breach", r.TenantID)
		}
	}
}

func TestList_CrossTenantWithoutAdmin_Rejected(t *testing.T) {
	t.Parallel()
	svc := newService(t, sampleRows())
	req := prototypes.SessionsListRequest{
		Identity: t1Identity(),
		Filter:   prototypes.SessionFilter{TenantIDs: []string{"t2"}},
	}
	_, err := svc.List(context.Background(), req, false)
	if !errors.Is(err, sessionsprotocol.ErrCrossTenantScope) {
		t.Fatalf("cross-tenant list without admin error = %v, want ErrCrossTenantScope (D-079)", err)
	}
}

func TestList_CrossTenantWithAdmin_Allowed(t *testing.T) {
	t.Parallel()
	svc := newService(t, sampleRows())
	req := prototypes.SessionsListRequest{
		Identity: t1Identity(),
		Filter:   prototypes.SessionFilter{TenantIDs: []string{"t2"}},
	}
	resp, err := svc.List(context.Background(), req, true)
	if err != nil {
		t.Fatalf("cross-tenant list with admin: %v", err)
	}
	if len(resp.Rows) != 1 || resp.Rows[0].TenantID != "t2" {
		t.Fatalf("admin cross-tenant list = %+v, want a single t2 row", resp.Rows)
	}
}

func TestList_StatusFilter_Narrows(t *testing.T) {
	t.Parallel()
	svc := newService(t, sampleRows())
	req := prototypes.SessionsListRequest{
		Identity: t1Identity(),
		Filter:   prototypes.SessionFilter{Statuses: []prototypes.SessionStatus{prototypes.SessionStatusFailed}},
	}
	resp, err := svc.List(context.Background(), req, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Rows) != 1 || resp.Rows[0].Status != prototypes.SessionStatusFailed {
		t.Fatalf("status=failed filter = %+v, want a single Failed row", resp.Rows)
	}
}

func TestList_CostAboveFilter_Narrows(t *testing.T) {
	t.Parallel()
	svc := newService(t, sampleRows())
	above := int64(80)
	req := prototypes.SessionsListRequest{
		Identity: t1Identity(),
		Filter:   prototypes.SessionFilter{CostAboveCents: &above},
	}
	resp, err := svc.List(context.Background(), req, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	// t1 rows have cost 100, 500, 50 — two are strictly above 80.
	if len(resp.Rows) != 2 {
		t.Fatalf("cost-above=80 filter returned %d rows, want 2", len(resp.Rows))
	}
}

func TestList_HasFailedTaskFilter_Narrows(t *testing.T) {
	t.Parallel()
	svc := newService(t, sampleRows())
	yes := true
	req := prototypes.SessionsListRequest{
		Identity: t1Identity(),
		Filter:   prototypes.SessionFilter{HasFailedTask: &yes},
	}
	resp, err := svc.List(context.Background(), req, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Rows) != 1 || !resp.Rows[0].HasFailedTask {
		t.Fatalf("has_failed_task=true filter = %+v, want a single failed-task row", resp.Rows)
	}
}

func TestList_SortCostDesc(t *testing.T) {
	t.Parallel()
	svc := newService(t, sampleRows())
	req := prototypes.SessionsListRequest{Identity: t1Identity(), Sort: prototypes.SessionSortCostDesc}
	resp, err := svc.List(context.Background(), req, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for i := 1; i < len(resp.Rows); i++ {
		if resp.Rows[i-1].TotalCostCents < resp.Rows[i].TotalCostCents {
			t.Fatalf("cost_desc sort not descending: %+v", resp.Rows)
		}
	}
}

func TestList_AgentIDsFilter_LoudReject(t *testing.T) {
	t.Parallel()
	svc := newService(t, sampleRows())
	req := prototypes.SessionsListRequest{
		Identity: t1Identity(),
		Filter:   prototypes.SessionFilter{AgentIDs: []string{"agent-a"}},
	}
	_, err := svc.List(context.Background(), req, false)
	if !errors.Is(err, sessionsprotocol.ErrInvalidRequest) {
		t.Fatalf("filter.agent_ids over unpopulated binding error = %v, want ErrInvalidRequest (loud, D-309 WARN-4)", err)
	}
}

func TestList_Query_MatchesSessionAndUser_NeverWholeReject(t *testing.T) {
	t.Parallel()
	svc := newService(t, sampleRows())
	// A query that touches the (nil) agent sub-fields must NOT be failed
	// loud as a whole — it still matches the populated session_id / user_id.
	// "s-a2" is a session-id substring; the query must resolve to a 200.
	req := prototypes.SessionsListRequest{
		Identity: t1Identity(),
		Filter:   prototypes.SessionFilter{Query: "s-a2"},
	}
	resp, err := svc.List(context.Background(), req, false)
	if err != nil {
		t.Fatalf("query over session_id must succeed, never whole-reject (WARN-4): %v", err)
	}
	if len(resp.Rows) != 1 || resp.Rows[0].SessionID != "s-a2" {
		t.Fatalf("query=s-a2 matched %+v, want the single s-a2 row via session_id", resp.Rows)
	}
	// A query matching the user id also resolves (not agent).
	respU, err := svc.List(context.Background(), prototypes.SessionsListRequest{
		Identity: t1Identity(),
		Filter:   prototypes.SessionFilter{Query: "u1"},
	}, false)
	if err != nil {
		t.Fatalf("query over user_id must succeed: %v", err)
	}
	if len(respU.Rows) != 3 {
		t.Fatalf("query=u1 matched %d rows, want 3 (all t1 rows via user_id)", len(respU.Rows))
	}
}

func TestList_CounterFacets_UnwiredProjector_LoudReject(t *testing.T) {
	t.Parallel()
	// WARN-3: a projector with NO enricher wired must NOT return a
	// false-empty counter page. Every numeric-counter facet / cost_desc sort
	// loud-rejects rather than silently excluding/mis-ordering.
	svc, err := sessionsprotocol.NewService(&fakeProjector{rows: sampleRows(), noCounters: true})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	above := int64(80)
	yes := true
	cases := map[string]prototypes.SessionsListRequest{
		"cost_above_cents": {Identity: t1Identity(), Filter: prototypes.SessionFilter{CostAboveCents: &above}},
		"has_failed_task":  {Identity: t1Identity(), Filter: prototypes.SessionFilter{HasFailedTask: &yes}},
		"has_intervention": {Identity: t1Identity(), Filter: prototypes.SessionFilter{HasIntervention: &yes}},
		"cost_desc_sort":   {Identity: t1Identity(), Sort: prototypes.SessionSortCostDesc},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			resp, err := svc.List(context.Background(), req, false)
			if !errors.Is(err, sessionsprotocol.ErrInvalidRequest) {
				t.Fatalf("%s over unwired counters returned (%d rows, err=%v), want ErrInvalidRequest — never a false-empty page (WARN-3)", name, len(resp.Rows), err)
			}
		})
	}
	// A non-counter query (status facet) still works on the unwired build —
	// the gate is scoped to the counter axes only.
	statusReq := prototypes.SessionsListRequest{
		Identity: t1Identity(),
		Filter:   prototypes.SessionFilter{Statuses: []prototypes.SessionStatus{prototypes.SessionStatusFailed}},
	}
	if _, err := svc.List(context.Background(), statusReq, false); err != nil {
		t.Fatalf("status facet on unwired build must still work: %v", err)
	}
}

func TestList_PartialRow_SurfacesHonestSignal_NotExcluded(t *testing.T) {
	t.Parallel()
	// A partial-counter row (CountersPartial=true) is a lower bound; the
	// cost_above filter must still surface it (never silently exclude a row
	// whose true cost is unknown-but-at-least-N) and carry the honest signal.
	base := time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC)
	rows := []prototypes.SessionRow{{
		SessionID: "s-part", Status: prototypes.SessionStatusRunning, UserID: "u1", TenantID: "t1",
		StartedAt: base, LastActivityAt: base.Add(time.Minute),
		TotalCostCents: 1000, CountersPartial: true,
		Identity: prototypes.IdentityScope{Tenant: "t1", User: "u1", Session: "s-part"},
	}}
	svc := newService(t, rows)
	above := int64(80)
	resp, err := svc.List(context.Background(), prototypes.SessionsListRequest{
		Identity: t1Identity(), Filter: prototypes.SessionFilter{CostAboveCents: &above},
	}, false)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Rows) != 1 || !resp.Rows[0].CountersPartial {
		t.Fatalf("partial row must be surfaced with its honest CountersPartial signal, got %+v", resp.Rows)
	}
}

func TestList_InvalidLimit_Rejected(t *testing.T) {
	t.Parallel()
	svc := newService(t, sampleRows())
	req := prototypes.SessionsListRequest{Identity: t1Identity(), Limit: prototypes.MaxSessionListLimit + 1}
	_, err := svc.List(context.Background(), req, false)
	if !errors.Is(err, sessionsprotocol.ErrInvalidRequest) {
		t.Fatalf("over-max limit error = %v, want ErrInvalidRequest (no silent clamp)", err)
	}
}

func TestList_CursorPagination_Stable(t *testing.T) {
	t.Parallel()
	svc := newService(t, sampleRows())
	// Page through t1's three rows with Limit=2.
	req := prototypes.SessionsListRequest{Identity: t1Identity(), Limit: 2}
	p1, err := svc.List(context.Background(), req, false)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(p1.Rows) != 2 || !p1.Truncated || p1.NextCursor == "" {
		t.Fatalf("page 1 = %d rows truncated=%v cursor=%q, want 2 / true / non-empty", len(p1.Rows), p1.Truncated, p1.NextCursor)
	}
	req.Cursor = p1.NextCursor
	p2, err := svc.List(context.Background(), req, false)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(p2.Rows) != 1 || p2.Truncated || p2.NextCursor != "" {
		t.Fatalf("page 2 = %d rows truncated=%v cursor=%q, want 1 / false / empty", len(p2.Rows), p2.Truncated, p2.NextCursor)
	}
	// No row repeats across pages.
	seen := map[string]bool{}
	for _, r := range append(p1.Rows, p2.Rows...) {
		if seen[r.SessionID] {
			t.Fatalf("cursor pagination repeated row %q", r.SessionID)
		}
		seen[r.SessionID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("cursor pagination yielded %d distinct rows, want 3", len(seen))
	}
}

func TestList_MalformedCursor_FailsLoud(t *testing.T) {
	t.Parallel()
	svc := newService(t, sampleRows())
	req := prototypes.SessionsListRequest{Identity: t1Identity(), Cursor: "!!!not-base64!!!"}
	_, err := svc.List(context.Background(), req, false)
	if !errors.Is(err, sessionsprotocol.ErrInvalidRequest) {
		t.Fatalf("malformed cursor error = %v, want ErrInvalidRequest (no silent reset to page 1)", err)
	}
}

func TestInspect_HappyPath(t *testing.T) {
	t.Parallel()
	svc := newService(t, sampleRows())
	req := prototypes.SessionsInspectRequest{Identity: t1Identity(), SessionID: "s-a1"}
	resp, err := svc.Inspect(context.Background(), req, false)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if resp.Row.SessionID != "s-a1" {
		t.Fatalf("Inspect row = %q, want s-a1", resp.Row.SessionID)
	}
}

func TestInspect_CrossTenantWithoutAdmin_NotFound(t *testing.T) {
	t.Parallel()
	svc := newService(t, sampleRows())
	// s-b1 is a t2 session; a t1 caller without admin must not see it.
	req := prototypes.SessionsInspectRequest{Identity: t1Identity(), SessionID: "s-b1"}
	_, err := svc.Inspect(context.Background(), req, false)
	if !errors.Is(err, sessionsprotocol.ErrSessionNotFound) {
		t.Fatalf("cross-tenant inspect without admin error = %v, want ErrSessionNotFound", err)
	}
}

func TestInspect_EmptySessionID_Rejected(t *testing.T) {
	t.Parallel()
	svc := newService(t, sampleRows())
	_, err := svc.Inspect(context.Background(), prototypes.SessionsInspectRequest{Identity: t1Identity()}, false)
	if !errors.Is(err, sessionsprotocol.ErrInvalidRequest) {
		t.Fatalf("empty session_id error = %v, want ErrInvalidRequest", err)
	}
}
