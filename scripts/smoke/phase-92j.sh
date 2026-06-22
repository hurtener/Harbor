#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 92j smoke — agent-config PER-AGENT LLM PARAMETERS (the versioned
# model/temperature/max-tokens/reasoning-effort section). Asserts the section
# type + the set_llm_params method/handler/admin-gate + the shared run-start
# projection (called from BOTH binaries — the D-094 twin) + the extended
# 3-arg ComposeLLMOverrides + the typed Console client + the generated docs;
# live: the admin route is mounted + admin-gated.
#
# Conventions (AGENTS.md §4.2): 404/405/501 -> SKIP; OK >= 1 once shipped;
# use scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. The LLMParams section type is single-sourced in internal/agentcfg.
assert_grep_present 'type LLMParams struct' \
    internal/agentcfg/agentcfg.go \
    'phase 92j: LLMParams section type present'
assert_grep_present 'LLMParams    \*LLMParams' \
    internal/agentcfg/agentcfg.go \
    'phase 92j: ConfigPayload carries the LLMParams section'
assert_grep_present 'func DiffLLMParams' \
    internal/agentcfg/agentcfg.go \
    'phase 92j: DiffLLMParams helper present'

# 2. The set_llm_params method constant.
assert_grep_present 'MethodAgentConfigSetLLMParams Method = "agent_config.set_llm_params"' \
    internal/protocol/methods/methods.go \
    'phase 92j: set_llm_params method constant present'

# 3. The service verb (sibling-preserving) + the set-time model validation.
assert_grep_present 'func \(s \*Service\) SetLLMParams' \
    internal/runtime/agentcfg/protocol/llmparams.go \
    'phase 92j: SetLLMParams service method present'
assert_grep_present 'func WithValidModels' \
    internal/runtime/agentcfg/protocol/service.go \
    'phase 92j: WithValidModels option (set-time model validation) present'
assert_grep_present 'ErrUnknownModel' \
    internal/runtime/agentcfg/protocol/service.go \
    'phase 92j: ErrUnknownModel sentinel present'

# 4. The handler branch routes set_llm_params.
assert_grep_present 'serveSetLLMParams' \
    internal/protocol/transports/stream/agentconfig_handler.go \
    'phase 92j: set_llm_params handler branch present'

# 5. The shared run-start projection, grepped in BOTH binaries (the D-094
#    twin — the per-agent layer must resolve identically in production + dev).
assert_grep_present 'func ActiveLLMOverrides' \
    internal/runtime/agentcfg/projection/projection.go \
    'phase 92j: ActiveLLMOverrides projection present'
assert_grep_present 'projection.ActiveLLMOverrides' \
    cmd/harbor/cmd_dev_runloop.go \
    'phase 92j: production run loop calls ActiveLLMOverrides'
assert_grep_present 'projection.ActiveLLMOverrides' \
    harbortest/devstack/devstack.go \
    'phase 92j: devstack twin calls ActiveLLMOverrides (D-094)'

# 6. The extended 3-arg ComposeLLMOverrides (session > agent > tenant).
assert_grep_present 'func ComposeLLMOverrides\(session \*PendingOverride, agent, tenant \*planner.LLMOverrides\)' \
    internal/runtime/runs/protocol/overrides.go \
    'phase 92j: ComposeLLMOverrides folds the per-agent layer'

# 7. The typed Console method + wire interface.
assert_grep_present 'setLlmParams' \
    web/console/src/lib/protocol/client.ts \
    'phase 92j: typed Console setLlmParams method present'
assert_grep_present 'export interface AgentConfigLLMParams' \
    web/console/src/lib/protocol/agentconfig.ts \
    'phase 92j: typed Console AgentConfigLLMParams interface present'

# 8. The generated Protocol docs carry the method.
assert_grep_present 'agent_config.set_llm_params' \
    docs/site/protocol/methods.md \
    'phase 92j: generated methods.md carries set_llm_params'

# 9. Live (preflight dev server): the admin route is mounted and admin-gated
#    (an unauthenticated POST is 401/403, never 200; 404 -> SKIP unbuilt).
ADMIN_ROUTE="$(api_url /v1/agent_config/set_llm_params)"
if skip_if_404 "${ADMIN_ROUTE}" 'phase 92j: agent_config.set_llm_params route mounted'; then
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H 'Content-Type: application/json' -d '{}' "${ADMIN_ROUTE}" || true)
    case "${code}" in
        401|403) ok "phase 92j: set_llm_params stays identity/admin-gated (${code})" ;;
        501)     ok "phase 92j: set_llm_params present but unwired (501)" ;;
        200)     fail "phase 92j: set_llm_params answered 200 unauthenticated — admin gate missing" ;;
        *)       fail "phase 92j: set_llm_params unexpected status ${code}" ;;
    esac
fi

smoke_summary
