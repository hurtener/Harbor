#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 92c smoke — agent-config skills control (the first consumer of the
# 92a registry primitive): the `agent_config.skills.{list,upsert,delete}`
# Protocol methods, the SkillsSelection payload section, the typed Console
# module entries, and the generated-docs rows.
#
# Conventions (AGENTS.md §4.2): 404/405/501 -> SKIP; OK >= 1 once shipped;
# use scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. The three skills method constants are single-sourced in methods.go.
for m in MethodAgentConfigSkillsList MethodAgentConfigSkillsUpsert MethodAgentConfigSkillsDelete; do
    assert_grep_present "${m} Method = \"agent_config.skills\." \
        internal/protocol/methods/methods.go \
        "phase 92c: ${m} constant present"
done

# 2. The SkillsSelection payload section is on the config envelope.
assert_grep_present 'Skills[[:space:]]+\*SkillsSelection' \
    internal/agentcfg/agentcfg.go \
    'phase 92c: ConfigPayload.Skills section present'
assert_grep_present 'type SkillsSelection struct' \
    internal/agentcfg/agentcfg.go \
    'phase 92c: SkillsSelection type present'

# 3. The skills service drives the SkillStore + records a revision, and
#    surfaces the pack-overwrite refusal as a typed error (no silent
#    overwrite — §13).
assert_grep_present 'SkillsUpsert' \
    internal/runtime/agentcfg/protocol/skills.go \
    'phase 92c: skills upsert service method present'
assert_grep_present 'recordSkillsMembership' \
    internal/runtime/agentcfg/protocol/skills.go \
    'phase 92c: skills mutation records a config revision'

# 4. The typed Console skills methods exist.
assert_grep_present 'skillsUpsert' \
    web/console/src/lib/protocol/client.ts \
    'phase 92c: typed Console skills methods present'
assert_grep_present 'export interface AgentConfigSkillInput' \
    web/console/src/lib/protocol/agentconfig.ts \
    'phase 92c: typed Console skill-input interface present'

# 5. The generated Protocol docs carry the skills method rows.
assert_grep_present 'agent_config.skills.upsert' \
    docs/site/protocol/methods.md \
    'phase 92c: generated methods.md carries agent_config.skills rows'

# 6. Live (preflight dev server): the skills route is admin-gated. An
# unauthenticated POST must NOT be 200 — 401 / 403 / 501 are all healthy.
# A 404 means the surface is not mounted (SKIP).
ROUTE="$(api_url /v1/agent_config/skills/upsert)"
if skip_if_404 "${ROUTE}" 'phase 92c: agent_config.skills.upsert route mounted'; then
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H 'Content-Type: application/json' -d '{}' "${ROUTE}" || true)
    case "${code}" in
        401|403) ok "phase 92c: agent_config.skills.upsert is identity/admin-gated (${code})" ;;
        501)     ok "phase 92c: agent_config.skills.upsert route present but unwired (501)" ;;
        200)     fail "phase 92c: agent_config.skills.upsert answered 200 unauthenticated — admin gate missing" ;;
        *)       fail "phase 92c: agent_config.skills.upsert unexpected status ${code}" ;;
    esac
fi

smoke_summary
