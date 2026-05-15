package telemetry_test

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

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/telemetry"

	// Blank-import the metric-exporter drivers so the registry resolves
	// "prometheus" / "otlpmetric" — same self-registration path
	// cmd/harbor uses.
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/otlpmetric"
	_ "github.com/hurtener/Harbor/internal/telemetry/drivers/prometheus"
)

// goodCfg is a minimal valid TelemetryConfig for metrics construction.
func goodCfg() config.TelemetryConfig {
	return config.TelemetryConfig{
		LogFormat:   "json",
		LogLevel:    "info",
		ServiceName: "harbor-test",
	}
}

// quad builds an identity quadruple for an event. The metrics layer
// must NEVER read it onto a label — the tests assert that.
func quad(tenant, user, session, run string) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  tenant,
			UserID:    user,
			SessionID: session,
		},
		RunID: run,
	}
}

// ev builds an events.Event with the given type, identity quadruple,
// and Extra map. RegisterEvent reads only Type + Extra.
func ev(t events.EventType, q identity.Quadruple, extra map[string]string) events.Event {
	return events.Event{Type: t, Identity: q, Extra: extra}
}

// collectSum reads the harbor_events_total counter from a ManualReader
// and returns a map from "event_type|producer|node" to the summed
// int64 value across data points with that label triple.
func collectSum(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("ManualReader.Collect: %v", err)
	}
	out := map[string]int64{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			if m.Name != "harbor_events_total" {
				continue
			}
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("harbor_events_total is %T, want metricdata.Sum[int64]", m.Data)
			}
			for _, dp := range sum.DataPoints {
				var et, pr, nd string
				for _, kv := range dp.Attributes.ToSlice() {
					switch string(kv.Key) {
					case "event_type":
						et = kv.Value.AsString()
					case "producer":
						pr = kv.Value.AsString()
					case "node":
						nd = kv.Value.AsString()
					}
				}
				out[et+"|"+pr+"|"+nd] += dp.Value
			}
		}
	}
	return out
}

func TestNewMetricsRegistry_EmptyEndpointSelectsPrometheus(t *testing.T) {
	reg, shutdown, err := telemetry.NewMetricsRegistry(goodCfg())
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}
	if reg == nil {
		t.Fatal("NewMetricsRegistry returned a nil registry")
	}
	if shutdown == nil {
		t.Fatal("NewMetricsRegistry returned a nil shutdown func")
	}
	// The prometheus driver was selected — PrometheusHandler must work.
	if _, err := telemetry.PrometheusHandler(reg); err != nil {
		t.Fatalf("PrometheusHandler on a prometheus-backed registry: %v", err)
	}
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
}

func TestNewMetricsRegistry_NonEmptyEndpointSelectsOTLP(t *testing.T) {
	cfg := goodCfg()
	cfg.OTelEndpoint = "127.0.0.1:4317" // lazy-connect; no live collector needed
	reg, shutdown, err := telemetry.NewMetricsRegistry(cfg)
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	// The otlpmetric driver was selected — it has no pull surface, so
	// PrometheusHandler must fail loudly.
	if _, err := telemetry.PrometheusHandler(reg); !errors.Is(err, telemetry.ErrPrometheusHandlerUnavailable) {
		t.Fatalf("PrometheusHandler on an otlpmetric-backed registry: want ErrPrometheusHandlerUnavailable, got %v", err)
	}
}

func TestNewMetricsRegistry_EmptyServiceNameFailsLoudly(t *testing.T) {
	cfg := goodCfg()
	cfg.ServiceName = ""
	_, _, err := telemetry.NewMetricsRegistry(cfg)
	if !errors.Is(err, telemetry.ErrMetricsNotConfigured) {
		t.Fatalf("want ErrMetricsNotConfigured, got %v", err)
	}
}

func TestNewMetricsRegistry_UnknownDriverListsRegistered(t *testing.T) {
	_, _, err := telemetry.NewMetricsRegistry(goodCfg(),
		telemetry.WithMetricExporterDriver("nope-not-real"))
	if !errors.Is(err, telemetry.ErrMetricExporterUnknown) {
		t.Fatalf("want ErrMetricExporterUnknown, got %v", err)
	}
	// The error message must name the registered drivers so a
	// misconfiguration is obvious.
	msg := err.Error()
	for _, want := range []string{"otlpmetric", "prometheus"} {
		if !strings.Contains(msg, want) {
			t.Errorf("ErrMetricExporterUnknown message %q missing registered driver %q", msg, want)
		}
	}
}

func TestRegisterEvent_DerivesLabelsFromTypeAndExtra(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	reg, shutdown, err := telemetry.NewMetricsRegistry(goodCfg(),
		telemetry.WithMetricReader(reader))
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	q := quad("t1", "u1", "s1", "run-abc")
	reg.RegisterEvent(context.Background(), ev(events.EventTypeRuntimeError, q,
		map[string]string{"producer": "planner", "node": "react"}))
	reg.RegisterEvent(context.Background(), ev(events.EventTypeRuntimeError, q,
		map[string]string{"producer": "planner", "node": "react"}))

	got := collectSum(t, reader)
	key := "runtime.error|planner|react"
	if got[key] != 2 {
		t.Fatalf("harbor_events_total{%s} = %d, want 2 (full map: %v)", key, got[key], got)
	}
}

func TestRegisterEvent_AbsentExtraUsesProducerUnknownAndEmptyNode(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	reg, shutdown, err := telemetry.NewMetricsRegistry(goodCfg(),
		telemetry.WithMetricReader(reader))
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	q := quad("t1", "u1", "s1", "run-1")
	// No Extra map at all.
	reg.RegisterEvent(context.Background(), ev(events.EventTypeRuntimeWarning, q, nil))

	got := collectSum(t, reader)
	key := "runtime.warning|unknown|"
	if got[key] != 1 {
		t.Fatalf("harbor_events_total{%s} = %d, want 1 (full map: %v)", key, got[key], got)
	}
}

