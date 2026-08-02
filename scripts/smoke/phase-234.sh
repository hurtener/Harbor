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

assert_go_tests_pass "${P234_TMP}/retirement.log" '-race -count=1 ./internal/agentcfg/drivers/statestore ./internal/runtime/agentcfg/protocol ./internal/runtime/serve ./internal/protocol/transports/stream' \
  'phase 234: terminal state, frozen cleanup, protocol replay, and start refusal run under race' \
  TestRetirement_TerminalHistoryAndReplay \
  TestRetirement_ConcurrentSameOperationAndTenantIsolation \
  TestRetirement_ProgressIsFrozenCASState \
  TestAgentConfigHandler_Retire_AdminReplayAndTerminalRefusal \
  TestAgentResolverAdapter_DefaultTombstoneWins

smoke_summary
