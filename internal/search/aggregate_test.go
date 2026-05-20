package search_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
)

// stubSearcher implements search.Searcher with caller-controlled rows.
// Used in aggregate tests to pin the dispatcher's merge / paginate /
// fan-out semantics without depending on any per-index Searcher.
type stubSearcher struct {
	idx    types.SearchIndex
	rows   []types.SearchResultRow
	err    error
	delay  time.Duration
	called int
	mu     sync.Mutex
}

func (s *stubSearcher) Index() types.SearchIndex { return s.idx }

func (s *stubSearcher) Search(ctx context.Context, _ types.SearchRequest) (types.SearchResponse, error) {
	s.mu.Lock()
	s.called++
	s.mu.Unlock()
	if s.delay > 0 {
		select {
		case <-time.After(s.delay):
		case <-ctx.Done():
			return types.SearchResponse{}, ctx.Err()
		}
	}
	if s.err != nil {
		return types.SearchResponse{}, s.err
	}
	out := make([]types.SearchResultRow, len(s.rows))
	copy(out, s.rows)
	return types.SearchResponse{Rows: out}, nil
}

func mkRow(idx types.SearchIndex, id string, occurred time.Time) types.SearchResultRow {
	return types.SearchResultRow{
		Index:      idx,
		ID:         id,
		TenantID:   "t1",
		UserID:     "u1",
		SessionID:  "sess-1",
		OccurredAt: occurred,
		Preview:    id,
	}
}

func callerID(tenant string) identity.Identity {
	return identity.Identity{
		TenantID:  tenant,
		UserID:    "u1",
		SessionID: "sess-1",
	}
}

func TestQuery_RejectsNilRegistry(t *testing.T) {
	t.Parallel()
	_, err := search.Query(context.Background(), nil, callerID("t1"), allowAdmin, types.SearchRequest{})
	if !errors.Is(err, search.ErrInvalidRequest) {
		t.Fatalf("Query nil registry: got err=%v, want ErrInvalidRequest", err)
	}
}

func TestQuery_RejectsMissingIdentity(t *testing.T) {
	t.Parallel()
	reg, _ := search.NewRegistry(&stubSearcher{idx: types.SearchIndexSessions})
	_, err := search.Query(context.Background(), reg, identity.Identity{}, allowAdmin, types.SearchRequest{})
	if !errors.Is(err, search.ErrIdentityRequired) {
		t.Fatalf("Query empty identity: got err=%v, want ErrIdentityRequired", err)
	}
}

func TestQuery_RejectsCrossTenantWithoutAdminScope(t *testing.T) {
	t.Parallel()
	reg, _ := search.NewRegistry(&stubSearcher{idx: types.SearchIndexSessions})
	req := types.SearchRequest{
		Filter: types.SearchFilter{TenantIDs: []string{"t1", "t2"}},
	}
	_, err := search.Query(context.Background(), reg, callerID("t1"), denyAdmin, req)
	if !errors.Is(err, search.ErrCrossTenantRequiresAdmin) {
		t.Fatalf("Query cross-tenant w/o admin: got err=%v, want ErrCrossTenantRequiresAdmin", err)
	}
}

func TestQuery_AllowsCrossTenantWithAdminScope(t *testing.T) {
	t.Parallel()
	reg, _ := search.NewRegistry(&stubSearcher{
		idx:  types.SearchIndexSessions,
		rows: []types.SearchResultRow{mkRow(types.SearchIndexSessions, "s1", time.Unix(100, 0))},
	})
	req := types.SearchRequest{
		Filter:  types.SearchFilter{TenantIDs: []string{"t1", "t2"}},
		Indexes: []types.SearchIndex{types.SearchIndexSessions},
	}
	resp, err := search.Query(context.Background(), reg, callerID("t1"), allowAdmin, req)
	if err != nil {
		t.Fatalf("Query cross-tenant w/ admin: unexpected error %v", err)
	}
	if len(resp.Rows) != 1 {
		t.Errorf("Query cross-tenant w/ admin: got %d rows, want 1", len(resp.Rows))
	}
}

