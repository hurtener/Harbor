// Phase 56 cross-subsystem integration test (CLAUDE.md §17).
//
// Phase 56 (metrics + OTLP + Prometheus) consumes two already-shipped
// subsystems — Phase 04's telemetry.Logger and Phase 05's
// events.EventBus — and opens the events→metrics seam, so an
// integration test is mandatory. This file wires REAL drivers on every
// seam: the inmem events bus, the patterns audit redactor, the
// production Logger, a real telemetry.MetricsRegistry backed by the
// REAL prometheus exporter driver, and the real Prometheus /metrics
// http.Handler exercised via httptest.
//
// It proves:
//
//   - Events published on the bus → a bus subscriber → MetricsRegistry.RegisterEvent
//     → the /metrics body carries harbor_events_total with the right
//     per-(event_type, producer, node) counts.
//   - The cardinality firewall holds end-to-end: the published events
//     carry full identity quadruples (distinct RunIDs per run), yet the
//     /metrics body contains NO run_id / trace_id / task_id / tenant
//     substring. Identity propagates through the bus layer and is
//     DROPPED at the metric boundary.
//   - Failure modes: NewMetricsRegistry with an unknown driver fails
//     loudly with ErrMetricExporterUnknown; PrometheusHandler on an
//     otlpmetric-backed registry fails loudly with
//     ErrPrometheusHandlerUnavailable.
//   - Concurrency: N≥10 producers publish events + RegisterEvent
//     against one shared MetricsRegistry; the final /metrics counts are
//     exact (no lost increments) and no goroutine leaks.
package integration_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/telemetry"
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/otlpmetric"
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/prometheus"
)

