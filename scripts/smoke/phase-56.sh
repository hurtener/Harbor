#!/usr/bin/env bash
# Phase 56 smoke — metrics + OTLP + Prometheus drivers.
#
# Phase 56 ships the telemetry MetricsRegistry (canonical counters
# derived from events.Event, keyed by event_type / producer / node
# only — never by RunID / TraceID), the §4.4 metric-exporter driver
# seam (otlpmetric default + prometheus), the built-in Prometheus
# /metrics http.Handler, and a go/parser static cardinality-lint that
# fails CI on a RunID / TraceID metric label. The Runtime server that
# mounts /metrics at a live endpoint is a later phase — correctness
# here is verified by `go test`: this smoke runs the metrics + driver
# + cardinality-lint unit tests and the Phase 56 cross-subsystem
# integration test (which exercises the /metrics httptest surface)
# under -race, so the preflight gate catches a regression the moment
# it happens.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. Telemetry metrics + driver + cardinality-lint packages.
if [ -f "internal/telemetry/metrics.go" ]; then
    if go test -race ./internal/telemetry/... >/tmp/phase-56-telemetry.log 2>&1; then
        ok "phase 56: telemetry metrics + driver + cardinality-lint tests pass (-race)"
    else
        fail "phase 56: telemetry metrics/driver/cardinality-lint tests failed"
        printf -- '--- go test output ---\n'
        cat /tmp/phase-56-telemetry.log
    fi
else
    skip "phase 56: internal/telemetry/metrics.go absent (metrics registry not yet implemented)"
fi

# 2. Static cardinality-lint — the build gate that fails on a RunID /
#    TraceID metric label. Run it explicitly so a regression is
#    obviously a cardinality breach in the smoke output.
if [ -f "internal/telemetry/cardinalitylint/cardinalitylint.go" ]; then
    if go test -race -run TestCardinalityLint_TelemetryTreeIsClean ./internal/telemetry/cardinalitylint/ >/tmp/phase-56-cardinality.log 2>&1; then
        ok "phase 56: cardinality-lint clean — no RunID/TraceID-derived metric labels"
    else
        fail "phase 56: cardinality-lint found a forbidden high-cardinality metric label"
        printf -- '--- go test output ---\n'
        cat /tmp/phase-56-cardinality.log
    fi
else
    skip "phase 56: internal/telemetry/cardinalitylint absent (lint not yet implemented)"
fi

# 3. Phase 56 cross-subsystem integration test (events bus + Logger +
#    MetricsRegistry + /metrics httptest).
if [ -f "test/integration/phase56_metrics_test.go" ]; then
    if go test -race -run TestE2E_Phase56 ./test/integration/ >/tmp/phase-56-integration.log 2>&1; then
        ok "phase 56: cross-subsystem integration test passes (-race)"
    else
        fail "phase 56: cross-subsystem integration test failed"
        printf -- '--- go test output ---\n'
        cat /tmp/phase-56-integration.log
    fi
else
    skip "phase 56: test/integration/phase56_metrics_test.go absent"
fi

# 4. Live Prometheus /metrics endpoint. Phase 56 ships PrometheusHandler
#    as a standalone http.Handler; the Runtime server that mounts it at
#    /metrics is the Phase 60+ bootstrap. There is no `harbor dev`
#    server to hit yet (cmd/harbor is a stub until Phase 09+), so
#    preflight does not boot one and the live endpoint is unreachable
#    rather than 404 — the 404/405/501 SKIP convention does not apply to
#    a connection-refused. The integration test in step 3 already proves
#    the handler serves the right shape via httptest, so this line is a
#    documented SKIP, identical in spirit to phase-55.sh's no-endpoint
#    line. It flips to a real assert_status once the Phase 60+ server
#    bootstrap mounts the handler.
skip "phase 56: Prometheus /metrics has no live Runtime endpoint yet (server bootstrap is Phase 60+; httptest coverage is in the integration test)"

smoke_summary
