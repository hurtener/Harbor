// Package protocol_test exercises the observability.query domain service
// against the real in-memory rollup driver and a real rollup projector —
// the corrected rollup core's own artifacts. The service's public surface
// is the only thing under test: authority / isolation, the closed
// contract, the mandatory freshness block, and the widened-read audit.
package protocol_test

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/observability/protocol"
	"github.com/hurtener/Harbor/internal/observability/rollups"
	"github.com/hurtener/Harbor/internal/observability/rollups/memstore"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
)

// Fixed reference hour for every fixture window — aligned to the fixed UTC
// hour grid so windows are valid at BucketHour.
var refHour = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

// caller is the canonical ordinary caller for the authority tests.
var caller = identity.Identity{TenantID: "tenant-a", UserID: "user-A", SessionID: "session-s1"}

// foreignRows are the identity axes an ordinary caller must never see:
// another user in the caller's tenant, and another tenant entirely.
var foreignRows = []struct {
	tenant, user, session, model string
	bucket                       time.Time
	costMicros                   int64
}{
	{"tenant-a", "user-B", "session-s2", "gpt-x", refHour, 999},
	{"tenant-b", "user-C", "session-s3", "gpt-y", refHour, 7777},
}

// fleetScope is the production-shaped admin|console:fleet predicate: it
// reads the verified scope set from the request context (the closed
// two-scope admit set — never a request-body value).
func fleetScope(ctx context.Context) bool {
	return protocolauth.HasScope(ctx, protocolauth.ScopeAdmin) ||
		protocolauth.HasScope(ctx, protocolauth.ScopeConsoleFleet)
}

// scopedCtx attaches the verified identity and (optionally) the verified
// scope set to a fresh context, exactly as the transport's auth middleware
// does for a real request.
func scopedCtx(t *testing.T, id identity.Identity, scopes ...protocolauth.Scope) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(context.Background(), id)
	if err != nil {
		t.Fatalf("WithVerified(%+v): %v", id, err)
	}
	if len(scopes) > 0 {
		ctx = protocolauth.WithScopes(ctx, scopes)
	}
	return ctx
}

// auditRecorder is the concurrency-safe AuditSink used across the tests.
type auditRecorder struct {
	mu     sync.Mutex
	events []events.Event
	err    error
}

func (r *auditRecorder) publish(ctx context.Context, ev events.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.events = append(r.events, ev)
	return nil
}

func (r *auditRecorder) snapshot() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.Event(nil), r.events...)
}

// testQuality is the controllable QualitySource: state / watermarkAt are
// scripted by the test; watermark and retention come from the store the
// Querier reads, so the freshness block always describes the rows being
// served.
type testQuality struct {
	mu    sync.Mutex
	store *memstore.Store
	state rollups.State
	wmAt  time.Time
	fail  error
}

func (q *testQuality) Quality(ctx context.Context) (rollups.Quality, error) {
	if q.fail != nil {
		return rollups.Quality{}, q.fail
	}
	ckpt, err := q.store.Checkpoint(ctx)
	if err != nil {
		return rollups.Quality{}, err
	}
	oldest, newest, err := q.store.Retention(ctx)
	if err != nil {
		return rollups.Quality{}, err
	}
	q.mu.Lock()
	state, wmAt := q.state, q.wmAt
	q.mu.Unlock()
	return rollups.Quality{
		State:          state,
		Watermark:      ckpt,
		WatermarkAt:    wmAt,
		RetentionStart: oldest,
		RetentionEnd:   newest,
	}, nil
}

func (q *testQuality) setState(s rollups.State) {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.state = s
}

// harness wires a seeded memstore, a scripted quality source, an audit
// recorder, and one shared Service.
type harness struct {
	store   *memstore.Store
	quality *testQuality
	rec     *auditRecorder
	svc     *protocol.Service
}

func newHarness(t *testing.T, state rollups.State) *harness {
	t.Helper()
	store := memstore.New()
	q := &testQuality{store: store, state: state}
	rec := &auditRecorder{}
	svc, err := protocol.NewService(store, q, fleetScope, rec.publish, patterns.New(),
		protocol.WithLogger(slog.New(slog.DiscardHandler)))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return &harness{store: store, quality: q, rec: rec, svc: svc}
}

// seed applies one delta batch with the given checkpoint.
func seed(t *testing.T, store *memstore.Store, checkpoint uint64, key rollups.Key, add rollups.MeasureSet) {
	t.Helper()
	if err := store.ApplyBatch(context.Background(), rollups.Batch{
		Checkpoint: checkpoint,
		Deltas:     []rollups.Delta{{Key: key, Add: add}},
	}); err != nil {
		t.Fatalf("ApplyBatch(ckpt=%d): %v", checkpoint, err)
	}
}

// seedStandardRows seeds the canonical fixture: two rows for the caller's
// own triple (two models across two buckets) plus the foreign rows.
func seedStandardRows(t *testing.T, h *harness) {
	t.Helper()
	seed(t, h.store, 1, rollups.Key{
		BucketStart: refHour,
		TenantID:    caller.TenantID, UserID: caller.UserID, SessionID: caller.SessionID,
		Model: "gpt-x",
	}, rollups.MeasureSet{
		LLMCompletions:  1,
		LLMTokensPrompt: 100, LLMTokensCompletion: 50, LLMTokensTotal: 150,
		LLMCostMicros: 123456,
	})
	seed(t, h.store, 2, rollups.Key{
		BucketStart: refHour.Add(time.Hour),
		TenantID:    caller.TenantID, UserID: caller.UserID, SessionID: caller.SessionID,
		Model: "gpt-y",
	}, rollups.MeasureSet{TasksCompleted: 3})
	for i, fr := range foreignRows {
		seed(t, h.store, uint64(3+i), rollups.Key{
			BucketStart: fr.bucket,
			TenantID:    fr.tenant, UserID: fr.user, SessionID: fr.session,
			Model: fr.model,
		}, rollups.MeasureSet{LLMCompletions: 1, LLMCostMicros: fr.costMicros})
	}
}

