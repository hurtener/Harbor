// Wave v1.13 boundary regression gate (§17.5 / §17.7 step 5).
//
// The v1.13 wave (phases 159-165) shipped two NEW durable read surfaces
// that the per-phase tests exercise in isolation: `events.list`'s
// windowed read with its honest `truncated` retention-gap flag (Phase
// 162 / D-294) and the observed retention horizon on `runtime.health`
// (Phase 163 / D-296). Neither per-phase test COMPOSES them. This gate
// proves the two surfaces tell ONE consistent story about the same
// durable substrate: the oldest event a windowed read returns is exactly
// the retention horizon the posture surface reports, and `truncated`
// fires precisely when that horizon has advanced past the true oldest
// event (the "expect gaps past X / this read hit the gap" pairing D-296
// promised with D-294).
//
// Real drivers at every seam (§17.3): the real inmem EventBus (a
// HistoryReplayer + RetentionReporter), the real ArtifactStore, the real
// EventsListHandler over httptest gated by the real ES256 auth.Validator.
// Identity propagation, a missing-identity failure mode (401), and an
// N≥12 concurrency leg. -race is the gate.
package integration_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	runtimeposture "github.com/hurtener/Harbor/internal/runtime/posture"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	statinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

const waveV113Kid = "harbor-wave-v113-k1"

var (
	waveV113Now  = time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	waveV113Base = time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
)

type waveV113Stack struct {
	srv  *httptest.Server
	bus  events.EventBus
	priv *ecdsa.PrivateKey
}

// newWaveV113Stack builds a real inmem-bus stack whose replay ring holds
// `ringSize` events, wired behind the real events.list handler + ES256
// validator. inmem (a best-effort ring) is deliberate: only an evicting
// substrate exercises the truncated↔horizon compose (the durable log is
// gap-free and never truncates).
func newWaveV113Stack(t *testing.T, ringSize int) *waveV113Stack {
	t.Helper()
	priv, pub := loadES256Phase61(t)
	red := auditpatterns.New()

	st, err := statinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              time.Minute,
		DropWindow:               time.Second,
		ReplayBufferSize:         ringSize,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	store, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store: st, Bus: bus, Redactor: audit.Redactor(red),
		Cfg: config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		t.Fatalf("NewControlSurface: %v", err)
	}
	keys := newES256KeySet(waveV113Kid, pub)
	v, err := auth.NewValidator(keys, auth.WithClock(func() time.Time { return waveV113Now }), auth.WithRedactor(red))
	if err != nil {
		t.Fatalf("auth.NewValidator: %v", err)
	}
	mux, err := transports.NewMux(surface, bus,
		transports.WithValidator(v),
		transports.WithEventsList(store),
	)
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.Close()
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = st.Close(context.Background())
	})
	return &waveV113Stack{srv: srv, bus: bus, priv: priv}
}

func (s *waveV113Stack) token(t *testing.T, id identity.Identity, scopes []string) string {
	t.Helper()
	claims := jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     id.UserID,
		"exp":     waveV113Now.Add(15 * time.Minute).Unix(),
		"nbf":     waveV113Now.Add(-1 * time.Minute).Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
		"scopes":  scopes,
	}
	return signES256Wave10(t, s.priv, claims, waveV113Kid)
}

func (s *waveV113Stack) list(t *testing.T, body, token string) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, s.srv.URL+"/v1/events/list", strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/events/list: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, raw
}

// seedWaveV113 publishes n events under id, each stamped with an
// explicit, strictly-increasing OccurredAt so "oldest retained" is
// well-defined. Returns the times published (index-aligned).
func seedWaveV113(t *testing.T, bus events.EventBus, id identity.Identity, n int) []time.Time {
	t.Helper()
	times := make([]time.Time, n)
	for i := range n {
		at := waveV113Base.Add(time.Duration(i) * time.Hour)
		times[i] = at
		ev := events.Event{
			Type:       events.EventTypeRuntimeWarning,
			Identity:   identity.Quadruple{Identity: id},
			OccurredAt: at,
			Payload:    events.SubscriptionIdleClosedPayload{SubscriberID: uint64(i)},
		}
		if err := bus.Publish(context.Background(), ev); err != nil {
			t.Fatalf("publish #%d: %v", i, err)
		}
	}
	return times
}

// eventsHorizon reads the observed events retention horizon through the
// same RetentionProvider that feeds runtime.health.
func eventsHorizon(t *testing.T, bus events.EventBus, id identity.Identity) time.Time {
	t.Helper()
	horizons := runtimeposture.RetentionProvider(bus, nil, nil)(context.Background(), id, false)
	for _, h := range horizons {
		if h.Surface == "events" && h.OldestRetainedAt != nil {
			return *h.OldestRetainedAt
		}
	}
	t.Fatalf("no events retention horizon reported (%+v)", horizons)
	return time.Time{}
}

