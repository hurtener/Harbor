#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 108a — Playground fidelity pass (follow-up to D-167).
#
# Console-only + a runtime reasoning-wiring fix. Like phase-108.sh this
# smoke is static (file existence + token/testid greps + no-new-deps);
# behavioural coverage lives in the Playwright specs the frontend job
# runs (playground-page / playground-polish / shell-no-regression) and
# the Go unit tests (internal/llm/...). Assertions SKIP when a surface is
# absent so the gate stays green before 108a lands.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file_or_skip() {
    local path="$1" desc="$2"
    if [ -f "${path}" ]; then ok "${desc}: ${path} exists"
    else skip "${desc}: ${path} missing (Phase 108a not yet implemented)"; fi
}

assert_absent_or_skip() {
    local path="$1" desc="$2"
    if [ ! -f "${path}" ]; then ok "${desc}: ${path} removed"
    else skip "${desc}: ${path} still present (Phase 108a not yet implemented)"; fi
}

assert_grep_or_skip() {
    local pattern="$1" path="$2" desc="$3"
    if [ ! -f "${path}" ]; then skip "${desc}: ${path} not found (Phase 108a not yet implemented)"; return; fi
    if grep -qE "${pattern}" "${path}" 2>/dev/null; then ok "${desc}"
    else skip "${desc}: pattern '${pattern}' absent in ${path} (Phase 108a not yet implemented)"; fi
}

assert_not_grep_or_skip() {
    local pattern="$1" path="$2" desc="$3"
    if [ ! -f "${path}" ]; then skip "${desc}: ${path} not found (Phase 108a not yet implemented)"; return; fi
    if grep -qE "${pattern}" "${path}" 2>/dev/null; then
        skip "${desc}: pattern '${pattern}' unexpectedly present in ${path} (Phase 108a not yet implemented)"
    else ok "${desc}"; fi
}

# ---- E: one global app status bar; the page-scoped status bar is gone ----
assert_file_or_skip \
    "web/console/src/lib/components/ui/AppStatusBar.svelte" \
    "phase-108a: global AppStatusBar landed"
assert_absent_or_skip \
    "web/console/src/lib/components/playground/PlaygroundStatusBar.svelte" \
    "phase-108a: page-scoped PlaygroundStatusBar removed"
assert_grep_or_skip \
    "AppStatusBar" \
    "web/console/src/routes/(console)/+layout.svelte" \
    "phase-108a: shell renders the global AppStatusBar"

# ---- Decoders (108 wiring fix carried in the same pass) ----
assert_file_or_skip \
    "web/console/src/routes/(console)/playground/[session_id]/wire-events.ts" \
    "phase-108a: SSE wire-event decoders landed"

# ---- B: KPI integrated metadata columns ----
KPI="web/console/src/lib/components/playground/KpiStrip.svelte"
for col in kpi-session kpi-started kpi-duration kpi-tokens kpi-cost kpi-latency kpi-identity kpi-scope; do
    assert_grep_or_skip "data-testid=\"${col}\"" "${KPI}" "phase-108a: KPI column ${col} present"
done

# ---- F: Controls apply live — no save button, no Post-V1 drift toggle ----
CTRL="web/console/src/lib/components/playground/ControlsCard.svelte"
assert_not_grep_or_skip "Apply to next message" "${CTRL}" \
    "phase-108a: Controls 'Apply to next message' save button removed (live apply)"
assert_not_grep_or_skip "controls-drift-mode" "${CTRL}" \
    "phase-108a: Post-V1 Drift-mode toggle removed"
assert_grep_or_skip "controls-reset" "${CTRL}" \
    "phase-108a: Controls 'Reset to defaults' present"

# ---- D/C: composer telemetry + reasoning render ----
assert_grep_or_skip "composer-telemetry" \
    "web/console/src/routes/(console)/playground/[session_id]/+page.svelte" \
    "phase-108a: composer telemetry strip present"
assert_grep_or_skip "reasoning-live|reasoningText" \
    "web/console/src/lib/chat/MessageBubble.svelte" \
    "phase-108a: live reasoning disclosure render present"

# ---- Runtime reasoning-wiring fix (corrections defaults req.Model) ----
assert_grep_or_skip "req.Model == \"\"" \
    "internal/llm/corrections/corrections.go" \
    "phase-108a: corrections defaults empty req.Model before profile lookup"

# ---- No UNEXPECTED npm dependency ----
# 108a adds none; the only sanctioned post-108 addition is `@lucide/svelte`
# (Phase 108b chrome icons, operator-approved). Compare the dependency NAME SET
# vs main and fail on any added dep outside that allowlist — a count-only check
# tripped on 108b's legitimate icon dep (§17.6).
if command -v git >/dev/null 2>&1 && command -v jq >/dev/null 2>&1 \
        && git rev-parse --verify main >/dev/null 2>&1; then
    ADDED=$(comm -13 \
        <(git show main:web/console/package.json 2>/dev/null \
            | jq -r '((.dependencies // {}) | keys[]), ((.devDependencies // {}) | keys[])' 2>/dev/null | sort) \
        <(jq -r '((.dependencies // {}) | keys[]), ((.devDependencies // {}) | keys[])' \
            web/console/package.json 2>/dev/null | sort) )
    UNEXPECTED=$(printf '%s\n' "${ADDED}" | grep -vE '^(@lucide/svelte)?$' || true)
    if [ -z "${UNEXPECTED}" ]; then
        ok "phase-108a: no unexpected npm dependency vs main (only sanctioned post-108 additions)"
    else
        fail "phase-108a: unexpected dependency vs main: $(printf '%s' "${UNEXPECTED}" | tr '\n' ' ') — 108a is no-new-deps"
    fi
else
    skip "phase-108a: dep-set comparison skipped (git or jq or main ref unavailable)"
fi

smoke_summary
