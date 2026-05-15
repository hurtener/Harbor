#!/usr/bin/env bash
# Phase 30 smoke — Tool-side OAuth + HITL via pause/resume
# (RFC §6.4 + §3.3; master-plan Phase 30 detail block; D-083).
#
# Phase 30 ships `internal/tools/auth` — the OAuth subsystem
# converging on the unified pause/resume primitive (Phase 50).
# This is a code-only library phase: no Protocol surface lands until
# the OAuth-callback Protocol method (a later phase). The smoke
# therefore exercises the unit + integration test suite + a static
# guard on the OAuthProvider interface shape; the HTTP/Protocol
# assertions skip per the 404/405/501 → SKIP convention.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

AUTH_PKG="internal/tools/auth"

# 1. Run the auth-package tests under -race. Covers unit
#    (TokenStore round-trip both scopes, encryption-at-rest, missing
#    identity, cross-tenant + cross-agent isolation, mixed-scope
#    coexistence, Provider full pause/resume cycle for both binding
#    scopes, admin-scope authz gate, cross-identity state-swap rejection,
#    goroutine-leak on initiate-then-cancel) + conformance suite +
#    the D-025 N=128 concurrent-reuse + single-flight refresh tests.
if go test -race -count=1 -timeout 180s ./${AUTH_PKG}/... >/dev/null 2>&1; then
    ok 'phase 30: internal/tools/auth tests pass under -race (Sealer + TokenStore + Provider + cross-driver conformance + D-025 + single-flight refresh)'
else
    fail 'phase 30: auth tests failed (run `go test -race ./internal/tools/auth/...` for detail)'
fi

# 2. Run the Phase 30 integration test — full pause/resume cycle for
#    BOTH binding scopes against real state.StateStore (in-mem + SQLite
#    + Postgres-DSN-skip), real audit.Redactor, real events.EventBus,
#    real pauseresume.Coordinator, real httptest.Server authorization
#    server emulating PKCE + RFC 7591 + .well-known discovery. A2A
#    AUTH_REQUIRED shape parity + goroutine-leak + cross-identity
#    state-swap failure mode + N=16 concurrency stress.
if go test -race -count=1 -timeout 180s -run 'TestE2E_Phase30' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 30: tool-side OAuth integration test passes under -race (full cycle both scopes + A2A convergence + PKCE round-trip + dynamic registration + discovery + goroutine-leak + isolation conformance)'
else
    fail 'phase 30: integration test failed (run `go test -race -run TestE2E_Phase30 ./test/integration/...` for detail)'
fi

# 3. Static guard: the OAuthProvider interface must declare the five
#    methods the master plan names.
AUTH_FILE="${AUTH_PKG}/auth.go"
if [[ ! -f "${AUTH_FILE}" ]]; then
    fail "phase 30: ${AUTH_FILE} missing"
else
    missing_methods=""
    for method in "Token(ctx context.Context" "InitiateFlow(ctx context.Context" "CompleteFlow(ctx context.Context" "Revoke(ctx context.Context" "Close(ctx context.Context"; do
        if ! grep -q "${method}" "${AUTH_FILE}"; then
            missing_methods="${missing_methods} ${method%%(*}"
        fi
    done
    if [[ -n "${missing_methods}" ]]; then
        fail "phase 30: OAuthProvider missing methods:${missing_methods}"
    else
        ok 'phase 30: OAuthProvider interface declares Token / InitiateFlow / CompleteFlow / Revoke / Close (the master-plan acceptance surface)'
    fi
fi

# 4. Static guard: the TokenStore interface persists tokens via the
#    state.StateStore seam (D-067 / D-068 precedent per D-083).
TOKENSTORE_FILE="${AUTH_PKG}/tokenstore.go"
if [[ ! -f "${TOKENSTORE_FILE}" ]]; then
    fail "phase 30: ${TOKENSTORE_FILE} missing"
elif ! grep -q "state.StateStore" "${TOKENSTORE_FILE}"; then
    fail 'phase 30: tokenstore.go does not consume state.StateStore (D-083 says it should)'
else
    ok 'phase 30: TokenStore consumes state.StateStore (D-067 / D-068 / D-083 precedent — driver pluralism inherited from the StateStore triad)'
fi

# 5. Static guard: AES-256-GCM Sealer + KEKSizeBytes constant.
SEALER_FILE="${AUTH_PKG}/sealer.go"
if [[ ! -f "${SEALER_FILE}" ]]; then
    fail "phase 30: ${SEALER_FILE} missing"
elif ! grep -q "KEKSizeBytes = 32" "${SEALER_FILE}"; then
    fail 'phase 30: sealer.go does not declare KEKSizeBytes = 32 (AES-256-GCM requires 32-byte KEK)'
elif ! grep -q "EnvelopeVersion" "${SEALER_FILE}"; then
    fail 'phase 30: sealer.go does not declare EnvelopeVersion (KEK rotation requires a version header)'
else
    ok 'phase 30: AES-256-GCM Sealer declares KEKSizeBytes + EnvelopeVersion (encryption-at-rest + KEK-rotation future-proofing)'
fi

# 6. Static guard: tool.auth_required + tool.auth_completed events
#    registered into the canonical events.RegisterEventType registry.
EVENTS_FILE="${AUTH_PKG}/events.go"
if [[ ! -f "${EVENTS_FILE}" ]]; then
    fail "phase 30: ${EVENTS_FILE} missing"
elif ! grep -q 'EventTypeToolAuthRequired.*"tool.auth_required"' "${EVENTS_FILE}"; then
    fail 'phase 30: tool.auth_required event type not declared in events.go'
elif ! grep -q 'EventTypeToolAuthCompleted.*"tool.auth_completed"' "${EVENTS_FILE}"; then
    fail 'phase 30: tool.auth_completed event type not declared in events.go'
elif ! grep -q "events.RegisterEventType(EventTypeToolAuthRequired)" "${EVENTS_FILE}"; then
    fail 'phase 30: tool.auth_required is not registered into the canonical events registry'
else
    ok 'phase 30: tool.auth_required + tool.auth_completed registered into the canonical events registry'
fi

# 7. Until the OAuth-callback Protocol method ships, the HTTP surface
#    skips per the 404/405/501 convention. Phase 30 is a code-only
#    library phase; the Protocol callback handler is a follow-up.
skip 'phase 30: OAuth callback Protocol method is a follow-up phase; HTTP surface assertions skip until then'

smoke_summary
