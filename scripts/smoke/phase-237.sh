#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 237 — governed agent-pack authoring and revision convergence.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source scripts/smoke/common.sh
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
