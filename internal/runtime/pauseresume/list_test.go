package pauseresume_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

// listClock returns a controllable clock whose first N calls produce
// strictly increasing timestamps one minute apart, anchored at a fixed
// base — so PausedAt ordering in List is deterministic (CLAUDE.md §11).
func listClock(base time.Time) (func() time.Time, *int) {
	calls := 0
	return func() time.Time {
		t := base.Add(time.Duration(calls) * time.Minute)
		calls++
		return t
	}, &calls
}

// requestPause is a small helper that records a pause under the given
// identity + run + reason and returns its Token.
func requestPause(t *testing.T, c pauseresume.Coordinator, id identity.Identity, runID string, reason pauseresume.Reason) pauseresume.Token {
	t.Helper()
	ctx := runCtx(t, id, runID)
	p, err := c.Request(ctx, pauseresume.PauseRequest{Identity: id, Reason: reason})
	if err != nil {
		t.Fatalf("Request: %v", err)
	}
	return p.Token
}

func TestList_EmptyFilterDefaultsToCallerScopePaused(t *testing.T) {
	t.Parallel()
	clk, _ := listClock(time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC))
	c := pauseresume.New(pauseresume.WithClock(clk))

	requestPause(t, c, testID, "run-a", pauseresume.ReasonApprovalRequired)
	requestPause(t, c, testID, "run-b", pauseresume.ReasonAwaitInput)

	resp, err := c.List(context.Background(), pauseresume.ListRequest{Identity: testID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if resp.TotalRows != 2 {
		t.Fatalf("TotalRows = %d, want 2", resp.TotalRows)
	}
	if len(resp.Snapshots) != 2 {
		t.Fatalf("len(Snapshots) = %d, want 2", len(resp.Snapshots))
	}
	if resp.Page != 1 || resp.PageSize != pauseresume.DefaultListPageSize {
		t.Fatalf("Page/PageSize = %d/%d, want 1/%d", resp.Page, resp.PageSize, pauseresume.DefaultListPageSize)
	}
	// Default state filter = paused — both are still paused.
	for i, st := range resp.Statuses {
		if st.State != pauseresume.StatusPaused {
			t.Errorf("Snapshots[%d].State = %q, want paused", i, st.State)
		}
	}
}

func TestList_OrdersPausedAtDescending(t *testing.T) {
	t.Parallel()
	clk, _ := listClock(time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC))
	c := pauseresume.New(pauseresume.WithClock(clk))

	// Three pauses recorded at t, t+1m, t+2m.
	requestPause(t, c, testID, "run-1", pauseresume.ReasonApprovalRequired)
	requestPause(t, c, testID, "run-2", pauseresume.ReasonApprovalRequired)
	requestPause(t, c, testID, "run-3", pauseresume.ReasonApprovalRequired)

	resp, err := c.List(context.Background(), pauseresume.ListRequest{Identity: testID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Snapshots) != 3 {
		t.Fatalf("len(Snapshots) = %d, want 3", len(resp.Snapshots))
	}
	for i := 1; i < len(resp.Snapshots); i++ {
		if resp.Snapshots[i-1].PausedAt.Before(resp.Snapshots[i].PausedAt) {
			t.Errorf("Snapshots not PausedAt-descending: [%d]=%v before [%d]=%v",
				i-1, resp.Snapshots[i-1].PausedAt, i, resp.Snapshots[i].PausedAt)
		}
	}
}

func TestList_IdentityMandatory(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	_, err := c.List(context.Background(), pauseresume.ListRequest{
		Identity: identity.Identity{TenantID: "t1"}, // incomplete
	})
	if !errors.Is(err, pauseresume.ErrIdentityRequired) {
		t.Fatalf("List with incomplete identity: err = %v, want ErrIdentityRequired", err)
	}
}

func TestList_PaginationMath(t *testing.T) {
	t.Parallel()
	clk, _ := listClock(time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC))
	c := pauseresume.New(pauseresume.WithClock(clk))

	// 25 records, PageSize 10 ⇒ 3 pages (10, 10, 5).
	for i := 0; i < 25; i++ {
		requestPause(t, c, testID, "run", pauseresume.ReasonApprovalRequired)
	}
	cases := []struct {
		page, wantLen int
	}{
		{1, 10}, {2, 10}, {3, 5}, {4, 0},
	}
	for _, tc := range cases {
		resp, err := c.List(context.Background(), pauseresume.ListRequest{
			Identity: testID, Page: tc.page, PageSize: 10,
		})
		if err != nil {
			t.Fatalf("List page %d: %v", tc.page, err)
		}
		if resp.TotalRows != 25 {
			t.Errorf("page %d: TotalRows = %d, want 25", tc.page, resp.TotalRows)
		}
		if resp.PageCount != 3 {
			t.Errorf("page %d: PageCount = %d, want 3", tc.page, resp.PageCount)
		}
		if len(resp.Snapshots) != tc.wantLen {
			t.Errorf("page %d: len(Snapshots) = %d, want %d", tc.page, len(resp.Snapshots), tc.wantLen)
		}
	}
}