// baseRequest is the canonical valid request over the standard fixture.
func baseRequest() protocol.Request {
	return protocol.Request{
		From:     refHour,
		To:       refHour.Add(3 * time.Hour),
		Bucket:   rollups.BucketHour,
		Measures: []rollups.Measure{rollups.MeasureLLMCostMicros, rollups.MeasureLLMCompletions},
		Limit:    100,
	}
}

// sum returns the accumulated N of one measure across the response rows.
func sum(t *testing.T, resp protocol.Response, m rollups.Measure) int64 {
	t.Helper()
	var total int64
	for _, r := range resp.Rows {
		v, ok := r.Measures[m]
		if !ok {
			t.Fatalf("response row %+v lacks measure %q (measures: %v)", r, m, r.Measures)
		}
		total += v.N
	}
	return total
}

func TestQuery_OrdinaryCallerForcedToOwnTriple(t *testing.T) {
	h := newHarness(t, rollups.StateCurrent)
	seedStandardRows(t, h)

	// An ordinary caller with EMPTY filters must see only their own
	// triple's rows: cost 123456 micros, never the foreign 999 or 7777.
	resp, err := h.svc.Query(scopedCtx(t, caller), baseRequest())
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := sum(t, resp, rollups.MeasureLLMCostMicros); got != 123456 {
		t.Fatalf("ordinary caller cost = %d, want 123456 (foreign rows leaked)", got)
	}
	if len(resp.Rows) == 0 {
		t.Fatal("expected rows for the caller's own triple")
	}
	if len(h.rec.snapshot()) != 0 {
		t.Fatalf("ordinary own-scope read emitted %d audit events, want 0",
			len(h.rec.snapshot()))
	}
}

func TestQuery_OrdinaryCallerGroupByNeverLeaksForeignAxisValues(t *testing.T) {
	h := newHarness(t, rollups.StateCurrent)
	seedStandardRows(t, h)

	// Grouping by user / session / tenant must surface ONLY the caller's
	// own values — enumerating other principals through group_by is the
	// same disclosure as enumerating them through filters.
	for _, g := range [][]rollups.Dimension{
		{rollups.DimensionUser},
		{rollups.DimensionSession},
		{rollups.DimensionTenant},
	} {
		req := baseRequest()
		req.GroupBy = g
		resp, err := h.svc.Query(scopedCtx(t, caller), req)
		if err != nil {
			t.Fatalf("Query group_by %v: %v", g, err)
		}
		for _, r := range resp.Rows {
			for _, d := range g {
				got := r.Dimensions[d]
				want := caller.TenantID
				if d == rollups.DimensionUser {
					want = caller.UserID
				}
				if d == rollups.DimensionSession {
					want = caller.SessionID
				}
				if got != want {
					t.Fatalf("group_by %v leaked foreign axis value %q, want %q", g, got, want)
				}
			}
		}
	}
}

func TestQuery_OrdinaryCallerNamingForeignIdentityFailsClosed(t *testing.T) {
	h := newHarness(t, rollups.StateCurrent)
	seedStandardRows(t, h)

	cases := []struct {
		name    string
		mutate  func(*protocol.Request)
		wantErr error
	}{
		{"foreign tenant", func(r *protocol.Request) {
			r.Filters.TenantIDs = []string{"tenant-b"}
		}, protocol.ErrCrossTenantScope},
		{"foreign user", func(r *protocol.Request) {
			r.Filters.UserIDs = []string{"user-B"}
		}, protocol.ErrCrossUserScope},
		{"foreign session", func(r *protocol.Request) {
			r.Filters.SessionIDs = []string{"session-s2"}
		}, protocol.ErrCrossSessionScope},
		{"own tenant + foreign user", func(r *protocol.Request) {
			r.Filters.TenantIDs = []string{"tenant-a"}
			r.Filters.UserIDs = []string{"user-B"}
		}, protocol.ErrCrossUserScope},
		{"own user + sibling session", func(r *protocol.Request) {
			r.Filters.UserIDs = []string{"user-A"}
			r.Filters.SessionIDs = []string{"session-s1", "session-other"}
		}, protocol.ErrCrossSessionScope},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := baseRequest()
			tc.mutate(&req)
			_, err := h.svc.Query(scopedCtx(t, caller), req)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Query error = %v, want %v", err, tc.wantErr)
			}
			if n := len(h.rec.snapshot()); n != 0 {
				t.Fatalf("refused request emitted %d audit events, want 0", n)
			}
		})
	}

	// The caller's OWN values in the filters are accepted — the effective
	// filter is still exactly their triple.
	req := baseRequest()
	req.Filters = protocol.Filters{
		TenantIDs:  []string{"tenant-a"},
		UserIDs:    []string{"user-A"},
		SessionIDs: []string{"session-s1"},
		Models:     []string{"gpt-x"},
	}
	resp, err := h.svc.Query(scopedCtx(t, caller), req)
	if err != nil {
		t.Fatalf("own-scope filter Query: %v", err)
	}
	if got := sum(t, resp, rollups.MeasureLLMCostMicros); got != 123456 {
		t.Fatalf("own-scope filtered cost = %d, want 123456", got)
	}
}

func TestQuery_MissingVerifiedIdentityFailsClosed(t *testing.T) {
	h := newHarness(t, rollups.StateCurrent)
	seedStandardRows(t, h)

	// No verified identity on ctx: the request body cannot supply one, so
	// the service fails closed — identity is mandatory.
	_, err := h.svc.Query(context.Background(), baseRequest())
	if !errors.Is(err, protocol.ErrIdentityRequired) {
		t.Fatalf("Query error = %v, want ErrIdentityRequired", err)
	}
}

