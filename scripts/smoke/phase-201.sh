#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 201 — Skills Postgres driver (durable/shared storage).
#
# When the surface lands, assert (static):
#   - internal/skills/drivers/postgres/ exists and implements SkillStore.
#   - its blank import is present in internal/drivers/prod/prod.go (next to localdb).
# The behavioral parity gate is the shared internal/skills/conformancetest suite
# run against a real Postgres in CI, not this smoke.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file "internal/skills/drivers/postgres/postgres.go" "skills postgres driver package exists"
assert_file "internal/skills/drivers/postgres/search.go" "skills postgres FTS ranking ladder exists"
assert_file "internal/skills/drivers/postgres/migrations/0001_init.sql" "skills postgres forward-only migration exists"

# The driver self-registers under "postgres" from init().
assert_grep_present 'skills.Register\(driverName' \
    "internal/skills/drivers/postgres/postgres.go" \
    "skills postgres driver self-registers via skills.Register"

# Its blank import is present in the single sanctioned aggregator home (D-196),
# next to the localdb line.
assert_grep_present 'internal/skills/drivers/postgres' \
    "internal/drivers/prod/prod.go" \
    "skills postgres driver blank-imported in internal/drivers/prod"

# Migration is forward-only + idempotent (ON CONFLICT DO NOTHING into schema_migrations).
assert_grep_present 'ON CONFLICT DO NOTHING' \
    "internal/skills/drivers/postgres/migrations/0001_init.sql" \
    "skills postgres migration records its version idempotently"

smoke_summary
