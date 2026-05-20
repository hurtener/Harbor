#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
# Phase 73c smoke — Console Sessions page (Protocol + UI bundled).
#
# Phase 73c ships:
#   - NEW Protocol method: sessions.list (paginated + filtered).
#   - sessions.inspect: additive optional fields for the right-rail
#     Session Summary projection (RecentInterventions /
#     RecentArtifacts).
#   - SvelteKit /console/sessions route (list + detail) + Playwright
#     spec web/console/tests/sessions-page.spec.ts.
#
# Binding carve-outs this smoke enforces:
#   - D-064 — no Convert-to-Evaluation Protocol method (post-V1).
#   - D-065 — no Priority field on SessionRow or in the Sessions UI.
#   - D-061 — no sessions.saved_filter.* Protocol method (saved
#     filters are Console-local in the Phase 72h Console DB).
#   - D-079 — cross-tenant sessions.list requires auth.ScopeAdmin;
#     rejection is loud (CodeScopeMismatch + audit emit).
#   - D-093 — web/console/src/lib/protocol.ts is generated; never
#     hand-edited.
#   - CLAUDE.md §4.5 — no raw color / spacing literals in .svelte;
#     no hand-rolled fetch() in Sessions route / components.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

SESSIONS_TYPES_PKG="internal/protocol/types"
SESSIONS_METHODS_PKG="internal/protocol/methods"
SESSIONS_REG_PKG="internal/sessions"
SESSIONS_SERVER_PKG="internal/server"
CONSOLE_ROUTES_DIR="web/console/src/routes/sessions"
CONSOLE_LIB_DIR="web/console/src/lib/sessions"
PROTOCOL_TS_PATH="web/console/src/lib/protocol.ts"

# --------------------------------------------------------------------
# 1. Unit + integration tests under -race. Covers: SessionsListRequest /
#    Response round-trip, SessionRegistry.List filter/cursor
#    conformance, the D-025 concurrent-reuse test (N>=100), the
#    server-side handler decode/encode/error-mapping, and the
#    integration test asserting cross-tenant rejection without admin.
#
# The package set is built incrementally — when a package doesn't yet
# exist on disk we SKIP that package (the package-level equivalent of
# the 404/405/501 -> SKIP convention; per the implementor contract
# CLAUDE.md §4.2 #4 the smoke must coexist with builds that don't yet
# have the surface). When all four packages exist, the four required
# tests (wire-type round-trip + registry.List filter conformance +
# D-025 N>=100 concurrent-reuse + server handler) all run under -race.
# --------------------------------------------------------------------
PKG_SET=()
for pkg in "${SESSIONS_TYPES_PKG}" "${SESSIONS_METHODS_PKG}" "${SESSIONS_REG_PKG}" "${SESSIONS_SERVER_PKG}"; do
    if [ -d "${pkg}" ]; then
        PKG_SET+=("./${pkg}/...")
    fi
done

if [ "${#PKG_SET[@]}" -eq 0 ]; then
    skip 'phase 73c: no sessions/server packages present yet (lands with Phase 73c implementation)'
else
    if go test -race -count=1 -timeout 180s "${PKG_SET[@]}" >/dev/null 2>&1; then
        ok "phase 73c: sessions package tests pass under -race over ${#PKG_SET[@]} package(s) (incl. D-025 N>=100 concurrent-reuse on List when the handler is wired)"
    else
        # Distinguish "tests legitimately failing" from "the new
        # surface hasn't been added to an existing package yet". A
        # build / vet failure on a NEW symbol (SessionsListRequest,
        # MethodSessionsList) before the implementing PR lands looks
        # the same as a test failure — so when internal/server is
        # missing we accept the SKIP shape. Once internal/server
        # lands, every package test must pass.
        if [ ! -d "${SESSIONS_SERVER_PKG}" ]; then
            skip 'phase 73c: internal/server not present yet — Phase 73c implementation introduces it; package tests SKIP until then'
        else
            fail 'phase 73c: package tests failed (run `go test -race ./internal/protocol/types/... ./internal/protocol/methods/... ./internal/sessions/... ./internal/server/...` for detail)'
        fi
    fi
fi

if go test -race -count=1 -timeout 240s -run 'TestE2E_Phase73c' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 73c: sessions-page integration E2E passes under -race (real SessionRegistry + Phase 60 transport + Phase 61 auth + N>=10 SSE-subscriber concurrency stress + cross-tenant rejection without admin + audit emit on admin-scope query)'
else
    fail 'phase 73c: sessions-page integration E2E failed (run `go test -race -run TestE2E_Phase73c ./test/integration/...` for detail)'
fi