func TestQuery_ElevatedCallerWildcardFanInAuditedExactlyOnce(t *testing.T) {
	h := newHarness(t, rollups.StateCurrent)
	seedStandardRows(t, h)

	// Admin caller, own tenant (folded), user axis elided → wildcard
	// fan-in within the tenant: sees user-A AND user-B rows, but never the
	// tenant-b row. Exactly ONE audit.admin_scope_used precedes the read.
	admin := scopedCtx(t, caller, protocolauth.ScopeAdmin)
	req := baseRequest()
	req.Filters = protocol.Filters{} // elided user = fan-in
	resp, err := h.svc.Query(admin, req)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := sum(t, resp, rollups.MeasureLLMCostMicros); got != 123456+999 {
		t.Fatalf("fleet fan-in cost = %d, want %d (123456 + 999)", got, 123456+999)
	}

	got := h.rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("audit events = %d, want exactly 1: %+v", len(got), got)
	}
	ev := got[0]
	if ev.Type != events.EventTypeAdminScopeUsed {
		t.Fatalf("audit event type = %q, want %q", ev.Type, events.EventTypeAdminScopeUsed)
	}
	if ev.Identity.Identity != caller {
		t.Fatalf("audit envelope identity = %+v, want the verified actor %+v", ev.Identity.Identity, caller)
	}
	payload, ok := ev.Payload.(events.AdminScopeUsedPayload)
	if !ok {
		t.Fatalf("audit payload = %T, want AdminScopeUsedPayload", ev.Payload)
	}
	// Tenant elision is the fold (own tenant); user/session elision is the
	// canonical wildcard spelling.
	if payload.Tenant != caller.TenantID || payload.User != "" || payload.Session != "" {
		t.Fatalf("audit payload = %+v, want tenant=%q user=\"\" session=\"\"", payload, caller.TenantID)
	}
}

func TestQuery_ElevatedCallerCrossTenantAudited(t *testing.T) {
	h := newHarness(t, rollups.StateCurrent)
	seedStandardRows(t, h)

	admin := scopedCtx(t, caller, protocolauth.ScopeAdmin)
	req := baseRequest()
	req.Filters.TenantIDs = []string{"tenant-b"}
	resp, err := h.svc.Query(admin, req)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := sum(t, resp, rollups.MeasureLLMCostMicros); got != 7777 {
		t.Fatalf("cross-tenant cost = %d, want 7777", got)
	}
	got := h.rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("audit events = %d, want exactly 1", len(got))
	}
	payload, ok := got[0].Payload.(events.AdminScopeUsedPayload)
	if !ok {
		t.Fatalf("audit payload = %T, want AdminScopeUsedPayload", got[0].Payload)
	}
	if payload.Tenant != "tenant-b" {
		t.Fatalf("audit target tenant = %q, want tenant-b", payload.Tenant)
	}
}

func TestQuery_ConsoleFleetScopeWidensLikeAdmin(t *testing.T) {
	h := newHarness(t, rollups.StateCurrent)
	seedStandardRows(t, h)

	fleet := scopedCtx(t, caller, protocolauth.ScopeConsoleFleet)
	req := baseRequest()
	resp, err := h.svc.Query(fleet, req) // elided user = fleet fan-in
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := sum(t, resp, rollups.MeasureLLMCostMicros); got != 123456+999 {
		t.Fatalf("fleet fan-in cost = %d, want %d", got, 123456+999)
	}
	if n := len(h.rec.snapshot()); n != 1 {
		t.Fatalf("fleet fan-in audit events = %d, want exactly 1", n)
	}
}

func TestQuery_ElevatedCallerOwnScopeNotAudited(t *testing.T) {
	h := newHarness(t, rollups.StateCurrent)
	seedStandardRows(t, h)

	admin := scopedCtx(t, caller, protocolauth.ScopeAdmin)
	req := baseRequest()
	req.Filters = protocol.Filters{
		TenantIDs:  []string{caller.TenantID},
		UserIDs:    []string{caller.UserID},
		SessionIDs: []string{caller.SessionID},
	}
	resp, err := h.svc.Query(admin, req)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := sum(t, resp, rollups.MeasureLLMCostMicros); got != 123456 {
		t.Fatalf("own-scope admin cost = %d, want 123456", got)
	}
	if n := len(h.rec.snapshot()); n != 0 {
		t.Fatalf("own-scope elevated read emitted %d audit events, want 0", n)
	}
}

func TestQuery_AuditEmitFailureFailsReadLoud(t *testing.T) {
	h := newHarness(t, rollups.StateCurrent)
	seedStandardRows(t, h)

	sinkErr := errors.New("bus down")
	h.rec.err = sinkErr
	admin := scopedCtx(t, caller, protocolauth.ScopeAdmin)
	req := baseRequest() // elided user = widening, must audit
	_, err := h.svc.Query(admin, req)
	if !errors.Is(err, protocol.ErrAuditFailed) || !errors.Is(err, sinkErr) {
		t.Fatalf("Query error = %v, want ErrAuditFailed wrapping %v", err, sinkErr)
	}
}

func TestQuery_RedactorRefusalFailsReadLoud(t *testing.T) {
	h := newHarness(t, rollups.StateCurrent)
	seedStandardRows(t, h)

	// A refusing redactor cannot be swapped post-construction (the
	// Service is immutable), so build a second service over the same
	// store with a redactor that always errors.
	store := h.store
	q := &testQuality{store: store, state: rollups.StateCurrent}
	rec := &auditRecorder{}
	refusing := redactorFunc(func(context.Context, any) (any, error) {
		return nil, errors.New("redact refused")
	})
	svc, err := protocol.NewService(store, q, fleetScope, rec.publish, refusing)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	admin := scopedCtx(t, caller, protocolauth.ScopeAdmin)
	_, err = svc.Query(admin, baseRequest())
	if !errors.Is(err, protocol.ErrAuditFailed) {
		t.Fatalf("Query error = %v, want ErrAuditFailed", err)
	}
	if n := len(rec.snapshot()); n != 0 {
		t.Fatalf("refused redaction still emitted %d events, want 0", n)
	}
}

// redactorFunc adapts a function to the audit.Redactor seam.
type redactorFunc func(context.Context, any) (any, error)

func (f redactorFunc) Redact(ctx context.Context, payload any) (any, error) {
	return f(ctx, payload)
}

