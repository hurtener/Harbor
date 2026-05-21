#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
# Phase 73c smoke — Console Sessions page (Protocol + UI bundled; D-122).
#
# Phase 73c ships:
#   - NEW Protocol methods: sessions.list (paginated + filtered) and
#     sessions.inspect (full per-session snapshot). The wire-transport
#     route is POST /v1/sessions/{list,inspect} — the stream-package
#     Sessions handler (the same posture the Phase 73f tools.* + Phase
#     73i flows.* handlers take; the codebase has no internal/server/
#     package — a documented D-122 deviation).
#   - SvelteKit /sessions route (list + detail) + Playwright spec
#     web/console/tests/sessions-page.spec.ts.
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
SESSIONS_PROTOCOL_PKG="internal/sessions/protocol"
SESSIONS_HANDLER_PKG="internal/protocol/transports/stream"
CONSOLE_ROUTES_DIR="web/console/src/routes/(console)/sessions"
CONSOLE_LIB_DIR="web/console/src/lib/sessions"
CONSOLE_COMPONENTS_DIR="web/console/src/lib/components/sessions"
PROTOCOL_TS_PATH="web/console/src/lib/protocol.ts"

# --------------------------------------------------------------------
# 1. Unit + integration tests under -race. Covers: SessionsListRequest /
#    Response round-trip, sessions/protocol.Service filter/cursor
#    conformance, the D-025 concurrent-reuse test (N>=100), the
#    stream-package Sessions handler decode/encode/error-mapping, and
#    the integration test asserting cross-tenant rejection without
#    admin + the audit emit.
# --------------------------------------------------------------------
PKG_SET=()
for pkg in "${SESSIONS_TYPES_PKG}" "${SESSIONS_METHODS_PKG}" "${SESSIONS_PROTOCOL_PKG}" "${SESSIONS_HANDLER_PKG}"; do
    if [ -d "${pkg}" ]; then
        PKG_SET+=("./${pkg}/...")
    fi
done

if [ "${#PKG_SET[@]}" -eq 0 ]; then
    skip 'phase 73c: no sessions/protocol/handler packages present yet (lands with Phase 73c implementation)'
elif [ ! -d "${SESSIONS_PROTOCOL_PKG}" ]; then
    skip 'phase 73c: internal/sessions/protocol not present yet — Phase 73c implementation introduces it; package tests SKIP until then'
else
    if go test -race -count=1 -timeout 180s "${PKG_SET[@]}" >/dev/null 2>&1; then
        ok "phase 73c: sessions package tests pass under -race over ${#PKG_SET[@]} package(s) (incl. D-025 N>=100 concurrent-reuse on the Service)"
    else
        fail 'phase 73c: package tests failed (run `go test -race ./internal/protocol/types/... ./internal/protocol/methods/... ./internal/sessions/protocol/... ./internal/protocol/transports/stream/...` for detail)'
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
    skip 'phase 73c: HARBOR_DEV_TOKEN not set; skipping live-wire assertions (run under `make preflight`)'
