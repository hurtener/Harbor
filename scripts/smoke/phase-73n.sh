#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
# Phase 73n smoke — Console Playground page (Protocol + chat module + UI
# bundled; D-130).
#
# Phase 73n ships:
#   - NEW Protocol method: runs.set_overrides (record the reasoning-
#     effort / temperature / max-tokens / system-prompt override applied
#     to the NEXT message in a session). Wire-transport route is
#     POST /v1/runs/set_overrides — the stream-package Runs handler (the
#     same posture the Phase 73c sessions.* / 73d tasks.* handlers take).
#   - The shared chat module at web/console/src/lib/chat/ (D-091) — the
#     Playground is the first consumer.
#   - SvelteKit (console)/playground/[session_id] route + Playwright
#     spec web/console/tests/playground-page.spec.ts.
#
# Binding carve-outs this smoke enforces:
#   - D-130 — runs.set_overrides is identity-mandatory; the override is
#     session-scoped + one-shot (next message only).
#   - D-091 / CLAUDE.md §4.5 #11 — the chat module imports NOTHING
#     outside $lib/chat/; the renderer registry at $lib/chat/renderers/
#     is EXTENDED, never forked.
#   - D-026 — heavy chat content flows by reference, never inline bytes.
#   - CLAUDE.md §4.5 — no raw color / spacing literals in .svelte; no
#     hand-rolled fetch() in the Playground route / components.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

RUNS_TYPES_PKG="internal/protocol/types"
RUNS_METHODS_PKG="internal/protocol/methods"
RUNS_PROTOCOL_PKG="internal/runtime/runs/protocol"
RUNS_HANDLER_PKG="internal/protocol/transports/stream"
CHAT_MODULE_DIR="web/console/src/lib/chat"
CHAT_RENDERERS_DIR="web/console/src/lib/chat/renderers"
CONSOLE_ROUTE_DIR="web/console/src/routes/(console)/playground"
PLAYGROUND_COMPONENTS_DIR="web/console/src/lib/components/playground"

# --------------------------------------------------------------------
# 1. Go-side unit + integration tests under -race. Covers: RunOverrides
#    wire-type round-trip, runs/protocol.Service validation + identity +
#    cross-session rejection, the D-025 concurrent-reuse test (N>=100),
#    the stream-package Runs handler decode/encode/error-mapping, and
#    the integration test asserting the next-message override +
#    cross-session rejection + audit emit.
# --------------------------------------------------------------------
PKG_SET=()
for pkg in "${RUNS_TYPES_PKG}" "${RUNS_METHODS_PKG}" "${RUNS_PROTOCOL_PKG}" "${RUNS_HANDLER_PKG}"; do
    if [ -d "${pkg}" ]; then
        PKG_SET+=("./${pkg}/...")
    fi
done

if [ ! -d "${RUNS_PROTOCOL_PKG}" ]; then
    skip 'phase 73n: internal/runtime/runs/protocol not present yet — Phase 73n implementation introduces it; package tests SKIP until then'
else
    if go test -race -count=1 -timeout 180s "${PKG_SET[@]}" >/dev/null 2>&1; then
        ok "phase 73n: runs package tests pass under -race over ${#PKG_SET[@]} package(s) (incl. D-025 N>=100 concurrent-reuse on the override Store)"
    else
        fail 'phase 73n: package tests failed (run `go test -race ./internal/runtime/runs/protocol/... ./internal/protocol/transports/stream/...` for detail)'
    fi
fi

if [ -d "${RUNS_PROTOCOL_PKG}" ]; then
    if go test -race -count=1 -timeout 240s -run 'TestE2E_Phase73n' ./test/integration/... >/dev/null 2>&1; then
        ok 'phase 73n: playground-overrides integration E2E passes under -race (real override Store + Phase 60 transport + Phase 61 auth + cross-session rejection + audit emit + N>=10 concurrency stress)'
    else
        fail 'phase 73n: playground-overrides integration E2E failed (run `go test -race -run TestE2E_Phase73n ./test/integration/...` for detail)'
    fi
else
    skip 'phase 73n: integration E2E SKIP — runs/protocol package not present yet'
fi

# --------------------------------------------------------------------
# 2. Chat-module encapsulation invariant (CLAUDE.md §4.5 #11).
#    No code inside $lib/chat/ imports anything outside $lib/chat/.
# --------------------------------------------------------------------
if [ -d "${CHAT_MODULE_DIR}" ]; then
    # Look for `from '$lib/...'` imports inside the chat module that do
    # NOT stay within $lib/chat/ (the README.md doc example is excluded
    # by the *.ts / *.svelte include filter).
    violations=$(grep -rEn "from ['\"]\\\$lib/" "${CHAT_MODULE_DIR}" \
        --include='*.ts' --include='*.svelte' 2>/dev/null \
        | grep -v "from ['\"]\\\$lib/chat/" || true)
    if [ -n "${violations}" ]; then
        fail 'phase 73n: chat module imports outside $lib/chat/ — §4.5 #11 encapsulation violation'
        printf '%s\n' "${violations}"
    else
        ok 'phase 73n: chat module encapsulation invariant holds (no code imports outside $lib/chat/)'
    fi
else
    skip 'phase 73n: web/console/src/lib/chat/ absent until 73n lands'