func TestQuery_ClosedContractValidation(t *testing.T) {
	h := newHarness(t, rollups.StateCurrent)
	seedStandardRows(t, h)
	ctx := scopedCtx(t, caller)

	cases := []struct {
		name    string
		mutate  func(*protocol.Request)
		wantErr error
	}{
		{"missing window", func(r *protocol.Request) {
			r.From, r.To = time.Time{}, time.Time{}
		}, protocol.ErrInvalidRequest},
		{"misaligned window edge", func(r *protocol.Request) {
			r.From = refHour.Add(30 * time.Second)
		}, protocol.ErrInvalidRequest},
		{"closed group_by rejects agent", func(r *protocol.Request) {
			r.GroupBy = []rollups.Dimension{"agent"}
		}, protocol.ErrInvalidRequest},
		{"unsupported measure fails typed", func(r *protocol.Request) {
			r.Measures = []rollups.Measure{"attempts"}
		}, protocol.ErrInvalidRequest},
		{"unsupported measure never inferred zero", func(r *protocol.Request) {
			r.Measures = []rollups.Measure{rollups.MeasureLLMCostMicros, "user_messages"}
		}, protocol.ErrInvalidRequest},
		{"unknown bucket", func(r *protocol.Request) {
			r.Bucket = "week"
		}, protocol.ErrInvalidRequest},
		{"empty measures", func(r *protocol.Request) {
			r.Measures = nil
		}, protocol.ErrInvalidRequest},
		{"unknown sort", func(r *protocol.Request) {
			r.Sort = "by_magic"
		}, protocol.ErrInvalidRequest},
		{"measure sort without selected measure", func(r *protocol.Request) {
			r.Sort = rollups.SortKeyMeasureDesc
			r.SortMeasure = rollups.MeasureTasksCompleted // not among selected measures
		}, protocol.ErrInvalidRequest},
		{"missing limit", func(r *protocol.Request) {
			r.Limit = 0
		}, protocol.ErrInvalidRequest},
		{"page limit over budget", func(r *protocol.Request) {
			r.Limit = rollups.MaxRowsPerQuery + 1
		}, protocol.ErrBudgetExceeded},
		{"window over bucket budget", func(r *protocol.Request) {
			r.Bucket = rollups.BucketMinute
			r.To = refHour.Add(3 * 24 * time.Hour) // 4320 minute buckets > MaxBuckets
		}, protocol.ErrBudgetExceeded},
		{"malformed cursor", func(r *protocol.Request) {
			r.Cursor = "not-a-real-cursor"
		}, protocol.ErrBadCursor},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := baseRequest()
			tc.mutate(&req)
			_, err := h.svc.Query(ctx, req)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Query error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestQuery_CursorBoundToQueryShapeAndIdentity(t *testing.T) {
	h := newHarness(t, rollups.StateCurrent)

	// Seed nine distinct rows for the caller's triple — three models per
	// hour across three buckets — each with a unique cost so the
	// measure-descending order is total and paging is observable.
	models := []string{"m-a", "m-b", "m-c"}
	ckpt := uint64(0)
	for i := 0; i < 3; i++ {
		for j, m := range models {
			ckpt++
			seed(t, h.store, ckpt, rollups.Key{
				BucketStart: refHour.Add(time.Duration(i) * time.Hour),
				TenantID:    caller.TenantID, UserID: caller.UserID, SessionID: caller.SessionID,
				Model: m,
			}, rollups.MeasureSet{LLMCompletions: 1, LLMCostMicros: int64(i*1000 + j + 1)})
		}
	}

	req := baseRequest()
	req.GroupBy = []rollups.Dimension{rollups.DimensionModel}
	req.Sort = rollups.SortKeyMeasureDesc
	req.SortMeasure = rollups.MeasureLLMCostMicros
	req.Limit = 2

	ctx := scopedCtx(t, caller)
	var seen []int64
	cursor := ""
	for page := 0; page < 10; page++ {
		req.Cursor = cursor
		resp, err := h.svc.Query(ctx, req)
		if err != nil {
			t.Fatalf("Query page %d: %v", page, err)
		}
		if page == 0 && len(resp.Rows) == 0 {
			t.Fatal("expected paged rows")
		}
		for _, r := range resp.Rows {
			seen = append(seen, r.Measures[rollups.MeasureLLMCostMicros].N)
		}
		if resp.NextCursor == "" {
			break
		}
		cursor = resp.NextCursor
	}
	if len(seen) != 9 {
		t.Fatalf("paged rows = %d, want 9 (no skip / no repeat)", len(seen))
	}
	// measure_desc total order: descending cost, no duplicates.
	for i := 1; i < len(seen); i++ {
		if seen[i-1] <= seen[i] {
			t.Fatalf("page order not strictly descending: %v", seen)
		}
	}

	// The last cursor is bound to the shape: a different window, a
	// different identity scope, or a different measure set must be
	// rejected with ErrBadCursor — never silently re-paged.
	reshaped := req
	reshaped.Cursor = cursor
	reshaped.From = reshaped.From.Add(time.Hour)
	reshaped.To = reshaped.To.Add(time.Hour)
	if _, err := h.svc.Query(ctx, reshaped); !errors.Is(err, protocol.ErrBadCursor) {
		t.Fatalf("cursor across a different window: error = %v, want ErrBadCursor", err)
	}
	remeasured := req
	remeasured.Cursor = cursor
	remeasured.Measures = []rollups.Measure{rollups.MeasureLLMTokensTotal}
	remeasured.SortMeasure = rollups.MeasureLLMTokensTotal
	if _, err := h.svc.Query(ctx, remeasured); !errors.Is(err, protocol.ErrBadCursor) {
		t.Fatalf("cursor across a different measure set: error = %v, want ErrBadCursor", err)
	}
	otherCaller := identity.Identity{TenantID: "tenant-a", UserID: "user-A", SessionID: "session-other"}
	// NOTE: the foreign-session filter would be rejected by the identity
	// fold before the cursor check for an ordinary caller; use an elevated
	// caller in the same tenant reading the same session set to prove the
	// cursor itself cannot be re-purposed across identities.
	adminCtx := scopedCtx(t, otherCaller, protocolauth.ScopeAdmin)
	adminReq := baseRequest()
	adminReq.Filters.SessionIDs = []string{caller.SessionID}
	adminReq.GroupBy = req.GroupBy
	adminReq.Sort = req.Sort
	adminReq.SortMeasure = req.SortMeasure
	adminReq.Limit = 2
	adminReq.Cursor = cursor
	if _, err := h.svc.Query(adminCtx, adminReq); !errors.Is(err, protocol.ErrBadCursor) {
		t.Fatalf("cursor reused under a different identity scope: error = %v, want ErrBadCursor", err)
	}
}

func TestQuery_ExactIntegerDecimalValues(t *testing.T) {
	h := newHarness(t, rollups.StateCurrent)
	seedStandardRows(t, h)

	req := baseRequest()
	req.Measures = []rollups.Measure{
		rollups.MeasureLLMCostMicros,
		rollups.MeasureLLMTokensTotal,
		rollups.MeasureTasksCompleted,
	}
	resp, err := h.svc.Query(scopedCtx(t, caller), req)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	var micros int64
	var scale uint32
	for _, r := range resp.Rows {
		v := r.Measures[rollups.MeasureLLMCostMicros]
		micros += v.N
		scale = v.Scale
	}
	if micros != 123456 {
		t.Fatalf("exact cost micros = %d, want 123456 (integer, never float-normalised)", micros)
	}
	if scale != rollups.CostScaleMicros {
		t.Fatalf("cost scale = %d, want %d", scale, rollups.CostScaleMicros)
	}
	if got := sum(t, resp, rollups.MeasureLLMTokensTotal); got != 150 {
		t.Fatalf("exact token total = %d, want 150", got)
	}
	if got := sum(t, resp, rollups.MeasureTasksCompleted); got != 3 {
		t.Fatalf("exact tasks completed = %d, want 3", got)
	}
}

func TestQuery_FreshnessCurrentWatermarkAndCoverage(t *testing.T) {
	h := newHarness(t, rollups.StateCurrent)
	seedStandardRows(t, h)
	h.quality.setState(rollups.StateCurrent)

	// The retained envelope is [refHour, refHour+1h+1min) (rows at the
	// two minute buckets refHour and refHour+1h); an hour-aligned window
	// fully inside it is [refHour, refHour+1h).
	req := baseRequest()
	req.From, req.To = refHour, refHour.Add(time.Hour)
	resp, err := h.svc.Query(scopedCtx(t, caller), req)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	q := resp.Quality
	if q.State != rollups.StateCurrent {
		t.Fatalf("state = %q, want current", q.State)
	}
	// The watermark is the last applied sequence of the local durable
	// sequence — the store's durable checkpoint, relayed verbatim.
	if q.Watermark != 4 {
		t.Fatalf("watermark = %d, want 4", q.Watermark)
	}
	if !q.RetentionStart.Equal(refHour) || !q.RetentionEnd.Equal(refHour.Add(time.Hour)) {
		t.Fatalf("retention = [%s, %s], want [%s, %s]",
			q.RetentionStart, q.RetentionEnd, refHour, refHour.Add(time.Hour))
	}
	if q.Coverage != protocol.CoverageCovered {
		t.Fatalf("coverage = %q, want covered", q.Coverage)
	}
	if q.Err != nil {
		t.Fatalf("current state carried an error: %v", q.Err)
	}
}

func TestQuery_FreshnessCatchingUpRelayedHonestly(t *testing.T) {
	h := newHarness(t, rollups.StateCurrent)
	seedStandardRows(t, h)
	h.quality.setState(rollups.StateCatchingUp)

	resp, err := h.svc.Query(scopedCtx(t, caller), baseRequest())
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if resp.Quality.State != rollups.StateCatchingUp {
		t.Fatalf("state = %q, want catching_up", resp.Quality.State)
	}
	// Rows stay exact even while catching up — the caller sees partial
	// freshness, never fake zeros.
	if got := sum(t, resp, rollups.MeasureLLMCostMicros); got != 123456 {
		t.Fatalf("catching_up cost = %d, want exact 123456", got)
	}
}

func TestQuery_FreshnessUnavailableNeverZero(t *testing.T) {
	h := newHarness(t, rollups.StateCurrent)
	seedStandardRows(t, h)
	h.quality.setState(rollups.StateUnavailable)

	ctx := scopedCtx(t, caller)

	// A window WITH rows: exact values, state unavailable — the caller can
	// distinguish "unavailable" from "zero".
	resp, err := h.svc.Query(ctx, baseRequest())
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if resp.Quality.State != rollups.StateUnavailable {
		t.Fatalf("state = %q, want unavailable", resp.Quality.State)
	}
	if got := sum(t, resp, rollups.MeasureLLMCostMicros); got != 123456 {
		t.Fatalf("unavailable-state cost = %d, want exact 123456", got)
	}

	// A window WITHOUT rows: an empty page stamped unavailable + gap —
	// never a page of zeros masquerading as complete totals.
	req := baseRequest()
	req.From = refHour.Add(30 * 24 * time.Hour)
	req.To = req.From.Add(3 * time.Hour)
	resp, err = h.svc.Query(ctx, req)
	if err != nil {
		t.Fatalf("Query empty window: %v", err)
	}
	if len(resp.Rows) != 0 {
		t.Fatalf("empty window returned %d rows, want 0", len(resp.Rows))
	}
	if resp.Quality.State != rollups.StateUnavailable {
		t.Fatalf("empty-window state = %q, want unavailable", resp.Quality.State)
	}
	if resp.Quality.Coverage != protocol.CoverageGap {
		t.Fatalf("empty-window coverage = %q, want gap", resp.Quality.Coverage)
	}
}

func TestQuery_QualityReadFailureFailsLoud(t *testing.T) {
	h := newHarness(t, rollups.StateCurrent)
	seedStandardRows(t, h)
	h.quality.fail = errors.New("quality source broken")

	_, err := h.svc.Query(scopedCtx(t, caller), baseRequest())
	if !errors.Is(err, protocol.ErrQualityFailed) {
		t.Fatalf("Query error = %v, want ErrQualityFailed", err)
	}
}

func TestQuery_CoverageQualities(t *testing.T) {
	h := newHarness(t, rollups.StateCurrent)
	seedStandardRows(t, h)

	ctx := scopedCtx(t, caller)
	cases := []struct {
		name      string
		from, to  time.Time
		wantCover protocol.Coverage
	}{
		// Retained envelope: [refHour, refHour+1h+1min) (rows at minute
		// buckets refHour and refHour+1h).
		{"window exactly the retained envelope", refHour, refHour.Add(time.Hour), protocol.CoverageCovered},
		{"window inside retention at hour grain", refHour.Add(-time.Hour), refHour.Add(time.Hour), protocol.CoveragePartial},
		{"window extending past newest", refHour, refHour.Add(2 * time.Hour), protocol.CoveragePartial},
		{"window starting inside ending outside", refHour.Add(time.Hour), refHour.Add(3 * time.Hour), protocol.CoveragePartial},
		{"window entirely after retention", refHour.Add(5 * time.Hour), refHour.Add(7 * time.Hour), protocol.CoverageGap},
		{"window entirely before retention", refHour.Add(-2 * time.Hour), refHour, protocol.CoverageGap},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := baseRequest()
			req.From, req.To = tc.from, tc.to
			resp, err := h.svc.Query(ctx, req)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if resp.Quality.Coverage != tc.wantCover {
				t.Fatalf("coverage = %q, want %q", resp.Quality.Coverage, tc.wantCover)
			}
		})
	}

	// An empty store reports gap for every window.
	empty := newHarness(t, rollups.StateCurrent)
	resp, err := empty.svc.Query(ctx, baseRequest())
	if err != nil {
		t.Fatalf("empty-store Query: %v", err)
	}
	if resp.Quality.Coverage != protocol.CoverageGap {
		t.Fatalf("empty-store coverage = %q, want gap", resp.Quality.Coverage)
	}
}