// TestE2E_WaveV113_TruncatedWindowFloorEqualsRetentionHorizon is the
// wave's cross-phase compose: on an evicting ring, the windowed read's
// oldest returned row (Phase 162) sits exactly at the reported retention
// horizon (Phase 163), and `truncated` is true precisely because that
// horizon has advanced past the true first event. Plus identity
// propagation and the 401 failure mode.
func TestE2E_WaveV113_TruncatedWindowFloorEqualsRetentionHorizon(t *testing.T) {
	const ring = 4
	stack := newWaveV113Stack(t, ring)
	id := identity.Identity{TenantID: "t-w", UserID: "u-w", SessionID: "s-w"}
	times := seedWaveV113(t, stack.bus, id, 10) // 10 into a 4-ring → eviction

	status, raw := stack.list(t, `{"filter":{},"limit":50}`, stack.token(t, id, nil))
	if status != http.StatusOK {
		t.Fatalf("own-scope events.list status = %d, want 200; body=%s", status, raw)
	}
	var page prototypes.EventsListResponse
	mustJSON(t, raw, &page)

	// Phase 162: the honest retention-gap flag fires on the evicting ring.
	if !page.Truncated {
		t.Fatal("truncated = false on an evicting ring, want true (Phase 162 honest gap signal)")
	}
	if len(page.Events) == 0 || len(page.Events) > ring {
		t.Fatalf("windowed read returned %d rows, want 1..%d (ring capacity)", len(page.Events), ring)
	}
	// Identity propagation: every returned row is the caller's own.
	for _, ev := range page.Events {
		if ev.Tenant != id.TenantID || ev.Session != id.SessionID {
			t.Fatalf("windowed read leaked a foreign row (%s/%s)", ev.Tenant, ev.Session)
		}
	}
	// Rows are oldest-first within the window.
	oldestReturned := page.Events[0].OccurredAt

	// Phase 163: the observed retention horizon over the SAME bus.
	horizon := eventsHorizon(t, stack.bus, id)

	// THE COMPOSE: the windowed read's floor == the reported horizon, and
	// the horizon has advanced strictly past the true first event — which
	// is exactly what `truncated` is telling the consumer.
	if !oldestReturned.Equal(horizon) {
		t.Fatalf("windowed-read floor %v != retention horizon %v — the two surfaces disagree about the retention edge",
			oldestReturned, horizon)
	}
	if !horizon.After(times[0]) {
		t.Fatalf("retention horizon %v did not advance past the true first event %v, yet truncated=true — dishonest gap signal",
			horizon, times[0])
	}

	// Failure mode (§17.3 #3): a read with no bearer is rejected 401.
	if status, _ := stack.list(t, `{"filter":{}}`, ""); status != http.StatusUnauthorized {
		t.Fatalf("missing-bearer events.list status = %d, want 401", status)
	}
}

// TestE2E_WaveV113_NonTruncatedHorizonIsTrueOldest is the other side of
// the honesty pair: within ring capacity nothing evicts, so truncated is
// false and the horizon IS the true first event — equal to the oldest
// row the complete windowed read returns.
func TestE2E_WaveV113_NonTruncatedHorizonIsTrueOldest(t *testing.T) {
	stack := newWaveV113Stack(t, 8)
	id := identity.Identity{TenantID: "t-nt", UserID: "u-nt", SessionID: "s-nt"}
	times := seedWaveV113(t, stack.bus, id, 3) // 3 into an 8-ring → no eviction

	_, raw := stack.list(t, `{"filter":{},"limit":50}`, stack.token(t, id, nil))
	var page prototypes.EventsListResponse
	mustJSON(t, raw, &page)
	if page.Truncated {
		t.Fatal("truncated = true within ring capacity, want false (complete read)")
	}
	if len(page.Events) != 3 {
		t.Fatalf("complete read returned %d rows, want 3", len(page.Events))
	}
	horizon := eventsHorizon(t, stack.bus, id)
	if !horizon.Equal(times[0]) || !page.Events[0].OccurredAt.Equal(times[0]) {
		t.Fatalf("non-truncated horizon %v / floor %v != true first event %v",
			horizon, page.Events[0].OccurredAt, times[0])
	}
}

// TestE2E_WaveV113_ComposeConcurrency runs N≥12 concurrent readers that
// each drive BOTH surfaces (windowed read + retention horizon) against
// one shared stack under -race, asserting no cross-caller bleed and that
// the floor==horizon invariant holds under contention.
func TestE2E_WaveV113_ComposeConcurrency(t *testing.T) {
	const ring = 4
	stack := newWaveV113Stack(t, ring)
	const identities = 6
	ids := make([]identity.Identity, identities)
	for i := range ids {
		ids[i] = identity.Identity{
			TenantID:  "t-cc" + string(rune('A'+i)),
			UserID:    "u-cc",
			SessionID: "s-cc" + string(rune('A'+i)),
		}
		seedWaveV113(t, stack.bus, ids[i], 10) // evict on every identity
	}

	const readers = 18
	var wg sync.WaitGroup
	errs := make(chan string, readers)
	for r := range readers {
		wg.Add(1)
		go func(r int) {
			defer wg.Done()
			id := ids[r%identities]
			tok := stack.token(t, id, nil)
			for range 5 {
				status, raw := stack.list(t, `{"filter":{},"limit":50}`, tok)
				if status != http.StatusOK {
					errs <- "status " + http.StatusText(status)
					return
				}
				var page prototypes.EventsListResponse
				if err := json.Unmarshal(raw, &page); err != nil {
					errs <- "decode: " + err.Error()
					return
				}
				for _, ev := range page.Events {
					if ev.Tenant != id.TenantID || ev.Session != id.SessionID {
						errs <- "row bleed from " + ev.Tenant
						return
					}
				}
				if len(page.Events) == 0 {
					continue
				}
				horizons := runtimeposture.RetentionProvider(stack.bus, nil, nil)(context.Background(), id, false)
				var horizon time.Time
				for _, h := range horizons {
					if h.Surface == "events" && h.OldestRetainedAt != nil {
						horizon = *h.OldestRetainedAt
					}
				}
				if !page.Events[0].OccurredAt.Equal(horizon) {
					errs <- "floor != horizon under contention"
					return
				}
			}
		}(r)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("compose concurrency failure: %s", e)
	}
}
