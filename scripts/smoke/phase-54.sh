#!/usr/bin/env bash
# Phase 54 smoke — Protocol task control surface (RFC §5.2, §6.3;
# master-plan Phase 54 detail block; D-072).
#
# Phase 54 creates the Harbor Protocol layer (`internal/protocol/`) and
# ships its task control surface: the ten canonical task-control method
# names, the request/response wire types, the Protocol error codes, the
# Protocol version pin, and the transport-agnostic in-process
# `ControlSurface` handler that maps a Protocol method call onto the
# already-shipped runtime (`start` -> Phase 20 tasks.TaskRegistry; the
# nine controls -> a Phase 52 steering.ControlEvent on the run's inbox).
#
# Phase 54 ships NO wire transport — the SSE+REST binding is Phase 60.
# So the HTTP/Protocol-wire assertions skip with a reason per the
# 404/405/501 -> SKIP convention; the surface is exercised in-process
# via the package + integration tests.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

PROTO_PKG="internal/protocol"

# Run the protocol package tests under -race. Covers unit (the ten
# method names + IsValidMethod/Methods, the wire types' JSON round-trip
# + the version pin, the error codes' stability + the *Error interface,
# Dispatch routing for each of the ten methods, the
# identity/scope/payload/unknown-method failure modes, the runtime-error
# -> Protocol-code mapping) + the D-025 concurrent-reuse test (N>=100
# Dispatch calls against one shared ControlSurface).
if go test -race -count=1 -timeout 180s ./${PROTO_PKG}/... >/dev/null 2>&1; then
    ok 'phase 54: internal/protocol tests pass under -race (10 method names + wire types + error codes + Dispatch routing for each method + identity/scope/payload failure modes + D-025 concurrent-reuse)'
else
    fail 'phase 54: protocol tests failed (run `go test -race ./internal/protocol/...` for detail)'
fi

# Run the Wave 9 wave-end E2E — the ten-method surface composed against
# the REAL Wave 9 runtime surface (pauseresume.Coordinator + Agent
# Registry + steering inbox/RunLoop + the Phase 54 ControlSurface). Real
# drivers on every seam; identity propagation; >=1 failure mode; N>=10
# concurrency stress.
if go test -race -count=1 -timeout 240s -run 'TestE2E_Wave9' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 54: Wave 9 wave-end E2E passes under -race (Protocol-driven HITL run end-to-end + fail-closed-at-the-edge failure modes + N>=10 concurrency stress)'
else
    fail 'phase 54: Wave 9 E2E failed (run `go test -race -run TestE2E_Wave9 ./test/integration/...` for detail)'
fi

# Static guard: the ten canonical task-control method names must be
# present verbatim (RFC §5.2 "Task control" row).
METHODS_FILE="${PROTO_PKG}/methods/methods.go"
if [[ ! -f "${METHODS_FILE}" ]]; then
    fail "phase 54: ${METHODS_FILE} missing"
else
    missing_methods=""
    for m in start cancel pause resume redirect inject_context approve reject prioritize user_message; do
        if ! grep -q "\"${m}\"" "${METHODS_FILE}"; then
            missing_methods="${missing_methods} ${m}"
        fi
    done
    if [[ -n "${missing_methods}" ]]; then
        fail "phase 54: methods.go missing canonical method names:${missing_methods}"
    else
        ok 'phase 54: methods.go declares all ten canonical task-control method names (start / cancel / pause / resume / redirect / inject_context / approve / reject / prioritize / user_message)'
    fi
fi

# Static guard: the Phase 54 control surface must NOT import net/http —
# the wire transport (SSE+REST) is Phase 60; Phase 54 ships the
# transport-agnostic surface only. The Phase 60 wire binding legitimately
# lives under internal/protocol/transports/ and DOES import net/http
# (D-078) — so the guard excludes that subtree: it asserts the
# transport-AGNOSTIC packages (methods/errors/types + the ControlSurface)
# stay net/http-free, not the transport tree built on top of them.
if grep -rIn --include='*.go' '"net/http"' "${PROTO_PKG}/" 2>/dev/null | grep -v '/transports/' | grep -q .; then
    fail 'phase 54: the transport-agnostic Protocol layer imports net/http — the wire transport is Phase 60 and lives under internal/protocol/transports/ (RFC §5.4, D-072, D-078)'
