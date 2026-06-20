#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 92e smoke — agent-config layered system prompt: the
# `agent_config.set_prompt_layers` admin Protocol method, the PromptLayers
# payload section, the run-start projection + composition symbols, the typed
# Console module entries, and the generated-docs rows.
#
# Conventions (AGENTS.md §4.2): 404/405/501 -> SKIP; OK >= 1 once shipped;
# use scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. The set_prompt_layers method constant is single-sourced in methods.go.
assert_grep_present 'MethodAgentConfigSetPromptLayers Method = "agent_config.set_prompt_layers"' \
    internal/protocol/methods/methods.go \
    'phase 92e: set_prompt_layers method constant present'

# 2. The PromptLayers payload section is on the envelope (base + user).
assert_grep_present 'type PromptLayers struct' \
    internal/agentcfg/agentcfg.go \
    'phase 92e: PromptLayers type present'
assert_grep_present 'Base \*string' \
    internal/agentcfg/agentcfg.go \
    'phase 92e: PromptLayers.Base field present'

# 3. The prompt-layers service method records a revision, preserving siblings.
assert_grep_present 'func \(s \*Service\) SetPromptLayers' \
    internal/runtime/agentcfg/protocol/promptlayers.go \
    'phase 92e: SetPromptLayers service method present'

# 4. The run-start projection resolves the active base + user layers.
assert_grep_present 'func ActivePromptLayers' \
    internal/runtime/agentcfg/projection/projection.go \
    'phase 92e: prompt-layer run-start projection present'
assert_grep_present 'func ApplyPromptLayers' \
    internal/runtime/agentcfg/projection/projection.go \
    'phase 92e: prompt-layer overlay (shared run-loop seam) present'

# 5. The ReAct prompt builder composes the user layer below the base.
assert_grep_present 'func renderUserInstructions' \
    internal/planner/react/prompt.go \
    'phase 92e: <user_instructions> lower-trust section renderer present'
assert_grep_present 'BasePromptLayer' \
    internal/planner/planner.go \
    'phase 92e: durable base/user prompt-layer override fields present'

# 6. The typed Console method + wire interface exist.
assert_grep_present 'setPromptLayers' \
    web/console/src/lib/protocol/client.ts \
    'phase 92e: typed Console setPromptLayers method present'
assert_grep_present 'export interface AgentConfigPromptLayers' \
    web/console/src/lib/protocol/agentconfig.ts \
    'phase 92e: typed Console PromptLayers interface present'

# 7. The generated Protocol docs carry the method.
assert_grep_present 'agent_config.set_prompt_layers' \
    docs/site/protocol/methods.md \
    'phase 92e: generated methods.md carries set_prompt_layers'

# 8. Live (preflight dev server): the set_prompt_layers route is admin-gated.
# An unauthenticated POST must NOT be 200 — 401 / 403 / 501 are all healthy.
# A 404 means the surface is not mounted (SKIP).
ROUTE="$(api_url /v1/agent_config/set_prompt_layers)"
if skip_if_404 "${ROUTE}" 'phase 92e: agent_config.set_prompt_layers route mounted'; then
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H 'Content-Type: application/json' -d '{}' "${ROUTE}" || true)
    case "${code}" in
        401|403) ok "phase 92e: agent_config.set_prompt_layers is identity/admin-gated (${code})" ;;
        501)     ok "phase 92e: agent_config.set_prompt_layers route present but unwired (501)" ;;
        200)     fail "phase 92e: agent_config.set_prompt_layers answered 200 unauthenticated — admin gate missing" ;;
        *)       fail "phase 92e: agent_config.set_prompt_layers unexpected status ${code}" ;;
    esac
fi

smoke_summary
