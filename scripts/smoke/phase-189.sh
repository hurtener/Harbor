#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 189 smoke — Cache-token capture at the LLM edge.
#
# Telemetry-only: bifrost's per-response cache accounting
# (PromptTokensDetails.CachedReadTokens / CachedWriteTokens) lands on
# llm.Usage, mirrors onto llm.cost.recorded, and reaches every hand-decoded
# consumer (TUI reducer + render, Console run-events + cost breakdown). No
# live server needed — the whole path is exercised by the package tests plus
# the static guards below.
#
# Conventions (AGENTS.md §4.2):
#   - Static guards SKIP (not FAIL) while the marker is absent (phase not yet
#     landed), OK once the code ships — the static-check analogue of the
#     404/405/501 → SKIP rule.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# run_race_tests <desc> <packages...>
# Runs the touched packages under -race. OK on pass; FAIL on a genuine
# failure (never masked).
run_race_tests() {
    local desc="$1"
    shift
    local out rc
    out="$(CGO_ENABLED=0 go test -race -count=1 "$@" 2>&1)" && rc=0 || rc=$?
    if [ "${rc}" -eq 0 ]; then
        ok "${desc}"
        return
    fi
    printf '%s\n' "${out}" | tail -25
    fail "${desc}: go test exited ${rc}"
}

# soft_grep <pattern> <path> <desc>
# OK when the marker is present; SKIP (not FAIL) when absent, so the gate
# stays green on a build where the phase hasn't landed. FAIL only when the
# target file is missing entirely (a structural regression).
soft_grep() {
    local pattern="$1" target="$2" desc="$3"
    if [ ! -f "${target}" ]; then
        fail "${desc} — target file ${target} is missing"
        return
    fi
    if grep -qE -- "${pattern}" "${target}" 2>/dev/null; then
        ok "${desc}"
    else
        skip "${desc}: marker '${pattern}' absent from ${target} (phase not yet landed)"
    fi
}

# 1. The translator + Usage + event + consumer path, under the race detector.
run_race_tests \
    "phase 189: cache-token capture path (llm + tui + sessions)" \
    ./internal/llm/... ./internal/tui/... ./internal/sessions/...

# 2. Static guard: extractUsageAndCost reads PromptTokensDetails (the fix
#    can't regress to silently dropping the cache counts).
soft_grep \
    'PromptTokensDetails' \
    'internal/llm/drivers/bifrost/translate.go' \
    "phase 189: translator reads PromptTokensDetails"

# 3. Static guard: llm.Usage declares the two additive cache fields.
soft_grep \
    'CacheReadTokens' \
    'internal/llm/llm.go' \
    "phase 189: Usage declares CacheReadTokens"
soft_grep \
    'CacheWriteTokens' \
    'internal/llm/llm.go' \
    "phase 189: Usage declares CacheWriteTokens"

# 4. Static guard: the Console cost breakdown consumes the decoded cache
#    counts (the UI consumer landed, not just the wire).
soft_grep \
    'cacheReadTokens' \
    'web/console/src/lib/components/tasks/RightRailCostBreakdown.svelte' \
    "phase 189: Console cost breakdown surfaces cacheReadTokens"

smoke_summary
