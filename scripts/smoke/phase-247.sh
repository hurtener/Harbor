#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 247 smoke — Durable observability rollups (HA-65).
#
# PENDING STATIC SKELETON. Phase 247 is Planned (master-plan row 247 carries
# Status `Pending`), so this smoke records the plan + decision and pins the
# load-bearing plan contracts. It does NOT claim the surface is implemented:
# there is no live-server leg and no "surface works" assertion. When the
# phase ships, the implementor extends this script with the live assertions
# from the plan's "Smoke script additions" section (a >10,000-event durable
# session answers the one administrative query `observability.query` with
# projection-backed totals
# without counters_partial and without read-time scans; a stale/unavailable
# projection surfaces catching_up/unavailable — never zero — and the session
# enricher falls back honestly; a widened fleet query emits the established
# audit evidence and an ordinary caller cannot enumerate another identity;
# the storage dimension set is exactly the fixed UTC minute bucket plus
# authoritative tenant/user/session/model — no agent_id, query may coarsen;
# attempts/failed calls/retry-downgrade/task-spawned/user-message counts are
# unsupported/unavailable and never trigger new canonical events).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source scripts/smoke/common.sh
assert_file docs/plans/phase-247-observability-rollups.md "phase 247 plan exists"
assert_grep_present "D-426" docs/decisions.md "D-426 is recorded (HA-65)"
assert_grep_present "Pending" docs/plans/README.md "phase 247 is Planned/Pending in the master plan"
assert_grep_present "best-effort" docs/plans/phase-247-observability-rollups.md "best-effort rollups are planned (never billing-exact)"
assert_grep_present "watermark" docs/plans/phase-247-observability-rollups.md "durable watermark is planned"
assert_grep_present "current" docs/plans/phase-247-observability-rollups.md "completeness states are planned"
assert_grep_present "D-296" docs/plans/phase-247-observability-rollups.md "D-296 amendment is planned"
assert_grep_present "indexed" docs/plans/phase-247-observability-rollups.md "indexed triad is planned"
assert_grep_present "observability.query" docs/plans/phase-247-observability-rollups.md "the one administrative query is observability.query"
assert_grep_present "not a rollup" docs/plans/phase-247-observability-rollups.md "agent_id is not a rollup dimension (settled)"
assert_grep_present "MINUTE" docs/plans/phase-247-observability-rollups.md "fixed UTC minute storage bucket is the base grain (settled)"
assert_grep_present "unsupported" docs/plans/phase-247-observability-rollups.md "attempts/failed calls/retry-downgrade/task-spawned/user-message counts are unsupported/unavailable (settled)"
smoke_summary
