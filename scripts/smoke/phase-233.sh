#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 233 — mandatory StateStore conditional save.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source "scripts/smoke/common.sh"

STATE_GO='internal/state/state.go'
INMEM_GO='internal/state/drivers/inmem/inmem.go'
SQLITE_GO='internal/state/drivers/sqlite/sqlite.go'
POSTGRES_GO='internal/state/drivers/postgres/postgres.go'
AGENTCFG_GO='internal/agentcfg/drivers/statestore/statestore.go'
P233_TMP="$(mktemp -d "${TMPDIR:-/tmp}/harbor-phase233.XXXXXX")"
trap 'rm -rf "${P233_TMP}"' EXIT

phase233_sha256() {
  local file="$1"
  if command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "${file}" | awk '{print $1}'
    return
  fi
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "${file}" | awk '{print $1}'
    return
  fi
  return 127
}

assert_frozen_migration_hash() {
  local file="$1"
  local expected="$2"
  local actual
  if ! actual="$(phase233_sha256 "${file}")"; then
    fail "phase 233: no SHA-256 utility available for ${file}"
    return
  fi
  if [[ "${actual}" == "${expected}" ]]; then
    ok "phase 233: frozen migration hash matches (${file})"
    return
  fi
  fail "phase 233: frozen migration hash changed (${file}; got ${actual}, want ${expected})"
}

assert_grep_present 'SaveIf\(ctx context\.Context, expectations \[\]SlotExpectation, next StateRecord\)' "${STATE_GO}" \
  'phase 233: StateStore declares mandatory SaveIf'
assert_grep_present 'ErrConditionFailed = errors\.New' "${STATE_GO}" \
  'phase 233: condition-failed sentinel is declared'
assert_grep_present 'func \(d \*driver\) SaveIf' "${INMEM_GO}" \
  'phase 233: in-memory driver implements SaveIf'
assert_grep_present 'func \(d \*driver\) SaveIf' "${SQLITE_GO}" \
  'phase 233: SQLite driver implements SaveIf'
assert_grep_present 'func \(d \*driver\) SaveIf' "${POSTGRES_GO}" \
  'phase 233: Postgres driver implements SaveIf'
assert_grep_present 'conditionalAdvisoryLockIDs' "${POSTGRES_GO}" \
  'phase 233: Postgres orders actual advisory lock IDs'
assert_grep_present 'validateTxlock' "${SQLITE_GO}" \
  'phase 233: SQLite rejects unsafe transaction lock modes'
assert_grep_present 'activeExpectations' "${AGENTCFG_GO}" \
  'phase 233: agent-config conditions pointer generations'
assert_frozen_migration_hash 'internal/state/drivers/sqlite/migrations/0001_init.sql' \
  '79eae9b5908a0fd242a7dc0ce300ed769f7901e84cce9b298050297fcb96adc7'
assert_frozen_migration_hash 'internal/state/drivers/postgres/migrations/0001_init.sql' \
  '3b06f45ca3febb73c79b6acb60786878222bd9cc2aaa6f74c17a6de9a6d29e59'

assert_go_tests_pass "${P233_TMP}/conformance.log" '-race -count=1 ./internal/state/conformancetest/' \
  'phase 233: conditional-save StateStore conformance' \
  'TestRun_SelfApplied'
assert_go_tests_pass "${P233_TMP}/agentcfg.log" '-race -count=1 ./internal/agentcfg/drivers/statestore/' \
  'phase 233: shared-SQLite agent-config one-winner race' \
  'TestConditionalWrite_SharedSQLiteTwoRegistries_OneWinner'

smoke_summary