else
    ok 'phase 54: the transport-agnostic Protocol layer does not import net/http (the SSE+REST wire binding is confined to internal/protocol/transports/ — Phase 60, D-078)'
fi

# Import-graph guard: the Protocol layer must NOT import the Console —
# the Runtime never imports Console code (CLAUDE.md §13).
if grep -rIn --include='*.go' '"github.com/hurtener/Harbor/web/console' "${PROTO_PKG}/" 2>/dev/null | grep -q .; then
    fail 'phase 54: internal/protocol imports the Console — the Runtime never imports Console code (CLAUDE.md §13)'
else
    ok 'phase 54: internal/protocol does not import the Console (Runtime/Console boundary preserved)'
fi

# Single-source guard (CLAUDE.md §8): the Protocol version constant is
# defined exactly once, in internal/protocol/types/version.go.
VERSION_FILE="${PROTO_PKG}/types/version.go"
if [[ ! -f "${VERSION_FILE}" ]]; then
    fail "phase 54: ${VERSION_FILE} missing — the Protocol version must be pinned in internal/protocol/types/version.go (CLAUDE.md §8)"
elif ! grep -q 'ProtocolVersion' "${VERSION_FILE}"; then
    fail 'phase 54: internal/protocol/types/version.go does not declare ProtocolVersion'
else
    ok 'phase 54: Protocol version pinned in internal/protocol/types/version.go (single source — CLAUDE.md §8)'
fi

# Single-source guard (CLAUDE.md §8): no Protocol error Code constant is
# declared outside internal/protocol/errors. The codes are the
# client-facing contract; a second definition site is the §13 anti-pattern
# Phase 58 formalises a lint for. The grep excludes internal/protocol/
# transports/ — the Phase 60 wire transport legitimately *consumes* the
# protoerrors.Code TYPE in handler signatures + a Code→HTTP-status table
# (it constructs no new Code constants, which the precise Phase 58 AST
# lint — singlesource — gates exactly).
if grep -rIn --include='*.go' 'protoerrors\.Code(' "${PROTO_PKG}/" 2>/dev/null | grep -v '/errors/' | grep -v '/transports/' | grep -q .; then
    fail 'phase 54: a Protocol error Code is constructed outside internal/protocol/errors — error codes are single-sourced (CLAUDE.md §8)'
else
    ok 'phase 54: Protocol error codes are single-sourced in internal/protocol/errors (CLAUDE.md §8; Phase 58 formalises the AST lint, which also covers internal/protocol/transports/)'
fi

# §13 / §4.4 guard: the ControlSurface is an in-process handler with no
# plausible alternate backend — Phase 54 must NOT mint a driver-registry
# tree (the same call D-070 / D-071 made for the steering primitives).
if [[ -d "${PROTO_PKG}/drivers" ]]; then
    fail 'phase 54: internal/protocol/drivers/ exists — the ControlSurface is an in-process handler with no alternate backend (no §4.4 seam needed — D-072)'
else
    ok 'phase 54: no driver-registry tree under internal/protocol (the ControlSurface is an in-process handler — D-072)'
fi

# §13 guard: pause-family control methods (pause / resume / approve /
# reject) must converge on the unified pauseresume primitive — the
# ControlSurface enqueues a steering.ControlEvent and the Phase 53
# RunLoop routes it through pauseresume.Coordinator. Phase 54 must NOT
# mint a parallel pause coordinator.
if grep -rIn --include='*.go' 'type .*Coordinator .*interface' "${PROTO_PKG}/" 2>/dev/null | grep -q .; then
    fail 'phase 54: internal/protocol declares a pause Coordinator — pause/resume/approve/reject must converge on the unified pauseresume primitive (CLAUDE.md §7 rule 4)'
else
    ok 'phase 54: no parallel pause coordinator under internal/protocol (pause-family control methods map onto steering -> the unified pauseresume primitive — CLAUDE.md §7 rule 4)'
fi

# Phase 54 ships no Protocol/HTTP wire surface — the SSE+REST transport
# binding lands in Phase 60. Skip the wire assertions per the
# 404/405/501 -> SKIP convention.
skip "phase 54: the task control surface is transport-agnostic and in-process-invocable; the SSE+REST wire transport (the HTTP endpoints) lands in Phase 60 (RFC §5.4, D-072)"

smoke_summary