else
    SESSIONS_LIST_URL="$(api_url /v1/sessions/list)"
    SESSIONS_INSPECT_URL="$(api_url /v1/sessions/inspect)"

    # Identity-mandatory: a request without (tenant, user, session)
    # must be rejected loud (CodeIdentityRequired -> HTTP 401).
    if command -v curl >/dev/null 2>&1; then
        actual=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
            -X POST -H 'Content-Type: application/json' \
            -d '{}' "${SESSIONS_LIST_URL}" || echo "000")
        case "${actual}" in
            404|405|501)
                skip 'phase 73c: sessions.list not yet mounted (404/405/501 -> SKIP); will OK once the route registers'
                ;;
            401)
                ok 'phase 73c: sessions.list rejects missing-identity request 401 (CodeIdentityRequired) — CLAUDE.md §6 identity-mandatory'
                ;;
            *)
                fail "phase 73c: sessions.list missing-identity test expected 401, got ${actual} — identity must be mandatory per CLAUDE.md §6"
                ;;
        esac
    else
        skip 'phase 73c: curl not available; cannot exercise live wire'
    fi

    # Happy path: an authenticated tenant-scoped sessions.list returns
    # 200 with the response shape (rows / next_cursor / truncated).
    if command -v curl >/dev/null 2>&1 && command -v jq >/dev/null 2>&1; then
        body=$(curl -s --max-time 5 \
            -X POST -H 'Content-Type: application/json' \
            -H "Authorization: Bearer ${DEV_TOKEN}" \
            -d '{"filter":{},"limit":10}' \
            "${SESSIONS_LIST_URL}" || echo '{}')
        status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
            -X POST -H 'Content-Type: application/json' \
            -H "Authorization: Bearer ${DEV_TOKEN}" \
            -d '{"filter":{},"limit":10}' \
            "${SESSIONS_LIST_URL}" || echo "000")
        case "${status}" in
            404|405|501)
                skip 'phase 73c: sessions.list happy-path SKIP (route not yet mounted)'
                ;;
            200)
                has_rows=$(printf '%s' "${body}" | jq 'has("rows") and has("next_cursor") and has("truncated")' 2>/dev/null || echo 'false')
                if [ "${has_rows}" = 'true' ]; then
                    ok 'phase 73c: sessions.list happy-path response carries rows + next_cursor + truncated (D-026 fail-loudly on truncation, not a silent total)'
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
    # tenant_ids[] entry outside the operator's own tenant. The dev
    # token carries the admin + console:fleet scopes (cmd_dev.go), so
    # it PASSES the D-079 gate and the call 200s (admin-scoped). A 403
    # WITHOUT admin is covered end-to-end by the integration test with
    # a real non-admin ES256 token. This live check proves the route
    # honours the cross-tenant filter under the admin-scoped dev token.
    if command -v curl >/dev/null 2>&1; then
        actual=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
            -X POST -H 'Content-Type: application/json' \
            -H "Authorization: Bearer ${DEV_TOKEN}" \
            -d '{"filter":{"tenant_ids":["smoke-other-tenant"]},"limit":10}' \
            "${SESSIONS_LIST_URL}" || echo "000")
        case "${actual}" in
            404|405|501)
                skip 'phase 73c: sessions.list cross-tenant check SKIP (route not yet mounted)'
                ;;
            200)
                ok 'phase 73c: sessions.list cross-tenant filter reachable with the admin-scoped dev token (D-079 admin gate admits the scoped token)'
                ;;
            403)
                fail 'phase 73c: sessions.list rejected the admin-scoped dev token with 403 — D-079 gate is over-rejecting'
                ;;
            *)
                fail "phase 73c: cross-tenant sessions.list expected 200/403, got ${actual}"
                ;;
        esac
    fi

    # sessions.inspect: a 404 on an unknown session id proves the route
    # is mounted AND identity-scoped.
    if command -v curl >/dev/null 2>&1; then
        actual=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
            -X POST -H 'Content-Type: application/json' \
            -H "Authorization: Bearer ${DEV_TOKEN}" \
            -d '{"session_id":"smoke-unknown-session"}' \
            "${SESSIONS_INSPECT_URL}" || echo "000")
        case "${actual}" in
            404)
                ok 'phase 73c: sessions.inspect on an unknown session id returns 404 (CodeNotFound) — route mounted + identity-scoped'
                ;;
            405|501)
                skip 'phase 73c: sessions.inspect not yet mounted (405/501 -> SKIP)'
                ;;
            *)
                fail "phase 73c: sessions.inspect on an unknown id expected 404, got ${actual}"
                ;;
        esac
    fi
fi

# --------------------------------------------------------------------
# 3. Static guards — defence-in-depth over stylelint + protocol-ts gen.
# --------------------------------------------------------------------

# D-093 / D-132 (Wave 13 §17.5 W10): the `cmd/harbor-gen-protocol-ts`
# generator was never built — `protocol.ts` is hand-maintained. The
# checkpoint corrected the formerly-false `CODE GENERATED … DO NOT EDIT`
# header to an accurate "HAND-MAINTAINED" notice (the generator + its CI
# gate are tracked post-Wave-13). The smoke asserts the accurate header.
if [ -f "${PROTOCOL_TS_PATH}" ]; then
    if grep -q 'HAND-MAINTAINED' "${PROTOCOL_TS_PATH}"; then
        ok 'phase 73c: web/console/src/lib/protocol.ts carries the accurate hand-maintained header (D-093 / D-132)'
    else
        fail 'phase 73c: web/console/src/lib/protocol.ts is missing the hand-maintained header (D-093 / D-132)'
    fi
