#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 213 — heavy-content threshold split by purpose.
#
# The LLM-CONTEXT arm (config.DefaultHeavyOutputThresholdBytes) rose to
# 128 KiB; every non-LLM arm PINNED at 32 KiB behind its own named
# constant. The static block below is the pins' MECHANICAL gate: it is
# the check that turns OK into FAIL the moment someone restores an alias
# to the LLM-context constant. The live block proves the retargeted
# wiring still mounts its routes and that the one Protocol surface which
# REPORTS the resolved threshold reports the new number.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# 1. Static — the de-aliasing pins. Restoring any alias FAILS here.
# ----------------------------------------------------------------------------

assert_grep_absent 'config\.DefaultHeavyOutputThresholdBytes' \
    'internal/search/search.go' \
    'phase 213: the search preview bound carries its own literal'

assert_grep_present 'const HeavyPreviewThreshold = 32 \* 1024' \
    'internal/search/search.go' \
    'phase 213: search.HeavyPreviewThreshold is pinned at 32 KiB'

assert_grep_absent 'config\.DefaultHeavyOutputThresholdBytes' \
    'internal/tui/renderers/registry.go' \
    'phase 213: the TUI heavy fold carries its own literal'

assert_grep_present 'const heavyFoldThreshold = 32 \* 1024' \
    'internal/tui/renderers/registry.go' \
    'phase 213: the TUI fold constant is pinned at 32 KiB'

assert_grep_present 'const defaultHeavyThreshold = config\.DefaultConsoleInlinePayloadBytes' \
    'internal/mcpconsole/apps.go' \
    'phase 213: the MCP-console fallback is sourced on the Console inline bound'

assert_grep_present 'threshold = config\.DefaultConsoleInlinePayloadBytes' \
    'internal/mcpconsole/toolcontext.go' \
    'phase 213: the tool-context fallback is sourced on the Console inline bound'

# ----------------------------------------------------------------------------
# 2. Static — the operator-path pins (the Console-facing wiring). These
#    call sites must pass the PINNED constant; re-threading
#    cfg.Artifacts.HeavyOutputThresholdBytes into any of them re-couples
#    a Console-facing Protocol reply to the prompt-size knob.
# ----------------------------------------------------------------------------

assert_grep_count 'config\.DefaultConsoleInlinePayloadBytes' \
    'internal/runtime/serve/mux.go' 3 \
    'phase 213: mux.go pins the apps / pause.list / flow-catalog wiring'

assert_grep_present 'Threshold: config\.DefaultConsoleInlinePayloadBytes' \
    'internal/runtime/assemble/assemble.go' \
    'phase 213: the tool-context store wiring is pinned'

# The reporting site is deliberately NOT pinned — tools.content_stats
# echoes the offload threshold, so pinning it there would make the field
# lie.
assert_grep_present 'HeavyThresholdBytes: int64\(cfg\.Artifacts\.HeavyOutputThresholdBytes\)' \
    'internal/runtime/serve/mux.go' \
    'phase 213: tools.content_stats still REPORTS the operator threshold'

# ----------------------------------------------------------------------------
# 3. Static — the two constants and the operator-facing docs.
# ----------------------------------------------------------------------------

assert_grep_present 'const DefaultHeavyOutputThresholdBytes = 128 \* 1024' \
    'internal/config/config.go' \
    'phase 213: the LLM-context arm is 128 KiB'

assert_grep_present 'const DefaultConsoleInlinePayloadBytes = 32 \* 1024' \
    'internal/config/config.go' \
    'phase 213: the Console inline-payload bound is 32 KiB'

for example in examples/harbor.yaml examples/dev.yaml examples/serve.yaml; do
    if [ -f "${example}" ]; then
        assert_grep_absent 'heavy_output_threshold_bytes: 32768' \
            "${example}" \
            "phase 213: ${example} no longer pins the old default"
    else
        skip "phase 213: ${example} absent"
    fi
done

assert_grep_present '131072' 'docs/CONFIG.md' \
    'phase 213: docs/CONFIG.md states the new default'

# ----------------------------------------------------------------------------
# 4. Live — the reported threshold and the retargeted wiring's routes.
# ----------------------------------------------------------------------------

skip_all_if_server_down 'phase 213'

DEV_ID='{"identity":{"tenant":"dev","user":"dev","session":"dev"}}'

# Every route below is behind the auth middleware, which fails closed with
# 401 BEFORE the handler runs. An unauthenticated probe therefore says
# nothing about whether the retargeted wiring mounted its routes — the
# first cut of this script asserted 200 with no bearer and got 401 from
# every one. Resolve the dev bootstrap bearer the way the pause.list /
# memory.list smokes do.
DEV_BEARER="$(dev_bearer)"
if [ -z "${DEV_BEARER}" ]; then
    if [ -n "${HARBOR_DATA_DIR:-}" ]; then
        # The preflight harness IS present, so the token must be
        # resolvable; a miss here is a harness regression, not an absent
        # surface. Fail rather than skip (§4.2 item 5).
        fail 'phase 213: HARBOR_DATA_DIR is set but no dev bearer could be resolved from server.log'
    else
        skip 'phase 213: no dev bearer available (standalone run outside the preflight harness) — live legs skipped'
    fi
    smoke_summary
    exit 0
fi

