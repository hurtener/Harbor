package protocol

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions"
)

type catalogLister struct {
	snapshots []sessions.SessionSnapshot
}

func (l catalogLister) ListSnapshots(ctx context.Context, f sessions.SessionListFilter) ([]sessions.SessionSnapshot, error) {
	out := make([]sessions.SessionSnapshot, 0, len(l.snapshots))
	for _, snap := range l.snapshots {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !catalogFilterAllows(f.TenantIDs, snap.Identity.TenantID) ||
			!catalogFilterAllows(f.UserIDs, snap.Identity.UserID) ||
			!catalogFilterAllows(f.SessionIDs, snap.ID) {
			continue
		}
		out = append(out, snap)
	}
	return out, nil
}

func catalogFilterAllows(values []string, candidate string) bool {
	for _, value := range values {
		if value == candidate {
			return true
		}
	}
	return len(values) == 0
}

type countingEnricher struct {
	calls atomic.Int64
}

func (e *countingEnricher) Counters(_ context.Context, _ identity.Identity, _ string) SessionCounters {
	e.calls.Add(1)
	return SessionCounters{}
}

type zeroEnricher struct{}

func (zeroEnricher) Counters(context.Context, identity.Identity, string) SessionCounters {
	return SessionCounters{}
}

type blockingEnricher struct {
	active  atomic.Int64
	peak    atomic.Int64
	started chan struct{}
	release <-chan struct{}
}

type cancelBlockingEnricher struct {
	calls      atomic.Int64
	active     atomic.Int64
	started    chan struct{}
	cancelSeen chan struct{}
	release    <-chan struct{}
}

func (e *cancelBlockingEnricher) Counters(ctx context.Context, _ identity.Identity, _ string) SessionCounters {
	e.calls.Add(1)
	e.active.Add(1)
	e.started <- struct{}{}
	<-ctx.Done()
	e.cancelSeen <- struct{}{}
	<-e.release
	e.active.Add(-1)
	return SessionCounters{Partial: true}
}