else
    skip 'phase 73c: web/console/src/lib/protocol.ts not present yet'
fi

# CLAUDE.md §4.5 #5 + §13: no hand-rolled fetch() in the Sessions
# routes / components — every wire call goes through the typed client.
if [ -d "${CONSOLE_ROUTES_DIR}" ] || [ -d "${CONSOLE_LIB_DIR}" ] || [ -d "${CONSOLE_COMPONENTS_DIR}" ]; then
    if grep -rIn --include='*.svelte' --include='*.ts' '\bfetch(' \
            "${CONSOLE_ROUTES_DIR}" "${CONSOLE_LIB_DIR}" "${CONSOLE_COMPONENTS_DIR}" 2>/dev/null \
            | grep -v '\.spec\.ts' | grep -q .; then
        fail 'phase 73c: hand-rolled fetch() found under the Sessions routes/components — every wire call MUST go through the typed HarborClient (CLAUDE.md §4.5 #5 + §13)'
    else
        ok 'phase 73c: no hand-rolled fetch() under the Sessions routes/components (typed HarborClient only)'
    fi
else
    skip 'phase 73c: sessions Svelte routes/components not present yet'
fi

# D-065: no Priority column / filter / field on Sessions. The grep
# matches a Priority IDENTIFIER (a struct field, a TS property, a JSON
# key) — a doc comment that explains the D-065 carve-out ("No Priority
# field — D-065") is not a violation, so comment lines (// and *) are
# excluded before the match.
if [ -d "${CONSOLE_ROUTES_DIR}" ] || [ -d "${CONSOLE_LIB_DIR}" ] || [ -d "${CONSOLE_COMPONENTS_DIR}" ]; then
    hit=''
    for d in "${CONSOLE_ROUTES_DIR}" "${CONSOLE_LIB_DIR}" "${CONSOLE_COMPONENTS_DIR}"; do
        [ -d "${d}" ] || continue
        if grep -rIhE --include='*.svelte' --include='*.ts' '[Pp]riority' "${d}" 2>/dev/null \
                | grep -vE '^\s*(//|\*)' \
                | grep -E '[Pp]riority\s*[:=]|"[Pp]riority"|priority_' \
                | grep -q .; then
            hit='1'
        fi
    done
    if [ -n "${hit}" ]; then
        fail 'phase 73c: a Priority identifier found in Sessions UI — D-065 dropped session-level priority from V1'
    else
        ok 'phase 73c: no Priority field / property in Sessions UI (D-065 enforced — carve-out comments excluded)'
    fi
else
    skip 'phase 73c: D-065 grep SKIP (Sessions UI not present yet)'
fi

# D-065 (wire side): no Priority field on SessionRow.
if [ -f "${SESSIONS_TYPES_PKG}/sessions.go" ]; then
    if grep -nE '^\s+Priority\s' "${SESSIONS_TYPES_PKG}/sessions.go" | grep -q .; then
        fail 'phase 73c: a Priority struct field found in internal/protocol/types/sessions.go — D-065 dropped session-level priority from V1'
    else
        ok 'phase 73c: no Priority field on SessionRow / SessionsListRequest (D-065 enforced)'
    fi
else
    skip 'phase 73c: internal/protocol/types/sessions.go not present yet'
fi

# D-064: no Convert-to-Evaluation Protocol method.
if [ -f "${SESSIONS_METHODS_PKG}/methods.go" ]; then
    if grep -nE 'MethodEvaluation|convert_to_evaluation' "${SESSIONS_METHODS_PKG}/methods.go" | grep -q .; then
        fail 'phase 73c: an Evaluation Protocol method found — D-064 defers Evaluations to post-V1'
    else
        ok 'phase 73c: no Evaluation Protocol method registered (D-064 enforced — Evaluations is post-V1)'
    fi
fi

