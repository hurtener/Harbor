#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 92g smoke — agent-config session-user SAFE SUBSET (the non-admin
# lower tier of the authorization matrix). Asserts the session-safe method
# constants + the per-route tier gate + the session-overlay store + the
# narrow-only / base-unwritable composition + the typed Console client + the
# generated docs rows; live: a session-safe route is NOT admin-gated and an
# admin route still 403s a non-admin caller.
#
# Conventions (AGENTS.md §4.2): 404/405/501 -> SKIP; OK >= 1 once shipped;
# use scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. The session-safe method constants are single-sourced in methods.go.
assert_grep_present 'MethodAgentConfigSessionSetUserPrompt Method = "agent_config.session.set_user_prompt"' \
    internal/protocol/methods/methods.go \
    'phase 92g: session set_user_prompt method constant present'
assert_grep_present 'MethodAgentConfigSessionSetSourceDisables Method = "agent_config.session.set_source_disables"' \
    internal/protocol/methods/methods.go \
    'phase 92g: session set_source_disables method constant present'

# 2. The session-method tier classifier (non-admin lower tier).
assert_grep_present 'func IsAgentConfigSessionMethod' \
    internal/protocol/methods/methods.go \
    'phase 92g: IsAgentConfigSessionMethod tier classifier present'

# 3. The session-overlay store (keyed by the real triple — session-isolated).
assert_grep_present 'func NewStore' \
    internal/agentcfg/sessionoverlay/sessionoverlay.go \
    'phase 92g: session-overlay store constructor present'

# 4. The session-safe service methods.
assert_grep_present 'func \(s \*Service\) SessionSetUserPrompt' \
    internal/runtime/agentcfg/protocol/session.go \
    'phase 92g: SessionSetUserPrompt service method present'
assert_grep_present 'func \(s \*Service\) SessionSetSourceDisables' \
    internal/runtime/agentcfg/protocol/session.go \
    'phase 92g: SessionSetSourceDisables service method present'

# 5. The per-route gate: session-safe routes bypass the admin gate.
assert_grep_present 'agentConfigSessionSafeRoutes' \
    internal/protocol/transports/stream/agentconfig_handler.go \
    'phase 92g: per-route session-safe gate present'

# 6. NARROW-ONLY composition: the projection unions the session disable set
#    into the admin exclusion set (can only narrow).
assert_grep_present 'func unionSorted' \
    internal/runtime/agentcfg/projection/projection.go \
    'phase 92g: narrow-only union composition present'
assert_grep_present 'func composeUserLayer' \
    internal/runtime/agentcfg/projection/projection.go \
    'phase 92g: session user-layer composition present'

# 7. The typed Console method + wire interface exist.
assert_grep_present 'sessionSetUserPrompt' \
    web/console/src/lib/protocol/client.ts \
    'phase 92g: typed Console sessionSetUserPrompt method present'
assert_grep_present 'export interface AgentConfigSessionOverlay' \
    web/console/src/lib/protocol/agentconfig.ts \
    'phase 92g: typed Console AgentConfigSessionOverlay interface present'

# 8. The generated Protocol docs carry the session method.
assert_grep_present 'agent_config.session.set_user_prompt' \
    docs/site/protocol/methods.md \
    'phase 92g: generated methods.md carries session.set_user_prompt'

# 9. Live (preflight dev server): the session-safe route is mounted and is
#    NOT admin-gated (an unauthenticated POST is 401 for identity, never the
#    403 admin gate; a 404 means unmounted -> SKIP). The admin route still
#    403s a non-admin caller.
ROUTE="$(api_url /v1/agent_config/session/set_user_prompt)"
if skip_if_404 "${ROUTE}" 'phase 92g: agent_config.session.set_user_prompt route mounted'; then
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H 'Content-Type: application/json' -d '{}' "${ROUTE}" || true)
    case "${code}" in
        401)     ok "phase 92g: session route is identity-gated, not admin-gated (401)" ;;
        400|200) ok "phase 92g: session route accepts a non-admin caller (${code})" ;;
        403)     fail "phase 92g: session-safe route 403'd — it must NOT require the admin scope" ;;
        501)     ok "phase 92g: session route present but unwired (501)" ;;
        *)       fail "phase 92g: session route unexpected status ${code}" ;;
    esac
fi

ADMIN_ROUTE="$(api_url /v1/agent_config/set_prompt_layers)"
if skip_if_404 "${ADMIN_ROUTE}" 'phase 92g: agent_config.set_prompt_layers admin route mounted'; then
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H 'Content-Type: application/json' -d '{}' "${ADMIN_ROUTE}" || true)
    case "${code}" in
        401|403) ok "phase 92g: admin route stays identity/admin-gated (${code})" ;;
        501)     ok "phase 92g: admin route present but unwired (501)" ;;
        200)     fail "phase 92g: admin route answered 200 unauthenticated — admin gate missing" ;;
        *)       fail "phase 92g: admin route unexpected status ${code}" ;;
    esac
fi

smoke_summary
