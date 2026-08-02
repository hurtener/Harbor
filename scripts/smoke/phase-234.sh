#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 234 — terminal agent-config retirement.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

P234_TMP="$(mktemp -d "${TMPDIR:-/tmp}/harbor-phase234.XXXXXX")"
trap 'rm -rf "${P234_TMP}"' EXIT

assert_file docs/plans/phase-234-agent-config-retirement.md 'phase 234: plan exists'
assert_grep_present '^## D-399 ' docs/decisions.md 'phase 234: terminal lifecycle decision is recorded'
assert_grep_present 'MethodAgentConfigRetire Method = "agent_config.retire"' internal/protocol/methods/methods.go \
  'phase 234: admin retirement Protocol verb is registered'
assert_grep_present 'CodeAgentRetired' internal/protocol/errors/errors.go \
  'phase 234: typed retired error is registered'
assert_grep_present 'CodeAgentRetirementConflict' internal/protocol/errors/errors.go \
  'phase 234: typed retirement conflict is registered'
assert_grep_present 'func \(r \*registry\) Retire' internal/agentcfg/drivers/statestore/statestore.go \
  'phase 234: StateStore CAS tombstone implementation exists'
assert_grep_present 'func \(r \*registry\) CompleteRetirementStep' internal/agentcfg/drivers/statestore/statestore.go \
  'phase 234: frozen cleanup progress is durably acknowledged by CAS'
assert_grep_present 'completeRetirementCleanup' internal/runtime/agentcfg/protocol/service.go \
  'phase 234: same-operation cleanup replay is runtime-wired'
assert_grep_present 'RetirementStatus' internal/runtime/serve/agent_resolver.go \
  'phase 234: run resolver refuses a retired effective target'

assert_go_tests_pass "${P234_TMP}/retirement.log" '-race -count=1 ./cmd/harbor ./internal/agentcfg/drivers/statestore ./internal/runtime/agentcfg/protocol ./internal/runtime/serve ./internal/protocol/transports/stream' \
	'phase 234: terminal state, frozen cleanup, production HTTP start refusal, and protocol replay run under race' \
	TestRetirement_TerminalHistoryAndReplay \
	TestRetirement_ConcurrentSameOperationAndTenantIsolation \
	TestRetirement_ProgressIsFrozenCASState \
	TestRetirement_CommitThenAckLossConverges \
	TestRetirement_EventPublishFailureStaysCheckpointed \
	TestRetirement_SQLiteRestartRetainsTerminalLifecycle \
	TestDevComposition_RetiredDefaultRefusesExplicitAndImplicitStartBeforeSpawn \
	TestAgentConfigHandler_Retire_AdminReplayAndTerminalRefusal \
	TestAgentResolverAdapter_DefaultTombstoneWins

# The dev bearer is an admin and reaches the dev agent. This is a real mux
# exercise when preflight has booted the server; standalone runs degrade only
# when the server/tools are absent.
if command -v curl >/dev/null 2>&1 && command -v jq >/dev/null 2>&1 && [ -n "$(dev_bearer)" ]; then
  P234_TOKEN="$(dev_bearer)"
  p234_call() { curl -sS --max-time 10 -X POST "$(api_url "$1")" -H "Authorization: Bearer ${P234_TOKEN}" -H 'X-Harbor-Tenant: dev' -H 'X-Harbor-User: dev' -H "X-Harbor-Session: $2" -H 'Content-Type: application/json' -d "$3"; }
  if curl -s --max-time 3 "$(api_url /healthz)" >/dev/null 2>&1; then
    # Never retire the boot default: preflight runs later phase smokes in the
    # same dev process. This isolated config id still exercises the real
    # admin lifecycle surface without poisoning subsequent checks.
    P234_AGENT='phase234-live-agent'
    P234_SEED=$(p234_call /v1/agent_config/set_revision phase234-live '{"agent_id":"phase234-live-agent","payload":{"skills":{"names":["phase234"]}}}')
    P234_HASH=$(printf '%s' "$P234_SEED" | jq -r '.revision.content_hash // empty')
    if [ -n "$P234_HASH" ]; then
      P234_RETIRE=$(p234_call /v1/agent_config/retire phase234-live "{\"agent_id\":\"${P234_AGENT}\",\"operation_id\":\"phase234-live-op\",\"expected_content_hash\":\"${P234_HASH}\"}")
      if printf '%s' "$P234_RETIRE" | jq -e '.status.prior_content_hash == $h' --arg h "$P234_HASH" >/dev/null; then ok 'phase 234: live retire records exact prior hash'; else fail 'phase 234: live retire did not return exact prior hash'; fi
      P234_REPLAY=$(p234_call /v1/agent_config/retire phase234-live "{\"agent_id\":\"${P234_AGENT}\",\"operation_id\":\"phase234-live-op\",\"expected_content_hash\":\"${P234_HASH}\"}")
      if printf '%s' "$P234_REPLAY" | jq -e '.status.operation_id == "phase234-live-op"' >/dev/null; then ok 'phase 234: live same-operation retry replays'; else fail 'phase 234: live same-operation retry failed'; fi
      P234_CONFLICT=$(p234_call /v1/agent_config/retire phase234-live "{\"agent_id\":\"${P234_AGENT}\",\"operation_id\":\"phase234-other\",\"expected_content_hash\":\"${P234_HASH}\"}")
      if printf '%s' "$P234_CONFLICT" | jq -e '.code == "agent_retirement_conflict"' >/dev/null; then ok 'phase 234: live different operation conflicts'; else fail 'phase 234: live different operation did not conflict'; fi
      P234_HISTORY=$(p234_call /v1/agent_config/list_revisions phase234-live "{\"agent_id\":\"${P234_AGENT}\"}")
      if printf '%s' "$P234_HISTORY" | jq -e '.revisions | length > 0' >/dev/null; then ok 'phase 234: live immutable history remains readable'; else fail 'phase 234: live history missing after retirement'; fi
    else fail 'phase 234: live seed did not return content hash'; fi
  else skip 'phase 234: dev server unreachable — live assertions skipped'; fi
else skip 'phase 234: curl/jq/dev bearer unavailable — live assertions skipped'; fi

smoke_summary
