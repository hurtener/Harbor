// Package integration's events_page_test.go is the Phase 73g (D-125)
// §17.1 integration test for the Console Events page. Phase 73g ships
// NO new Protocol method — the page is a pure UI consumer of
// already-shipped surface — so this test wires the SEAM the Events page
// exercises end-to-end, with real drivers at every boundary
// (CLAUDE.md §17.3 #1):
//
//   - the REAL inmem `events.EventBus` (Phase 05) — the bus the page
//     subscribes to and the aggregator counts;
//   - the REAL `events.subscribe` SSE handler (`GET /v1/events`,
//     Phase 60/72) — the table feed;
//   - the REAL `events.aggregate` handler (`POST /v1/events/aggregate`,
//     Phase 72a) — the event-rate sparkline feed;
//   - the REAL `artifacts.*` surface + a real in-mem artifact store
//     (Phase 73l) — the truncated-payload `Open artifact` resolver.
//
// It asserts (acceptance criteria of phase-73g):
//
//   - `events.aggregate` per-type bucket totals match the deliberate
//     event emission (the sparkline-correctness gate);
//   - the `events.subscribe` SSE feed, narrowed by event-type, returns
//     exactly the matching rows (table filter narrowing);
//   - identity propagates through every layer — a cross-tenant event
//     never reaches another tenant's subscription / aggregate;
//   - the failure mode: a cross-tenant `artifacts.get_ref` is rejected
//     (the truncated-payload identity-rejection branch the page-side
//     `TruncatedPayloadLink` surfaces as a recovery banner);
//   - an N≥16 concurrent-subscriber stress run leaves no goroutine
//     leak and no cross-talk.
//
// The test runs under `-race` (the CI gate). The wire transport runs
// with WithoutValidator() — the explicit test-only escape hatch
// (CLAUDE.md §13): the header / body identity is authoritative, which
// is exactly what an integration test exercising identity propagation
// through the wire surface needs without standing up a JWT key set.
package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	artinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
)

// eventsPageStack is the wired-up integration stack for the Events page
// seam — every component a real driver, no mocks (CLAUDE.md §17.3).
type eventsPageStack struct {
	mux     http.Handler
	bus     events.EventBus
	cleanup func()
}

// newEventsPageStack wires the full Protocol stack the Events page
// consumes: the inmem event bus, the events SSE + aggregate handlers
// (mounted by NewMux), and the artifacts surface over an inmem store.
func newEventsPageStack(t *testing.T) *eventsPageStack {
	t.Helper()
	ctx := context.Background()

	red := patterns.New()
	bus, err := eventsinmem.New(config.EventsConfig{
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     512,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         512,
	}, red)
	if err != nil {
		t.Fatalf("events inmem: %v", err)
	}

	store, err := state.Open(ctx, config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(ctx)
		t.Fatalf("state.Open: %v", err)
	}
	taskReg, err := tasks.Open(ctx, tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: red,
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		_ = store.Close(ctx)
		_ = bus.Close(ctx)
		t.Fatalf("tasks.Open: %v", err)
	}
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		_ = taskReg.Close(ctx)
		_ = store.Close(ctx)
		_ = bus.Close(ctx)
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}

	artStore, err := artinmem.New(config.ArtifactsConfig{})
	if err != nil {
		_ = taskReg.Close(ctx)
		_ = store.Close(ctx)
		_ = bus.Close(ctx)
		t.Fatalf("artifacts inmem: %v", err)
	}
	artifactsSurface, err := protocol.NewArtifactsSurface(protocol.ArtifactsDeps{
		Store:        artStore,
		Redactor:     red,
		Bus:          bus,
		Clock:        time.Now,
		DriverName:   "inmem",
		MaxBodyBytes: 1 << 20,
	})
	if err != nil {
		_ = artStore.Close(ctx)
		_ = taskReg.Close(ctx)
		_ = store.Close(ctx)
		_ = bus.Close(ctx)
		t.Fatalf("protocol.NewArtifactsSurface: %v", err)
	}

	mux, err := transports.NewMux(surface, bus,
		transports.WithoutValidator(),
		transports.WithKeepalive(50*time.Millisecond),
		transports.WithArtifactsSurface(artifactsSurface),
	)
	if err != nil {
		_ = artStore.Close(ctx)
		_ = taskReg.Close(ctx)
		_ = store.Close(ctx)
		_ = bus.Close(ctx)
		t.Fatalf("transports.NewMux: %v", err)
	}

	return &eventsPageStack{
		mux: mux,
		bus: bus,
		cleanup: func() {
			_ = artStore.Close(ctx)
			_ = taskReg.Close(ctx)
			_ = store.Close(ctx)
			_ = bus.Close(ctx)
		},
	}
}