func TestQuery_FansOutAndMergesAcrossIndexes(t *testing.T) {
	t.Parallel()
	now := time.Unix(1700000000, 0).UTC()
	sessionsS := &stubSearcher{
		idx:  types.SearchIndexSessions,
		rows: []types.SearchResultRow{mkRow(types.SearchIndexSessions, "s-1", now.Add(3*time.Second))},
	}
	tasksS := &stubSearcher{
		idx:  types.SearchIndexTasks,
		rows: []types.SearchResultRow{mkRow(types.SearchIndexTasks, "t-1", now.Add(2*time.Second))},
	}
	eventsS := &stubSearcher{
		idx:  types.SearchIndexEvents,
		rows: []types.SearchResultRow{mkRow(types.SearchIndexEvents, "e-1", now.Add(1*time.Second))},
	}
	artifactsS := &stubSearcher{
		idx:  types.SearchIndexArtifacts,
		rows: []types.SearchResultRow{mkRow(types.SearchIndexArtifacts, "a-1", now)},
	}
	reg, err := search.NewRegistry(sessionsS, tasksS, eventsS, artifactsS)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	req := types.SearchRequest{
		Query:   "",
		Indexes: nil, // empty = all four
	}
	resp, err := search.Query(context.Background(), reg, callerID("t1"), allowAdmin, req)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(resp.Rows) != 4 {
		t.Fatalf("Query: got %d rows, want 4", len(resp.Rows))
	}
	// Verify newest-first ordering.
	if resp.Rows[0].ID != "s-1" {
		t.Errorf("Query: newest row should be s-1, got %s", resp.Rows[0].ID)
	}
	if resp.Rows[3].ID != "a-1" {
		t.Errorf("Query: oldest row should be a-1, got %s", resp.Rows[3].ID)
	}
	// Each searcher was called exactly once.
	for _, s := range []*stubSearcher{sessionsS, tasksS, eventsS, artifactsS} {
		s.mu.Lock()
		called := s.called
		s.mu.Unlock()
		if called != 1 {
			t.Errorf("searcher %s called=%d, want 1", s.idx, called)
		}
	}
	if resp.ProtocolVersion == "" {
		t.Errorf("ProtocolVersion should be populated")
	}
}

func TestQuery_PaginatesUnion(t *testing.T) {
	t.Parallel()
	now := time.Unix(1700000000, 0).UTC()
	makeRows := func(idx types.SearchIndex, n int, prefix string) []types.SearchResultRow {
		out := make([]types.SearchResultRow, n)
		for i := 0; i < n; i++ {
			out[i] = mkRow(idx, fmt.Sprintf("%s-%d", prefix, i), now.Add(time.Duration(n-i)*time.Second))
		}
		return out
	}
	reg, _ := search.NewRegistry(
		&stubSearcher{idx: types.SearchIndexSessions, rows: makeRows(types.SearchIndexSessions, 25, "s")},
		&stubSearcher{idx: types.SearchIndexTasks, rows: makeRows(types.SearchIndexTasks, 25, "t")},
	)
	req := types.SearchRequest{
		Page:     1,
		PageSize: 20,
	}
	resp, err := search.Query(context.Background(), reg, callerID("t1"), allowAdmin, req)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if resp.TotalCount != 50 {
		t.Errorf("TotalCount: got %d, want 50", resp.TotalCount)
	}
	if len(resp.Rows) != 20 {
		t.Errorf("Page 1 rows: got %d, want 20", len(resp.Rows))
	}
	if resp.PageCount != 3 {
		t.Errorf("PageCount: got %d, want 3", resp.PageCount)
	}
	if !resp.HasMore {
		t.Error("HasMore: want true")
	}

	req.Page = 3
	resp, err = search.Query(context.Background(), reg, callerID("t1"), allowAdmin, req)
	if err != nil {
		t.Fatalf("Query page 3: %v", err)
	}
	if len(resp.Rows) != 10 {
		t.Errorf("Page 3 rows: got %d, want 10", len(resp.Rows))
	}
	if resp.HasMore {
		t.Error("HasMore on last page: want false")
	}
}

func TestQuery_GracefullyDegradesOnSoftIndexFailure(t *testing.T) {
	t.Parallel()
	now := time.Unix(1700000000, 0).UTC()
	good := &stubSearcher{
		idx:  types.SearchIndexSessions,
		rows: []types.SearchResultRow{mkRow(types.SearchIndexSessions, "s-1", now)},
	}
	bad := &stubSearcher{
		idx: types.SearchIndexTasks,
		err: errors.New("some upstream blew up"),
	}
	reg, _ := search.NewRegistry(good, bad)
	resp, err := search.Query(context.Background(), reg, callerID("t1"), allowAdmin, types.SearchRequest{})
	if err != nil {
		t.Fatalf("Query: should degrade gracefully on soft failure, got %v", err)
	}
	if len(resp.Rows) != 1 || resp.Rows[0].ID != "s-1" {
		t.Errorf("Query: got %v, want one row 's-1'", resp.Rows)
	}
}

