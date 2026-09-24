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

# 1. The SQLite memory driver passes the full conformance suite across
#    all two modes under -race (delegation to the shared
#    cumulative owner), plus the restart-rehydration durability test.
if go test -race -count=1 -timeout 180s ./internal/memory/drivers/sqlite/... >/dev/null 2>&1; then
  ok "phase 25a: sqlite memory conformance (none/rolling_summary) + rehydration passes under -race"
else
  fail "phase 25a: sqlite memory driver tests failed (run \`go test -race ./internal/memory/drivers/sqlite/...\` for detail)"
fi

# 2. The Postgres memory driver tests (skip-clean without HARBOR_PG_DSN;
#    exercise all two modes + rehydration when the DSN is set).
if [ -z "${HARBOR_PG_DSN:-}" ]; then
  skip "phase 25a: HARBOR_PG_DSN unset; PostgreSQL conformance not exercised"
elif go test -race -count=1 -timeout 180s ./internal/memory/drivers/postgres/... >/dev/null 2>&1; then
  ok "phase 25a: postgres memory driver tests pass under -race"
else
  fail "phase 25a: postgres memory driver tests failed (run \`go test -race ./internal/memory/drivers/postgres/...\` for detail)"
fi

# 3. All drivers delegate execution history to the same cumulative owner.
if go test -race -count=1 -timeout 120s ./internal/memory/ ./internal/memory/session/... >/dev/null 2>&1; then
  ok "phase 25a: memory registry + cumulative owner pass under -race"
else
  fail "phase 25a: memory registry/owner tests failed (run \`go test -race ./internal/memory/...\` for detail)"
fi

smoke_summary
