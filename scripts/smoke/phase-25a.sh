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

# Keep each failed suite's diagnostic output visible in the preflight log.
GO_LOG="$(mktemp "${TMPDIR:-/tmp}/harbor-phase-25a-go.XXXXXX")"
trap 'rm -f "${GO_LOG}"' EXIT

# 1. The SQLite memory driver passes the full conformance suite across
#    all two modes under -race (delegation to the shared
#    cumulative owner), plus the restart-rehydration durability test.
if go test -race -count=1 -timeout 180s ./internal/memory/drivers/sqlite/... >"${GO_LOG}" 2>&1; then
  ok "phase 25a: sqlite memory conformance (none/rolling_summary) + rehydration passes under -race"
else
  fail "phase 25a: sqlite memory driver tests failed (run \`go test -race ./internal/memory/drivers/sqlite/...\` for detail)"
  # Keep the failure headline as well as the end of a long timeout trace.
  head -40 "${GO_LOG}"
  tail -80 "${GO_LOG}"
fi

# 2. The Postgres memory driver tests (skip-clean without HARBOR_PG_DSN;
#    exercise all two modes + rehydration when the DSN is set).
if [ -z "${HARBOR_PG_DSN:-}" ]; then
  skip "phase 25a: HARBOR_PG_DSN unset; PostgreSQL conformance not exercised"
elif go test -race -count=1 -timeout 180s ./internal/memory/drivers/postgres/... >"${GO_LOG}" 2>&1; then
  ok "phase 25a: postgres memory driver tests pass under -race"
else
  fail "phase 25a: postgres memory driver tests failed (run \`go test -race ./internal/memory/drivers/postgres/...\` for detail)"
  # Keep the failure headline as well as the end of a long timeout trace.
  head -40 "${GO_LOG}"
  tail -80 "${GO_LOG}"
fi

# 3. All drivers delegate execution history to the same cumulative owner.
# The owner now includes full 260-turn / 300-step / large-evidence regressions
# on three stores. This is an aggregate correctness-suite budget (Go's normal
# default), not a performance SLA; per-operation deadlines remain unchanged.
if go test -race -count=1 -timeout 10m ./internal/memory/ ./internal/memory/session/... >"${GO_LOG}" 2>&1; then
  ok "phase 25a: memory registry + cumulative owner pass under -race"
else
  fail "phase 25a: memory registry/owner tests failed (run \`go test -race ./internal/memory/...\` for detail)"
  # Keep the failure headline as well as the end of a long timeout trace.
  head -40 "${GO_LOG}"
  tail -80 "${GO_LOG}"
fi

smoke_summary
