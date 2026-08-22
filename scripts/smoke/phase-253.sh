#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 253 smoke — v1.29.3 offline legacy durable-head integrity repair.
# This is a non-vacuous structural gate only. The real PostgreSQL fixture is
# selected by the hosted state-postgres CI job; this script never contacts a
# database and never runs local preflight.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file \
    "docs/plans/phase-253-v1293-legacy-head-repair.md" \
    "phase 253 plan exists"
assert_grep_present \
    '## D-433' \
    "docs/decisions.md" \
    "D-433 decision exists"
assert_grep_present \
    'HA-69 v1\.29\.3 compatibility extension' \
    "docs/notes/downstream-asks.md" \
    "HA-69 second compatibility extension is registered"
assert_file \
    "docs/recipes/repair-legacy-durable-heads.md" \
    "legacy-head repair recipe exists"
assert_file \
    "docs/site/recipes/repair-legacy-durable-heads.md" \
    "legacy-head repair site page exists"
assert_grep_present \
    'repair-legacy-durable-heads' \
    "docs/site/.vitepress/config.ts" \
    "legacy-head repair site navigation is indexed"
assert_grep_present \
    'repair-legacy-heads' \
    "docs/recipes/README.md" \
    "repair recipe is indexed"
assert_grep_present \
    'repair-legacy-heads' \
    "docs/CONFIG.md" \
    "offline repair is documented in CONFIG"
assert_grep_present \
    'repair-legacy-heads' \
    "examples/harbor.yaml" \
    "example config points to offline repair guidance"
assert_grep_present \
    'repair-legacy-heads' \
    "cmd/harbor/init/templates/default/harbor.yaml.tmpl" \
    "scaffold config points to offline repair guidance"
assert_grep_present \
    'newEventsCmd' \
    "cmd/harbor/root.go" \
    "root registers the repair command"
assert_file \
    "cmd/harbor/cmd_events_repair.go" \
    "repair CLI implementation exists"
assert_file \
    "internal/events/drivers/durable/repair.go" \
    "durable repair implementation exists"
assert_grep_present \
    'ListKindBounded' \
    "internal/state/state.go" \
    "StateStore exposes bounded maintenance enumeration"
assert_grep_present \
    'ListKindBounded' \
    "internal/state/drivers" \
    "all StateStore drivers implement bounded enumeration"
assert_grep_present \
    'InspectLegacyHeads' \
    "internal/events/drivers/durable/repair.go" \
    "inspect API is exported for the CLI seam"
assert_grep_present \
    'RepairLegacyHeads' \
    "internal/events/drivers/durable/repair.go" \
    "apply API is exported for the CLI seam"
assert_grep_present \
    'TestLegacyRepair_' \
    "internal/events/drivers/durable" \
    "durable repair adversarial tests exist"
assert_grep_present \
    'TestEventsRepairLegacyHeads_' \
    "cmd/harbor" \
    "CLI repair tests exist"
assert_grep_present \
    'TestLegacyRepair_PostgresDirect5432Acceptance' \
    "internal/events/drivers/durable" \
    "real direct-5432 repair acceptance exists"
assert_grep_present \
    'TestLegacyRepair_PostgresDirect5432Acceptance' \
    ".github/workflows/ci.yml" \
    "hosted CI selects the exact real-Postgres repair test"
assert_grep_present \
    'v1\.29\.3 legacy-head repair Postgres acceptance skipped or did not execute' \
    ".github/workflows/ci.yml" \
    "hosted CI rejects skipped or vacuous repair acceptance"
assert_grep_present \
    'freeze/drain' \
    "docs/recipes/repair-legacy-durable-heads.md" \
    "repair requires explicit writer freeze/drain"
assert_grep_present \
    '5432' \
    "docs/recipes/repair-legacy-durable-heads.md" \
    "repair recipe requires direct PostgreSQL"
assert_grep_present \
    '6432' \
    "internal/events/drivers/durable/repair.go" \
    "repair implementation rejects PgBouncer mutation"
assert_grep_present \
    'content-free' \
    "docs/plans/phase-253-v1293-legacy-head-repair.md" \
    "content-free receipt contract is planned"
assert_grep_present \
    'payload_metadata_hash_sha256' \
    "docs/plans/phase-253-v1293-legacy-head-repair.md" \
    "receipt schema binds payload metadata hash"
assert_grep_present \
    'receipt_version' \
    "docs/plans/phase-253-v1293-legacy-head-repair.md" \
    "receipt schema is versioned"
assert_grep_present \
    'repair-legacy-heads' \
    "CHANGELOG.md" \
    "public changelog names the repair command"

smoke_summary