func (e *blockingEnricher) Counters(_ context.Context, _ identity.Identity, _ string) SessionCounters {
	active := e.active.Add(1)
	for {
		peak := e.peak.Load()
		if active <= peak || e.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	e.started <- struct{}{}
	<-e.release
	e.active.Add(-1)
	return SessionCounters{}
}

// TestService_List_LastActivityPage_EnrichesOnlyReturnedRows is the hosted
// catalog regression: a lifecycle-only filter and last-activity sort must
// page before counter enrichment. The legacy full-projection calculation is
// retained as the output oracle so the fast path cannot change ordering,
// cursor, or truncation semantics while reducing 500 matching-session counter
// scans to 50.
func TestService_List_LastActivityPage_EnrichesOnlyReturnedRows(t *testing.T) {
	base := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	snapshots := make([]sessions.SessionSnapshot, 0, 1_000)
	for i := range 1_000 {
		sessionID := fmt.Sprintf("session-%03d", i)
		tenantID, userID := "tenant-1", "user-1"
		if i%2 != 0 {
			tenantID, userID = "tenant-other", "user-other"
		}
		snapshots = append(snapshots, sessions.SessionSnapshot{
			Session: sessions.Session{
				ID:       sessionID,
				Identity: identity.Identity{TenantID: tenantID, UserID: userID, SessionID: sessionID},
				OpenedAt: base.Add(time.Duration(i) * time.Minute),
				LastSeen: base.Add(time.Duration(i) * time.Minute),
			},
			Running: true,
		})
	}
	req := prototypes.SessionsListRequest{
		Identity: prototypes.IdentityScope{Tenant: "tenant-1", User: "user-1", Session: "request-session"},
		Filter:   prototypes.SessionFilter{Statuses: []prototypes.SessionStatus{prototypes.SessionStatusRunning}},
		Sort:     prototypes.SessionSortLastActivityDesc,
		Limit:    50,
	}

	legacy, err := NewListerProjector(catalogLister{snapshots: snapshots}, WithEnricher(zeroEnricher{}))
	if err != nil {
		t.Fatalf("NewListerProjector legacy: %v", err)
	}
	caller := identity.Identity{TenantID: "tenant-1", UserID: "user-1", SessionID: "request-session"}
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
	start, end, expectedTruncated, expectedCursor := pageBounds(filtered, nil, req.Sort, req.Limit)
	expected := filtered[start:end]

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
	if got := enricher.calls.Load(); got != int64(req.Limit) {
		t.Fatalf("counter enrichments = %d, want %d returned rows (not 500 matching catalog rows)", got, req.Limit)
	}
	if resp.Truncated != expectedTruncated || resp.NextCursor != expectedCursor || len(resp.Rows) != len(expected) {
		t.Fatalf("fast page metadata = rows=%d truncated=%v cursor=%q; legacy = rows=%d truncated=%v cursor=%q", len(resp.Rows), resp.Truncated, resp.NextCursor, len(expected), expectedTruncated, expectedCursor)
	}
	for i := range expected {
		if resp.Rows[i].SessionID != expected[i].SessionID {
			t.Fatalf("row %d session_id = %q, want legacy %q", i, resp.Rows[i].SessionID, expected[i].SessionID)
		}
		if resp.Rows[i].TenantID != "tenant-1" || resp.Rows[i].UserID != "user-1" {
			t.Fatalf("row %d leaked %q/%q outside the caller's tenant/user scope", i, resp.Rows[i].TenantID, resp.Rows[i].UserID)
		}
	}

	req.Cursor = resp.NextCursor
	resp2, err := svc.List(context.Background(), req, false)
	if err != nil {
		t.Fatalf("page 2 List: %v", err)
	}
	if len(resp2.Rows) != req.Limit || enricher.calls.Load() != int64(2*req.Limit) {
		t.Fatalf("page 2 enriched %d total rows for %d returned rows per page, want %d", enricher.calls.Load(), req.Limit, 2*req.Limit)
	}
}

// TestListerProjector_ListSessions_CounterEnrichmentBounded pins the fallback
// path: a counter filter or cost sort cannot page before enrichment, but a
// large catalog still has at most maxCounterEnrichmentWorkers scans in flight.
func TestListerProjector_ListSessions_CounterEnrichmentBounded(t *testing.T) {
	snapshots := make([]sessions.SessionSnapshot, 0, maxCounterEnrichmentWorkers+2)
	base := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	for i := range cap(snapshots) {
		sessionID := fmt.Sprintf("bounded-%d", i)
		snapshots = append(snapshots, sessions.SessionSnapshot{Session: sessions.Session{
			ID:       sessionID,
			Identity: identity.Identity{TenantID: "tenant-1", UserID: "user-1", SessionID: sessionID},
			OpenedAt: base,
			LastSeen: base.Add(time.Duration(i) * time.Minute),
		}})
	}
	release := make(chan struct{})
	enricher := &blockingEnricher{started: make(chan struct{}, maxCounterEnrichmentWorkers), release: release}
	projector, err := NewListerProjector(catalogLister{snapshots: snapshots}, WithEnricher(enricher))
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, listErr := projector.ListSessions(context.Background(), identity.Identity{TenantID: "tenant-1", UserID: "user-1", SessionID: "request-session"}, prototypes.SessionFilter{}, false)
		done <- listErr
	}()
	for range maxCounterEnrichmentWorkers {
		<-enricher.started
	}
	if got := enricher.peak.Load(); got != int64(maxCounterEnrichmentWorkers) {
		t.Fatalf("simultaneous counter enrichments = %d, want worker bound %d", got, maxCounterEnrichmentWorkers)
	}
	close(release)
	if listErr := <-done; listErr != nil {
		t.Fatalf("ListSessions: %v", listErr)
	}
}

