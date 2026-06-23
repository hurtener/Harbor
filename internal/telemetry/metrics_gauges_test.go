package telemetry_test

import (
	"context"
	"strings"
	"testing"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/hurtener/Harbor/internal/telemetry"
)

// Canonical runtime-gauge metric names. Hard-coded here (the consts are
// unexported) so the test pins the wire names a /metrics scraper and a
// metrics.snapshot reader depend on — a rename is a wire-contract change
// and must break this test.
const (
	gaugeActiveRuns    = "harbor_runtime_active_runs"
	gaugeEngineCap     = "harbor_runtime_engine_capacity_entries"
	gaugeGovCache      = "harbor_runtime_governance_cache_entries"
	gaugeEventsDropped = "harbor_runtime_events_dropped"
)

// TestRegisterRuntimeGauges_SnapshotYieldsGauges feeds known callback
// values through RegisterRuntimeGauges and asserts Snapshot projects all
// four gauges with those exact values onto MetricSnapshot.Gauges.
func TestRegisterRuntimeGauges_SnapshotYieldsGauges(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	reg, shutdown, err := telemetry.NewMetricsRegistry(goodCfg(), telemetry.WithMetricReader(reader))
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	want := map[string]float64{
		gaugeActiveRuns:    3,
		gaugeEngineCap:     7,
		gaugeGovCache:      2,
		gaugeEventsDropped: 11,
	}
	if err := reg.RegisterRuntimeGauges(telemetry.RuntimeGaugeSource{
		ActiveRuns:             func() int64 { return 3 },
		EngineCapacityEntries:  func() int64 { return 7 },
		GovernanceCacheEntries: func() int64 { return 2 },
		EventsDropped:          func() int64 { return 11 },
	}); err != nil {
		t.Fatalf("RegisterRuntimeGauges: %v", err)
	}

	snap, err := reg.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	got := map[string]float64{}
	for _, g := range snap.Gauges {
		got[g.Name] = g.Value
		if len(g.Labels) != 0 {
			t.Errorf("gauge %q carries labels %v; runtime gauges are unlabelled by construction", g.Name, g.Labels)
		}
	}
	for name, val := range want {
		if got[name] != val {
			t.Errorf("gauge %q = %v, want %v (all gauges: %v)", name, got[name], val, got)
		}
	}
}

// TestRegisterRuntimeGauges_NilCallbackSkipped asserts a nil callback is
// skipped (the gauge seam's documented behaviour) — only the gauges with
// a non-nil source appear in the snapshot.
func TestRegisterRuntimeGauges_NilCallbackSkipped(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	reg, shutdown, err := telemetry.NewMetricsRegistry(goodCfg(), telemetry.WithMetricReader(reader))
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	// Only EventsDropped is sourced — the engine/governance stack of a
	// planner-shaped runtime leaves the others nil.
	if err := reg.RegisterRuntimeGauges(telemetry.RuntimeGaugeSource{
		EventsDropped: func() int64 { return 5 },
	}); err != nil {
		t.Fatalf("RegisterRuntimeGauges: %v", err)
	}
	snap, err := reg.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	names := map[string]struct{}{}
	for _, g := range snap.Gauges {
		names[g.Name] = struct{}{}
	}
	if _, ok := names[gaugeEventsDropped]; !ok {
		t.Errorf("expected %q in snapshot, got %v", gaugeEventsDropped, names)
	}
	for _, absent := range []string{gaugeActiveRuns, gaugeEngineCap, gaugeGovCache} {
		if _, ok := names[absent]; ok {
			t.Errorf("nil-callback gauge %q should be skipped, but it appeared", absent)
		}
	}
}

// TestPrometheusHandler_ServesRuntimeAndProcessCollectors asserts the
// prometheus-backed scrape carries the Go + process collector metrics
// (go_goroutines and a go_memstats_* series) the prometheus driver
// registers on its per-instance registry — the signals that reveal a
// goroutine/heap leak in a long-running runtime.
func TestPrometheusHandler_ServesRuntimeAndProcessCollectors(t *testing.T) {
	reg, shutdown, err := telemetry.NewMetricsRegistry(goodCfg())
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	h, err := telemetry.PrometheusHandler(reg)
	if err != nil {
		t.Fatalf("PrometheusHandler: %v", err)
	}
	body := scrape(t, h)
	if !strings.Contains(body, "go_goroutines") {
		t.Errorf("/metrics body missing go_goroutines (Go collector):\n%s", body)
	}
	if !strings.Contains(body, "go_memstats_") {
		t.Errorf("/metrics body missing go_memstats_* (Go collector):\n%s", body)
	}
}

