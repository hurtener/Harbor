// Phase 72a cross-subsystem integration test per CLAUDE.md §17 — the
// events.aggregate Protocol method exercised end-to-end against the
// real wire transport + the real inmem event bus + the real
// auth.Validator (Phase 61), with no mocks at any seam.
//
// Surfaces composed:
//
//   - Phase 05 events.EventBus (`events/drivers/inmem`) — the bus the
//     aggregator counts events from.
//   - Phase 60 internal/protocol/transports — the wire surface the
//     events.aggregate handler is mounted on.
//   - Phase 61 internal/protocol/auth — the JWT validator + middleware
//     gating the cross-tenant scope claim.
//   - Phase 72a internal/events + internal/protocol/transports/stream —
//     the aggregator + the events.aggregate HTTP handler.
//
// The test ships the §13 primitive-with-consumer discharge for
// Phase 72a: the aggregator is the consumer of the EventFilter wire
// shape this phase introduces, and this test exercises it
// end-to-end at the wire boundary.
package integration_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// fixedNowPhase72a is the deterministic clock the Phase 72a integration
// test uses — both the test and the validator share it so exp/nbf
// behaviour is reproducible.
var fixedNowPhase72a = time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)

// fixedAggregatorClockPhase72a returns fixedNowPhase72a from Now() so
// the integration test can backdate events and assert bucket
// arithmetic deterministically. Mirrors the per-package fixedAggregatorClock
// shape in internal/events/aggregate_test.go.
type fixedAggregatorClockPhase72a struct{ t time.Time }

func (f fixedAggregatorClockPhase72a) Now() time.Time { return f.t }

const phase72aKid = "harbor-phase72a-k1"

// phase72aDeps wires the REAL runtime drivers — no mocks at any seam
// (CLAUDE.md §17.3 #1).
type phase72aDeps struct {
	mux     http.Handler
	bus     events.EventBus
	priv    *ecdsa.PrivateKey
	now     func() time.Time
	cleanup func()
}

func newPhase72aDeps(t *testing.T) *phase72aDeps {
	t.Helper()

	priv, pub := loadES256Phase61(t)

	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     512,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         512,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}

	// The events.aggregate path does not exercise the task registry,
	// but transports.NewMux requires a ControlSurface backed by a real
	// registry. Wire a real inmem TaskRegistry; we just never call any
	// control method on it from this test.
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("state.Open: %v", err)
	}
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: audit.Redactor(red),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("tasks.Open: %v", err)
	}
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}

	keys := newES256KeySet(phase72aKid, pub)
	now := func() time.Time { return fixedNowPhase72a }
	v, err := auth.NewValidator(keys, auth.WithClock(now), auth.WithRedactor(red))
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("auth.NewValidator: %v", err)
	}

	mux, err := transports.NewMux(surface, bus,
		transports.WithKeepalive(50*time.Millisecond),
		transports.WithValidator(v),
		transports.WithAggregateClock(fixedAggregatorClockPhase72a{t: fixedNowPhase72a}),
	)
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("transports.NewMux: %v", err)
	}

	return &phase72aDeps{
		mux:  mux,
		bus:  bus,
		priv: priv,
		now:  now,
		cleanup: func() {
			_ = taskReg.Close(context.Background())
			_ = store.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

// publishPhase72aEvent backdates a runtime.error event into the bus so
// the aggregator's bucket arithmetic is testable against known timestamps.
func publishPhase72aEvent(t *testing.T, bus events.EventBus, tenant, user, session string, at time.Time) {
	t.Helper()
	ev := events.Event{
		Type: events.EventTypeRuntimeError,
		Identity: identity.Quadruple{
			Identity: identity.Identity{TenantID: tenant, UserID: user, SessionID: session},
		},
		OccurredAt: at.UTC(),
		Payload: events.BusDroppedPayload{
			FromSeq: 1, ToSeq: 1, DroppedCount: 0, SubscriberID: 0,
		},
	}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("publish %s: %v", ev.Type, err)
	}
}

// phase72aClaims mints a JWT MapClaims with the test's standard
// expiry / issuer / identity shape.
func phase72aClaims(id identity.Identity, scopes []string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     id.UserID,
		"exp":     fixedNowPhase72a.Add(15 * time.Minute).Unix(),
		"nbf":     fixedNowPhase72a.Add(-1 * time.Minute).Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
		"scopes":  scopes,
	}
}