# D-061: no sessions.saved_filter.* Protocol method (saved filters are
# Console-local in the Phase 72h Console DB, never the wire).
if [ -f "${SESSIONS_METHODS_PKG}/methods.go" ]; then
    if grep -nE 'saved_filter|MethodSavedFilter' "${SESSIONS_METHODS_PKG}/methods.go" | grep -q .; then
        fail 'phase 73c: a saved_filter Protocol method found — D-061 mandates saved filters live in the Console DB only'
    else
        ok 'phase 73c: no saved_filter Protocol method (D-061 — Console DB is local-only, never a runtime-entity shadow)'
    fi
fi

# CLAUDE.md §13 + §6: the runtime never imports the Console.
if [ -d "${SESSIONS_PROTOCOL_PKG}" ]; then
    if grep -rIn --include='*.go' '"github.com/hurtener/Harbor/web/console' "${SESSIONS_PROTOCOL_PKG}/" 2>/dev/null | grep -q .; then
        fail 'phase 73c: the runtime imports the Console — CLAUDE.md §13 forbids the Runtime importing Console code'
    else
        ok 'phase 73c: no runtime->Console import (CLAUDE.md §13 boundary preserved)'
    fi
fi

# Single-source guard: no Protocol error Code constructed in the
# sessions handler (single-sourced in internal/protocol/errors).
if [ -f "${SESSIONS_HANDLER_PKG}/sessions_handler.go" ]; then
    if grep -nE 'protoerrors\.Code\(|protocol/errors\.Code\(' "${SESSIONS_HANDLER_PKG}/sessions_handler.go" 2>/dev/null | grep -q .; then
        fail 'phase 73c: a Protocol error Code is constructed in sessions_handler.go — error codes are single-sourced in internal/protocol/errors (CLAUDE.md §8)'
    else
        ok 'phase 73c: no Protocol error Code redefined in sessions_handler.go (single-source preserved — CLAUDE.md §8)'
    fi
fi

# --------------------------------------------------------------------
# 4. Optional Playwright invocation when the Console build is present
#    AND Playwright is installed AND the Phase 73c spec file exists.
#
#    "Console build present" means the SvelteKit bundle is STAGED into
#    `cmd/harbor/consoledist/` — that is what `harbor console` embeds
#    via `embed.FS` and serves. `make preflight` builds `bin/harbor`
#    with a plain `go build` and does NOT run `make console-build`, so
#    the preflight binary embeds only `consoledist/.gitkeep` and serves
#    the "Console not bundled" placeholder — the Playwright spec then
#    cannot hydrate. Detect the staged bundle and SKIP cleanly when it
#    is absent (the directory-missing → SKIP convention, CLAUDE.md §4.2;
#    D-131 console-build ordering). The CI `frontend-e2e` job runs
#    `make console-build` before `make build`, so it exercises the spec
#    for real.
# --------------------------------------------------------------------
if [ ! -f 'web/console/tests/sessions-page.spec.ts' ]; then
    skip 'phase 73c: web/console/tests/sessions-page.spec.ts absent — SKIP per the 404->SKIP convention (CLAUDE.md §4.2)'
elif [ ! -f 'cmd/harbor/consoledist/index.html' ]; then
    skip 'phase 73c: Console bundle not staged into cmd/harbor/consoledist/ (run `make console-build`) — SKIP per the directory-missing convention; CI frontend-e2e exercises the spec for real'
elif [ -f 'web/console/package.json' ] && [ -d 'web/console/node_modules/@playwright/test' ]; then
    if (cd web/console && npm run test:e2e -- sessions-page.spec.ts >/dev/null 2>&1); then
        ok 'phase 73c: Playwright sessions-page.spec.ts passes (catalog rows + mockup columns + faceted filter + sub-header chips + bulk-action toolbar + right-rail Session Summary + bottom-dock tabs; SKIPs cleanly pre-Phase-73m harbor console)'
    else
        fail 'phase 73c: Playwright sessions-page.spec.ts failed (run `(cd web/console && npm run test:e2e -- sessions-page.spec.ts)` for detail)'
    fi
else
    skip 'phase 73c: Playwright not installed; SKIP per Phase 75 baseline-harness gate'
fi

smoke_summary