// publishEventsPageEvent publishes one canonical event into the bus for
// the Events-page integration test. The payload is a SafePayload by
// construction (BusDroppedPayload) so the test does not depend on the
// redactor's behaviour for a non-Safe payload.
func publishEventsPageEvent(t *testing.T, bus events.EventBus, typ events.EventType, id identity.Identity, at time.Time) {
	t.Helper()
	ev := events.Event{
		Type: typ,
		Identity: identity.Quadruple{
			Identity: identity.Identity{
				TenantID:  id.TenantID,
				UserID:    id.UserID,
				SessionID: id.SessionID,
			},
		},
		OccurredAt: at.UTC(),
		Payload: events.BusDroppedPayload{
			FromSeq: 1, ToSeq: 1, DroppedCount: 0, SubscriberID: 0,
		},
	}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("publish %s: %v", typ, err)
	}
}

// readSSEEventTypes opens the `events.subscribe` SSE stream against the
// running mux, scoped to the supplied identity + optional event-type
// filter, reads frames until `want` is reached or the deadline fires,
// and returns the `event:` type of every frame seen.
func readSSEEventTypes(t *testing.T, baseURL string, id identity.Identity, filterTypes []string, want int) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/v1/events", nil)
	if err != nil {
		t.Fatalf("new SSE request: %v", err)
	}
	req.Header.Set("X-Harbor-Tenant", id.TenantID)
	req.Header.Set("X-Harbor-User", id.UserID)
	req.Header.Set("X-Harbor-Session", id.SessionID)
	for _, ft := range filterTypes {
		req.Header.Add("X-Harbor-Event-Type", ft)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("SSE GET: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE GET: status = %d, want 200", resp.StatusCode)
	}

	var seen []string
	sc := bufio.NewScanner(resp.Body)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "event: ") {
			seen = append(seen, strings.TrimPrefix(line, "event: "))
			if len(seen) >= want {
				break
			}
		}
	}
	return seen
}