// scriptedSource is a rollups.Source fixture: a fixed event list replayed
// in sequence order, then (nil, nil) — the "caught up" proof.
type scriptedSource struct {
	mu     sync.Mutex
	events []events.Event
	failAt int // fail the Nth non-empty read (0 = never)
	fail   error
	reads  int
}

func (s *scriptedSource) Next(ctx context.Context, after uint64, limit int) ([]events.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAt > 0 && s.reads == s.failAt-1 {
		return nil, s.fail
	}
	s.reads++
	var out []events.Event
	for _, ev := range s.events {
		if ev.Sequence > after && len(out) < limit {
			out = append(out, ev)
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

// costEvent builds one canonical llm.cost.recorded event for the identity.
func costEvent(seq uint64, id identity.Identity, at time.Time, model string, usd float64) events.Event {
	return events.Event{
		Type:       llm.EventTypeCostRecorded,
		Identity:   identity.Quadruple{Identity: id},
		OccurredAt: at,
		Sequence:   seq,
		Payload: llm.CostRecordedPayload{
			Model: model,
			Cost:  llm.Cost{TotalCost: usd, Currency: "USD"},
			Usage: llm.Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, LatencyMS: 42},
		},
	}
}

// realProjectorHarness wires the REAL projector (corrected core) over the
// memstore and the scripted source, then builds the service over the same
// store with the projector as the quality source — the production wiring
// the phase intends. The projector is returned so tests can drive its
// Advance / CatchUp state machine.
func realProjectorHarness(t *testing.T, src *scriptedSource) (*memstore.Store, *rollups.Projector, *protocol.Service, *auditRecorder) {
	t.Helper()
	store := memstore.New()
	proj, err := rollups.NewProjector(src, store)
	if err != nil {
		t.Fatalf("NewProjector: %v", err)
	}
	rec := &auditRecorder{}
	svc, err := protocol.NewService(store, proj, fleetScope, rec.publish, patterns.New())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return store, proj, svc, rec
}

func TestQuery_RealProjectorSubCentCostReconcilesExactly(t *testing.T) {
	// 0.1 + 0.2 USD across two canonical events must reconcile to exactly
	// 300_000 micro-units — the decimal fidelity the corrected core's
	// exact integer model pins: the source float is converted to integer
	// micro-units EXACTLY ONCE per event and never accumulated as float.
	src := &scriptedSource{events: []events.Event{
		costEvent(1, caller, refHour, "gpt-x", 0.1),
		costEvent(2, caller, refHour, "gpt-x", 0.2),
	}}
	_, proj, svc, _ := realProjectorHarness(t, src)
	if err := proj.CatchUp(context.Background()); err != nil {
		t.Fatalf("CatchUp: %v", err)
	}
	req := baseRequest()
	req.Measures = []rollups.Measure{
		rollups.MeasureLLMCostMicros,
		rollups.MeasureLLMLatencyCount,
		rollups.MeasureLLMLatencySumMS,
		rollups.MeasureLLMLatencyMinMS,
		rollups.MeasureLLMLatencyMaxMS,
	}
	resp, err := svc.Query(scopedCtx(t, caller), req)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if got := sum(t, resp, rollups.MeasureLLMCostMicros); got != 300_000 {
		t.Fatalf("cost micros = %d, want exactly 300000 (0.1 + 0.2 USD)", got)
	}
	// The latency fold survived the projector path: two completions, sum
	// 84 ms, min 42 ms, max 42 ms — exact integers, never float.
	if got := sum(t, resp, rollups.MeasureLLMLatencyCount); got != 2 {
		t.Fatalf("latency count = %d, want 2", got)
	}
	if got := sum(t, resp, rollups.MeasureLLMLatencySumMS); got != 84 {
		t.Fatalf("latency sum = %d, want 84", got)
	}
	if got := sum(t, resp, rollups.MeasureLLMLatencyMinMS); got != 42 {
		t.Fatalf("latency min = %d, want 42", got)
	}
	if got := sum(t, resp, rollups.MeasureLLMLatencyMaxMS); got != 42 {
		t.Fatalf("latency max = %d, want 42", got)
	}
}

func TestQuery_ProjectorStateTransitionsReachTheFreshnessBlock(t *testing.T) {
	// Drive the real projector through the three honest states and assert
	// the service relays each one — current after a caught-up drain,
	// catching_up after a single non-empty advance, unavailable after a
	// source failure — with exact rows throughout and never a zero
	// masquerade.

	t.Run("current", func(t *testing.T) {
		src := &scriptedSource{events: []events.Event{
			costEvent(1, caller, refHour, "gpt-x", 0.25),
		}}
		_, proj, svc, _ := realProjectorHarness(t, src)
		if err := proj.CatchUp(context.Background()); err != nil {
			t.Fatalf("CatchUp: %v", err)
		}
		resp, err := svc.Query(scopedCtx(t, caller), baseRequest())
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if resp.Quality.State != rollups.StateCurrent {
			t.Fatalf("state = %q, want current", resp.Quality.State)
		}
		if resp.Quality.Watermark != 1 {
			t.Fatalf("watermark = %d, want 1", resp.Quality.Watermark)
		}
		if got := sum(t, resp, rollups.MeasureLLMCostMicros); got != 250_000 {
			t.Fatalf("cost micros = %d, want exactly 250000 (0.25 USD)", got)
		}
	})

	t.Run("catching_up", func(t *testing.T) {
		src := &scriptedSource{events: []events.Event{
			costEvent(1, caller, refHour, "gpt-x", 0.25),
		}}
		_, proj, svc, _ := realProjectorHarness(t, src)
		// One non-empty advance does NOT prove caught up — the projector
		// honestly reports catching_up until a subsequent empty read.
		if _, err := proj.Advance(context.Background()); err != nil {
			t.Fatalf("Advance: %v", err)
		}
		resp, err := svc.Query(scopedCtx(t, caller), baseRequest())
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if resp.Quality.State != rollups.StateCatchingUp {
			t.Fatalf("state = %q, want catching_up", resp.Quality.State)
		}
		if got := sum(t, resp, rollups.MeasureLLMCostMicros); got != 250_000 {
			t.Fatalf("catching_up cost = %d, want exact 250000", got)
		}
	})

	t.Run("unavailable", func(t *testing.T) {
		src := &scriptedSource{
			events: []events.Event{costEvent(1, caller, refHour, "gpt-x", 0.25)},
			failAt: 2,
			fail:   errors.New("durable log read failed"),
		}
		_, proj, svc, _ := realProjectorHarness(t, src)
		// First advance applies the event; the second read fails and the
		// projector lands in StateUnavailable with the failure recorded.
		if _, err := proj.Advance(context.Background()); err != nil {
			t.Fatalf("Advance 1: %v", err)
		}
		if _, err := proj.Advance(context.Background()); err == nil {
			t.Fatal("Advance 2 succeeded, want the source failure")
		}
		resp, err := svc.Query(scopedCtx(t, caller), baseRequest())
		if err != nil {
			t.Fatalf("Query: %v", err)
		}
		if resp.Quality.State != rollups.StateUnavailable {
			t.Fatalf("state = %q, want unavailable", resp.Quality.State)
		}
		if resp.Quality.Err == nil {
			t.Fatal("unavailable state must carry the last ingestion failure")
		}
		// The applied row stays EXACT: the caller sees the honest state
		// next to exact values, never zeros pretending to be totals.
		if got := sum(t, resp, rollups.MeasureLLMCostMicros); got != 250_000 {
			t.Fatalf("unavailable-state cost = %d, want exact 250000", got)
		}
	})
}

// Compile-time seam assertions: the corrected core's own artifacts satisfy
// the service's read seams without any adapter.
var (
	_ protocol.Querier       = (*memstore.Store)(nil)
	_ protocol.QualitySource = (*rollups.Projector)(nil)
)

func TestNewService_MissingDependencyFailsLoud(t *testing.T) {
	q := &testQuality{store: memstore.New(), state: rollups.StateCurrent}
	rec := &auditRecorder{}
	ok := func() *memstore.Store { return memstore.New() }
	cases := []struct {
		name string
	}{
		{"nil querier"},
		{"nil quality"},
		{"nil scope"},
		{"nil audit"},
		{"nil redactor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			switch tc.name {
			case "nil querier":
				_, err = protocol.NewService(nil, q, fleetScope, rec.publish, patterns.New())
			case "nil quality":
				_, err = protocol.NewService(ok(), nil, fleetScope, rec.publish, patterns.New())
			case "nil scope":
				_, err = protocol.NewService(ok(), q, nil, rec.publish, patterns.New())
			case "nil audit":
				_, err = protocol.NewService(ok(), q, fleetScope, nil, patterns.New())
			case "nil redactor":
				_, err = protocol.NewService(ok(), q, fleetScope, rec.publish, nil)
			}
			if !errors.Is(err, protocol.ErrMisconfigured) {
				t.Fatalf("NewService error = %v, want ErrMisconfigured", err)
			}
		})
	}
}