// TestListerProjector_ListSessions_CancellationStopsDispatchAndJoinsWorkers
// pins the full-candidate fallback cancellation contract. Cancellation must
// stop dispatching untouched catalog rows, wait for in-flight counter reads to
// exit, and return context.Canceled instead of successful partial counters.
func TestListerProjector_ListSessions_CancellationStopsDispatchAndJoinsWorkers(t *testing.T) {
	snapshots := cancellationSnapshots(maxCounterEnrichmentWorkers + 20)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWorkers := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseWorkers)
	enricher := &cancelBlockingEnricher{
		started:    make(chan struct{}, maxCounterEnrichmentWorkers),
		cancelSeen: make(chan struct{}, maxCounterEnrichmentWorkers),
		release:    release,
	}
	projector, err := NewListerProjector(catalogLister{snapshots: snapshots}, WithEnricher(enricher))
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, listErr := projector.ListSessions(ctx, identity.Identity{TenantID: "tenant-1", UserID: "user-1", SessionID: "request-session"}, prototypes.SessionFilter{}, false)
		done <- listErr
	}()
	waitForEnrichmentSignals(t, enricher.started, "worker starts")
	cancel()
	waitForEnrichmentSignals(t, enricher.cancelSeen, "worker cancellation")
	assertListStillJoining(t, done)
	releaseWorkers()
	assertCanceledListResult(t, done)
	if got := enricher.calls.Load(); got != int64(maxCounterEnrichmentWorkers) {
		t.Fatalf("counter enrichments after cancellation = %d, want only %d in-flight workers", got, maxCounterEnrichmentWorkers)
	}
	if got := enricher.active.Load(); got != 0 {
		t.Fatalf("active counter enrichments after return = %d, want 0 joined workers", got)
	}
}

// TestService_List_PageEnrichmentCancellationJoinsWorkers applies the same
// cancellation guarantee to lifecycle-only requests after paging. Only the
// returned page is eligible for enrichment, and no partial page may escape.
func TestService_List_PageEnrichmentCancellationJoinsWorkers(t *testing.T) {
	snapshots := cancellationSnapshots(200)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWorkers := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseWorkers)
	enricher := &cancelBlockingEnricher{
		started:    make(chan struct{}, maxCounterEnrichmentWorkers),
		cancelSeen: make(chan struct{}, maxCounterEnrichmentWorkers),
		release:    release,
	}
	projector, err := NewListerProjector(catalogLister{snapshots: snapshots}, WithEnricher(enricher))
	if err != nil {
		t.Fatalf("NewListerProjector: %v", err)
	}
	svc, err := NewService(projector)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, listErr := svc.List(ctx, prototypes.SessionsListRequest{
			Identity: prototypes.IdentityScope{Tenant: "tenant-1", User: "user-1", Session: "request-session"},
			Sort:     prototypes.SessionSortLastActivityDesc,
			Limit:    50,
		}, false)
		done <- listErr
	}()
	waitForEnrichmentSignals(t, enricher.started, "page worker starts")
	cancel()
	waitForEnrichmentSignals(t, enricher.cancelSeen, "page worker cancellation")
	assertListStillJoining(t, done)
	releaseWorkers()
	assertCanceledListResult(t, done)
	if got := enricher.calls.Load(); got != int64(maxCounterEnrichmentWorkers) {
		t.Fatalf("page counter enrichments after cancellation = %d, want only %d in-flight workers", got, maxCounterEnrichmentWorkers)
	}
	if got := enricher.active.Load(); got != 0 {
		t.Fatalf("active page counter enrichments after return = %d, want 0 joined workers", got)
	}
}

func cancellationSnapshots(count int) []sessions.SessionSnapshot {
	base := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	snapshots := make([]sessions.SessionSnapshot, 0, count)
	for i := range count {
		sessionID := fmt.Sprintf("cancel-%03d", i)
		snapshots = append(snapshots, sessions.SessionSnapshot{Session: sessions.Session{
			ID:       sessionID,
			Identity: identity.Identity{TenantID: "tenant-1", UserID: "user-1", SessionID: sessionID},
			OpenedAt: base,
			LastSeen: base.Add(time.Duration(i) * time.Minute),
		}})
	}
	return snapshots
}

func waitForEnrichmentSignals(t *testing.T, signals <-chan struct{}, label string) {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for range maxCounterEnrichmentWorkers {
		select {
		case <-signals:
		case <-timer.C:
			t.Fatalf("timed out waiting for %s", label)
		}
	}
}

func assertListStillJoining(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("list returned %v before canceled enrichment workers exited", err)
	default:
	}
}

func assertCanceledListResult(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("list error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for canceled list to join workers")
	}
}
