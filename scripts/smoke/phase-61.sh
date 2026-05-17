#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 61 smoke — Protocol auth + identity-scope enforcement (RFC §5.5,
# §4; master-plan Phase 61 detail block; D-079).
#
# Phase 61 turns the Phase 60 wire transports' trust-based identity
# carriers into cryptographically verified ones: a JWT validator + an
# http.Handler middleware sit at the Phase 60 transport edge. Every
# request carries a JWT signed with one of the six asymmetric algorithms
# (RS256/RS384/RS512/ES256/ES384/ES512); HS* and `none` are rejected at
# the parser level; the (tenant, user, session) triple flows out of the
# JWT claims into the request context.Context; extended scope claims
# (admin, console:fleet) gate cross-session / cross-tenant subscriptions.
#
# There is no live HTTP server in the binary yet — `harbor dev` (the
# server that mounts the transport mux + the middleware) is Phase 64. So
# the live-HTTP assertions skip per the 404/405/501 -> SKIP convention;
# the auth surface is exercised end-to-end via httptest in the package +
# integration + security tests, which this smoke runs.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

AUTH_PKG="internal/protocol/auth"
TRANSPORTS_PKG="internal/protocol/transports"
ERRORS_PKG="internal/protocol/errors"

# Run the auth package tests under -race. Covers: the Validator (parser
# rejects HS*/alg:none at WithValidMethods, every typed sentinel
# exercised), the middleware (bearer parse, identity injection, audit on
# rejection), the scope helpers (WithScopes/HasScope), the security
# suite (algorithm-confusion attacks + alg:none + scope-escalation +
# expired/nbf), and the D-025 concurrent-reuse + goroutine-leak test.
if go test -race -count=1 -timeout 180s ./${AUTH_PKG}/... >/dev/null 2>&1; then
    ok 'phase 61: internal/protocol/auth tests pass under -race (Validator + middleware + scopes + security suite + D-025 + leak)'
else
    fail 'phase 61: auth tests failed (run `go test -race ./internal/protocol/auth/...` for detail)'
fi

# Run the transports tests too — Phase 61 deepens transports.go (the
# WithValidator option), control/status.go (CodeAuthRejected -> 401), and
# stream/stream.go (ctx-first identity + admin scope gate). The full
# phase-60 surface must keep passing (no regression).
if go test -race -count=1 -timeout 180s ./${TRANSPORTS_PKG}/... >/dev/null 2>&1; then
    ok 'phase 61: internal/protocol/transports tests pass under -race (Phase 60 surface preserved + Phase 61 WithValidator + ctx-first identity + admin scope gate)'
else
    fail 'phase 61: transports tests failed after Phase 61 deepening (run `go test -race ./internal/protocol/transports/...` for detail)'
fi

# Run the Phase 61 auth E2E — the auth middleware composed with the
# Phase 60 transports, against the REAL runtime surface (a real
# protocol.ControlSurface over a real inprocess tasks.TaskRegistry + a
# real in-mem events.EventBus). A real ES256 keypair from testdata/
# signs the bearer; the test exercises the happy path (valid bearer →
# start → SSE event arrives) plus every documented rejection mode
# (no token, HS256, alg:none, expired, identity-claim mismatch,
# ?admin=1 without the scope).
if go test -race -count=1 -timeout 240s -run 'TestE2E_Phase61' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 61: auth E2E passes under -race (valid bearer round-trip + 6 rejection modes + N>=10 stress)'
else
    fail 'phase 61: auth E2E failed (run `go test -race -run TestE2E_Phase61 ./test/integration/...` for detail)'
fi

# Static guard: the auth sub-package exists (the §3 layout — auth as a
# peer of transports/{control,stream}).
if [[ -d "${AUTH_PKG}" ]]; then
    ok "phase 61: ${AUTH_PKG} exists (the §3 auth layout)"
else
    fail "phase 61: ${AUTH_PKG} missing — RFC §5.5 / CLAUDE.md §3 pin Protocol auth under internal/protocol/auth"
