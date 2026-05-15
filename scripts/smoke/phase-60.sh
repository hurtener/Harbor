#!/usr/bin/env bash
# Phase 60 smoke — Protocol wire transport (SSE + REST) (RFC §5.4, §11
# Q-1; master-plan Phase 60 detail block; D-078).
#
# Phase 60 binds the transport-agnostic Protocol surface onto the wire:
# a REST/JSON control surface (POST /v1/control/{method}) over Phase
# 54's protocol.ControlSurface, and an SSE event stream (GET /v1/events)
# over Phase 05's events.EventBus. RFC §11 Q-1 resolved 2026-05-14 to
# SSE + REST, so Phase 60 is a normal implementation phase.
#
# There is no live HTTP server in the binary yet — `harbor dev` (the
# server that mounts the transport mux) is Phase 64. So the live-HTTP
# assertions skip per the 404/405/501 -> SKIP convention; the wire
# surface is exercised end-to-end via httptest in the package +
# integration tests, which this smoke runs. Both directions (SSE event
# stream out, REST control in) are covered by the E2E.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

TRANSPORTS_PKG="internal/protocol/transports"

# Run the transport package tests under -race. Covers: the REST control
# handler (route validation, decode, identity-edge rejection, the
# errors.Code -> HTTP status table, JSON response shape), the SSE stream
# handler (frame encoding, identity-edge rejection, triple-scoped
# no-cross-talk, keepalive emission, Last-Event-ID reconnect replay),
# NewMux composition, the D-025 concurrent-reuse test (N>=100 mixed
# requests against one shared mux), and the goroutine-leak test.
if go test -race -count=1 -timeout 180s ./${TRANSPORTS_PKG}/... >/dev/null 2>&1; then
    ok 'phase 60: internal/protocol/transports tests pass under -race (REST control handler + SSE stream handler + NewMux + D-025 concurrent-reuse + goroutine-leak)'
else
    fail 'phase 60: transport tests failed (run `go test -race ./internal/protocol/transports/...` for detail)'
fi

# Run the Phase 60 wire-transport E2E — the two transports composed
# against the REAL runtime surface (protocol.ControlSurface over a real
# inprocess tasks.TaskRegistry + a real in-mem events.EventBus). Both
# directions over the wire: a client opens the SSE event stream, submits
# `start` over REST, and observes the task.spawned lifecycle event on
# its stream. Includes the missing-identity fail-closed mode + a
# full-duplex N>=10 concurrency stress.
if go test -race -count=1 -timeout 240s -run 'TestE2E_Phase60' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 60: wire-transport E2E passes under -race (SSE events out + REST control in, both directions end-to-end + fail-closed-at-the-edge + N>=10 full-duplex stress)'
else
    fail 'phase 60: wire-transport E2E failed (run `go test -race -run TestE2E_Phase60 ./test/integration/...` for detail)'
fi

# Static guard: the transport sub-packages exist (the §3 layout —
# transports/{control,stream}).
for sub in control stream; do
    if [[ -d "${TRANSPORTS_PKG}/${sub}" ]]; then
        ok "phase 60: ${TRANSPORTS_PKG}/${sub} exists (the §3 transport layout)"
    else
        fail "phase 60: ${TRANSPORTS_PKG}/${sub} missing — RFC §5.4 / CLAUDE.md §3 pin transports under internal/protocol/transports/{stream,control}"
    fi
done

# Static guard: transports.go declares NewMux — the seam a future server
# (harbor dev, Phase 64) mounts; structured so a WebSocket transport is
# additive, not a fork (RFC §5.4).
if grep -q 'func NewMux' "${TRANSPORTS_PKG}/transports.go" 2>/dev/null; then
    ok 'phase 60: internal/protocol/transports declares NewMux (the SSE+REST composition seam — WebSocket stays additive)'
else
    fail 'phase 60: internal/protocol/transports/transports.go does not declare NewMux'
fi

# Single-source guard (CLAUDE.md §8, defence-in-depth over the Phase 58
# lint): no Protocol error Code constant is constructed under the
# transport tree.
if grep -rIn --include='*.go' 'protoerrors\.Code(' "${TRANSPORTS_PKG}/" 2>/dev/null | grep -v '_test.go' | grep -q .; then
    fail 'phase 60: a Protocol error Code is constructed under internal/protocol/transports — error codes are single-sourced in internal/protocol/errors (CLAUDE.md §8)'
else
    ok 'phase 60: no Protocol error Code redefined under internal/protocol/transports (single-source preserved — CLAUDE.md §8)'
fi

# Import-graph guard: the transport layer must NOT import the Console —
# the Runtime never imports Console code (CLAUDE.md §13).
if grep -rIn --include='*.go' '"github.com/hurtener/Harbor/web/console' "${TRANSPORTS_PKG}/" 2>/dev/null | grep -q .; then
    fail 'phase 60: internal/protocol/transports imports the Console — the Runtime never imports Console code (CLAUDE.md §13)'
else
    ok 'phase 60: internal/protocol/transports does not import the Console (Runtime/Console boundary preserved)'
fi

# Phase 60 ships no live HTTP server — `harbor dev` (the server that
# mounts the transport mux) is Phase 64. Skip the live-wire assertions
# per the 404/405/501 -> SKIP convention; the wire surface is exercised
# via httptest in the package + integration tests above.
skip "phase 60: the SSE + REST transports are exercised end-to-end via httptest in the package + integration tests; the live HTTP server that mounts the transport mux (\`harbor dev\`) lands in Phase 64"

smoke_summary