func TestQuery_PropagatesHardIdentityErrorAsFailure(t *testing.T) {
	t.Parallel()
	bad := &stubSearcher{
		idx: types.SearchIndexSessions,
		err: search.ErrIdentityRequired,
	}
	reg, _ := search.NewRegistry(bad)
	_, err := search.Query(context.Background(), reg, callerID("t1"), allowAdmin, types.SearchRequest{})
	if !errors.Is(err, search.ErrIdentityRequired) {
		t.Fatalf("Query: hard identity error should propagate, got %v", err)
	}
}

func TestQuery_RejectsUnknownIndex(t *testing.T) {
	t.Parallel()
	reg, _ := search.NewRegistry(&stubSearcher{idx: types.SearchIndexSessions})
	req := types.SearchRequest{
		Indexes: []types.SearchIndex{types.SearchIndexSessions, "tools"}, // 'tools' is Console-side
	}
	_, err := search.Query(context.Background(), reg, callerID("t1"), allowAdmin, req)
	if !errors.Is(err, search.ErrUnknownIndex) {
		t.Fatalf("Query: unknown index should reject, got %v", err)
	}
}

func TestQuery_ConcurrentReuse_NoCrossTalk(t *testing.T) {
	// D-025 concurrent-reuse stress against the aggregate dispatcher.
	// One shared registry; N goroutines each with a distinct tenant +
	// asserting their own results come back.
	const N = 128
	now := time.Unix(1700000000, 0).UTC()

	// Each searcher echoes back a row tagged with the caller's
	// tenant from ctx so we can detect cross-talk if the dispatcher
	// (or the registry) accidentally shared state.
	echo := &echoTenantSearcher{idx: types.SearchIndexSessions}
	reg, err := search.NewRegistry(echo)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}

	runtime.GC()
	baseline := runtime.NumGoroutine()

	var wg sync.WaitGroup
	failures := make(chan string, N)
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", i),
				UserID:    "u",
				SessionID: "s",
			}
			ctx, _ := identity.With(context.Background(), id)
			resp, qerr := search.Query(ctx, reg, id, allowAdmin, types.SearchRequest{})
			if qerr != nil {
				failures <- fmt.Sprintf("g%d: %v", i, qerr)
				return
			}
			if len(resp.Rows) != 1 {
				failures <- fmt.Sprintf("g%d: got %d rows, want 1", i, len(resp.Rows))
				return
			}
			if resp.Rows[0].TenantID != id.TenantID {
				failures <- fmt.Sprintf("g%d: row tenant=%s, want %s (CROSS-TALK)", i, resp.Rows[0].TenantID, id.TenantID)
			}
			_ = now
		}()
	}
	wg.Wait()
	close(failures)
	var msgs []string
	for f := range failures {
		msgs = append(msgs, f)
	}
	if len(msgs) > 0 {
		sort.Strings(msgs)
		t.Fatalf("concurrent-reuse failures (%d):\n  %s", len(msgs), msgs)
	}

	// Allow lingering ctx-cancel goroutines from the WithTimeout
	// inside Query to wind down before snapshotting.
	time.Sleep(100 * time.Millisecond)
	runtime.GC()
	after := runtime.NumGoroutine()
	if after > baseline+5 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, after)
	}
}

// echoTenantSearcher returns one row whose tenant is read from the
// caller's ctx — the D-025 witness for "no per-call state on the
// shared artifact."
type echoTenantSearcher struct {
	idx types.SearchIndex
}

func (e *echoTenantSearcher) Index() types.SearchIndex { return e.idx }
func (e *echoTenantSearcher) Search(ctx context.Context, _ types.SearchRequest) (types.SearchResponse, error) {
	id, ok := identity.From(ctx)
	if !ok {
		return types.SearchResponse{}, search.ErrIdentityRequired
	}
	return types.SearchResponse{
		Rows: []types.SearchResultRow{
			{
				Index:      e.idx,
				ID:         id.TenantID + "/r",
				TenantID:   id.TenantID,
				UserID:     id.UserID,
				SessionID:  id.SessionID,
				OccurredAt: time.Now().UTC(),
			},
		},
	}, nil
}
