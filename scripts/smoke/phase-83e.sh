#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83e — react-reasoning-channel-decoupling smoke.
#
# Static-only assertions (no live server): the reasoning-capture
# surface is a library + config change. The smoke pins:
#   - the five recorded per-provider reasoning fixtures exist and carry
#     a non-empty `reasoning_details` field;
#   - the narrowed Decision_CallTool shape no longer carries `Reasoning`;
#   - `examples/harbor.yaml` documents the `planner.reasoning_replay`
#     enum with both valid values named.
# See docs/plans/phase-83e-react-reasoning-channel-decoupling.md
# § "Smoke script additions".

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

FIXTURE_DIR="internal/llm/drivers/bifrost/testdata/reasoning_fixtures"

# 1. The five probed-provider fixtures exist and carry reasoning_details.
for fx in openrouter-claude openrouter-deepseek-r1 openrouter-o4-mini \
          openrouter-gemini-flash gemini-direct-gemini-flash; do
    assert_file "${FIXTURE_DIR}/${fx}.json" "phase-83e: reasoning fixture ${fx}"
    assert_grep_present 'reasoning_details' "${FIXTURE_DIR}/${fx}.json" \
        "phase-83e: fixture ${fx} carries a reasoning_details array"
done

# 2. Decision_CallTool is narrowed — the struct no longer carries a
#    `Reasoning string` field (D-147).
assert_grep_absent 'Reasoning string' internal/planner/decision.go \
    "phase-83e: Decision_CallTool dropped the Reasoning field (D-147)"

# 3. CompleteResponse gained the Reasoning carrier.
assert_grep_present 'Reasoning string' internal/llm/llm.go \
    "phase-83e: llm.CompleteResponse carries the Reasoning field"

# 4. The bifrost reasoning helper + typed budget error ship.
assert_grep_present 'func reasoningFromMessage' \
    internal/llm/drivers/bifrost/reasoning.go \
    "phase-83e: bifrost reasoningFromMessage helper present"
assert_grep_present 'ErrReasoningBudgetTooLow' \
    internal/llm/drivers/bifrost/reasoning.go \
    "phase-83e: bifrost ErrReasoningBudgetTooLow typed error present"

# 5. The replay knob is documented in the example config with both
#    enum values named.
assert_grep_present 'reasoning_replay' examples/harbor.yaml \
    "phase-83e: examples/harbor.yaml documents planner.reasoning_replay"
assert_grep_present 'never' examples/harbor.yaml \
    "phase-83e: examples/harbor.yaml names the 'never' replay mode"

smoke_summary
