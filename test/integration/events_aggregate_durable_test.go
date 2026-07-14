// events.aggregate durable-driver parity integration test (CLAUDE.md §17.1,
// §17.8). It proves the HA-18 fix end-to-end: events.aggregate RETURNS (HTTP
// 200 with a well-formed series) on the DURABLE event-log driver — backed by a
// REAL inmem StateStore at the seam, no mock — for the session-less admin
// request that previously 500'd because the aggregator replayed through the
// per-session Replayer.Replay path the durable driver refuses.
//
// Surfaces composed (no mocks at any seam):
//
//   - internal/events/drivers/durable — the durable event log, wired with a
//     real internal/state/drivers/inmem StateStore.
//   - internal/protocol/transports — the wire surface the aggregate handler
//     mounts on.
//   - internal/protocol/auth — the JWT validator + middleware gating the
//     cross-tenant scope claim.
//   - internal/events + internal/protocol/transports/stream — the aggregator
//     (now sourcing from the HistoryReplayer windowed fan-in) + the handler.
package integration_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

const aggregateDurableKid = "harbor-aggregate-durable-k1"

// aggregateDurableDeps wires the REAL durable event bus (with a real inmem
// StateStore at the seam) behind the real wire transport + auth validator.
type aggregateDurableDeps struct {
	mux     http.Handler
	bus     events.EventBus
	priv    *ecdsa.PrivateKey
	cleanup func()
}