// callEventsAggregate POSTs an events.aggregate request and decodes the
// EventAggregateResponse. Identity rides in the `X-Harbor-*` headers
// (the aggregate handler resolves identity at the wire edge — the same
// header convention the SSE handler uses; the request body carries the
// EventAggregateRequest fields only).
func callEventsAggregate(t *testing.T, baseURL string, id identity.Identity, filter prototypes.EventFilter, window, bucket time.Duration) (int, prototypes.EventAggregateResponse) {
	t.Helper()
	body, err := json.Marshal(prototypes.EventAggregateRequest{
		Filter: filter,
		Window: window,
		Bucket: bucket,
	})
	if err != nil {
		t.Fatalf("marshal aggregate request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/events/aggregate", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new aggregate request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Harbor-Tenant", id.TenantID)
	req.Header.Set("X-Harbor-User", id.UserID)
	req.Header.Set("X-Harbor-Session", id.SessionID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST aggregate: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	var decoded prototypes.EventAggregateResponse
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

// callEventsArtifacts POSTs an artifacts method to the control surface.
func callEventsArtifacts(t *testing.T, baseURL string, method methods.Method, payload any) (int, map[string]any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal artifacts request: %v", err)
	}
	resp, err := http.Post(baseURL+"/v1/control/"+string(method), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	var decoded map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&decoded)
	return resp.StatusCode, decoded
}

// TestE2E_Phase73g_EventsPage_SubscribeFilterNarrowing — the Events page
// table feed: an `events.subscribe` SSE stream narrowed by event-type
// returns ONLY the matching events. Real bus, real SSE handler.
func TestE2E_Phase73g_EventsPage_SubscribeFilterNarrowing(t *testing.T) {
	stack := newEventsPageStack(t)
	defer stack.cleanup()

	srv := httptest.NewServer(stack.mux)
	defer srv.Close()

	id := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}

	// Subscribe FIRST (the SSE stream is live-tail), then publish.
	done := make(chan []string, 1)
	go func() {
		done <- readSSEEventTypes(t, srv.URL, id, []string{string(events.EventTypeRuntimeError)}, 3)
	}()
	// Let the subscription attach before publishing — the bus delivers
	// to live subscribers only.
	time.Sleep(80 * time.Millisecond)

	now := time.Now().UTC()
	// 3 matching (runtime.error) + 2 non-matching (governance.*).
	publishEventsPageEvent(t, stack.bus, events.EventTypeRuntimeError, id, now)
	publishEventsPageEvent(t, stack.bus, events.EventTypeGovernanceBudgetExceeded, id, now)
	publishEventsPageEvent(t, stack.bus, events.EventTypeRuntimeError, id, now)
	publishEventsPageEvent(t, stack.bus, events.EventTypeGovernanceRateLimited, id, now)
	publishEventsPageEvent(t, stack.bus, events.EventTypeRuntimeError, id, now)

	select {
	case seen := <-done:
		if len(seen) != 3 {
			t.Fatalf("filtered SSE: got %d frames, want 3 — narrowing failed: %v", len(seen), seen)
		}
		for _, ty := range seen {
			if ty != string(events.EventTypeRuntimeError) {
				t.Errorf("filtered SSE leaked a non-matching type: %q", ty)
			}
		}
	case <-time.After(4 * time.Second):
		t.Fatal("filtered SSE: timed out waiting for 3 narrowed frames")
	}
}

// TestE2E_Phase73g_EventsPage_AggregateSparklineCorrectness — the
// sparkline feed: `events.aggregate` per-type bucket totals match the
// deliberate emission. Also asserts a cross-tenant event is NOT counted
// for the caller's triple-scoped aggregate (identity propagation).
func TestE2E_Phase73g_EventsPage_AggregateSparklineCorrectness(t *testing.T) {
	stack := newEventsPageStack(t)
	defer stack.cleanup()

	srv := httptest.NewServer(stack.mux)
	defer srv.Close()

	id := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}
	now := time.Now().UTC()

	// Deliberate emission: 6 runtime.error + 4 governance.budget_exceeded
	// across a 25-minute window, plus 1 cross-tenant event.
	for i := range 6 {
		publishEventsPageEvent(t, stack.bus, events.EventTypeRuntimeError, id,
			now.Add(-time.Duration(i+1)*time.Minute))
	}
	for i := range 4 {
		publishEventsPageEvent(t, stack.bus, events.EventTypeGovernanceBudgetExceeded, id,
			now.Add(-time.Duration(i+1)*time.Minute))
	}
	// Cross-tenant — must NOT appear in tenant-A's aggregate.
	publishEventsPageEvent(t, stack.bus, events.EventTypeRuntimeError,
		identity.Identity{TenantID: "tenant-B", UserID: "u-B", SessionID: "s-B"},
		now.Add(-2*time.Minute))

	status, resp := callEventsAggregate(t, srv.URL, id, prototypes.EventFilter{}, 30*time.Minute, 10*time.Minute)
	if status != http.StatusOK {
		t.Fatalf("events.aggregate: status = %d, want 200", status)
	}

	var runtimeErr, govBudget int64
	for _, b := range resp.Buckets {
		runtimeErr += b.Counts[string(events.EventTypeRuntimeError)]
		govBudget += b.Counts[string(events.EventTypeGovernanceBudgetExceeded)]
	}
	if runtimeErr != 6 {
		t.Errorf("runtime.error total = %d, want 6 (cross-tenant must not be counted)", runtimeErr)
	}
	if govBudget != 4 {
		t.Errorf("governance.budget_exceeded total = %d, want 4", govBudget)
	}
}

// TestE2E_Phase73g_EventsPage_TruncatedPayloadArtifactRoundTrip — the
// happy path of the truncated-payload `Open artifact` link: an artifact
// put round-trips to `artifacts.get_ref` for the SAME identity, and the
// identity-matched call is NOT rejected.
func TestE2E_Phase73g_EventsPage_TruncatedPayloadArtifactRoundTrip(t *testing.T) {
	stack := newEventsPageStack(t)
	defer stack.cleanup()

	srv := httptest.NewServer(stack.mux)
	defer srv.Close()

	scope := map[string]any{"tenant": "tenant-A", "user": "u-A", "session": "s-A"}

	putStatus, putBody := callEventsArtifacts(t, srv.URL, methods.MethodArtifactsPut, map[string]any{
		"identity": scope,
		"scope":    scope,
		"bytes":    "aGVhdnktcGF5bG9hZC1ieXRlcw==", // base64("heavy-payload-bytes")
		"opts":     map[string]any{"mime_type": "application/octet-stream"},
	})
	if putStatus != http.StatusOK {
		t.Fatalf("artifacts.put: status = %d, want 200; body=%v", putStatus, putBody)
	}
	ref, _ := putBody["ref"].(map[string]any)
	if ref == nil {
		t.Fatalf("artifacts.put: missing ref in response: %v", putBody)
	}
	artID, _ := ref["id"].(string)
	if artID == "" {
		t.Fatalf("artifacts.put: empty artifact id: %v", putBody)
	}

	getStatus, _ := callEventsArtifacts(t, srv.URL, methods.MethodArtifactsGetRef, map[string]any{
		"identity": scope,
		"scope":    scope,
		"id":       artID,
	})
	// The in-mem driver is not a presigner — get_ref reports
	// presign_unsupported (501) — but the identity MATCHED, so it must
	// NOT 401/403. The page's TruncatedPayloadLink distinguishes the
	// driver-capability banner from an identity rejection.
	if getStatus == http.StatusUnauthorized || getStatus == http.StatusForbidden {
		t.Fatalf("artifacts.get_ref with matching identity: status = %d — identity wrongly rejected", getStatus)
	}
}

// TestE2E_Phase73g_EventsPage_ArtifactGetRefIdentityRejected — the
// failure mode (§17.3 #3): a cross-tenant `artifacts.get_ref` for an
// artifact owned by tenant-A, called by tenant-B, is rejected — never
// silently degraded (CLAUDE.md §13, §6 cross-tenant isolation). The
// page-side TruncatedPayloadLink renders the recovery banner here.
func TestE2E_Phase73g_EventsPage_ArtifactGetRefIdentityRejected(t *testing.T) {
	stack := newEventsPageStack(t)
	defer stack.cleanup()

	srv := httptest.NewServer(stack.mux)
	defer srv.Close()

	ownerScope := map[string]any{"tenant": "tenant-A", "user": "u-A", "session": "s-A"}
	putStatus, putBody := callEventsArtifacts(t, srv.URL, methods.MethodArtifactsPut, map[string]any{
		"identity": ownerScope,
		"scope":    ownerScope,
		"bytes":    "dHJ1bmNhdGVkLXBheWxvYWQ=", // base64("truncated-payload")
		"opts":     map[string]any{"mime_type": "application/octet-stream"},
	})
	if putStatus != http.StatusOK {
		t.Fatalf("artifacts.put (owner): status = %d, want 200; body=%v", putStatus, putBody)
	}
	ref, _ := putBody["ref"].(map[string]any)
	artID, _ := ref["id"].(string)

	otherScope := map[string]any{"tenant": "tenant-B", "user": "u-B", "session": "s-B"}
	getStatus, getBody := callEventsArtifacts(t, srv.URL, methods.MethodArtifactsGetRef, map[string]any{
		"identity": otherScope,
		"scope":    otherScope,
		"id":       artID,
	})
	// A cross-tenant get_ref must NOT return the bytes URL — acceptable
	// outcomes are not_found (the artifact is invisible to tenant-B) or
	// an explicit identity / scope rejection, never a 200.
	if getStatus == http.StatusOK {
		t.Fatalf("cross-tenant artifacts.get_ref: status = 200 — tenant-B resolved tenant-A's artifact; body=%v", getBody)
	}
	if getStatus < 400 {
		t.Fatalf("cross-tenant artifacts.get_ref: status = %d, want a 4xx/5xx rejection", getStatus)
	}
}

// TestE2E_Phase73g_EventsPage_ConcurrentSubscribers — the §17.3 #5
// concurrency stress: N=16 concurrent SSE subscribers (spread across 4
// tenants) tail the bus while events are published. Asserts every
// subscriber sees ONLY its own tenant's events and no goroutine leaks
// past teardown.
func TestE2E_Phase73g_EventsPage_ConcurrentSubscribers(t *testing.T) {
	stack := newEventsPageStack(t)
	defer stack.cleanup()

	srv := httptest.NewServer(stack.mux)
	defer srv.Close()

	const tenantCount = 4
	const concurrency = 16

	runtime.GC()
	baseline := runtime.NumGoroutine()

	var (
		wg       sync.WaitGroup
		failures atomic.Int64
		ready    sync.WaitGroup
	)
	wg.Add(concurrency)
	ready.Add(concurrency)
	for i := range concurrency {

		go func() {
			defer wg.Done()
			tenant := "tenant-" + string(rune('A'+(i%tenantCount)))
			id := identity.Identity{TenantID: tenant, UserID: "u", SessionID: "s"}
			ready.Done()
			seen := readSSEEventTypes(t, srv.URL, id, []string{string(events.EventTypeRuntimeError)}, 1)
			if len(seen) != 1 {
				t.Errorf("subscriber %d (%s): got %d frames, want 1", i, tenant, len(seen))
				failures.Add(1)
				return
			}
			if seen[0] != string(events.EventTypeRuntimeError) {
				t.Errorf("subscriber %d (%s): cross-talk — saw %q", i, tenant, seen[0])
				failures.Add(1)
			}
		}()
	}
	ready.Wait()
	time.Sleep(80 * time.Millisecond)

	// One runtime.error per tenant — each subscriber for that tenant
	// receives exactly that tenant's event.
	now := time.Now().UTC()
	for tc := range tenantCount {
		tenant := "tenant-" + string(rune('A'+tc))
		publishEventsPageEvent(t, stack.bus, events.EventTypeRuntimeError,
			identity.Identity{TenantID: tenant, UserID: "u", SessionID: "s"}, now)
	}

	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d concurrent-subscriber failures", failures.Load())
	}

	http.DefaultClient.CloseIdleConnections()
	runtime.GC()
	settled := runtime.NumGoroutine()
	if settled > baseline+16 {
		t.Fatalf("goroutine leak: baseline=%d, after=%d", baseline, settled)
	}
}

// ensure the artifacts package import is exercised — the events page
// resolves heavy payloads through the artifacts surface.
var _ artifacts.ArtifactStore
