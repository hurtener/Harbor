#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 55 smoke — OTel traces + propagation.
#
# Phase 55 ships the telemetry Tracer wrapper, the W3C TraceContext
# propagation carriers (traceparent HTTP / _meta MCP / HARBOR_TRACEPARENT
# env), and the §4.4 span-exporter driver seam (noop + otlp). There is
# no HTTP / Protocol surface — the Console consumes traces via the
# Protocol in a later phase. Correctness is verified by `go test`:
# this smoke runs the tracing + propagation + driver unit tests and
# the Phase 55 cross-subsystem integration test under -race, so the
# preflight gate catches a regression the moment it happens.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. Telemetry tracing + propagation + driver packages.
if [ -f "internal/telemetry/tracing.go" ]; then
    if go test -race ./internal/telemetry/... >/tmp/phase-55-telemetry.log 2>&1; then
        ok "phase 55: telemetry tracing + propagation + driver tests pass (-race)"
    else
        fail "phase 55: telemetry tracing/propagation/driver tests failed"
        printf -- '--- go test output ---\n'
        cat /tmp/phase-55-telemetry.log
    fi
else
    skip "phase 55: internal/telemetry/tracing.go absent (tracer not yet implemented)"
fi

# 2. Phase 55 cross-subsystem integration test (events bus + Logger + Tracer).
if [ -f "test/integration/phase55_otel_test.go" ]; then
    if go test -race -run TestE2E_Phase55 ./test/integration/ >/tmp/phase-55-integration.log 2>&1; then
        ok "phase 55: cross-subsystem integration test passes (-race)"
    else
        fail "phase 55: cross-subsystem integration test failed"
        printf -- '--- go test output ---\n'
        cat /tmp/phase-55-integration.log
    fi
else
    skip "phase 55: test/integration/phase55_otel_test.go absent"
fi

# Phase 55 has no live HTTP/Protocol endpoint — traces reach the
# Console via the Protocol in a later phase, not via a Runtime REST
# surface here.
skip "phase 55: OTel tracing has no HTTP/Protocol surface yet (Protocol trace export is a later phase)"

smoke_summary