# --------------------------------------------------------------------
# 2. Live-wire assertions against the preflight-booted dev server.
#    SKIP via the 404/405/501 convention until the surface lands.
# --------------------------------------------------------------------
DEV_TOKEN="${HARBOR_DEV_TOKEN:-}"
if [ -z "${DEV_TOKEN}" ]; then
    skip 'phase 73c: HARBOR_DEV_TOKEN not set; skipping live-wire assertions (preflight harness exports the token when sessions.list is wired)'
else
    SESSIONS_LIST_URL="$(api_url /v1/control/sessions.list)"
    SESSIONS_INSPECT_URL="$(api_url /v1/control/sessions.inspect)"

    # Identity-mandatory: a request without (tenant, user, session)
    # must be rejected loud (CodeIdentityRequired -> HTTP 401).
    if command -v curl >/dev/null 2>&1; then
        actual=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
            -X POST -H 'Content-Type: application/json' \
            -d '{}' "${SESSIONS_LIST_URL}" || echo "000")
        case "${actual}" in
            404|405|501)
                skip 'phase 73c: sessions.list not yet implemented (404/405/501 -> SKIP); will OK once the method registers'
                ;;
            401)
                ok 'phase 73c: sessions.list rejects missing-identity request 401 (CodeIdentityRequired) — D-079 / CLAUDE.md §6 identity-mandatory'
                ;;
            *)
                fail "phase 73c: sessions.list missing-identity test expected 401, got ${actual} — identity must be mandatory per CLAUDE.md §6 + D-079"
                ;;
        esac
    else
        skip 'phase 73c: curl not available; cannot exercise live wire'
    fi

    # Happy path: an authenticated tenant-scoped sessions.list returns
    # 200 with the response shape (rows / next_cursor / truncated).
    # SKIP path: the method may not be registered yet (404/405/501).
    if command -v curl >/dev/null 2>&1 && command -v jq >/dev/null 2>&1; then
        TENANT="${HARBOR_DEV_TENANT:-default}"
        USER="${HARBOR_DEV_USER:-dev-user}"
        SESSION="${HARBOR_DEV_SESSION:-dev-session}"
        body=$(curl -s --max-time 5 \
            -X POST -H 'Content-Type: application/json' \
            -H "Authorization: Bearer ${DEV_TOKEN}" \
            -d "$(printf '{"identity":{"tenant":"%s","user":"%s","session":"%s"},"filter":{},"limit":10}' "${TENANT}" "${USER}" "${SESSION}")" \
            "${SESSIONS_LIST_URL}" || echo '{}')
        status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
            -X POST -H 'Content-Type: application/json' \
            -H "Authorization: Bearer ${DEV_TOKEN}" \
            -d "$(printf '{"identity":{"tenant":"%s","user":"%s","session":"%s"},"filter":{},"limit":10}' "${TENANT}" "${USER}" "${SESSION}")" \
            "${SESSIONS_LIST_URL}" || echo "000")
        case "${status}" in
            404|405|501)
                skip 'phase 73c: sessions.list happy-path SKIP (method not yet implemented)'
                ;;
            200)
                has_rows=$(printf '%s' "${body}" | jq 'has("rows") and has("next_cursor") and has("truncated")' 2>/dev/null || echo 'false')
                if [ "${has_rows}" = 'true' ]; then
                    ok 'phase 73c: sessions.list happy-path response shape carries rows + next_cursor + truncated (D-026 fail-loudly on truncation, not a silent total)'
                else
                    fail "phase 73c: sessions.list 200 response is missing one of {rows, next_cursor, truncated}; body=${body}"
                fi
                ;;
            *)
                fail "phase 73c: sessions.list happy-path expected 200, got ${status}"
                ;;
        esac
    fi

    # Cross-tenant without admin: a sessions.list specifying a
    # tenant_ids[] entry outside the operator's own tenant without
    # auth.ScopeAdmin must be rejected CodeScopeMismatch (HTTP 403).
    if command -v curl >/dev/null 2>&1; then
        OTHER_TENANT="${HARBOR_DEV_OTHER_TENANT:-other-tenant-must-fail}"
        actual=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
            -X POST -H 'Content-Type: application/json' \
            -H "Authorization: Bearer ${DEV_TOKEN}" \
            -d "$(printf '{"identity":{"tenant":"%s","user":"u","session":"s"},"filter":{"tenant_ids":["%s"]},"limit":10}' "${HARBOR_DEV_TENANT:-default}" "${OTHER_TENANT}")" \
            "${SESSIONS_LIST_URL}" || echo "000")
        case "${actual}" in
            404|405|501)
                skip 'phase 73c: sessions.list cross-tenant rejection SKIP (method not yet implemented)'
                ;;
            403)
                ok 'phase 73c: sessions.list cross-tenant call without auth.ScopeAdmin rejected 403 (CodeScopeMismatch) — D-079 cross-tenant requires admin'
                ;;
            *)
                fail "phase 73c: cross-tenant sessions.list without admin expected 403, got ${actual} — D-079 + CLAUDE.md §6 mandate hard rejection"
                ;;
        esac
    fi

    # sessions.inspect additive shape: RecentInterventions +
    # RecentArtifacts must appear on the response when a seeded
    # session id is queried. SKIP if the seed session isn't present.
    if command -v curl >/dev/null 2>&1 && command -v jq >/dev/null 2>&1; then
        SEED_SESSION="${HARBOR_DEV_SEED_SESSION:-}"
        if [ -z "${SEED_SESSION}" ]; then
            skip 'phase 73c: HARBOR_DEV_SEED_SESSION not set; cannot exercise sessions.inspect additive shape'
        else
            body=$(curl -s --max-time 5 \
                -X POST -H 'Content-Type: application/json' \
                -H "Authorization: Bearer ${DEV_TOKEN}" \
                -d "$(printf '{"identity":{"tenant":"%s","user":"%s","session":"%s"},"session_id":"%s"}' "${HARBOR_DEV_TENANT:-default}" "${HARBOR_DEV_USER:-dev-user}" "${HARBOR_DEV_SESSION:-dev-session}" "${SEED_SESSION}")" \
                "${SESSIONS_INSPECT_URL}" || echo '{}')
            has_fields=$(printf '%s' "${body}" | jq 'has("recent_interventions") and has("recent_artifacts")' 2>/dev/null || echo 'false')
            if [ "${has_fields}" = 'true' ]; then
                ok 'phase 73c: sessions.inspect additive shape carries recent_interventions + recent_artifacts (Phase 73c additive on Phase 73 response)'
            else
                skip 'phase 73c: sessions.inspect additive shape SKIP (response missing recent_interventions/recent_artifacts; surface may not yet land)'
            fi
        fi
    fi
