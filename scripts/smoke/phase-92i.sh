#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 92i smoke — Console agent-config REVISION UX (derived per-revision
# change summary + diff-before-rollback + atomic multi-section Save-all +
# the Model & sampling area). This phase adds NO Go / Protocol surface, so
# the assertions are STATIC source greps over the Console; the live gate is
# the Playwright spec (web/console/tests/agent-config-page.spec.ts) run
# against the embedded `harbor console` in the frontend-e2e CI job.
#
# Conventions (AGENTS.md §4.2): use scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

CONSOLE="web/console/src/lib"

# 1. The derived per-revision change summary + the section-label mapping
#    (client-side, no extra round-trip).
assert_grep_present 'revisionSummary' \
    "${CONSOLE}/agentconfig/state.svelte.ts" \
    'phase 92i: derived per-revision summary present'
assert_grep_present 'export function changedSectionLabels' \
    "${CONSOLE}/agentconfig/state.svelte.ts" \
    'phase 92i: changedSectionLabels section-mapping present'

# 2. The diff-before-rollback preview flow (never a blind repoint).
assert_grep_present 'async requestRollback' \
    "${CONSOLE}/agentconfig/state.svelte.ts" \
    'phase 92i: requestRollback (diff-before-rollback) present'
assert_grep_present 'async confirmRollbackPreview' \
    "${CONSOLE}/agentconfig/state.svelte.ts" \
    'phase 92i: confirmRollbackPreview present'
assert_grep_present "data-testid=\"agentcfg-rollback-preview\"" \
    "${CONSOLE}/components/agentconfig/RevisionHistoryCard.svelte" \
    'phase 92i: diff-preview modal markup present'

# 3. The atomic multi-section Save-all path + the staged indicator.
assert_grep_present 'async saveAll' \
    "${CONSOLE}/agentconfig/state.svelte.ts" \
    'phase 92i: atomic saveAll path present'
assert_grep_present 'get stagedAreaCount' \
    "${CONSOLE}/agentconfig/state.svelte.ts" \
    'phase 92i: stagedAreaCount indicator derived'
assert_grep_present "data-testid=\"agentcfg-save-all\"" \
    "${CONSOLE}/components/agentconfig/SaveAllBar.svelte" \
    'phase 92i: Save-all bar markup present'

# 4. The new Model & sampling area (so Save-all + summary cover 92j's section).
assert_grep_present "id: 'llm', label: 'Model & sampling'" \
    "${CONSOLE}/agentconfig/state.svelte.ts" \
    'phase 92i: Model & sampling (llm) area registered'

# 4b. Discoverability: the per-agent entry point on the Agents list card
#     (one click from the list to /agent-config?agent=<id> — not buried).
assert_grep_present "data-testid=\"agent-card-configure\"" \
    "${CONSOLE}/components/agents/AgentCard.svelte" \
    'phase 92i: Agents-list per-card Configure entry point present'
assert_grep_present "/agent-config\?agent=" \
    "${CONSOLE}/components/agents/AgentCard.svelte" \
    'phase 92i: Configure link deep-links the agent id'

# 5. Tokens-only hygiene on the new components (no raw hex / px literals; the
#    stylelint gate enforces this repo-wide, this is a fast trip-wire).
for f in SaveAllBar LLMParamsCard; do
    if grep -nE '#[0-9a-fA-F]{3,6}\b|: *[0-9]+px' "${CONSOLE}/components/agentconfig/${f}.svelte" >/dev/null 2>&1; then
        fail "phase 92i: ${f}.svelte carries a raw color/px literal (tokens only)"
    else
        ok "phase 92i: ${f}.svelte is tokens-only (no raw literals)"
    fi
done

# 6. No hand-rolled fetch in the new components (go through the typed client).
for f in SaveAllBar LLMParamsCard; do
    if grep -nE '\bfetch\(' "${CONSOLE}/components/agentconfig/${f}.svelte" >/dev/null 2>&1; then
        fail "phase 92i: ${f}.svelte has a hand-rolled fetch (use the typed client)"
    else
        ok "phase 92i: ${f}.svelte has no hand-rolled fetch"
    fi
done

smoke_summary
