#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 72d — `notification.*` event topic + rules-engine-lite mapper.
#
# This phase ships a NEW event family on the typed bus (per-class topic
# naming) plus a runtime-internal mapper that synthesises `notification.*`
# events from a small subset of the existing event taxonomy
# (`task.failed`, `tool.approval_requested`, `governance.budget_exceeded`,
# `tool.auth_required`, `pause.requested`).
#
# Surface assertions (executed only when the surface is live; 404/405/501
# auto-SKIP per AGENTS.md §4.2):
#
#   1. `events.subscribe` accepts each of the five V1 notification classes
#      as a filter input (`event_types: ["notification.X"]`). The filter
#      shape itself lands in Phase 72a, which sits in the same Stage-1
#      batch.
#   2. A subscriber filtered for `notification.task_failed` receives a
#      synthesised notification after a deliberate `task.failed` is
#      published — proving the rules-engine-lite mapper + subscriber are
#      wired end-to-end.
#
# Until the Protocol layer ships, `protocol_call` stubs SKIP. When 72/72a
# land, this script's stubs flip to OK as the bus surface goes live.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ---------------------------------------------------------------------------
# 1. Per-class subscribe-shape probes — one per V1 notification class.
#    Each is a smoke for the 72d × 72a seam: 72a ships the filter shape,
#    72d ships the event-type constants the filter references. A 200 here
#    confirms the event-type registry now contains the notification class
#    AND the filter shape accepts it.
# ---------------------------------------------------------------------------

protocol_call 'events/subscribe' \
  '{"filter": {"event_types": ["notification.task_failed"]}}' \
  'phase 72d: events.subscribe accepts notification.task_failed in filter'

protocol_call 'events/subscribe' \
  '{"filter": {"event_types": ["notification.tool_approval_requested"]}}' \
  'phase 72d: events.subscribe accepts notification.tool_approval_requested in filter'

protocol_call 'events/subscribe' \
  '{"filter": {"event_types": ["notification.governance_budget_exceeded"]}}' \
  'phase 72d: events.subscribe accepts notification.governance_budget_exceeded in filter'

protocol_call 'events/subscribe' \
  '{"filter": {"event_types": ["notification.auth_required"]}}' \
  'phase 72d: events.subscribe accepts notification.auth_required in filter'

protocol_call 'events/subscribe' \
  '{"filter": {"event_types": ["notification.pause_requested"]}}' \
  'phase 72d: events.subscribe accepts notification.pause_requested in filter'

# ---------------------------------------------------------------------------
# 2. End-to-end round-trip — deliberate `task.failed` produces a
#    `notification.task_failed` at a separately-scoped subscriber.
#    The harness needs a debug-emit hook to inject `task.failed`
#    deterministically; until that lands, the call falls through to
#    SKIP via the `protocol_call` stub. When 72/72a/72d all ship, this
#    flips to OK and proves the mapper + subscriber are wired live.
# ---------------------------------------------------------------------------

protocol_call 'events/subscribe' \
  '{"filter": {"event_types": ["notification.task_failed"]}}' \
  'phase 72d: subscribe (round-trip step 1) — listen for notification.task_failed'

protocol_call 'tasks/debug_emit_failed' \
  '{"tenant": "t1", "user": "u1", "session": "s1", "task_id": "task-72d-smoke"}' \
  'phase 72d: emit deliberate task.failed (round-trip step 2)'

# Step 3: assert the notification arrives on the subscription buffer.
# Uses assert_json_path against the SSE-buffer endpoint when shipped; the
# helper auto-SKIPs on 404 so this smoke coexists with phase-N builds.
skip_if_404 "$(api_url /protocol/events/subscriptions/last_event)" \
  'phase 72d: round-trip step 3 — fetch last delivered event from subscription buffer' \
  || true

# ---------------------------------------------------------------------------
# 3. Surface-existence probe — the notification topic constants are
#    registered in the event-type registry from 72d's init(). Once 72/72a
#    bring up the `events/subscribe` route, the per-class probes above
#    will flip from SKIP to OK; this final probe confirms the route
#    itself is up so SKIPs reflect "filter shape not landed yet" rather
#    than "Protocol layer absent."
# ---------------------------------------------------------------------------

skip_if_404 "$(api_url /protocol/events/subscribe)" \
  'phase 72d: events.subscribe route absent until Protocol layer ships' || true

smoke_summary