# 4a. The two Console-facing reads whose wiring this phase RETARGETED.
#     Two assertions per route, and both matter:
#       * unauthenticated => 401 proves the route is MOUNTED with the auth
#         gate in front of it (an un-mounted route answers 404).
#       * authenticated => 200 proves it still SERVES after the retarget.
#     A non-positive threshold at the mux leaves these routes UN-MOUNTED
#     (transports.go requires heavyThreshold > 0; the pause-list handler
#     fails loud on non-positive), so `assert_post_status_auth` treats a
#     404 here as a FAIL rather than the usual forward-compat SKIP —
#     these surfaces shipped long before this phase.
for probe in "pause.list:/v1/pause/list" "memory.list:/v1/memory/list"; do
    name="${probe%%:*}"
    path="${probe#*:}"
    url="$(api_url "${path}")"
    noauth=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
        -X POST -H 'Content-Type: application/json' -d "${DEV_ID}" "${url}" || true)
    if [ "${noauth}" = "401" ]; then
        ok "phase 213: ${name} is mounted with the auth gate in front (401 unauthenticated)"
    else
        fail "phase 213: ${name} unauthenticated expected 401, got ${noauth} (POST ${url})"
    fi
    assert_post_status_auth 200 "${url}" "${DEV_ID}" "${DEV_BEARER}" \
        "phase 213: ${name} is still mounted and serving after the pin retarget"
done

# 4b. tools.content_stats is the ONE Protocol surface reporting the
#     resolved threshold. The tools.list leg is asserted as a real OK
#     first: the previous cut inferred "dev catalog is empty" from an
#     empty `.tools[0].id`, which a 401 produces just as readily as a
#     genuinely empty catalog — the SKIP was hiding an auth failure.
if ! command -v jq >/dev/null 2>&1; then
    skip 'phase 213: jq not available — tools.content_stats assertion skipped'
else
    list_tmp="$(mktemp)"
    list_status=$(curl -s -o "${list_tmp}" -w '%{http_code}' --max-time 10 \
        -H "Authorization: Bearer ${DEV_BEARER}" \
        -H 'Content-Type: application/json' \
        -X POST "$(api_url /v1/tools/list)" -d "${DEV_ID}" || true)
    if [ "${list_status}" != "200" ]; then
        fail "phase 213: tools.list expected 200, got ${list_status} — the threshold report cannot be reached"
    else
        ok 'phase 213: tools.list round-trips 200 authenticated (the content_stats entry point)'
        first_id=$(jq -r '.tools[0].id // empty' "${list_tmp}" 2>/dev/null || echo '')
        if [ -z "${first_id}" ]; then
            # Honest skip, now provably about the catalog and not about
            # auth: examples/dev.yaml ships `built_in` commented out, so a
            # stock dev boot has a genuinely empty catalog. Self-activates
            # the day the dev config declares a tool.
            skip 'phase 213: tools.content_stats — dev catalog is genuinely empty (tools.list 200 with total_rows=0)'
        else
            stats_tmp="$(mktemp)"
            stats_status=$(curl -s -o "${stats_tmp}" -w '%{http_code}' --max-time 10 \
                -H "Authorization: Bearer ${DEV_BEARER}" \
                -H 'Content-Type: application/json' \
                -X POST "$(api_url /v1/tools/content_stats)" \
                -d "{\"identity\":{\"tenant\":\"dev\",\"user\":\"dev\",\"session\":\"dev\"},\"id\":\"${first_id}\"}" \
                || true)
            if [ "${stats_status}" != "200" ]; then
                fail "phase 213: tools.content_stats expected 200, got ${stats_status}"
            else
                reported=$(jq -r '.. | .heavy_threshold_bytes? // empty' "${stats_tmp}" 2>/dev/null | head -1 || echo '')
                if [ "${reported}" = "131072" ]; then
                    ok 'phase 213: tools.content_stats reports heavy_threshold_bytes=131072'
                else
                    fail "phase 213: tools.content_stats reports heavy_threshold_bytes=${reported:-<absent>}, want 131072"
                fi
            fi
            rm -f "${stats_tmp}"
        fi
    fi
    rm -f "${list_tmp}"
fi

# 4c. The search preview path after the de-aliasing.
#
#     THIS GUARD WAS INERT UNTIL THE WAVE-v1.24 CHECKPOINT AUDIT. It posted
#     to `/v1/search/query`, a route that has never existed in this tree —
#     the five `search.*` methods are dispatched through the generic
#     `POST /v1/control/{method}` pattern (internal/protocol/transports/
#     control/control.go:69). `assert_post_status` maps that 404 to SKIP, so
#     the check had never once reported OK or FAIL and never could; the
#     comment that rationalised the SKIP as "self-activates when the route
#     lands" was describing a route that was already there under a different
#     name. It was also unauthenticated, so even at the right URL the auth
#     middleware would have answered 401 before the handler ran.
#
#     Now: the shipped route, the shipped method name, and a bearer — via
#     assert_post_status_auth, for which a 404/405/501 is a FAIL because an
#     already-shipped route going missing is an un-mounted regression
#     (§4.2 item 5).
#
#     The ref-vs-inline flip itself stays unit-tested in
#     internal/search/preview_bound_test.go — a >= 32 KiB synthesised preview
#     is not drivable through a dev smoke, and asserting a mechanism the
#     smoke cannot produce is the defect this plan's first draft shipped.
assert_post_status_auth 200 "$(api_url /v1/control/search.query)" \
    '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"query":"","page_size":5}' \
    "${DEV_BEARER}" \
    'phase 213: search.query round-trips after the preview-bound de-aliasing'

smoke_summary
