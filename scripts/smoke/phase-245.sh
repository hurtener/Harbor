#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 245 smoke — Lifecycle-only session catalog and inspection projection
# (HA-63, D-424). Shipped (v1.28): pins the shipped contract — the additive
# `projection: "lifecycle"` selector on `sessions.list` / `sessions.inspect`
# with ZERO enrichment, the closed counter availability state
# `current | partial | not_requested | unavailable` (lifecycle counters
# `not_requested`, never zero-as-not-computed), the typed invalid request for
# a counter filter/sort paired with lifecycle, and the full projection
# remaining the default. The live assertions (a >100,000-event durable
# session: lifecycle reads perform zero enricher reads, bounded by page size
# before/after restart; a page of N rows never runs N counter scans; a
# `cost_desc` sort or `cost_above_cents` filter with lifecycle fails typed;
# the default projection still returns counters; cross-identity lifecycle
# reads are non-oracular not-found) are exercised by the phase's in-package
# suites (internal/sessions/protocol/,
# internal/runtime/serve/serve_seams_test.go), not duplicated here.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source scripts/smoke/common.sh
assert_file docs/plans/phase-245-session-lifecycle-projection.md "phase 245 plan exists"
assert_grep_present "D-424" docs/decisions.md "D-424 is recorded (HA-63)"
assert_grep_present "Shipped (v1.28)" docs/plans/README.md "phase 245 is Shipped (v1.28) in the master plan"
assert_grep_present 'projection: "lifecycle"' docs/plans/phase-245-session-lifecycle-projection.md "lifecycle selector is documented"
assert_grep_present "ZERO enrichment" docs/plans/phase-245-session-lifecycle-projection.md "zero-enrichment lifecycle path is documented"
assert_grep_present "typed invalid request" docs/plans/phase-245-session-lifecycle-projection.md "counter-filter/sort rejection is documented"
assert_grep_present "not_requested" docs/plans/phase-245-session-lifecycle-projection.md "lifecycle counters are not_requested (closed availability state)"
assert_grep_present "current \\| partial \\| not_requested \\| unavailable" docs/plans/phase-245-session-lifecycle-projection.md "closed CounterStatus is current|partial|not_requested|unavailable"
assert_grep_present "never zero-as-not-computed" docs/plans/phase-245-session-lifecycle-projection.md "zero never means not-computed"
assert_grep_present "defaults to" docs/plans/phase-245-session-lifecycle-projection.md "an omitted selector defaults to full"
assert_grep_present "lifecycle" docs/site/protocol/types.md "the wire carries the lifecycle projection selector"
smoke_summary