// TestPrometheusHandler_RendersRuntimeGauge is the empirical proof that
// an Int64ObservableGauge registered via RegisterRuntimeGauges actually
// renders in the Prometheus scrape TEXT (not just the ManualReader
// Snapshot path) — i.e. the OTel→prometheus exporter surfaces the
// observable gauge's callback value on /metrics. A scraper (and, by the
// same reader, metrics.snapshot) sees it.
func TestPrometheusHandler_RendersRuntimeGauge(t *testing.T) {
	reg, shutdown, err := telemetry.NewMetricsRegistry(goodCfg())
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	if err := reg.RegisterRuntimeGauges(telemetry.RuntimeGaugeSource{
		ActiveRuns:    func() int64 { return 42 },
		EventsDropped: func() int64 { return 9 },
	}); err != nil {
		t.Fatalf("RegisterRuntimeGauges: %v", err)
	}
	h, err := telemetry.PrometheusHandler(reg)
	if err != nil {
		t.Fatalf("PrometheusHandler: %v", err)
	}
	body := scrape(t, h)
	if !strings.Contains(body, gaugeActiveRuns) {
		t.Errorf("/metrics body missing observable gauge %q:\n%s", gaugeActiveRuns, body)
	}
	// The rendered sample line must carry the callback value. The OTel
	// prometheus exporter renders the series with bounded otel_scope_*
	// labels (constant, not a cardinality concern — the firewall is about
	// run_id/trace_id), so the line is `harbor_runtime_active_runs{...} 42`.
	// Find the sample line (not the # HELP / # TYPE lines) and check its value.
	var sample string
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, gaugeActiveRuns) && !strings.HasPrefix(line, "# ") {
			sample = line
			break
		}
	}
	if !strings.HasSuffix(sample, " 42") {
		t.Errorf("/metrics %q sample line did not carry callback value 42; got %q\nbody:\n%s", gaugeActiveRuns, sample, body)
	}
}

// TestMetricsRegistry_PerRegistryIsolation builds two independent
// prometheus-backed registries and asserts they do not cross-register:
// each registry's runtime gauge reports only its OWN callback value, and
// each /metrics scrape carries its own Go collector without colliding on
// the global prometheus registerer (the per-instance-registry guarantee).
func TestMetricsRegistry_PerRegistryIsolation(t *testing.T) {
	regA, shutA, err := telemetry.NewMetricsRegistry(goodCfg())
	if err != nil {
		t.Fatalf("NewMetricsRegistry A: %v", err)
	}
	defer func() { _ = shutA(context.Background()) }()
	regB, shutB, err := telemetry.NewMetricsRegistry(goodCfg())
	if err != nil {
		t.Fatalf("NewMetricsRegistry B: %v", err)
	}
	defer func() { _ = shutB(context.Background()) }()

	if err := regA.RegisterRuntimeGauges(telemetry.RuntimeGaugeSource{
		ActiveRuns: func() int64 { return 100 },
	}); err != nil {
		t.Fatalf("RegisterRuntimeGauges A: %v", err)
	}
	if err := regB.RegisterRuntimeGauges(telemetry.RuntimeGaugeSource{
		ActiveRuns: func() int64 { return 200 },
	}); err != nil {
		t.Fatalf("RegisterRuntimeGauges B: %v", err)
	}

	gaugeValue := func(reg *telemetry.MetricsRegistry, name string) (float64, bool) {
		t.Helper()
		snap, serr := reg.Snapshot(context.Background())
		if serr != nil {
			t.Fatalf("Snapshot: %v", serr)
		}
		for _, g := range snap.Gauges {
			if g.Name == name {
				return g.Value, true
			}
		}
		return 0, false
	}

	if v, ok := gaugeValue(regA, gaugeActiveRuns); !ok || v != 100 {
		t.Errorf("registry A %s = %v (found=%v), want 100 — registries cross-registered", gaugeActiveRuns, v, ok)
	}
	if v, ok := gaugeValue(regB, gaugeActiveRuns); !ok || v != 200 {
		t.Errorf("registry B %s = %v (found=%v), want 200 — registries cross-registered", gaugeActiveRuns, v, ok)
	}

	// Both scrape surfaces work independently (no global-registerer
	// collision on the second Go collector).
	for _, reg := range []*telemetry.MetricsRegistry{regA, regB} {
		h, herr := telemetry.PrometheusHandler(reg)
		if herr != nil {
			t.Fatalf("PrometheusHandler: %v", herr)
		}
		if body := scrape(t, h); !strings.Contains(body, "go_goroutines") {
			t.Errorf("scrape missing go_goroutines:\n%s", body)
		}
	}
}
