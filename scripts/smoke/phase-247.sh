#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 247 smoke — Durable observability rollups (HA-65, D-426). Shipped
# (v1.28): pins the shipped contract — the ONE bounded administrative query
# `observability.query`, the indexed triad projection of best-effort
# aggregates over successfully persisted canonical events (never
# billing-exact), fixed UTC minute storage buckets with exactly the
# authoritative tenant/user/session/model dimensions (no agent_id even
# conditionally), existing source-backed measures only, the explicit
# freshness/completeness state (current/catching_up/unavailable plus
# watermark/retention quality — never zero as a substitute for unavailable),
# the projection-backed session enricher with the honest fallback, the erasure
# fence, and the narrow D-296 amendment (general TSDB + identity-labelled OTel
# metrics still rejected). The live assertions (a >10,000-event durable
# session answers `observability.query` with projection-backed totals without
# `counters_partial` and without read-time scans; a stale/unavailable
# projection surfaces catching_up/unavailable — never zero; a widened fleet
# query emits the established audit evidence and an ordinary caller cannot
# enumerate another identity) are exercised by the phase's in-package suites
# (internal/observability/rollups/,
# internal/runtime/serve/projection_wiring_test.go), not duplicated here.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source scripts/smoke/common.sh
assert_file docs/plans/phase-247-observability-rollups.md "phase 247 plan exists"
assert_grep_present "D-426" docs/decisions.md "D-426 is recorded (HA-65)"
assert_grep_present "Shipped (v1.28)" docs/plans/README.md "phase 247 is Shipped (v1.28) in the master plan"
assert_grep_present "observability.query" docs/site/protocol/methods.md "the one administrative query is observability.query"
assert_grep_present "best-effort" docs/plans/phase-247-observability-rollups.md "best-effort rollups are documented (never billing-exact)"
assert_grep_present "watermark" docs/plans/phase-247-observability-rollups.md "durable applied-through watermark is documented"
assert_grep_present "MINUTE" docs/plans/phase-247-observability-rollups.md "fixed UTC minute storage bucket is the base grain (settled)"
assert_grep_present "agent_id.*is not a rollup" docs/plans/phase-247-observability-rollups.md "agent_id is not a rollup dimension (settled)"
assert_grep_present "unsupported" docs/plans/phase-247-observability-rollups.md "attempts/failed calls/retry-downgrade/task-spawned/user-message counts are unsupported/unavailable (settled)"
assert_grep_present "current" docs/plans/phase-247-observability-rollups.md "completeness states are documented"
assert_grep_present "D-296" docs/plans/phase-247-observability-rollups.md "D-296 amendment is documented"
assert_grep_present "indexed" docs/plans/phase-247-observability-rollups.md "indexed triad is documented"
assert_grep_present "quality" docs/site/protocol/types.md "the wire carries the freshness/quality block"
smoke_summary