// TestRegisterEvent_NeverTagsByIdentity is the cardinality firewall
// assertion: an event carrying a full identity quadruple — including a
// distinctive RunID — produces a metric series whose labels carry NONE
// of the quadruple's values.
func TestRegisterEvent_NeverTagsByIdentity(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	reg, shutdown, err := telemetry.NewMetricsRegistry(goodCfg(),
		telemetry.WithMetricReader(reader))
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	const distinctiveRun = "RUN-DISTINCTIVE-9f8e7d"
	q := quad("TENANT-XYZ", "USER-XYZ", "SESSION-XYZ", distinctiveRun)
	reg.RegisterEvent(context.Background(), ev(events.EventTypeRuntimeError, q,
		map[string]string{"producer": "tool:fetch"}))

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			sum, ok := m.Data.(metricdata.Sum[int64])
			if !ok {
				continue
			}
			for _, dp := range sum.DataPoints {
				for _, kv := range dp.Attributes.ToSlice() {
					k := string(kv.Key)
					v := kv.Value.AsString()
					// No label KEY may be an identity field.
					switch k {
					case "run_id", "trace_id", "span_id", "task_id",
						"tenant_id", "user_id", "session_id":
						t.Errorf("metric carries forbidden identity label key %q", k)
					}
					// No label VALUE may equal any quadruple component.
					for _, forbidden := range []string{
						distinctiveRun, "TENANT-XYZ", "USER-XYZ", "SESSION-XYZ",
					} {
						if v == forbidden {
							t.Errorf("metric label %q=%q leaks an identity value", k, v)
						}
					}
				}
			}
		}
	}
}

func TestPrometheusHandler_ServesHarborEventsTotal(t *testing.T) {
	reg, shutdown, err := telemetry.NewMetricsRegistry(goodCfg())
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	q := quad("t1", "u1", "s1", "run-1")
	reg.RegisterEvent(context.Background(), ev(events.EventTypeBusDropped, q,
		map[string]string{"producer": "runtime", "node": "bus"}))

	h, err := telemetry.PrometheusHandler(reg)
	if err != nil {
		t.Fatalf("PrometheusHandler: %v", err)
	}
	body := scrape(t, h)
	if !strings.Contains(body, "harbor_events_total") {
		t.Fatalf("/metrics body missing harbor_events_total:\n%s", body)
	}
	if !strings.Contains(body, `event_type="bus.dropped"`) {
		t.Fatalf("/metrics body missing event_type label:\n%s", body)
	}
	// The cardinality firewall holds at the exposition surface too.
	for _, forbidden := range []string{"run_id", "trace_id", "run-1"} {
		if strings.Contains(body, forbidden) {
			t.Errorf("/metrics body leaks forbidden token %q:\n%s", forbidden, body)
		}
	}
}

func TestPrometheusHandler_NilRegistryFailsLoudly(t *testing.T) {
	if _, err := telemetry.PrometheusHandler(nil); !errors.Is(err, telemetry.ErrPrometheusHandlerUnavailable) {
		t.Fatalf("want ErrPrometheusHandlerUnavailable for nil registry, got %v", err)
	}
}

// TestConcurrentReuse_MetricsRegistry is the D-025 contract: N≥100
// goroutines each call RegisterEvent against ONE shared *MetricsRegistry
// with a goroutine-unique identity quadruple AND goroutine-unique
// producer/node, under -race. Asserts: no data races (the -race gate),
// no label cross-talk (each goroutine's series carries its own
// producer/node and never any identity value), no goroutine leak
// (NumGoroutine baseline-restored after shutdown).
func TestConcurrentReuse_MetricsRegistry(t *testing.T) {
	const n = 150

	baseline := runtime.NumGoroutine()

	reader := sdkmetric.NewManualReader()
	reg, shutdown, err := telemetry.NewMetricsRegistry(goodCfg(),
		telemetry.WithMetricReader(reader))
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			// Goroutine-unique identity (must NOT reach a label) and
			// goroutine-unique producer/node (MUST reach the label set,
			// each its own series).
			q := quad(
				fmt.Sprintf("tenant-%d", i),
				fmt.Sprintf("user-%d", i),
				fmt.Sprintf("session-%d", i),
				fmt.Sprintf("run-%d", i),
			)
			extra := map[string]string{
				"producer": fmt.Sprintf("producer-%d", i),
				"node":     fmt.Sprintf("node-%d", i),
			}
			reg.RegisterEvent(context.Background(), ev(events.EventTypeRuntimeError, q, extra))
		}(i)
	}
	wg.Wait()

	got := collectSum(t, reader)
	// Every goroutine's own series must be present with exactly count 1
	// — no cross-talk, no lost increments.
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("runtime.error|producer-%d|node-%d", i, i)
		if got[key] != 1 {
			t.Fatalf("series %q = %d, want 1 (label cross-talk or lost increment)", key, got[key])
		}
	}
	// No series may carry an identity-shaped label value.
	for key := range got {
		if strings.Contains(key, "run-") || strings.Contains(key, "tenant-") ||
			strings.Contains(key, "session-") {
			t.Errorf("series key %q leaks an identity value into a label", key)
		}
	}

	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// Goroutine-leak check: baseline restored within a bounded window.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline {
		if time.Now().After(deadline) {
			t.Fatalf("goroutine leak: NumGoroutine=%d, baseline=%d", runtime.NumGoroutine(), baseline)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// scrape drives an http.Handler with a GET / and returns the body.
func scrape(t *testing.T, h http.Handler) string {
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
