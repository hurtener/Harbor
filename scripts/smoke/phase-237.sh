#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 237 — governed agent-pack authoring and revision convergence.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source scripts/smoke/common.sh

# Static executable-code guards run before the live probe, so this phase still
# reports meaningful coverage when the dev server is unreachable. The method
# names are canonical Protocol declarations; the handler counts are anchored
# to dispatch calls and function definitions rather than prose comments.
METHODS_GO='internal/protocol/methods/methods.go'
HANDLER_GO='internal/protocol/transports/stream/agentconfig_handler.go'
SERVICE_GO='internal/runtime/agentcfg/protocol/agentpacks.go'
assert_grep_present 'MethodAgentConfigAgentPacksPropose Method = "agent_config\.agent_packs\.propose"' "${METHODS_GO}" \
    'phase 237: canonical agent-pack propose method is declared'
assert_grep_present 'MethodAgentConfigAgentPacksCommit Method = "agent_config\.agent_packs\.commit"' "${METHODS_GO}" \
    'phase 237: canonical agent-pack commit method is declared'
assert_grep_count '^[[:space:]]*h\.serveAgentPacks[A-Za-z]+\(w, r, body, wireID\)' "${HANDLER_GO}" 5 \
    'phase 237: transport dispatch has exactly five executable agent-pack handlers'
assert_grep_count '^func \(h \*AgentConfigHandler\) serveAgentPacks[A-Za-z]+\(' "${HANDLER_GO}" 5 \
    'phase 237: transport defines exactly five executable agent-pack handlers'
assert_grep_present '^func \(s \*Service\) AgentPacksPropose' "${SERVICE_GO}" \
    'phase 237: service implements governed agent-pack propose'
assert_grep_present '^func \(s \*Service\) AgentPacksCommit' "${SERVICE_GO}" \
    'phase 237: service implements governed agent-pack commit'

PACKS_URL="$(api_url /v1/agent_config/agent_packs/list)"
if skip_if_404 "${PACKS_URL}" 'agent pack list route mounted'; then
	response="$(curl -sS -w '\n%{http_code}' -X POST "${PACKS_URL}" -H 'content-type: application/json' --data '{"agent_id":"smoke-agent"}')"
	status="${response##*$'\n'}"
	body="${response%$'\n'*}"
	if [[ "${status}" == "405" || "${status}" == "501" ]]; then
		skip "agent pack list route does not accept POST (${status})"
	else
		if [[ "${status}" == "401" ]]; then
			ok "agent pack list route enforces typed authentication"
		else
			fail "agent pack list route expected 401, got ${status}"
		fi
		if command -v jq >/dev/null 2>&1 && [[ "$(printf '%s' "${body}" | jq -r '.error.code' 2>/dev/null)" == "missing_identity" ]]; then
			ok "agent pack auth error is canonical"
		else
			fail "agent pack auth error body did not contain error.code=missing_identity"
		fi
	fi
fi
smoke_summary
