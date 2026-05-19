#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 73g — Console Events page (UI consumer of Phase 72a
# events.aggregate + saved-filter events.subscribe chips).
#
# This phase ships NO new Protocol method — it composes Phase 72a's
# events.subscribe filter extensions + events.aggregate time-bucket
# method and the already-shipped artifacts.get for heavy payloads.
#
# Smoke assertions follow the 404/405/501 → SKIP convention (CLAUDE.md
# §4.1 + §4.2): upstream Phase 72a methods SKIP until 72a's surface
# ships; the page route SKIPs until `harbor console` lands (Phase 73m
# bundles the harbor console subcommand per wave-13-decomposition §9).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Static-source guards (run regardless of dev-server posture; the smoke is
# classified live-server because the Protocol probes below need the server).
# ----------------------------------------------------------------------------

# 1. The phase plan exists (drift-audit also checks this, but smoke is
#    the second perimeter).
assert_file \
    "docs/plans/phase-73g-console-events-page.md" \
    "phase 73g plan present"

# 2. The per-page Playwright spec is part of the same PR — a binding
#    acceptance per the phase plan.
if [ -f "web/console/tests/events-page.spec.ts" ]; then
    ok "phase 73g per-page Playwright spec present (web/console/tests/events-page.spec.ts)"
else
    skip "phase 73g Playwright spec missing: web/console/tests/events-page.spec.ts (lands with page implementation)"
fi

# 3. The page route source file is part of the same PR.
if [ -f "web/console/src/routes/console/events/+page.svelte" ]; then
    ok "phase 73g page route present (web/console/src/routes/console/events/+page.svelte)"
else
    skip "phase 73g page route missing: web/console/src/routes/console/events/+page.svelte (lands with page implementation)"
fi

# 4. Raw colour / heavy-content guard: if the page source is present,
#    assert no inline hex colour literals (CLAUDE.md §4.5 §3 + §13).
#    The stylelint rule (web/console/.stylelintrc.cjs, lands with the
#    first Console phase) is the authoritative gate; this is the
#    second perimeter.
if [ -f "web/console/src/routes/console/events/+page.svelte" ]; then
    if grep -qE '#[0-9a-fA-F]{3,8}' "web/console/src/routes/console/events/+page.svelte"; then
        fail "phase 73g page route contains a raw hex colour literal (CLAUDE.md §4.5 §3 / §13 forbid raw literals in .svelte files — use tokens.css)"
    else
        ok "phase 73g page route has no raw hex colour literals"
    fi
else
    skip "phase 73g raw-literal guard: page route not yet present"
fi

# 5. Heavy-payload leak guard (D-026 read at the Console edge): when the
#    truncated-payload renderer ships, it must NEVER inline raw payload
#    bytes — the Open-artifact link goes through artifacts.get. This is
#    a defence-in-depth grep; the binding gate is the integration test.
if [ -f "web/console/src/lib/events/components/TruncatedPayloadLink.svelte" ]; then
    if grep -qE 'payload\.bytes|atob\(|btoa\(' \
        "web/console/src/lib/events/components/TruncatedPayloadLink.svelte"; then
        fail "phase 73g TruncatedPayloadLink appears to inline raw payload bytes (D-026 / CLAUDE.md §13: ArtifactStub only)"
    else
        ok "phase 73g TruncatedPayloadLink routes heavy payloads through artifacts.get (no raw-bytes leak)"
    fi
else
    skip "phase 73g truncated-payload component: not yet present"
fi

# ----------------------------------------------------------------------------
# Live-server probes — upstream Phase 72a Protocol surface + page route.
# All SKIP via protocol_call / 404→SKIP until the surfaces ship.
#
# Pre-gate: the live probes need the dev server up. If preflight did not
# boot it (e.g. the binary isn't built yet, or this smoke is run outside
# preflight), SKIP all live probes cleanly — the script stays a no-op
# until the surface is reachable, matching the phase-69 / Phase 64+
# pattern.
# ----------------------------------------------------------------------------

# Tight server-reachability check: curl /healthz, fail-fast if the dev
# server isn't up. We deliberately don't rely on `skip_if_404` here
# because its `actual=$(curl ... || echo "000")` pattern can yield a
# `000000` string when curl exits non-zero and also writes `000` to
# stdout, which then misses the `404|405|501|000` case. A direct curl
# with an explicit exit-status check is unambiguous.
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

# 6. events.subscribe (Phase 72a's filter extensions). Until 72a lands,
#    protocol_call SKIPs by design — Phase 73g's page is the consumer
#    waiting for the primitive.
protocol_call 'events.subscribe' \
    '{"identity":{"tenant_id":"t-smoke","user_id":"u-smoke","session_id":"s-smoke"},"types":["tool.failed"]}' \
    "events.subscribe (Phase 72a) responds — consumed by 73g page"

# 7. events.aggregate (Phase 72a's time-bucket method). SKIPs until 72a
#    lands; flips to OK once the surface is reachable. This is the
#    binding upstream probe for the sparkline.
protocol_call 'events.aggregate' \
    '{"identity":{"tenant_id":"t-smoke","user_id":"u-smoke","session_id":"s-smoke"},"window":"1h","bucket":"1m"}' \
    "events.aggregate (Phase 72a) responds — drives 73g sparkline"

# 8. artifacts.get — already shipped (Phase 73). The truncated-payload
#    Open-artifact link routes through this. Smoke probes the route
#    surface, not a real artifact (the integration test exercises the
#    artifact round-trip).
protocol_call 'artifacts.get' \
    '{"identity":{"tenant_id":"t-smoke","user_id":"u-smoke","session_id":"s-smoke"},"ref":"art_smoke_does_not_exist"}' \
    "artifacts.get (Phase 73, shipped) responds — heavy-payload resolver for 73g"

# 9. The page route serves a 200 once `harbor console` is wired (Phase
#    73m bundles the subcommand per wave-13-decomposition §9). Until
#    then the route is 404 → SKIP by convention. Same direct-curl
#    pattern as the /healthz pre-gate above so the case match is
#    unambiguous for both server-down and surface-not-yet-shipped.
ROUTE_CODE="$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$(api_url /console/events)" 2>/dev/null || true)"
case "${ROUTE_CODE}" in
    200)
        ok "console events page route serves 200 (${ROUTE_CODE}, $(api_url /console/events))"
        ;;
    404|405|501|"")
        skip "console events page route: ${ROUTE_CODE:-unreachable} (harbor console subcommand — Phase 73m — not yet wired)"
        ;;
    *)
        fail "console events page route: expected 200, got ${ROUTE_CODE} ($(api_url /console/events))"
        ;;
esac

smoke_summary