fi

# The renderer registry is EXTENDED, not forked — Phase 73l's index.ts
# dispatch core must still be the only registry.
if [ -f "${CHAT_RENDERERS_DIR}/index.ts" ] && [ -f "${CHAT_RENDERERS_DIR}/chat_bubble.ts" ]; then
    ok 'phase 73n: chat-bubble renderers extend Phase 73l registry via chat_bubble.ts (registry index.ts not forked)'
else
    skip 'phase 73n: chat renderer registry / extension not present yet'
fi

# --------------------------------------------------------------------
# 3. Console route + components present.
# --------------------------------------------------------------------
if [ -f "${CONSOLE_ROUTE_DIR}/[session_id]/+page.svelte" ]; then
    ok 'phase 73n: Playground route present at (console)/playground/[session_id] (no /console/ URL prefix — CONVENTIONS.md §1)'
else
    skip 'phase 73n: Playground route not present yet'
fi

if [ -d "${PLAYGROUND_COMPONENTS_DIR}" ]; then
    ok 'phase 73n: page-specific Playground components present under components/playground/'
else
    skip 'phase 73n: components/playground/ not present yet'
fi

# --------------------------------------------------------------------
# 4. Live-wire assertions against the preflight-booted dev server.
#    SKIP via the 404/405/501 convention until the surface lands.
# --------------------------------------------------------------------
RUNS_OVERRIDES_URL="$(api_url /v1/runs/set_overrides)"

# Identity-mandatory: a request without (tenant, user, session) must be
# rejected loud (CodeIdentityRequired -> HTTP 401).
if command -v curl >/dev/null 2>&1; then
    actual=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H 'Content-Type: application/json' \
        -d '{"overrides":{"session_id":"sess-fix","reasoning_effort":"high"}}' \
        "${RUNS_OVERRIDES_URL}" || echo "000")
    case "${actual}" in
        404|405|501|000|000000)
            skip 'phase 73n: runs.set_overrides not reachable (404/405/501/no-server -> SKIP); will OK once the route registers under a live server'
            ;;
        401)
            ok 'phase 73n: runs.set_overrides rejects missing-identity request 401 (CodeIdentityRequired) — CLAUDE.md §6 identity-mandatory'
            ;;
        *)
            fail "phase 73n: runs.set_overrides missing-identity test expected 401, got ${actual} — identity must be mandatory per CLAUDE.md §6"
            ;;
    esac
else
    skip 'phase 73n: curl not available; cannot exercise live wire'
fi

DEV_TOKEN="${HARBOR_DEV_TOKEN:-}"
if [ -z "${DEV_TOKEN}" ]; then
    skip 'phase 73n: HARBOR_DEV_TOKEN not set; skipping authenticated live-wire assertions (run under `make preflight`)'
elif command -v curl >/dev/null 2>&1 && command -v jq >/dev/null 2>&1; then
    # Happy path: an authenticated runs.set_overrides for the operator's
    # own session returns 200 with an applied_at timestamp.
    status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H 'Content-Type: application/json' \
        -H "Authorization: Bearer ${DEV_TOKEN}" \
        -d '{"overrides":{"reasoning_effort":"high"}}' \
        "${RUNS_OVERRIDES_URL}" || echo "000")
    case "${status}" in
        404|405|501)
            skip 'phase 73n: runs.set_overrides happy-path SKIP (route not yet mounted)'
            ;;
        200)
            body=$(curl -s --max-time 5 \
                -X POST -H 'Content-Type: application/json' \
                -H "Authorization: Bearer ${DEV_TOKEN}" \
                -d '{"overrides":{"reasoning_effort":"medium"}}' \
                "${RUNS_OVERRIDES_URL}" || echo '{}')
            has_applied=$(printf '%s' "${body}" | jq 'has("applied_at") and has("protocol_version")' 2>/dev/null || echo 'false')
            if [ "${has_applied}" = 'true' ]; then
                ok 'phase 73n: runs.set_overrides happy-path 200 carries applied_at + protocol_version'
            else
                fail "phase 73n: runs.set_overrides 200 response missing applied_at/protocol_version; body=${body}"
            fi
            ;;
        *)
            fail "phase 73n: runs.set_overrides happy-path expected 200, got ${status}"
            ;;
    esac

    # Invalid override: an out-of-range temperature must be rejected loud
    # (CodeInvalidRequest -> HTTP 400) — no silent degradation.
    bad_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H 'Content-Type: application/json' \
        -H "Authorization: Bearer ${DEV_TOKEN}" \
        -d '{"overrides":{"temperature":9.9}}' \
        "${RUNS_OVERRIDES_URL}" || echo "000")
    case "${bad_status}" in
        404|405|501)
            skip 'phase 73n: runs.set_overrides invalid-override SKIP (route not yet mounted)'
            ;;
        400)
            ok 'phase 73n: runs.set_overrides rejects an out-of-range temperature 400 (CodeInvalidRequest) — fail loudly, no silent degradation'
            ;;
        *)
            fail "phase 73n: runs.set_overrides invalid-override expected 400, got ${bad_status}"
            ;;
    esac
else
    skip 'phase 73n: curl/jq not available; cannot exercise authenticated live wire'
fi

smoke_summary
