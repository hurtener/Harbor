#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 92a smoke — the agent-config control plane: the versioned
# desired-state registry primitive + the admin-scoped `agent_config.*`
# Protocol family (get / set_revision / list_revisions / diff / rollback)
# + the two canonical events + the typed Console module + generated docs.
#
# Conventions (AGENTS.md §4.2): 404/405/501 -> SKIP; OK >= 1 once shipped;
# use scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. The registry primitive package exists.
assert_file internal/agentcfg/agentcfg.go 'phase 92a: internal/agentcfg package present'
assert_file internal/agentcfg/drivers/statestore/statestore.go 'phase 92a: StateStore-backed agentcfg driver present'

# 2. The five registry method constants are single-sourced in methods.go.
for m in MethodAgentConfigGet MethodAgentConfigSetRevision MethodAgentConfigListRevisions MethodAgentConfigDiff MethodAgentConfigRollback; do
    assert_grep_present "${m} Method = \"agent_config\." \
        internal/protocol/methods/methods.go \
        "phase 92a: ${m} constant present"
done

# 3. The closed method set + predicates exist.
assert_grep_present 'canonicalAgentConfigMethods' \
    internal/protocol/methods/methods.go \
    'phase 92a: canonicalAgentConfigMethods set present'
assert_grep_present 'func IsAgentConfigMethod' \
    internal/protocol/methods/methods.go \
    'phase 92a: IsAgentConfigMethod predicate present'
assert_grep_present 'func IsAgentConfigAdminMethod' \
    internal/protocol/methods/methods.go \
    'phase 92a: IsAgentConfigAdminMethod predicate present'

# 4. The two canonical events are declared + registered.
assert_grep_present 'EventTypeConfigRevised events.EventType = "agent.config.revised"' \
    internal/agentcfg/events.go \
    'phase 92a: agent.config.revised event declared'
assert_grep_present 'EventTypeConfigReverted events.EventType = "agent.config.reverted"' \
    internal/agentcfg/events.go \
    'phase 92a: agent.config.reverted event declared'

# 5. The wire types are single-sourced in types/agentconfig.go.
assert_grep_present 'AgentConfigSetRevisionRequest' \
    internal/protocol/types/agentconfig.go \
    'phase 92a: agent-config wire types single-sourced'

# 6. The StateStore-backed driver is registered in the production aggregator.
assert_grep_present 'agentcfg/drivers/statestore' \
    internal/drivers/prod/prod.go \
    'phase 92a: agentcfg statestore driver blank-imported in prod aggregator'

# 7. The typed Console module + namespace exist.
assert_grep_present 'export interface AgentConfigSetRevisionRequest' \
    web/console/src/lib/protocol/agentconfig.ts \
    'phase 92a: typed Console agent-config wire interfaces present'
assert_grep_present 'class AgentConfigNamespace' \
    web/console/src/lib/protocol/client.ts \
    'phase 92a: AgentConfigNamespace typed client present'

# 8. The generated Protocol docs carry the agent_config rows.
assert_grep_present 'agent_config.set_revision' \
    docs/site/protocol/methods.md \
    'phase 92a: generated methods.md carries agent_config rows'
assert_grep_present 'agent.config.revised' \
    docs/site/protocol/events.md \
    'phase 92a: generated events.md carries agent.config.revised'

# 9. Live (preflight dev server): the route is admin-gated. An
# unauthenticated POST must NOT be 200 — 401 / 403 / 501 are all healthy.
# A 404 means the surface is not mounted (SKIP).
ROUTE="$(api_url /v1/agent_config/set_revision)"
if skip_if_404 "${ROUTE}" 'phase 92a: agent_config.set_revision route mounted'; then
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H 'Content-Type: application/json' -d '{}' "${ROUTE}" || true)
    case "${code}" in
        401|403) ok "phase 92a: agent_config.set_revision is identity/admin-gated (${code})" ;;
        501)     ok "phase 92a: agent_config.set_revision route present but unwired (501)" ;;
        200)     fail "phase 92a: agent_config.set_revision answered 200 unauthenticated — admin gate missing" ;;
        *)       fail "phase 92a: agent_config.set_revision unexpected status ${code}" ;;
    esac
fi

smoke_summary