// TestE2E_Phase72a_AggregateHappyPath — the happy path: a triple-scoped
// caller submits an aggregate request, gets back the deterministic
// bucket series matching the events they published.
func TestE2E_Phase72a_AggregateHappyPath(t *testing.T) {
	deps := newPhase72aDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	id := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}

	// Backdate events 5/15/25 minutes ago.
	publishPhase72aEvent(t, deps.bus,
		id.TenantID, id.UserID, id.SessionID, fixedNowPhase72a.Add(-5*time.Minute))
	publishPhase72aEvent(t, deps.bus,
		id.TenantID, id.UserID, id.SessionID, fixedNowPhase72a.Add(-15*time.Minute))
	publishPhase72aEvent(t, deps.bus,
		id.TenantID, id.UserID, id.SessionID, fixedNowPhase72a.Add(-25*time.Minute))
	// An event for another tenant should NOT appear in the response.
	publishPhase72aEvent(t, deps.bus,
		"tenant-other", "u-x", "s-x", fixedNowPhase72a.Add(-10*time.Minute))

	tok := signES256Wave10(t, deps.priv, phase72aClaims(id, nil), phase72aKid)
	body, _ := json.Marshal(prototypes.EventAggregateRequest{
		Filter: prototypes.EventFilter{
			EventTypes: []string{string(events.EventTypeRuntimeError)},
		},
		Window: 30 * time.Minute,
		Bucket: 10 * time.Minute,
	})

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/events/aggregate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("aggregate happy: status = %d, want 200; body=%s", resp.StatusCode, raw)
	}
	var agg prototypes.EventAggregateResponse
	if err := json.NewDecoder(resp.Body).Decode(&agg); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(agg.Buckets) != 3 {
		t.Fatalf("expected 3 buckets (30m/10m), got %d", len(agg.Buckets))
	}

	// Bucket 0 [-30..-20] — 1 event (the 25m-ago one).
	if got := agg.Buckets[0].Counts[string(events.EventTypeRuntimeError)]; got != 1 {
		t.Errorf("bucket 0 count = %d, want 1", got)
	}
	// Bucket 1 [-20..-10] — 1 event (the 15m-ago one). Note the
	// other-tenant event at -10m belongs to bucket 2 if it matched, but
	// it shouldn't have matched at all.
	if got := agg.Buckets[1].Counts[string(events.EventTypeRuntimeError)]; got != 1 {
		t.Errorf("bucket 1 count = %d, want 1", got)
	}
	// Bucket 2 [-10..0] — 1 event (the 5m-ago one); the other-tenant
	// event must NOT appear.
	if got := agg.Buckets[2].Counts[string(events.EventTypeRuntimeError)]; got != 1 {
		t.Errorf("bucket 2 count = %d, want 1 (other-tenant must not be counted)", got)
	}
	if agg.ProtocolVersion == "" {
		t.Error("ProtocolVersion echo missing")
	}
}

