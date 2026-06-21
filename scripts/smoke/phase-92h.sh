#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 92h smoke — the consolidated Console agent-config control panel
# (a pure Protocol client; no Go backend). Static asserts that the route +
# the five area components + the runes state controller are present, the
# panel lives in the (console) route group, and the typed client covers the
# agent_config read+write methods; plus a skip-if-404 live probe that a
# feeding method answers + is admin-gated on the preflight dev server.
#
# Conventions (AGENTS.md §4.2): 404/405/501 -> SKIP; OK >= 1 once shipped;
# use scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

CONSOLE="web/console/src"

# 1. The panel route lives in the (console) group, no /console/ URL prefix.
assert_file \
    "${CONSOLE}/routes/(console)/agent-config/+page.svelte" \
    'phase 92h: /agent-config route present in the (console) group'

# 2. The five self-contained area components exist.
for comp in RevisionHistoryCard DiffView SkillsCard McpPolicyCard PromptLayersCard AddConnectionCard; do
    assert_file \
        "${CONSOLE}/lib/components/agentconfig/${comp}.svelte" \
        "phase 92h: agentconfig/${comp}.svelte present"
done

# 3. The runes state controller (mirrors the 92b TenantDefaultOverridesState).
assert_grep_present 'class AgentConfigPanelState' \
    "${CONSOLE}/lib/agentconfig/state.svelte.ts" \
    'phase 92h: AgentConfigPanelState runes controller present'
assert_grep_present 'get hasAdminScope' \
    "${CONSOLE}/lib/agentconfig/state.svelte.ts" \
    'phase 92h: admin-scope getter present (the 92b admin-gate precedent)'

# 4. The panel composes the four-state PageState boundary.
assert_grep_present 'PageState' \
    "${CONSOLE}/routes/(console)/agent-config/+page.svelte" \
    'phase 92h: the panel routes async state through <PageState>'

# 5. The typed client covers the agent_config read+write methods the five
#    areas consume (the panel adds NO Protocol surface — 92a–f shipped them).
for method in list_revisions diff rollback set_tool_exposure set_prompt_layers add_mcp_connection skills/list skills/upsert skills/delete; do
    assert_grep_present "/v1/agent_config/${method}" \
        "${CONSOLE}/lib/protocol/client.ts" \
        "phase 92h: typed client covers agent_config.${method}"
done

# 6. The tests are present (vitest state machine + typed-client + Playwright).
assert_file \
    "${CONSOLE}/lib/agentconfig/tests/state.svelte.spec.ts" \
    'phase 92h: vitest state-machine spec present'
assert_file \
    "${CONSOLE}/lib/protocol/tests/agentconfig-client.spec.ts" \
    'phase 92h: vitest typed-client spec present'
assert_file \
    "web/console/tests/agent-config-page.spec.ts" \
    'phase 92h: Playwright panel spec present'

# 7. The per-page design spec exists (CONVENTIONS.md rule 8/9).
assert_file \
    "docs/design/console/page-agent-config.md" \
    'phase 92h: per-page design spec present'

# 8. Live (preflight dev server): a feeding agent_config method is mounted +
#    admin-gated. An unauthenticated POST must NOT be 200 — 401/403/501 are
#    healthy; 404 means the surface is not mounted (SKIP).
ROUTE="$(api_url /v1/agent_config/list_revisions)"
if skip_if_404 "${ROUTE}" 'phase 92h: agent_config.list_revisions route mounted'; then
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H 'Content-Type: application/json' -d '{}' "${ROUTE}" || true)
    case "${code}" in
        401|403) ok "phase 92h: agent_config.list_revisions is identity/admin-gated (${code})" ;;
        501)     ok "phase 92h: agent_config.list_revisions route present but unwired (501)" ;;
        200)     fail "phase 92h: agent_config.list_revisions answered 200 unauthenticated — admin gate missing" ;;
        *)       fail "phase 92h: agent_config.list_revisions unexpected status ${code}" ;;
    esac
fi

smoke_summary