func TestList_RejectsInvalidPagination(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	cases := []struct {
		name           string
		page, pageSize int
	}{
		{"negative page", -1, 10},
		{"negative pagesize", 1, -1},
		{"pagesize over max", 1, pauseresume.MaxListPageSize + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := c.List(context.Background(), pauseresume.ListRequest{
				Identity: testID, Page: tc.page, PageSize: tc.pageSize,
			})
			if !errors.Is(err, pauseresume.ErrInvalidPage) {
				t.Fatalf("err = %v, want ErrInvalidPage", err)
			}
		})
	}
}

func TestList_FilterByReason(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	requestPause(t, c, testID, "r1", pauseresume.ReasonApprovalRequired)
	requestPause(t, c, testID, "r2", pauseresume.ReasonAwaitInput)
	requestPause(t, c, testID, "r3", pauseresume.ReasonApprovalRequired)

	resp, err := c.List(context.Background(), pauseresume.ListRequest{
		Identity: testID,
		Filter:   pauseresume.ListFilter{Reasons: []pauseresume.Reason{pauseresume.ReasonApprovalRequired}},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if resp.TotalRows != 2 {
		t.Fatalf("TotalRows = %d, want 2 (only ApprovalRequired)", resp.TotalRows)
	}
	for _, s := range resp.Snapshots {
		if s.Reason != pauseresume.ReasonApprovalRequired {
			t.Errorf("Reason = %q, want ApprovalRequired", s.Reason)
		}
	}
}

func TestList_FilterByRunID(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	requestPause(t, c, testID, "run-keep", pauseresume.ReasonApprovalRequired)
	requestPause(t, c, testID, "run-drop", pauseresume.ReasonApprovalRequired)

	resp, err := c.List(context.Background(), pauseresume.ListRequest{
		Identity: testID,
		Filter:   pauseresume.ListFilter{RunIDs: []string{"run-keep"}},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if resp.TotalRows != 1 {
		t.Fatalf("TotalRows = %d, want 1", resp.TotalRows)
	}
}

func TestList_FilterByTimeWindow(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	clk, _ := listClock(base)
	c := pauseresume.New(pauseresume.WithClock(clk))

	// Records at base, base+1m, base+2m.
	requestPause(t, c, testID, "w1", pauseresume.ReasonApprovalRequired)
	requestPause(t, c, testID, "w2", pauseresume.ReasonApprovalRequired)
	requestPause(t, c, testID, "w3", pauseresume.ReasonApprovalRequired)

	// Window [base+1m, base+1m] — only the middle record.
	resp, err := c.List(context.Background(), pauseresume.ListRequest{
		Identity: testID,
		Filter: pauseresume.ListFilter{
			Since: base.Add(time.Minute),
			Until: base.Add(time.Minute),
		},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if resp.TotalRows != 1 {
		t.Fatalf("TotalRows = %d, want 1 (only the base+1m record)", resp.TotalRows)
	}
}

func TestList_CrossTenantRequiresAdminScope(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	requestPause(t, c, testID, "run", pauseresume.ReasonApprovalRequired)

	// A filter naming a foreign tenant without AdminScoped → fail closed.
	_, err := c.List(context.Background(), pauseresume.ListRequest{
		Identity: testID,
		Filter:   pauseresume.ListFilter{TenantIDs: []string{"foreign-tenant"}},
	})
	if !errors.Is(err, pauseresume.ErrCrossTenantScope) {
		t.Fatalf("cross-tenant filter without admin: err = %v, want ErrCrossTenantScope", err)
	}

	// More than one tenant without AdminScoped → also fail closed.
	_, err = c.List(context.Background(), pauseresume.ListRequest{
		Identity: testID,
		Filter:   pauseresume.ListFilter{TenantIDs: []string{"t1", "t2"}},
	})
	if !errors.Is(err, pauseresume.ErrCrossTenantScope) {
		t.Fatalf("multi-tenant filter without admin: err = %v, want ErrCrossTenantScope", err)
	}
}

func TestList_CrossTenantWithAdminScopeReturnsBoth(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	tenantA := identity.Identity{TenantID: "tenant-a", UserID: "u", SessionID: "s"}
	tenantB := identity.Identity{TenantID: "tenant-b", UserID: "u", SessionID: "s"}
	requestPause(t, c, tenantA, "run-a", pauseresume.ReasonApprovalRequired)
	requestPause(t, c, tenantB, "run-b", pauseresume.ReasonApprovalRequired)

	resp, err := c.List(context.Background(), pauseresume.ListRequest{
		Identity:    tenantA,
		AdminScoped: true,
		Filter:      pauseresume.ListFilter{TenantIDs: []string{"tenant-a", "tenant-b"}},
	})
	if err != nil {
		t.Fatalf("admin-scoped cross-tenant List: %v", err)
	}
	if resp.TotalRows != 2 {
		t.Fatalf("TotalRows = %d, want 2 (both tenants)", resp.TotalRows)
	}
}

func TestList_OwnScopeExcludesForeignTenant(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	tenantA := identity.Identity{TenantID: "tenant-a", UserID: "u", SessionID: "s"}
	tenantB := identity.Identity{TenantID: "tenant-b", UserID: "u", SessionID: "s"}
	requestPause(t, c, tenantA, "run-a", pauseresume.ReasonApprovalRequired)
	requestPause(t, c, tenantB, "run-b", pauseresume.ReasonApprovalRequired)

	// Empty filter from tenant A — sees only A's pause.
	resp, err := c.List(context.Background(), pauseresume.ListRequest{Identity: tenantA})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if resp.TotalRows != 1 {
		t.Fatalf("TotalRows = %d, want 1 (own tenant only)", resp.TotalRows)
	}
	if resp.Snapshots[0].Identity.TenantID != "tenant-a" {
		t.Fatalf("snapshot tenant = %q, want tenant-a", resp.Snapshots[0].Identity.TenantID)
	}
}

func TestList_StatusFilterResumedTruncatedWhenAgedOut(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	// No resumed records in the registry — a status=resumed query
	// signals Truncated (the destructive-on-resume contract).
	resp, err := c.List(context.Background(), pauseresume.ListRequest{
		Identity: testID,
		Filter:   pauseresume.ListFilter{States: []pauseresume.State{pauseresume.StatusResumed}},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !resp.Truncated {
		t.Fatal("Truncated = false, want true (no resumed records in registry)")
	}
	if resp.TotalRows != 0 {
		t.Fatalf("TotalRows = %d, want 0", resp.TotalRows)
	}
}

func TestList_StatusFilterResumedSeesLiveResumedRecord(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	tok := requestPause(t, c, testID, "run-resume", pauseresume.ReasonApprovalRequired)
	ctx := runCtx(t, testID, "run-resume")
	if err := c.Resume(ctx, tok, pauseresume.DecisionApprove, nil); err != nil {
		t.Fatalf("Resume: %v", err)
	}
	resp, err := c.List(context.Background(), pauseresume.ListRequest{
		Identity: testID,
		Filter:   pauseresume.ListFilter{States: []pauseresume.State{pauseresume.StatusResumed}},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if resp.TotalRows != 1 {
		t.Fatalf("TotalRows = %d, want 1 (the live resumed record)", resp.TotalRows)
	}
	if resp.Truncated {
		t.Fatal("Truncated = true, want false (a resumed record IS in the registry)")
	}
	if resp.Statuses[0].State != pauseresume.StatusResumed {
		t.Fatalf("State = %q, want resumed", resp.Statuses[0].State)
	}
}

func TestList_HonoursCancelledContext(t *testing.T) {
	t.Parallel()
	c := pauseresume.New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := c.List(ctx, pauseresume.ListRequest{Identity: testID})
	if err == nil {
		t.Fatal("List with cancelled ctx: err = nil, want non-nil")
	}
}

func TestList_AllAxesCombination(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	clk, _ := listClock(base)
	c := pauseresume.New(pauseresume.WithClock(clk))

	// The one record that passes every axis.
	want := requestPause(t, c, testID, "run-match", pauseresume.ReasonApprovalRequired)
	// Noise records that each fail one axis.
	requestPause(t, c, testID, "run-other", pauseresume.ReasonAwaitInput)                                                                 // wrong reason
	requestPause(t, c, testID, "run-match", pauseresume.ReasonApprovalRequired)                                                           // right axes, later time
	requestPause(t, c, identity.Identity{TenantID: "t1", UserID: "u2", SessionID: "s1"}, "run-match", pauseresume.ReasonApprovalRequired) // wrong user

	resp, err := c.List(context.Background(), pauseresume.ListRequest{
		Identity: testID,
		Filter: pauseresume.ListFilter{
			States:     []pauseresume.State{pauseresume.StatusPaused},
			UserIDs:    []string{"u1"},
			SessionIDs: []string{"s1"},
			RunIDs:     []string{"run-match"},
			Reasons:    []pauseresume.Reason{pauseresume.ReasonApprovalRequired},
			Since:      base,
			Until:      base, // only the first record (recorded at base)
		},
	})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if resp.TotalRows != 1 {
		t.Fatalf("TotalRows = %d, want 1", resp.TotalRows)
	}
	if resp.Snapshots[0].Token != want {
		t.Fatalf("matched token = %q, want %q", resp.Snapshots[0].Token, want)
	}
}
