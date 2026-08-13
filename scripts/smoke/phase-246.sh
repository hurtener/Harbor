#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 246 smoke — Durable tail-paged conversation turns (HA-64).
#
# PENDING STATIC SKELETON. Phase 246 is Planned (master-plan row 246 carries
# Status `Pending`), so this smoke records the plan + decision and pins the
# load-bearing plan contracts. It does NOT claim the surface is implemented:
# there is no live-server leg and no "surface works" assertion. When the
# phase ships, the implementor extends this script with the live assertions
# from the plan's "Smoke script additions" section (open a durable session's
# chat in two reads — lifecycle + turn page — and assert the newest 20 turns
# render without a per-turn tasks.get/events.list; paging older history has
# no skip/duplicate while a new turn starts; inline Activity returns ordered
# entries with no arguments/results, and the conditional activity fallback
# ships only if the response ceiling forces it; cross-identity turns are
# typed not-found).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source scripts/smoke/common.sh
assert_file docs/plans/phase-246-durable-conversation-turns.md "phase 246 plan exists"
assert_grep_present "D-425" docs/decisions.md "D-425 is recorded (HA-64)"
assert_grep_present "Pending" docs/plans/README.md "phase 246 is Planned/Pending in the master plan"
assert_grep_present "sessions.turns.list" docs/plans/phase-246-durable-conversation-turns.md "tail-paged turn list is planned"
assert_grep_present "sessions.turns.get" docs/plans/phase-246-durable-conversation-turns.md "terminal reconciliation read is planned"
assert_grep_present "conditional fallback" docs/plans/phase-246-durable-conversation-turns.md "a named activity method is only a conditional fallback (settled)"
assert_grep_present "two reads" docs/plans/phase-246-durable-conversation-turns.md "two-read chat open is planned"
assert_grep_present "idempotent sequence checkpoints" docs/plans/phase-246-durable-conversation-turns.md "incremental projection is planned"
assert_grep_present "erased/fenced" docs/plans/phase-246-durable-conversation-turns.md "erasure fence is planned"
smoke_summary