fi

# Static guard: auth.go declares Validator + Middleware — the surface a
# future server (harbor dev, Phase 64) wraps the transport mux with.
for sym in 'Validator' 'NewValidator' 'Middleware'; do
    if grep -q "func ${sym}\|type ${sym}" "${AUTH_PKG}/auth.go" "${AUTH_PKG}/middleware.go" 2>/dev/null; then
        ok "phase 61: ${AUTH_PKG} declares ${sym} (the JWT-edge surface)"
    else
        fail "phase 61: ${AUTH_PKG} does not declare ${sym}"
    fi
done

# Static guard: transports.go declares WithValidator — the option that
# threads the validator into NewMux, wrapping both handlers in
# auth.Middleware. The Phase 60 mux composition seam Phase 61 extends.
if grep -q 'func WithValidator' "${TRANSPORTS_PKG}/transports.go" 2>/dev/null; then
    ok "phase 61: ${TRANSPORTS_PKG}/transports.go declares WithValidator (NewMux composition seam — auth wraps both handlers)"
else
    fail "phase 61: ${TRANSPORTS_PKG}/transports.go does not declare WithValidator"
fi

# Static guard: the new error code is single-sourced in
# internal/protocol/errors (CLAUDE.md §8).
if grep -q 'CodeAuthRejected' "${ERRORS_PKG}/errors.go" 2>/dev/null; then
    ok "phase 61: ${ERRORS_PKG}/errors.go declares CodeAuthRejected (single-source preserved)"
else
    fail "phase 61: ${ERRORS_PKG}/errors.go missing CodeAuthRejected — Protocol error codes are single-sourced (CLAUDE.md §8)"
fi

# Single-source guard (CLAUDE.md §8, defence-in-depth over the Phase 58
# lint): no Protocol error Code constant is constructed under the auth
# tree. The only legitimate use is reading a Code from the canonical
# errors package via the imported alias.
if grep -rIn --include='*.go' 'protoerrors\.Code(' "${AUTH_PKG}/" 2>/dev/null | grep -v '_test.go' | grep -q .; then
    fail 'phase 61: a Protocol error Code is constructed under internal/protocol/auth — error codes are single-sourced in internal/protocol/errors (CLAUDE.md §8)'
else
    ok 'phase 61: no Protocol error Code redefined under internal/protocol/auth (single-source preserved — CLAUDE.md §8)'
fi

# Import-graph guard: the auth layer must NOT import the Console — the
# Runtime never imports Console code (CLAUDE.md §13).
if grep -rIn --include='*.go' '"github.com/hurtener/Harbor/web/console' "${AUTH_PKG}/" 2>/dev/null | grep -q .; then
    fail 'phase 61: internal/protocol/auth imports the Console — the Runtime never imports Console code (CLAUDE.md §13)'
else
    ok 'phase 61: internal/protocol/auth does not import the Console (Runtime/Console boundary preserved)'
fi

# Hardcoded-secret backstop (CLAUDE.md §7 rule 2): testdata/ keys must
# carry a documented dummy disclaimer. A README.md absent here means a
# reviewer cannot tell at a glance that the keys are test-only.
if [[ -f "${AUTH_PKG}/testdata/README.md" ]]; then
    ok 'phase 61: internal/protocol/auth/testdata/README.md documents the dummy keypairs'
else
    fail 'phase 61: internal/protocol/auth/testdata/README.md missing — CLAUDE.md §7 requires documented dummy values for test fixtures'
fi

# Phase 61 ships no live HTTP server — `harbor dev` (Phase 64) is the
# server that mounts the transport mux + the middleware. Skip the
# live-wire assertions per the 404/405/501 -> SKIP convention; the auth
# surface is exercised via httptest in the package + integration tests
# above.
skip "phase 61: the JWT auth middleware is exercised end-to-end via httptest in the package + integration tests; the live HTTP server that mounts the transport mux + middleware (\`harbor dev\`) lands in Phase 64"

smoke_summary
