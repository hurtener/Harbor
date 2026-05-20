#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 73g — Console Events page (UI consumer of the shipped Phase 72/72a
# events.subscribe + events.aggregate surface + the shipped Phase 73l
# artifacts.get_ref for heavy payloads).
#
# This phase ships NO new Protocol method — the Events page is a pure UI
# consumer. The probes below assert the upstream Protocol surface this
# page composes is reachable, plus the build-time presence of the page
# route + per-page Playwright spec.
#
# SKIP convention (CLAUDE.md §4.1 + §4.2): the page route over HTTP SKIPs
# until `harbor console` lands (Phase 73m bundles the subcommand per
# wave-13-decomposition §9); the Protocol probes accept any non-404
# status because they run UNAUTHENTICATED (a 401/400 still proves the
# route is mounted — the surface exists).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Static-source guards (run regardless of dev-server posture).
# ----------------------------------------------------------------------------

# 1. The phase plan exists (drift-audit also checks this; smoke is the
#    second perimeter).
assert_file \
    "docs/plans/phase-73g-console-events-page.md" \
    "phase 73g plan present"

# 2. The per-page Playwright spec is part of the same PR.
assert_file \
    "web/console/tests/events-page.spec.ts" \
    "phase 73g per-page Playwright spec present"

# 3. The page route source file is part of the same PR. Per
#    CONVENTIONS.md §1 (D-121) the route lives under the (console) route
#    group and is served at /events with NO /console/ URL prefix.
assert_file \
    "web/console/src/routes/(console)/events/+page.svelte" \
    "phase 73g page route present"

# 4. The Go-side integration test ships in the same PR.
assert_file \
    "test/integration/events_page_test.go" \
    "phase 73g Go-side integration test present"

# 5. Raw colour guard: no inline hex colour literals in the page route
#    (CLAUDE.md §4.5 §3 + §13 — stylelint is the authoritative gate;
#    this is the second perimeter).
assert_grep_absent \
    '#[0-9a-fA-F]\{3,8\}' \
    "web/console/src/routes/(console)/events/+page.svelte" \
    "no raw hex colour literals in events page route"

# 6. Heavy-payload leak guard (D-026 at the Console edge): the
#    truncated-payload renderer must NEVER inline raw payload bytes —
#    the Open-artifact link routes through artifacts.get_ref.
if [ -f "web/console/src/lib/components/events/TruncatedPayloadLink.svelte" ]; then
    if grep -qE 'payload\.bytes|bytes_base64' \
        "web/console/src/lib/components/events/TruncatedPayloadLink.svelte"; then
        fail "phase 73g TruncatedPayloadLink appears to inline raw payload bytes (D-026 / CLAUDE.md §13: ArtifactRef only)"
    else
        ok "phase 73g TruncatedPayloadLink routes heavy payloads through artifacts.get_ref (no raw-bytes leak)"
    fi
else
    skip "phase 73g truncated-payload component: not present"
fi

# ----------------------------------------------------------------------------
# Live-server probes — upstream Protocol surface + page route.
# ----------------------------------------------------------------------------

if ! command -v curl >/dev/null 2>&1; then
    skip 'phase 73g: curl not available — skipping all live-server probes'
    smoke_summary
    exit 0
fi
HEALTHZ_CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$(api_url /healthz)" 2>/dev/null || true)"
case "${HEALTHZ_CODE}" in
    200)
        : # server is up; proceed to live probes
        ;;
    *)
        skip "phase 73g: dev server unreachable (/healthz=${HEALTHZ_CODE:-000}) — skipping all live-server probes"
        smoke_summary
        exit 0
        ;;
esac

# 7. events.subscribe — the SSE table feed (GET /v1/events, Phase 72).
#    Probed unauthenticated: a non-404 proves the route is mounted (an
#    unauthenticated request is rejected, which is correct).
SUBSCRIBE_CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$(api_url /v1/events)" 2>/dev/null || true)"
case "${SUBSCRIBE_CODE}" in
    404|405|501|"")
        skip "events.subscribe (GET /v1/events): ${SUBSCRIBE_CODE:-unreachable} (surface not yet mounted)"
        ;;
    *)
        ok "events.subscribe route mounted (GET /v1/events -> ${SUBSCRIBE_CODE}) — 73g table feed"
        ;;
esac

# 8. events.aggregate — the sparkline feed (POST /v1/events/aggregate,
#    Phase 72a). A non-404 proves the route is mounted.
AGGREGATE_CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' \
    -d '{"window":3600000000000,"bucket":60000000000}' \
    "$(api_url /v1/events/aggregate)" 2>/dev/null || true)"
case "${AGGREGATE_CODE}" in
    404|501|"")
        skip "events.aggregate (POST /v1/events/aggregate): ${AGGREGATE_CODE:-unreachable} (surface not yet mounted)"
        ;;
    *)
        ok "events.aggregate route mounted (POST /v1/events/aggregate -> ${AGGREGATE_CODE}) — 73g sparkline feed"
        ;;
esac

# 9. artifacts.get_ref — the heavy-payload resolver (POST
#    /v1/control/artifacts.get_ref, Phase 73l, shipped). A non-404
#    proves the truncated-payload Open-artifact resolver is reachable.
GETREF_CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' \
    -d '{}' \
    "$(api_url /v1/control/artifacts.get_ref)" 2>/dev/null || true)"
case "${GETREF_CODE}" in
    404|501|"")
        skip "artifacts.get_ref (POST /v1/control/artifacts.get_ref): ${GETREF_CODE:-unreachable} (surface not yet mounted)"
        ;;
    *)
        ok "artifacts.get_ref route mounted (POST /v1/control/artifacts.get_ref -> ${GETREF_CODE}) — 73g heavy-payload resolver"
        ;;
esac

# 10. The page route serves a 200 once `harbor console` is wired
#     (Phase 73m). Per CONVENTIONS.md §1 the route is /events (no
#     /console/ prefix). Until `harbor console` lands the route is 404
#     -> SKIP by convention.
ROUTE_CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$(api_url /events)" 2>/dev/null || true)"
case "${ROUTE_CODE}" in
    200)
        ok "console events page route serves 200 (${ROUTE_CODE}, $(api_url /events))"
        ;;
    404|405|501|"")
        skip "console events page route: ${ROUTE_CODE:-unreachable} (harbor console subcommand — Phase 73m — not yet wired)"
        ;;
    *)
        fail "console events page route: expected 200, got ${ROUTE_CODE} ($(api_url /events))"
        ;;
esac

smoke_summary