// TestE2E_Phase72a_AggregateRejectsCrossTenantWithoutScope — a request
// that names a cross-tenant filter without auth.ScopeAdmin OR
// auth.ScopeConsoleFleet is rejected 403 with CodeIdentityScopeRequired.
func TestE2E_Phase72a_AggregateRejectsCrossTenantWithoutScope(t *testing.T) {
	deps := newPhase72aDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	id := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}
	tok := signES256Wave10(t, deps.priv, phase72aClaims(id, nil), phase72aKid)

	body, _ := json.Marshal(prototypes.EventAggregateRequest{
		Filter: prototypes.EventFilter{
			// Cross-tenant: name a tenant OTHER than the caller's.
			TenantIDs: []string{"tenant-B"},
		},
		Window: 30 * time.Minute,
		Bucket: 10 * time.Minute,
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/events/aggregate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusForbidden {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("cross-tenant without scope: status = %d, want 403; body=%s", resp.StatusCode, raw)
	}
	var env protoerrors.Error
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env.Code != protoerrors.CodeIdentityScopeRequired {
		t.Fatalf("cross-tenant without scope: code = %q, want %q", env.Code, protoerrors.CodeIdentityScopeRequired)
	}
}

// TestE2E_Phase72a_AggregateAcceptsCrossTenantWithAdminScope — same
// cross-tenant request, but the JWT carries `admin` scope. The
// request succeeds and the response counts events from the named
// tenant.
func TestE2E_Phase72a_AggregateAcceptsCrossTenantWithAdminScope(t *testing.T) {
	deps := newPhase72aDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	id := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}

	// Publish an event for tenant-B (the cross-tenant target).
	publishPhase72aEvent(t, deps.bus,
		"tenant-B", "u-B", "s-B", fixedNowPhase72a.Add(-5*time.Minute))

	tok := signES256Wave10(t, deps.priv,
		phase72aClaims(id, []string{string(auth.ScopeAdmin)}), phase72aKid)

	body, _ := json.Marshal(prototypes.EventAggregateRequest{
		Filter: prototypes.EventFilter{
			TenantIDs:  []string{"tenant-B"},
			UserIDs:    []string{"u-B"},
			SessionIDs: []string{"s-B"},
		},
		Window: 30 * time.Minute,
		Bucket: 10 * time.Minute,
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/events/aggregate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("cross-tenant with admin: status = %d, want 200; body=%s", resp.StatusCode, raw)
	}
	var agg prototypes.EventAggregateResponse
	if err := json.NewDecoder(resp.Body).Decode(&agg); err != nil {
		t.Fatalf("decode: %v", err)
	}
	// Sum runtime.error counts across buckets — should be 1 (the
	// tenant-B event we published at -5m).
	var total int64
	for _, b := range agg.Buckets {
		total += b.Counts[string(events.EventTypeRuntimeError)]
	}
	if total != 1 {
		t.Fatalf("cross-tenant with admin: total = %d, want 1", total)
	}
}

// TestE2E_Phase72a_AggregateAcceptsCrossTenantWithConsoleFleetScope —
// `console:fleet` is the second canonical cross-tenant scope (D-079
// closed set). Same posture as admin.
func TestE2E_Phase72a_AggregateAcceptsCrossTenantWithConsoleFleetScope(t *testing.T) {
	deps := newPhase72aDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	id := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}
	publishPhase72aEvent(t, deps.bus,
		"tenant-B", "u-B", "s-B", fixedNowPhase72a.Add(-5*time.Minute))

	tok := signES256Wave10(t, deps.priv,
		phase72aClaims(id, []string{string(auth.ScopeConsoleFleet)}), phase72aKid)

	body, _ := json.Marshal(prototypes.EventAggregateRequest{
		Filter: prototypes.EventFilter{
			TenantIDs:  []string{"tenant-B"},
			UserIDs:    []string{"u-B"},
			SessionIDs: []string{"s-B"},
		},
		Window: 30 * time.Minute,
		Bucket: 10 * time.Minute,
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/events/aggregate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("cross-tenant with console:fleet: status = %d, want 200; body=%s", resp.StatusCode, raw)
	}
}

// TestE2E_Phase72a_AggregateRejectsMissingIdentity — a request with no
// `Authorization` header is rejected 401 with CodeAuthRejected (Phase
// 61's auth middleware short-circuits before the handler sees the
// request).
func TestE2E_Phase72a_AggregateRejectsMissingIdentity(t *testing.T) {
	deps := newPhase72aDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	body, _ := json.Marshal(prototypes.EventAggregateRequest{
		Window: 30 * time.Minute,
		Bucket: 10 * time.Minute,
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/events/aggregate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	// NO Authorization header.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// The Phase 61 auth middleware fails closed with 401.
	if resp.StatusCode != http.StatusUnauthorized {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("missing identity: status = %d, want 401; body=%s", resp.StatusCode, raw)
	}
}

// TestE2E_Phase72a_AggregateRejectsBadWindow — a Window/Bucket pair
// that doesn't divide evenly fails with CodeInvalidRequest (400).
func TestE2E_Phase72a_AggregateRejectsBadWindow(t *testing.T) {
	deps := newPhase72aDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	id := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}
	tok := signES256Wave10(t, deps.priv, phase72aClaims(id, nil), phase72aKid)

	body, _ := json.Marshal(prototypes.EventAggregateRequest{
		Window: time.Hour,
		Bucket: 7 * time.Minute, // 60 % 7 != 0
	})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/events/aggregate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("bad window: status = %d, want 400; body=%s", resp.StatusCode, raw)
	}
	var env protoerrors.Error
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		t.Fatalf("decode error envelope: %v", err)
	}
	if env.Code != protoerrors.CodeInvalidRequest {
		t.Fatalf("bad window: code = %q, want %q", env.Code, protoerrors.CodeInvalidRequest)
	}
}