func newAggregateDurableDeps(t *testing.T) *aggregateDurableDeps {
	t.Helper()

	priv, pub := loadES256Phase61(t)
	red := auditpatterns.New()

	// Real inmem StateStore at the durable seam (§17.8) — no mock.
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	bus, err := durable.New(context.Background(), config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     512,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         512,
	}, red, store)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}

	taskStore, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("state.Open: %v", err)
	}
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    taskStore,
		Bus:      bus,
		Redactor: audit.Redactor(red),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		_ = taskStore.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("tasks.Open: %v", err)
	}
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = taskStore.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}

	keys := newES256KeySet(aggregateDurableKid, pub)
	now := func() time.Time { return fixedNowPhase72a }
	v, err := auth.NewValidator(keys, auth.WithClock(now), auth.WithRedactor(red))
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = taskStore.Close(context.Background())
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
		_ = taskStore.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("transports.NewMux: %v", err)
	}

	return &aggregateDurableDeps{
		mux:  mux,
		bus:  bus,
		priv: priv,
		cleanup: func() {
			_ = taskReg.Close(context.Background())
			_ = taskStore.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

// TestE2E_AggregateDurable_SessionLessAdmin_Returns200 is the load-bearing
// HA-18 proof: a session-less admin (cross-tenant) aggregate returns 200 with
// a correct fan-in series on the DURABLE driver, where the old
// Replayer.Replay-backed aggregator 500'd every call.
func TestE2E_AggregateDurable_SessionLessAdmin_Returns200(t *testing.T) {
	deps := newAggregateDurableDeps(t)
	defer deps.cleanup()
	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	// Fan-in fixture: two tenants, each with its own session, all events
	// sharing the caller's user/session id so the handler's triple-fold (which
	// narrows the elided user/session axes onto the caller) still fans across
	// tenants — the same shape the events.list admin cross-tenant test uses.
	caller := identity.Identity{TenantID: "t-admin", UserID: "u-shared", SessionID: "s-shared"}
	publishPhase72aEvent(t, deps.bus, "t-one", caller.UserID, caller.SessionID, fixedNowPhase72a.Add(-5*time.Minute))
	publishPhase72aEvent(t, deps.bus, "t-one", caller.UserID, caller.SessionID, fixedNowPhase72a.Add(-6*time.Minute))
	publishPhase72aEvent(t, deps.bus, "t-two", caller.UserID, caller.SessionID, fixedNowPhase72a.Add(-7*time.Minute))

	body, _ := json.Marshal(prototypes.EventAggregateRequest{
		Filter: prototypes.EventFilter{TenantIDs: []string{"t-one", "t-two"}},
		Window: 30 * time.Minute,
		Bucket: time.Minute,
	})
	status, agg := doAggregateDurable(t, srv.URL, deps, caller,
		[]string{string(auth.ScopeAdmin)}, body)
	if status != http.StatusOK {
		t.Fatalf("session-less admin aggregate on durable: status = %d, want 200 (HA-18: previously 500)", status)
	}
	if got := sumRuntimeErrorDurable(agg); got != 3 {
		t.Fatalf("widened durable aggregate counted %d events, want 3 (cross-tenant fan-in)", got)
	}
	if len(agg.Buckets) != 30 {
		t.Fatalf("bucket count = %d, want 30 (30m/1m)", len(agg.Buckets))
	}
}

// TestE2E_AggregateDurable_NonAdminOwnSession_ScopedToTuple proves identity
// propagation on durable: a non-admin caller's aggregate sees ONLY its own
// tuple's events, never a sibling session's.
func TestE2E_AggregateDurable_NonAdminOwnSession_ScopedToTuple(t *testing.T) {
	deps := newAggregateDurableDeps(t)
	defer deps.cleanup()
	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	caller := identity.Identity{TenantID: "t-iso", UserID: "u-iso", SessionID: "s-iso"}
	// Caller's own events.
	publishPhase72aEvent(t, deps.bus, caller.TenantID, caller.UserID, caller.SessionID, fixedNowPhase72a.Add(-5*time.Minute))
	publishPhase72aEvent(t, deps.bus, caller.TenantID, caller.UserID, caller.SessionID, fixedNowPhase72a.Add(-6*time.Minute))
	// A sibling session in the SAME tenant+user — must NOT be counted.
	publishPhase72aEvent(t, deps.bus, caller.TenantID, caller.UserID, "s-other", fixedNowPhase72a.Add(-5*time.Minute))
	// A different tenant — must NOT be counted.
	publishPhase72aEvent(t, deps.bus, "t-foreign", "u-foreign", "s-foreign", fixedNowPhase72a.Add(-5*time.Minute))

	body, _ := json.Marshal(prototypes.EventAggregateRequest{
		Window: 30 * time.Minute,
		Bucket: time.Minute,
	})
	status, agg := doAggregateDurable(t, srv.URL, deps, caller, nil, body)
	if status != http.StatusOK {
		t.Fatalf("non-admin own-session aggregate on durable: status = %d, want 200", status)
	}
	if got := sumRuntimeErrorDurable(agg); got != 2 {
		t.Fatalf("non-admin aggregate counted %d events, want 2 (own tuple only — sibling session + foreign tenant excluded)", got)
	}
}

// TestE2E_AggregateDurable_CrossTenantWithoutScope_Rejected is the ≥1 failure
// mode: a cross-tenant filter WITHOUT the elevated scope is rejected (403,
// CodeIdentityScopeRequired) — never counted, never a 500.
func TestE2E_AggregateDurable_CrossTenantWithoutScope_Rejected(t *testing.T) {
	deps := newAggregateDurableDeps(t)
	defer deps.cleanup()
	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	caller := identity.Identity{TenantID: "t-caller", UserID: "u-caller", SessionID: "s-caller"}
	body, _ := json.Marshal(prototypes.EventAggregateRequest{
		Filter: prototypes.EventFilter{TenantIDs: []string{"t-foreign"}},
		Window: 30 * time.Minute,
		Bucket: time.Minute,
	})
	status, raw := doAggregateDurableRaw(t, srv.URL, deps, caller, nil, body)
	if status != http.StatusForbidden {
		t.Fatalf("cross-tenant aggregate WITHOUT scope: status = %d, want 403; body=%s", status, raw)
	}
	var perr protoerrors.Error
	if err := json.Unmarshal(raw, &perr); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if perr.Code != protoerrors.CodeIdentityScopeRequired {
		t.Fatalf("error code = %q, want %q", perr.Code, protoerrors.CodeIdentityScopeRequired)
	}
}

// TestE2E_AggregateDurable_OverBound_Truncated proves the DATA-not-500 partial
// signal on the REAL durable driver: a window with more matching events than
// the single-read aggregation bound returns partial buckets with
// truncated=true, never a 400. Driven at the aggregator seam (over the real
// durable bus + real StateStore) because the aggregation bound is not a wire
// knob.
func TestE2E_AggregateDurable_OverBound_Truncated(t *testing.T) {
	deps := newAggregateDurableDeps(t)
	defer deps.cleanup()

	caller := identity.Identity{TenantID: "t-trunc", UserID: "u-trunc", SessionID: "s-trunc"}
	for i := range 6 {
		publishPhase72aEvent(t, deps.bus, caller.TenantID, caller.UserID, caller.SessionID,
			fixedNowPhase72a.Add(-time.Duration(i+1)*time.Minute))
	}
	// A scan bound of 3 is below the 6 published in-window events → truncated.
	agg, err := events.NewAggregator(deps.bus,
		events.WithAggregatorClock(fixedAggregatorClockPhase72a{t: fixedNowPhase72a}),
		events.WithAggregateScanBound(3))
	if err != nil {
		t.Fatalf("NewAggregator: %v", err)
	}
	resp, err := agg.Aggregate(context.Background(), prototypes.EventAggregateRequest{
		Filter: prototypes.EventFilter{
			TenantIDs:  []string{caller.TenantID},
			UserIDs:    []string{caller.UserID},
			SessionIDs: []string{caller.SessionID},
		},
		Window: 30 * time.Minute,
		Bucket: time.Minute,
	}, false)
	if err != nil {
		t.Fatalf("durable aggregate over bound must not error (truncated is DATA, not a 400): %v", err)
	}
	if !resp.Truncated {
		t.Fatalf("durable window past the aggregation bound: Truncated=false, want true (partial counts must be flagged)")
	}
}

// TestE2E_AggregateDurable_EpochAnchor_AlignedSeries proves D-306's
// origin-anchored grid works end-to-end on the DURABLE driver (real
// StateStore at the seam, real wire transport + auth): an anchored
// request returns a series whose bucket boundaries fall on the fixed
// `epoch + k·Bucket` grid, and two requests at two clock instants share
// bucket coordinates (addressability). The clock is fixed for the mux, so
// the two-instant addressability leg is exercised at the aggregator seam
// in the unit tests; here we assert the wire path returns a grid-aligned,
// non-widened series on durable.
func TestE2E_AggregateDurable_EpochAnchor_AlignedSeries(t *testing.T) {
	deps := newAggregateDurableDeps(t)
	defer deps.cleanup()
	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	caller := identity.Identity{TenantID: "t-grid", UserID: "u-grid", SessionID: "s-grid"}
	publishPhase72aEvent(t, deps.bus, caller.TenantID, caller.UserID, caller.SessionID, fixedNowPhase72a.Add(-5*time.Minute))
	publishPhase72aEvent(t, deps.bus, caller.TenantID, caller.UserID, caller.SessionID, fixedNowPhase72a.Add(-6*time.Minute))

	epoch := time.Unix(0, 0).UTC()
	const window = 30 * time.Minute
	const bucket = time.Minute
	body, _ := json.Marshal(prototypes.EventAggregateRequest{
		Filter: prototypes.EventFilter{
			TenantIDs:  []string{caller.TenantID},
			UserIDs:    []string{caller.UserID},
			SessionIDs: []string{caller.SessionID},
		},
		Window: window,
		Bucket: bucket,
		Anchor: &epoch,
	})
	status, agg := doAggregateDurable(t, srv.URL, deps, caller, nil, body)
	if status != http.StatusOK {
		t.Fatalf("anchored aggregate on durable: status = %d, want 200", status)
	}
	if len(agg.Buckets) != int(window/bucket) {
		t.Fatalf("anchored bucket count = %d, want %d", len(agg.Buckets), int(window/bucket))
	}
	// Every bucket boundary lies on the epoch grid.
	for _, b := range agg.Buckets {
		if b.Start.Sub(epoch)%bucket != 0 || b.End.Sub(epoch)%bucket != 0 {
			t.Fatalf("durable anchored bucket [%v,%v) off the epoch grid", b.Start, b.End)
		}
	}
	// The counts still land (grid re-phases boundaries, it does not drop data).
	if got := sumRuntimeErrorDurable(agg); got != 2 {
		t.Fatalf("anchored durable aggregate counted %d events, want 2", got)
	}
}

// TestE2E_AggregateDurable_WidenedDistinctPrincipals_FansIn is the
// isolation-critical end-to-end proof of the fold fix (D-308) on the REAL
// durable driver: a widened admin aggregate naming ONLY a foreign tenant, with
// user/session elided, fans in across that tenant's DISTINCT users/sessions —
// NOT narrowed to tenant+caller-user+caller-session → EMPTY. Distinct
// principals (not the caller's shared ids) so a caller-fold would return 0.
func TestE2E_AggregateDurable_WidenedDistinctPrincipals_FansIn(t *testing.T) {
	deps := newAggregateDurableDeps(t)
	defer deps.cleanup()
	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	caller := identity.Identity{TenantID: "t-caller", UserID: "u-caller", SessionID: "s-caller"}
	// Foreign tenant, DISTINCT principals from the caller.
	publishPhase72aEvent(t, deps.bus, "t-fleet", "u-a", "s-a", fixedNowPhase72a.Add(-5*time.Minute))
	publishPhase72aEvent(t, deps.bus, "t-fleet", "u-b", "s-b", fixedNowPhase72a.Add(-6*time.Minute))
	publishPhase72aEvent(t, deps.bus, "t-fleet", "u-c", "s-c", fixedNowPhase72a.Add(-7*time.Minute))
	// Caller's own tenant event — the filter names ONLY t-fleet, must not count.
	publishPhase72aEvent(t, deps.bus, caller.TenantID, caller.UserID, caller.SessionID, fixedNowPhase72a.Add(-5*time.Minute))

	body, _ := json.Marshal(prototypes.EventAggregateRequest{
		Filter: prototypes.EventFilter{TenantIDs: []string{"t-fleet"}},
		Window: 30 * time.Minute,
		Bucket: time.Minute,
	})
	status, agg := doAggregateDurable(t, srv.URL, deps, caller,
		[]string{string(auth.ScopeAdmin)}, body)
	if status != http.StatusOK {
		t.Fatalf("widened distinct-principals aggregate on durable: status = %d, want 200", status)
	}
	if got := sumRuntimeErrorDurable(agg); got != 3 {
		t.Fatalf("widened durable aggregate counted %d, want 3 (distinct principals fanned in; a caller-fold would return 0)", got)
	}
}

func doAggregateDurable(t *testing.T, baseURL string, deps *aggregateDurableDeps, id identity.Identity, scopes []string, body []byte) (int, prototypes.EventAggregateResponse) {
	t.Helper()
	status, raw := doAggregateDurableRaw(t, baseURL, deps, id, scopes, body)
	var agg prototypes.EventAggregateResponse
	if status == http.StatusOK {
		if err := json.Unmarshal(raw, &agg); err != nil {
			t.Fatalf("decode aggregate response: %v; body=%s", err, raw)
		}
	}
	return status, agg
}

func doAggregateDurableRaw(t *testing.T, baseURL string, deps *aggregateDurableDeps, id identity.Identity, scopes []string, body []byte) (int, []byte) {
	t.Helper()
	tok := signES256Wave10(t, deps.priv, phase72aClaims(id, scopes), aggregateDurableKid)
	req, _ := http.NewRequest(http.MethodPost, baseURL+"/v1/events/aggregate", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/events/aggregate: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

func sumRuntimeErrorDurable(resp prototypes.EventAggregateResponse) int64 {
	var total int64
	for _, b := range resp.Buckets {
		total += b.Counts[string(events.EventTypeRuntimeError)]
	}
	return total
}
