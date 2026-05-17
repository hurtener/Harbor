#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 62 smoke — Protocol conformance suite (RFC §5; master-plan Phase
# 62 detail block; D-080).
#
# Phase 62 is Wave 10's primitive-with-consumer closer for the Protocol
# layer: a single conformance suite the protocol surface passes. It
# exhaustively exercises every Protocol method (the ten canonical
# task-control methods), every Protocol error code (the eight codes
# including Phase 61's CodeAuthRejected), every event-filter shape, the
# Phase 59 versioning + capability handshake, and the Phase 61 auth
# pipeline — against TWO transports: the in-process ControlSurface AND
# the Phase 60 wire mux under httptest.Server.
#
# The smoke runs the package + the Wave 10 wave-end E2E under -race plus
# the static guards that single-source / Console-boundary discipline is
# preserved.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

CONFORMANCE_PKG="internal/protocol/conformance"
ERRORS_PKG="internal/protocol/errors"

# Run the conformance suite under -race. The suite is exhaustive — every
# canonical Protocol method, every error code, every event-filter shape,
# the version handshake pin, the auth pipeline pin, and the D-025
# concurrent-reuse scenario (N≥100 mixed-method invocations).
if go test -race -count=1 -timeout 240s ./${CONFORMANCE_PKG}/... >/dev/null 2>&1; then
    ok 'phase 62: internal/protocol/conformance suite passes under -race (method matrix + error-code matrix + event-filter matrix + version handshake + auth pipeline + D-025 N=100)'
else
    fail 'phase 62: conformance suite failed (run `go test -race ./internal/protocol/conformance/...` for detail)'
fi

# Run the Wave 10 wave-end E2E — the cross-subsystem integration test
# composed across the full Wave 10 surface: real telemetry tracer +
# metrics + durable event log + Protocol single-source + versioning +
# wire transport + auth, with the conformance suite as the exhaustive
# consumer. Identity propagation through every layer; ≥1 failure mode;
# N≥10 concurrency stress.
if go test -race -count=1 -timeout 240s -run 'TestE2E_Wave10' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 62: Wave 10 wave-end E2E passes under -race (real-driver surface + identity propagation + failure modes + N>=10 stress)'
else
    fail 'phase 62: Wave 10 wave-end E2E failed (run `go test -race -run TestE2E_Wave10 ./test/integration/...` for detail)'
fi

# Static guard: the conformance package exists at the documented §3
# location.
if [[ -d "${CONFORMANCE_PKG}" ]]; then
    ok "phase 62: ${CONFORMANCE_PKG} exists (the §3 conformance package layout)"
else
    fail "phase 62: ${CONFORMANCE_PKG} missing — RFC §5 / Phase 62 plan pin the suite at internal/protocol/conformance"
fi

# Static guard: conformance.go declares RunSuite — the binding consumer
# entry point a `harbor lint` subcommand (later phase) or any future
# Protocol transport's conformance test wires through.
if grep -q '^func RunSuite' "${CONFORMANCE_PKG}/conformance.go" 2>/dev/null; then
    ok "phase 62: ${CONFORMANCE_PKG}/conformance.go declares RunSuite (the consumer entry point)"
else
    fail "phase 62: ${CONFORMANCE_PKG}/conformance.go does not declare RunSuite"
fi

# Single-source guard (CLAUDE.md §8, defence-in-depth over the Phase 58
# lint): no Protocol error Code constant is constructed under the
# conformance tree. The only legitimate use is reading a Code from the
# canonical errors package via the imported alias.
if grep -rIn --include='*.go' 'protoerrors\.Code(' "${CONFORMANCE_PKG}/" 2>/dev/null | grep -v '_test.go' | grep -q .; then
    fail 'phase 62: a Protocol error Code is constructed under internal/protocol/conformance — error codes are single-sourced in internal/protocol/errors (CLAUDE.md §8)'
else
    ok 'phase 62: no Protocol error Code redefined under internal/protocol/conformance (single-source preserved — CLAUDE.md §8)'
fi

# Import-graph guard: the conformance layer must NOT import the Console
# — the Runtime never imports Console code (CLAUDE.md §13).
if grep -rIn --include='*.go' '"github.com/hurtener/Harbor/web/console' "${CONFORMANCE_PKG}/" 2>/dev/null | grep -q .; then
    fail 'phase 62: internal/protocol/conformance imports the Console — the Runtime never imports Console code (CLAUDE.md §13)'
else
    ok 'phase 62: internal/protocol/conformance does not import the Console (Runtime/Console boundary preserved)'
fi

# Static guard: the conformance package consumes (imports) the
# canonical errors package. A conformance suite that did not import
# errors and instead duplicated codes would defeat its purpose.
if grep -q 'internal/protocol/errors' "${CONFORMANCE_PKG}/conformance.go" 2>/dev/null; then
    ok "phase 62: ${CONFORMANCE_PKG} consumes ${ERRORS_PKG} (every Code asserted in the matrix is the canonical declaration)"
else
    fail "phase 62: ${CONFORMANCE_PKG} does not consume ${ERRORS_PKG} — the error-code matrix must reference the canonical errors package"
fi

# Phase 62 ships no live HTTP server — `harbor dev` (Phase 64) is the
# server that mounts the transport mux + the middleware. The smoke
# exercises the conformance suite end-to-end via httptest.Server inside
# the test process; the live-wire assertions skip per the 404/405/501 →
# SKIP convention.
skip "phase 62: the conformance suite runs end-to-end via httptest.Server inside the test process; the live HTTP server that mounts the transport mux + middleware (\`harbor dev\`) lands in Phase 64"

smoke_summary
