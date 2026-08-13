#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 245 smoke — Lifecycle-only session catalog and inspection projection
# (HA-63).
#
# PENDING STATIC SKELETON. Phase 245 is Planned (master-plan row 245 carries
# Status `Pending`), so this smoke records the plan + decision and pins the
# load-bearing plan contracts. It does NOT claim the surface is implemented:
# there is no live-server leg and no "surface works" assertion. When the
# phase ships, the implementor extends this script with the live assertions
# from the plan's "Smoke script additions" section (lifecycle list returns
# lifecycle fields with counter fields marked not_requested under the closed
# availability state current|partial|not_requested|unavailable — never
# zero-as-not-computed; a counter filter/sort paired
# with the lifecycle selector fails with the canonical typed error; the
# default projection still returns counters; cross-identity lifecycle reads
# are non-oracular not-found).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source scripts/smoke/common.sh
assert_file docs/plans/phase-245-session-lifecycle-projection.md "phase 245 plan exists"
assert_grep_present "D-424" docs/decisions.md "D-424 is recorded (HA-63)"
assert_grep_present "Pending" docs/plans/README.md "phase 245 is Planned/Pending in the master plan"
assert_grep_present 'projection: "lifecycle"' docs/plans/phase-245-session-lifecycle-projection.md "lifecycle selector is planned"
assert_grep_present "ZERO enrichment" docs/plans/phase-245-session-lifecycle-projection.md "zero-enrichment lifecycle path is planned"
assert_grep_present "typed invalid request" docs/plans/phase-245-session-lifecycle-projection.md "counter-filter/sort rejection is planned"
assert_grep_present "not_requested" docs/plans/phase-245-session-lifecycle-projection.md "lifecycle counters are not_requested (closed availability state)"
assert_grep_present "current \\| partial \\| not_requested \\| unavailable" docs/plans/phase-245-session-lifecycle-projection.md "closed CounterStatus is current|partial|not_requested|unavailable"
assert_grep_present "never means" docs/plans/phase-245-session-lifecycle-projection.md "explicit counter availability is planned"
smoke_summary
