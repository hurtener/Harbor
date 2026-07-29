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
AUTH_HEADER=()
if [[ -n "${HARBOR_DEV_TOKEN:-}" ]]; then
    AUTH_HEADER=(-H "Authorization: Bearer ${HARBOR_DEV_TOKEN}")
fi

# 4a. tools.content_stats is the ONE Protocol surface reporting the
#     resolved threshold. Follows the tools-page smoke's shape,
#     including its empty-catalog skip.
if ! command -v jq >/dev/null 2>&1; then
    skip 'phase 213: jq not available — tools.content_stats assertion skipped'
else
    list_body=$(curl -s --max-time 5 "${AUTH_HEADER[@]}" \
        -H 'Content-Type: application/json' \
        -X POST "$(api_url /v1/tools/list)" \
        -d "${DEV_ID}" 2>/dev/null || echo '{}')
    first_id=$(printf '%s' "${list_body}" | jq -r '.tools[0].id // empty' 2>/dev/null || echo '')
    if [[ -z "${first_id}" ]]; then
        skip 'phase 213: tools.content_stats — dev catalog is empty'
    else
        stats_body=$(curl -s --max-time 5 "${AUTH_HEADER[@]}" \
            -H 'Content-Type: application/json' \
            -X POST "$(api_url /v1/tools/content_stats)" \
            -d "{\"identity\":{\"tenant\":\"dev\",\"user\":\"dev\",\"session\":\"dev\"},\"id\":\"${first_id}\"}" \
            2>/dev/null || echo '{}')
        reported=$(printf '%s' "${stats_body}" | jq -r \
            '.. | .heavy_threshold_bytes? // empty' 2>/dev/null | head -1 || echo '')
        if [[ -z "${reported}" ]]; then
            skip 'phase 213: tools.content_stats does not report heavy_threshold_bytes on this build'
        elif [[ "${reported}" == "131072" ]]; then
            ok 'phase 213: tools.content_stats reports heavy_threshold_bytes=131072'
        else
            fail "phase 213: tools.content_stats reports heavy_threshold_bytes=${reported}, want 131072"
        fi
    fi
fi

# 4b. The retargeted wiring must still MOUNT its routes. A nil or zero
#     threshold at the mux leaves pause.list / memory.* un-mounted, which
#     would otherwise look like an innocent skip.
assert_post_status 200 "$(api_url /v1/pause/list)" "${DEV_ID}" \
    'phase 213: pause.list is still mounted after the pin retarget'
assert_post_status 200 "$(api_url /v1/memory/list)" "${DEV_ID}" \
    'phase 213: memory.list is still mounted after the pin retarget'

# 4c. The search preview path is alive after the de-aliasing: ordinary
#     records still ship an inline preview. The ref-vs-inline flip
#     itself is unit-tested — a >= 32 KiB synthesised preview is not
#     drivable through a dev smoke.
assert_post_status 200 "$(api_url /v1/search/query)" \
    '{"identity":{"tenant":"dev","user":"dev","session":"dev"},"query":"a"}' \
    'phase 213: search.query round-trips after the preview-bound de-aliasing'

smoke_summary
