#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 252 smoke — v1.29.2 durable-event legacy-backfill compatibility.
# This is a non-vacuous structural gate only. The real PostgreSQL fixture is
# selected by the hosted state-postgres CI job; this script never contacts a
# database and never runs local preflight.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file \
    "docs/plans/phase-252-v1292-durable-backfill-compatibility.md" \
    "phase 252 plan exists"
assert_grep_present \
    '## D-432' \
    "docs/decisions.md" \
    "D-432 decision exists"
assert_grep_present \
    'HA-69 v1\.29\.2 compatibility extension' \
    "docs/notes/downstream-asks.md" \
    "HA-69 v1.29.2 extension is registered"
assert_grep_present \
    'ev\.Sequence != seq \|\| ev\.Identity\.Identity != id\.Identity' \
    "internal/events/drivers/durable/durable.go" \
    "backfill validates sequence and session triple"
assert_grep_absent \
    'ev\.Identity\.RunID != id\.RunID' \
    "internal/events/drivers/durable/durable.go" \
    "backfill does not compare payload RunID with storage RunID"
assert_grep_present \
    'TestDurable_MetadataIndex_LegacySessionHeadPreservesPayloadRunIDs' \
    "internal/events/drivers/durable/metadata_test.go" \
    "multi-run legacy fixture is present"
assert_grep_present \
    'TestDurable_Postgres_LegacyBackfillPreservesPayloadRunIDs' \
    "internal/events/drivers/durable/postgres_backfill_test.go" \
    "real PostgreSQL backfill fixture is present"
assert_grep_present \
    'TestDurable_Postgres_LegacyBackfillPreservesPayloadRunIDs' \
    ".github/workflows/ci.yml" \
    "hosted CI selects the exact real-Postgres test"
assert_grep_present \
    'v1\.29\.2 durable Postgres backfill acceptance skipped or did not execute' \
    ".github/workflows/ci.yml" \
    "hosted CI rejects skipped or vacuous Postgres acceptance"
assert_grep_present \
    '## \[1\.29\.2\]' \
    "CHANGELOG.md" \
    "v1.29.2 release notes exist"

smoke_summary