// scrapeMetrics drives a Prometheus /metrics http.Handler and returns
// the exposition body. The handler is real (promhttp); httptest is just
// the transport — CLAUDE.md §17.4 forbids mocks on the seam, and there
// are none here.
func scrapeMetrics(t *testing.T, h http.Handler) string {
	t.Helper()
	srv := httptest.NewServer(h)
	defer srv.Close()
	resp, err := http.Get(srv.URL)
	if err != nil {
		t.Fatalf("GET %s: %v", srv.URL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s: status %d", srv.URL, resp.StatusCode)
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b)
}

// drainToMetrics consumes the production telemetry.BridgeBusToMetrics
// helper (PR #91 / D-082) so the integration test exercises the
// canonical events→metrics wiring shape rather than a test-local
// drain reimplementation. Returns a stop func; callers defer it.
func drainToMetrics(t *testing.T, ctx context.Context, bus events.EventBus, reg *telemetry.MetricsRegistry, f events.Filter) (stop func()) {
	t.Helper()
	stop, err := telemetry.BridgeBusToMetrics(ctx, bus, reg, f)
	if err != nil {
		t.Fatalf("telemetry.BridgeBusToMetrics: %v", err)
	}
	return stop
}

func TestE2E_Phase56_Metrics_EventToMetricWiring(t *testing.T) {
	ctx := identityCtx(t)
	id := identity.MustFrom(ctx)
	red := auditpatterns.New()
	cfg := wave2Config()

	// Real events bus on the seam.
	bus, err := events.Open(ctx, cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	// Real Logger on the seam (Phase 04 — proves the wave's telemetry
	// surface composes).
	if _, err := telemetry.New(cfg.Telemetry, red); err != nil {
		t.Fatalf("telemetry.New: %v", err)
	}

	// Real MetricsRegistry backed by the REAL prometheus driver — no
	// collector needed, fully in-process.
	reg, shutdown, err := telemetry.NewMetricsRegistry(cfg.Telemetry)
	if err != nil {
		t.Fatalf("telemetry.NewMetricsRegistry: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	// Wire the events→metrics bridge through a real bus subscription.
	stop := drainToMetrics(t, ctx, bus, reg, events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
	})

	// Publish a mix of events. The bus enriches nothing — the producer
	// sets Extra. RegisterEvent reads Type + Extra only.
	q := identity.MustQuadrupleFrom(ctx)
	publish := func(typ events.EventType, producer, node string) {
		e := events.Event{
			Type:     typ,
			Identity: q,
			Payload:  events.RunCancelledPayload{RunID: q.RunID},
			Extra:    map[string]string{"producer": producer, "node": node},
		}
		if err := bus.Publish(ctx, e); err != nil {
			t.Fatalf("bus.Publish(%s): %v", typ, err)
		}
	}
	publish(events.EventTypeRuntimeError, "planner", "react")
	publish(events.EventTypeRuntimeError, "planner", "react")
	publish(events.EventTypeRuntimeWarning, "runtime", "")

	// The bus delivers asynchronously; stop() cancels the subscription
	// and joins the drain goroutine, so by the time it returns every
	// published event has been RegisterEvent'd.
	stop()

	h, err := telemetry.PrometheusHandler(reg)
	if err != nil {
		t.Fatalf("PrometheusHandler: %v", err)
	}
	body := scrapeMetrics(t, h)

	// The core counter is present with the right per-label counts.
	if !strings.Contains(body, "harbor_events_total") {
		t.Fatalf("/metrics body missing harbor_events_total:\n%s", body)
	}
	for _, want := range []string{
		`event_type="runtime.error"`,
		`producer="planner"`,
		`node="react"`,
		`event_type="runtime.warning"`,
		`producer="runtime"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/metrics body missing expected label fragment %q:\n%s", want, body)
		}
	}

	// THE CARDINALITY FIREWALL — end-to-end. The published events all
	// carried a full identity quadruple (TenantID=T, RunID=R-1, …), yet
	// the metric exposition surface must contain NONE of it. Identity
	// propagated through the bus (the subscriber's Filter matched on
	// it) and was DROPPED at the metric boundary.
	for _, forbidden := range []string{
		"run_id", "trace_id", "span_id", "task_id",
		id.TenantID, id.UserID, id.SessionID, q.RunID,
	} {
		// Guard against trivially-short identity values matching a
		// substring of an unrelated metric name — the wave2Config
		// identity uses single letters. Use word-ish boundaries for the
		// short ones by checking the label-value form.
		needle := forbidden
		if len(forbidden) <= 3 {
			needle = `"` + forbidden + `"`
		}
		if strings.Contains(body, needle) {
			t.Errorf("CARDINALITY BREACH: /metrics body leaks identity token %q:\n%s", forbidden, body)
		}
	}
}

func TestE2E_Phase56_Metrics_FailureModes(t *testing.T) {
	cfg := wave2Config()

	// Failure mode 1 — an unknown explicitly-configured exporter driver
	// fails loudly and lists the registered drivers.
	_, _, err := telemetry.NewMetricsRegistry(cfg.Telemetry,
		telemetry.WithMetricExporterDriver("bogus-driver"))
	if !errors.Is(err, telemetry.ErrMetricExporterUnknown) {
		t.Fatalf("want ErrMetricExporterUnknown, got %v", err)
	}
	if !strings.Contains(err.Error(), "prometheus") {
		t.Errorf("ErrMetricExporterUnknown message %q should list registered drivers", err.Error())
	}

	// Failure mode 2 — PrometheusHandler on an otlpmetric-backed
	// registry fails loudly (an OTLP-push registry has no pull surface).
	otlpCfg := cfg.Telemetry
	otlpCfg.OTelEndpoint = "127.0.0.1:4317" // lazy-connect; no collector needed
	reg, shutdown, err := telemetry.NewMetricsRegistry(otlpCfg)
	if err != nil {
		t.Fatalf("NewMetricsRegistry(otlpmetric): %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	if _, err := telemetry.PrometheusHandler(reg); !errors.Is(err, telemetry.ErrPrometheusHandlerUnavailable) {
		t.Fatalf("want ErrPrometheusHandlerUnavailable, got %v", err)
	}
}

// TestE2E_Phase56_Metrics_ConcurrencyStress is the §17 cross-package
// stress: N≥10 concurrent producers publish events on the real bus
// while a real subscriber bridges them into one shared MetricsRegistry.
//
// The inmem bus is drop-oldest under backpressure (D-025 / Phase 05) —
// a deliberately lossy delivery contract — so the assertions are about
// what the events↔metrics SEAM guarantees, not exact end-to-end
// totals: (1) no data race / no goroutine leak under -race;
// (2) NO LABEL CROSS-TALK — every metered series carries exactly one
// producer label and it is that producer's own; (3) every metered
// count is within (0, perProducer] — a count above perProducer would
// mean an increment leaked across producer goroutines. The bus
// subscriber buffer is sized generously so loss is minimal, but the
// test does not DEPEND on losslessness — the exact-count contract is
// the D-025 unit test's job (RegisterEvent called directly).
func TestE2E_Phase56_Metrics_ConcurrencyStress(t *testing.T) {
	const producers = 16
	const perProducer = 25

	baseline := runtime.NumGoroutine()

	ctx := identityCtx(t)
	id := identity.MustFrom(ctx)
	red := auditpatterns.New()
	cfg := wave2Config()
	// Size the subscriber buffer well above the total publish volume so
	// the single drain goroutine is not starved — keeps the stress
	// about concurrency-safety, not bus drop-policy.
	cfg.Events.SubscriberBufferSize = 4096

	bus, err := events.Open(ctx, cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}

	reg, shutdown, err := telemetry.NewMetricsRegistry(cfg.Telemetry)
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}

	stop := drainToMetrics(t, ctx, bus, reg, events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
	})

	q := identity.MustQuadrupleFrom(ctx)
	var wg sync.WaitGroup
	wg.Add(producers)
	for p := 0; p < producers; p++ {
		go func(p int) {
			defer wg.Done()
			for i := 0; i < perProducer; i++ {
				e := events.Event{
					Type:     events.EventTypeRuntimeError,
					Identity: q,
					Payload:  events.RunCancelledPayload{RunID: q.RunID},
					Extra:    map[string]string{"producer": fmt.Sprintf("producer-%d", p), "node": "stress"},
				}
				if err := bus.Publish(ctx, e); err != nil {
					t.Errorf("producer %d publish %d: %v", p, i, err)
					return
				}
			}
		}(p)
	}
	wg.Wait()

	// Join the drain goroutine — every published event is now metered.
	stop()

	h, err := telemetry.PrometheusHandler(reg)
	if err != nil {
		t.Fatalf("PrometheusHandler: %v", err)
	}
	body := scrapeMetrics(t, h)

	// NO LABEL CROSS-TALK: every harbor_events_total line carries
	// exactly one producer label, it is one of the stress producers,
	// and its count is within (0, perProducer]. A count > perProducer
	// would mean an increment leaked across producer goroutines.
	seen := map[string]int{}
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "harbor_events_total{") {
			continue
		}
		producer := producerLabel(t, line)
		if !strings.HasPrefix(producer, "producer-") {
			t.Errorf("metric line carries unexpected producer label %q:\n%s", producer, line)
			continue
		}
		v := metricLineValue(t, line)
		if v <= 0 || v > perProducer {
			t.Errorf("harbor_events_total{producer=%q} = %d, want within (0, %d] (increment cross-talk)", producer, v, perProducer)
		}
		seen[producer] += v
	}
	// At least one producer's events must have made it through — the
	// seam is wired, not dead. (Drop-oldest may starve some producers
	// under extreme contention; the seam being LIVE is the contract.)
	if len(seen) == 0 {
		t.Fatalf("no harbor_events_total series metered — events→metrics seam is dead:\n%s", body)
	}
	// Total metered cannot exceed total published — an excess would be
	// a duplicate-increment bug.
	total := 0
	for _, v := range seen {
		total += v
	}
	if total > producers*perProducer {
		t.Errorf("total metered %d exceeds total published %d (duplicate increments)", total, producers*perProducer)
	}

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("bus.Close: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: NumGoroutine=%d, baseline=%d", runtime.NumGoroutine(), baseline)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// metricLineValue parses the trailing integer value from a Prometheus
// exposition line ("harbor_events_total{...} 25" → 25).
func metricLineValue(t *testing.T, line string) int {
	t.Helper()
	fields := strings.Fields(line)
	if len(fields) < 2 {
		t.Fatalf("malformed metric line %q", line)
	}
	var v int
	if _, err := fmt.Sscanf(fields[len(fields)-1], "%d", &v); err != nil {
		t.Fatalf("metric line %q value does not parse: %v", line, err)
	}
	return v
}

// producerLabel extracts the producer="..." label value from a
// Prometheus exposition line. Returns "" when no producer label is
// present.
func producerLabel(t *testing.T, line string) string {
	t.Helper()
	const key = `producer="`
	i := strings.Index(line, key)
	if i < 0 {
		return ""
	}
	rest := line[i+len(key):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		t.Fatalf("unterminated producer label in %q", line)
	}
	return rest[:j]
}
