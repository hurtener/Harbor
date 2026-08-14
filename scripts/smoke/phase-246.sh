#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 246 smoke — Durable tail-paged conversation turns (HA-64, D-425).
# Shipped (v1.28): pins the shipped contract — `sessions.turns.list` / `.get`
# as the two named public methods, the dedicated runtime-owned durable
# projection, the two-read chat open (one Phase 245 lifecycle read + one turn
# page), the page-before-subscribe snapshot-to-live handoff (fold the durable
# page and establish bounded running/paused membership BEFORE opening the
# EventSource with `live_resume_seq` as the initial `resume_seq`; the server
# replays events strictly newer than the snapshot; reconnect Last-Event-ID
# takes precedence; one terminal event causes one `sessions.turns.get`; a page
# retry rebuilds stale live membership from freshly read authoritative rows
# without duplicating bubbles or re-admitting a terminal row), and the
# deferred named-activity follow-up. The live assertions (two-read open of a
# >100,000-event / >=10,000-turn session; no skip/duplicate while paging; no
# per-turn tasks.get/events.list; inline Activity without arguments/results;
# cross-identity typed not-found) are exercised by the phase's in-package
# suites (internal/sessions/turns/ incl. the materializer,
# internal/runtime/serve/projection_wiring_test.go), not duplicated here.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source scripts/smoke/common.sh
assert_file docs/plans/phase-246-durable-conversation-turns.md "phase 246 plan exists"
assert_grep_present "D-425" docs/decisions.md "D-425 is recorded (HA-64)"
assert_grep_present "Shipped \(v1\.28\)" docs/plans/README.md "phase 246 is Shipped (v1.28) in the master plan"
assert_grep_present "sessions.turns.list" docs/site/protocol/methods.md "turns.list is a canonical Protocol method"
assert_grep_present "sessions.turns.get" docs/site/protocol/methods.md "turns.get is a canonical Protocol method"
assert_grep_present "live_resume_seq" docs/site/protocol/types.md "the wire carries the live_resume_seq resume cursor"
assert_grep_present "two reads" docs/plans/phase-246-durable-conversation-turns.md "two-read chat open is documented"
assert_grep_present "deferred follow-up" docs/plans/phase-246-durable-conversation-turns.md "a named activity method is not a v1.28 method or acceptance (settled)"
assert_grep_present "idempotent sequence checkpoints" docs/plans/phase-246-durable-conversation-turns.md "incremental projection is documented"
assert_grep_present "erased/fenced" docs/plans/phase-246-durable-conversation-turns.md "erasure fence is documented"

# Page-before-subscribe snapshot-to-live handoff: the accepted contract is
# fold-then-subscribe, never the stale subscribe-before-page wording. Pin the
# exact sentences on every owned surface.
assert_grep_present "page-before-subscribe" docs/plans/phase-246-durable-conversation-turns.md "plan pins the page-before-subscribe handoff"
assert_grep_present "lost-wake poll" docs/plans/phase-246-durable-conversation-turns.md "bounded durable late task-record/answer convergence (lost-wake poll) is pinned"
assert_grep_present "page-before-subscribe" docs/plans/README.md "master plan pins the page-before-subscribe handoff"
assert_grep_present "page-before-subscribe" docs/decisions.md "D-425 pins the page-before-subscribe handoff"
assert_grep_present "page-before-subscribe" docs/glossary.md "glossary pins the page-before-subscribe handoff"
assert_grep_present "live_resume_seq.*initial" RFC-001-Harbor.md "RFC pins live_resume_seq as the initial resume_seq"
assert_grep_present "Last-Event-ID" RFC-001-Harbor.md "RFC pins reconnect Last-Event-ID precedence"
assert_grep_absent "subscribe-before-page" docs/plans/phase-246-durable-conversation-turns.md "no stale subscribe-before-page claim remains in the plan"
assert_grep_absent "subscribe-before-page" docs/plans/README.md "no stale subscribe-before-page claim remains in the master plan"
assert_grep_absent "subscribe-before-page" docs/decisions.md "no stale subscribe-before-page claim remains in D-425"
assert_grep_absent "subscribe-before-page" docs/glossary.md "no stale subscribe-before-page claim remains in the glossary"
smoke_summary