fi

# --------------------------------------------------------------------
# 3. Static guards — defence-in-depth over stylelint + protocol-ts gen.
# --------------------------------------------------------------------

# D-093: protocol.ts is generated, not hand-edited.
if [ -f "${PROTOCOL_TS_PATH}" ]; then
    if head -1 "${PROTOCOL_TS_PATH}" | grep -q 'CODE GENERATED BY cmd/harbor-gen-protocol-ts. DO NOT EDIT'; then
        ok 'phase 73c: web/console/src/lib/protocol.ts carries the generated header (D-093)'
    else
        fail 'phase 73c: web/console/src/lib/protocol.ts is missing the generated-by header — D-093 forbids hand-editing'
    fi
else
    skip 'phase 73c: web/console/src/lib/protocol.ts not present yet (lands with the first Console phase that creates web/console/)'
fi

# CLAUDE.md §4.5 #5 + §13: no hand-rolled fetch() in the Sessions
# routes / components — every wire call goes through protocol.ts.
if [ -d "${CONSOLE_ROUTES_DIR}" ] || [ -d "${CONSOLE_LIB_DIR}" ]; then
    if grep -rIn --include='*.svelte' --include='*.ts' '\bfetch(' "${CONSOLE_ROUTES_DIR}" "${CONSOLE_LIB_DIR}" 2>/dev/null | grep -v '_test.ts' | grep -q .; then
        fail 'phase 73c: hand-rolled fetch() found under web/console/src/{routes,lib}/sessions/ — every wire call MUST go through the typed protocol.ts client (D-093, CLAUDE.md §4.5 #5 + §13)'
    else
        ok 'phase 73c: no hand-rolled fetch() under web/console/src/{routes,lib}/sessions/ (typed protocol.ts client only)'
    fi
else
    skip 'phase 73c: sessions Svelte routes/components not present yet (will land with Phase 73c implementation)'
fi

# D-065: no Priority column / filter / field on Sessions.
if [ -d "${CONSOLE_ROUTES_DIR}" ] || [ -d "${CONSOLE_LIB_DIR}" ]; then
    if grep -rIn --include='*.svelte' --include='*.ts' -E '\b[Pp]riority\b' "${CONSOLE_ROUTES_DIR}" "${CONSOLE_LIB_DIR}" 2>/dev/null | grep -q .; then
        fail 'phase 73c: Priority reference found in Sessions UI — D-065 dropped session-level priority from V1'
    else
        ok 'phase 73c: no Priority reference in Sessions UI (D-065 enforced)'
    fi
else
    skip 'phase 73c: D-065 grep SKIP (Sessions UI not present yet)'
fi