// stubQuerier is a scriptable protocol.Querier for the store-error mapping
// and budget tests.
type stubQuerier struct {
	err error
}

func (s *stubQuerier) Query(ctx context.Context, q rollups.Query) (rollups.Result, error) {
	if s.err != nil {
		return rollups.Result{}, s.err
	}
	return rollups.Result{}, nil
}

func TestQuery_StoreErrorMapping(t *testing.T) {
	store := memstore.New()
	q := &testQuality{store: store, state: rollups.StateCurrent}
	rec := &auditRecorder{}
	ctx := scopedCtx(t, caller)

	cases := []struct {
		name    string
		stubErr error
		wantErr error
	}{
		{"store rejects invalid", fmtErr("store: %w", rollups.ErrQueryInvalid), protocol.ErrInvalidRequest},
		{"store rejects budget", fmtErr("store: %w", rollups.ErrQueryBudget), protocol.ErrBudgetExceeded},
		{"store rejects cursor", fmtErr("store: %w", rollups.ErrBadCursor), protocol.ErrBadCursor},
		{"store fails", errors.New("store exploded"), protocol.ErrQueryFailed},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc, err := protocol.NewService(&stubQuerier{err: tc.stubErr}, q, fleetScope, rec.publish, patterns.New())
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			_, err = svc.Query(ctx, baseRequest())
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Query error = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func fmtErr(format string, args ...interface{}) error {
	return fmt.Errorf(format, args...)
}

func TestQuery_AuditRedactorFailureModesFailLoud(t *testing.T) {
	store := memstore.New()
	q := &testQuality{store: store, state: rollups.StateCurrent}
	rec := &auditRecorder{}
	admin := scopedCtx(t, caller, protocolauth.ScopeAdmin)

	redactors := []struct {
		name string
		red  audit.Redactor
	}{
		{"non-map return", redactorFunc(func(context.Context, any) (any, error) {
			return "not-a-map", nil
		})},
		{"drops field", redactorFunc(func(context.Context, any) (any, error) {
			return map[string]any{"actor_tenant": "x"}, nil
		})},
		{"reshapes field", redactorFunc(func(context.Context, any) (any, error) {
			return map[string]any{"target_tenant": 42, "target_user": "", "target_session": ""}, nil
		})},
	}
	for _, tc := range redactors {
		t.Run(tc.name, func(t *testing.T) {
			svc, err := protocol.NewService(store, q, fleetScope, rec.publish, tc.red)
			if err != nil {
				t.Fatalf("NewService: %v", err)
			}
			_, err = svc.Query(admin, baseRequest()) // elided user = widening
			if !errors.Is(err, protocol.ErrAuditFailed) {
				t.Fatalf("Query error = %v, want ErrAuditFailed", err)
			}
			if n := len(rec.snapshot()); n != 0 {
				t.Fatalf("refused audit still emitted %d events, want 0", n)
			}
		})
	}
}

func TestQuery_ElevatedWideningAxesAudited(t *testing.T) {
	h := newHarness(t, rollups.StateCurrent)
	seedStandardRows(t, h)

	cases := []struct {
		name   string
		filter protocol.Filters
	}{
		// Session fan-in (elided session axis) with the user axis pinned
		// to the caller — widening is the SESSION axis alone.
		{"elided session fan-in", protocol.Filters{UserIDs: []string{caller.UserID}}},
		// Cross-tenant read with the user and session axes pinned to the
		// caller — widening is the TENANT axis alone.
		{"named cross-tenant", protocol.Filters{
			TenantIDs:  []string{"tenant-b"},
			UserIDs:    []string{caller.UserID},
			SessionIDs: []string{caller.SessionID},
		}},
		// Multi-value user axis (the caller repeated) — the fan-in
		// trigger fires even when every entry is the caller's own value.
		{"multi-value user fan-in", protocol.Filters{UserIDs: []string{caller.UserID, caller.UserID}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h.rec.events = nil
			req := baseRequest()
			req.Filters = tc.filter
			_, err := h.svc.Query(scopedCtx(t, caller, protocolauth.ScopeAdmin), req)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			got := h.rec.snapshot()
			if len(got) != 1 {
				t.Fatalf("audit events = %d, want exactly 1 for the widened fan-in", len(got))
			}
			if got[0].Type != events.EventTypeAdminScopeUsed {
				t.Fatalf("audit event type = %q, want %q", got[0].Type, events.EventTypeAdminScopeUsed)
			}
		})
	}
}

func TestQuery_ElevatedTenantAuditTargetPayloads(t *testing.T) {
	cases := []struct {
		name       string
		tenantIDs  []string
		wantTenant string // the EXACT target_tenant audit spelling
		wantCost   int64  // the exact fanned-in cost micros the read returns
	}{
		// An elided tenant axis FOLDS to the caller's own tenant: the read
		// never leaves the caller's tenant, so the payload records it.
		{"omitted tenant folds to actor tenant", nil, caller.TenantID, 123456 + 999},
		// A single named foreign tenant is spelled out verbatim.
		{"one foreign tenant exact", []string{"tenant-b"}, "tenant-b", 7777},
		// A multi-tenant read fans in ACROSS the tenant axis: the blank
		// canonical spelling records the fan-in, never a fold to the actor —
		// folding would make the audit look narrower than the actual read.
		{"multi-tenant fan-in stays blank", []string{"tenant-a", "tenant-b"}, "", 123456 + 999 + 7777},
		// An explicit empty member is a PRESENT (non-elided) filter form
		// whose canonical audit spelling is blank: folding it to the actor
		// tenant would misrepresent the read.
		{"explicit empty member stays blank", []string{""}, "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, rollups.StateCurrent)
			seedStandardRows(t, h)

			admin := scopedCtx(t, caller, protocolauth.ScopeAdmin)
			req := baseRequest()
			req.Filters = protocol.Filters{TenantIDs: tc.tenantIDs} // elided user/session widen
			resp, err := h.svc.Query(admin, req)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if got := sum(t, resp, rollups.MeasureLLMCostMicros); got != tc.wantCost {
				t.Fatalf("fanned-in cost = %d, want %d", got, tc.wantCost)
			}

			got := h.rec.snapshot()
			if len(got) != 1 {
				t.Fatalf("audit events = %d, want exactly 1: %+v", len(got), got)
			}
			ev := got[0]
			if ev.Type != events.EventTypeAdminScopeUsed {
				t.Fatalf("audit event type = %q, want %q", ev.Type, events.EventTypeAdminScopeUsed)
			}
			if ev.Identity.Identity != caller {
				t.Fatalf("audit envelope identity = %+v, want the verified actor %+v", ev.Identity.Identity, caller)
			}
			payload, ok := ev.Payload.(events.AdminScopeUsedPayload)
			if !ok {
				t.Fatalf("audit payload = %T, want AdminScopeUsedPayload", ev.Payload)
			}
			// The elided user/session axes are the canonical wildcard
			// spelling in every case; the tenant axis carries the case's
			// exact spelling (fold, verbatim tenant, or blank fan-in).
			if payload.Tenant != tc.wantTenant || payload.User != "" || payload.Session != "" {
				t.Fatalf("audit payload = %+v, want tenant=%q user=\"\" session=\"\"", payload, tc.wantTenant)
			}
		})
	}
}
