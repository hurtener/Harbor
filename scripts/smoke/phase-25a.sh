#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 25a smoke — durable memory strategies on the SQL drivers.
# Runs the memory conformance suite for the SQLite (and Postgres when
# HARBOR_PG_DSN is set) memory drivers across the two modes
# (none / rolling_summary), and asserts the SQL drivers
# no longer reject non-none strategies. No live server needed.
#
# The sqlite leg runs everywhere (modernc.org is CGo-free + bundles
# the engine). The postgres leg t.Skips cleanly without HARBOR_PG_DSN;
# CI's memory-postgres job sets it against the postgres:16 service
# container so the suite actually exercises that driver there.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

GOLOG_25A="$(mktemp "${TMPDIR:-/tmp}/phase25a-gotest.XXXXXX")"
trap 'rm -f "${GOLOG_25A}"' EXIT

# 1. The SQLite memory driver passes the full conformance suite across
#    all two modes under -race (delegation to the shared
#    cumulative owner), plus the restart-rehydration durability test.
if go test -race -count=1 -timeout 180s ./internal/memory/drivers/sqlite/... >"${GOLOG_25A}" 2>&1; then
  ok "phase 25a: sqlite memory conformance (none/rolling_summary) + rehydration passes under -race"
else
  tail -n 80 "${GOLOG_25A}"
  fail "phase 25a: sqlite memory driver tests failed (run \`go test -race ./internal/memory/drivers/sqlite/...\` for detail)"
fi

# 2. The Postgres memory driver tests (skip-clean without HARBOR_PG_DSN;
#    exercise all two modes + rehydration when the DSN is set).
if [ -z "${HARBOR_PG_DSN:-}" ]; then
  skip "phase 25a: HARBOR_PG_DSN unset; PostgreSQL conformance not exercised"
elif go test -race -count=1 -timeout 180s ./internal/memory/drivers/postgres/... >"${GOLOG_25A}" 2>&1; then
  ok "phase 25a: postgres memory driver tests pass under -race"
else
  tail -n 80 "${GOLOG_25A}"
  fail "phase 25a: postgres memory driver tests failed (run \`go test -race ./internal/memory/drivers/postgres/...\` for detail)"
fi

# 3. Require the registry and the durable owner's original contracts at this
# phase's existing bound. Phase 269 owns the expanded exact-evidence, large
# receipt, dispatch-journal and checkpoint suites; its full regression legs
# and make test still execute those tests under -race. Running that entire
# newer package here exceeded this historical two-minute smoke on Linux.
if go test -race -count=1 -timeout 120s ./internal/memory/ >"${GOLOG_25A}" 2>&1; then
  ok "phase 25a: complete memory registry passes under -race"
else
  tail -n 80 "${GOLOG_25A}"
  fail "phase 25a: memory registry tests failed"
fi
assert_go_tests_pass "${GOLOG_25A}" '-race -count=1 -timeout=120s ./internal/memory/session' \
  'phase 25a: cumulative owner durability, isolation, expiry and fail-closed contracts' \
  TestRetainedContext_ExactEvidenceRestartAndNoRecursiveHistory \
  TestRetainedCumulative_FailedRolloverPreservesCommittedState \
  TestRetainedCumulative_RolloverAndRestartDoNotRenewExpiry \
  TestRetainedContext_ErasureAndCorruptionFailClosed \
  TestSessionInspection_ConcurrentIdentityIsolation \
  TestSessionInspection_CumulativeDeletionFencesExecution
assert_grep_present 'TestRetainedCumulative_' scripts/smoke/phase-269.sh \
  'phase 25a: expanded cumulative evidence suite remains delegated to phase 269'
assert_grep_present 'TestRetainedJournal_' scripts/smoke/phase-269.sh \
  'phase 25a: expanded dispatch-journal suite remains delegated to phase 269'

smoke_summary