// TestE2E_Phase72a_ConcurrentAggregateClients — the §17 #5 concurrency
// stress: N=16 concurrent clients each issuing aggregate requests
// against the same shared mux + bus + aggregator under -race. Asserts
// no cross-talk, no goroutine leaks past teardown, every client sees
// only its own tenant's events.
func TestE2E_Phase72a_ConcurrentAggregateClients(t *testing.T) {
	// Cannot run with t.Parallel — the baseline goroutine count would
	// be polluted by parallel siblings.
	deps := newPhase72aDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	// Pre-publish: 4 distinct tenants, each with N events.
	const tenantCount = 4
	tenants := make([]identity.Identity, tenantCount)
	for i := range tenantCount {
		tenants[i] = identity.Identity{
			TenantID:  "tenant-" + string(rune('A'+i)),
			UserID:    "u",
			SessionID: "s",
		}
		// Each tenant gets (i+1) events.
		for j := range i + 1 {
			publishPhase72aEvent(t, deps.bus,
				tenants[i].TenantID, tenants[i].UserID, tenants[i].SessionID,
				fixedNowPhase72a.Add(-time.Duration(j+1)*time.Minute))
		}
	}

	runtime.GC()
	baseline := runtime.NumGoroutine()

	const concurrency = 16
	var (
		wg       sync.WaitGroup
		failures atomic.Int64
	)
	wg.Add(concurrency)
	for i := range concurrency {

		go func() {
			defer wg.Done()
			myID := tenants[i%tenantCount]
			wantCount := int64((i % tenantCount) + 1)
			tok := signES256Wave10(t, deps.priv, phase72aClaims(myID, nil), phase72aKid)
			body, _ := json.Marshal(prototypes.EventAggregateRequest{
				Filter: prototypes.EventFilter{
					EventTypes: []string{string(events.EventTypeRuntimeError)},
				},
				Window: 30 * time.Minute,
				Bucket: time.Minute,
			})
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/events/aggregate", bytes.NewReader(body))
			if err != nil {
				t.Errorf("goroutine %d: NewRequest: %v", i, err)
				failures.Add(1)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+tok)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Errorf("goroutine %d: Do: %v", i, err)
				failures.Add(1)
				return
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				raw, _ := io.ReadAll(resp.Body)
				t.Errorf("goroutine %d (%s): status = %d, want 200; body=%s", i, myID.TenantID, resp.StatusCode, raw)
				failures.Add(1)
				return
			}
			var agg prototypes.EventAggregateResponse
			if err := json.NewDecoder(resp.Body).Decode(&agg); err != nil {
				t.Errorf("goroutine %d: decode: %v", i, err)
				failures.Add(1)
				return
			}
			var got int64
			for _, b := range agg.Buckets {
				got += b.Counts[string(events.EventTypeRuntimeError)]
			}
			if got != wantCount {
				t.Errorf("goroutine %d (%s): got %d events, want %d — context bleed?", i, myID.TenantID, got, wantCount)
				failures.Add(1)
			}
		}()
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d concurrent failures", failures.Load())
	}

	// Drain idle HTTP keep-alive connections so the leak check is not
	// polluted by http.Transport's per-host goroutines.
	http.DefaultClient.CloseIdleConnections()

	// Goroutine leak window: the test's per-request goroutines must
	// have unwound. Allow a small slack for the httptest server's own
	// goroutines settling and the transport's idle-close lag.
	runtime.GC()
	settled := runtime.NumGoroutine()
	if settled > baseline+8 {
		t.Fatalf("goroutine leak: baseline=%d, after=%d", baseline, settled)
	}
}
