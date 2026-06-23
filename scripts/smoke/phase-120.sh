#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 120 — observability foundation. Asserts the runtime exposes:
#   - the runtime-gauge instruments + the RegisterRuntimeGauges seam;
#   - the prometheus driver's Go/Process collectors (go_goroutines etc.);
#   - the /metrics pull endpoint mounted on the dev server;
#   - the pprof debug listener kept OFF the Protocol mux (own mux, no
#     net/http/pprof blank import polluting DefaultServeMux);
#   - the loopback-only validation of server.debug_addr.
#
# Static greps run regardless of server state; the live /metrics check
# is guarded by skip_if_404 so the script is a no-op SKIP on a build
# that predates the endpoint.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

METRICS_GO="internal/telemetry/metrics.go"
PROM_GO="internal/telemetry/drivers/prometheus/prometheus.go"
ASSEMBLE_GO="internal/runtime/assemble/assemble.go"
CMD_DEV_GO="cmd/harbor/cmd_dev.go"
EVENTS_GO="internal/events/events.go"
VALIDATE_GO="internal/config/validate.go"

# ── static: the gauge surface in telemetry ─────────────────────────────
assert_grep_present 'metricActiveRuns +=' "$METRICS_GO" \
    "phase 120: runtime active-runs gauge name const present"
assert_grep_present 'metricEngineCapEntries +=' "$METRICS_GO" \
    "phase 120: engine-capacity gauge name const present"
assert_grep_present 'metricGovernanceCacheLen +=' "$METRICS_GO" \
    "phase 120: governance-cache gauge name const present"
assert_grep_present 'metricEventsDroppedTotal +=' "$METRICS_GO" \
    "phase 120: events-dropped gauge name const present"
assert_grep_present 'func \(r \*MetricsRegistry\) RegisterRuntimeGauges' "$METRICS_GO" \
    "phase 120: RegisterRuntimeGauges seam present"
assert_grep_present 'case metricdata.Gauge\[int64\]' "$METRICS_GO" \
    "phase 120: Snapshot projects Gauge[int64] data points"

# ── static: Go + Process collectors on the prometheus scrape ───────────
assert_grep_present 'collectors.NewGoCollector\(\)' "$PROM_GO" \
    "phase 120: prometheus driver registers the Go collector"
assert_grep_present 'collectors.NewProcessCollector\(' "$PROM_GO" \
    "phase 120: prometheus driver registers the process collector"

# ── static: gauges wired at the single shared assembly seam ────────────
assert_grep_present 'RegisterRuntimeGauges\(telemetry.RuntimeGaugeSource' "$ASSEMBLE_GO" \
    "phase 120: runtime gauges wired in the shared assembly"
assert_grep_present 'events.DroppedCounter' "$ASSEMBLE_GO" \
    "phase 120: events-dropped gauge sourced via DroppedCounter capability"

# ── static: the DroppedCounter capability exists ───────────────────────
assert_grep_present 'type DroppedCounter interface' "$EVENTS_GO" \
    "phase 120: events.DroppedCounter capability defined"

# ── static: /metrics mounted on the dev router ─────────────────────────
assert_grep_present 'telemetry.PrometheusHandler\(metricsReg\)' "$CMD_DEV_GO" \
    "phase 120: dev server builds the Prometheus handler"
assert_grep_present 'router.Handle\("/metrics"' "$CMD_DEV_GO" \
    "phase 120: /metrics mounted on the dev router"

# ── static: pprof on its OWN mux, never the Protocol/Default mux ────────
assert_grep_present 'nhpprof.Index' "$CMD_DEV_GO" \
    "phase 120: pprof handlers registered explicitly on a private mux"
assert_grep_present 'debugMux := http.NewServeMux\(\)' "$CMD_DEV_GO" \
    "phase 120: pprof served from a fresh ServeMux (not DefaultServeMux)"
# A blank net/http/pprof import would attach pprof to http.DefaultServeMux
# — forbidden. The explicit nhpprof alias import is what we want instead.
assert_grep_absent '_ "net/http/pprof"' "$CMD_DEV_GO" \
    "phase 120: no blank net/http/pprof import (keeps DefaultServeMux clean)"

# ── static: loopback-only validation of server.debug_addr ──────────────
assert_grep_present 'c.Server.DebugAddr != ""' "$VALIDATE_GO" \
    "phase 120: debug_addr validated when set"
assert_grep_present 'IsLoopback\(\)' "$VALIDATE_GO" \
    "phase 120: debug_addr must be a loopback address"

# ── pprof debug listener is OFF the Protocol mux (static) ──────────────
# Handlers are registered explicitly on a private mux, never blank-imported
# onto http.DefaultServeMux — so nothing pprof lands on the Protocol mux.
DEV_GO="cmd/harbor/cmd_dev.go"
assert_grep_present 'debugMux := http.NewServeMux\(\)' "$DEV_GO" \
    "phase 120: pprof on its own private mux"
assert_grep_present 'nhpprof.Index' "$DEV_GO" \
    "phase 120: pprof handlers registered explicitly (not blank-import)"
assert_grep_absent '_ "net/http/pprof"' "$DEV_GO" \
    "phase 120: net/http/pprof is NOT blank-imported (would leak onto DefaultServeMux)"

# ── live: /metrics serves go_goroutines once the dev server is up; and
#    pprof is ABSENT from the main Protocol/dev mux (the security gate).
#    Both are gated on /metrics being reachable (200), which means the
#    server is up — so a no-server run SKIPs cleanly rather than failing.
if skip_if_404 "$(api_url /metrics)" 'phase 120: /metrics endpoint'; then
    assert_status 200 "$(api_url /metrics)" "phase 120: /metrics returns 200"
    assert_body_contains 'go_goroutines' "$(api_url /metrics)" \
        "phase 120: /metrics exposes go_goroutines"
    # The debug listener (when enabled) binds a separate loopback port; the
    # main mux must never serve /debug/pprof. 404 here is the security gate.
    assert_status 404 "$(api_url /debug/pprof/)" \
        "phase 120: /debug/pprof absent from the Protocol mux"
fi

smoke_summary