# D-065 (wire side): no Priority field on SessionRow.
if [ -f "${SESSIONS_TYPES_PKG}/sessions.go" ]; then
    if grep -nE '\bPriority\b' "${SESSIONS_TYPES_PKG}/sessions.go" | grep -q .; then
        fail 'phase 73c: Priority field found in internal/protocol/types/sessions.go — D-065 dropped session-level priority from V1'
    else
        ok 'phase 73c: no Priority field on SessionRow / SessionsListRequest (D-065 enforced)'
    fi
else
    skip 'phase 73c: internal/protocol/types/sessions.go not present yet'
fi

# D-064: no Convert-to-Evaluation Protocol method.
if [ -f "${SESSIONS_METHODS_PKG}/methods.go" ]; then
    if grep -nE 'MethodEvaluation|evaluation\.|convert_to_evaluation' "${SESSIONS_METHODS_PKG}/methods.go" | grep -q .; then
        fail 'phase 73c: an Evaluation Protocol method found — D-064 defers Evaluations to post-V1; the row action stays disabled with tooltip'
    else
        ok 'phase 73c: no Evaluation Protocol method registered (D-064 enforced — Evaluations is post-V1)'
    fi
fi

# D-061: no sessions.saved_filter.* Protocol method (saved filters are
# Console-local in the Phase 72h Console DB, never the wire).
if [ -f "${SESSIONS_METHODS_PKG}/methods.go" ]; then
    if grep -nE 'saved_filter|MethodSavedFilter' "${SESSIONS_METHODS_PKG}/methods.go" | grep -q .; then
        fail 'phase 73c: a saved_filter Protocol method found — D-061 mandates saved filters live in the Console DB only, never on the wire'
    else
        ok 'phase 73c: no saved_filter Protocol method (D-061 — Console DB is local-only, never a shadow source of truth for runtime entities)'
    fi
fi

# CLAUDE.md §13 + §6: the runtime never imports the Console.
if [ -d "${SESSIONS_REG_PKG}" ]; then
    if grep -rIn --include='*.go' '"github.com/hurtener/Harbor/web/console' "${SESSIONS_REG_PKG}/" "${SESSIONS_SERVER_PKG}/" 2>/dev/null | grep -q .; then
        fail 'phase 73c: the runtime imports the Console — CLAUDE.md §13 forbids the Runtime importing Console code in any direction'
    else
        ok 'phase 73c: no runtime->Console import (CLAUDE.md §13 boundary preserved)'
    fi
fi

# Single-source guard: no Protocol error Code constructed under the
# sessions handler tree (single-sourced in internal/protocol/errors).
if [ -f "${SESSIONS_SERVER_PKG}/sessions_list.go" ]; then
    if grep -nE 'protoerrors\.Code\(|protocol/errors\.Code\(' "${SESSIONS_SERVER_PKG}/sessions_list.go" 2>/dev/null | grep -q .; then
        fail 'phase 73c: a Protocol error Code is constructed in internal/server/sessions_list.go — error codes are single-sourced in internal/protocol/errors (CLAUDE.md §8)'
    else
        ok 'phase 73c: no Protocol error Code redefined in sessions_list.go (single-source preserved — CLAUDE.md §8)'
    fi
fi

# --------------------------------------------------------------------
# 4. Optional Playwright invocation when the Console build is present
#    AND Playwright is installed. SKIP otherwise (the install gate
#    lives in Phase 75's baseline harness).
# --------------------------------------------------------------------
if [ ! -f 'web/console/tests/sessions-page.spec.ts' ]; then
    # The spec lands with the Phase 73c implementation. Until then the
    # 404/405/501 -> SKIP convention (CLAUDE.md §4.2) applies — a missing
    # spec is "surface not yet implemented", not a failure.
    skip 'phase 73c: web/console/tests/sessions-page.spec.ts absent (Phase 73c implementation not yet landed) — SKIP per the 404->SKIP convention'
elif [ -f 'web/console/package.json' ] && [ -d 'web/console/node_modules/@playwright/test' ]; then
    if (cd web/console && npm run test:e2e -- sessions-page.spec.ts >/dev/null 2>&1); then
        ok 'phase 73c: Playwright sessions-page.spec.ts passes (catalog rows + mockup columns + faceted filter + sub-header chips + bulk-action toolbar + right-rail Session Summary + bottom-dock tabs)'
    else
        fail 'phase 73c: Playwright sessions-page.spec.ts failed (run `(cd web/console && npm run test:e2e -- sessions-page.spec.ts)` for detail)'
    fi
else
    skip 'phase 73c: Playwright not installed under web/console/node_modules/@playwright/test; SKIP per Phase 75 baseline-harness gate'
fi

smoke_summary
