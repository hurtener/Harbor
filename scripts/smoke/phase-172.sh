#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 172 — events.aggregate origin-anchored (epoch-aligned) bucket grid
# (HA-16, D-306). An optional `anchor` on EventAggregateRequest floors bucket
# boundaries onto a fixed grid (anchor + k*Bucket); absent ⇒ today's
# clock-anchored behaviour. Additive wire field (D-223/D-209 lockstep).
#
# Live-server assertions (404/405/501 → SKIP per CLAUDE.md §4.2):
#   1. The route POST /v1/events/aggregate is mounted (no-bearer POST → 401).
#   2. Two aggregate POSTs (dev token) with the SAME `anchor` + window + bucket
#      a short interval apart → both 200, and at least one shared `bucket_start`
#      instant across the two responses (the addressability proof — a bucket
#      coordinate is re-requestable).
#   3. An aggregate POST with NO `anchor` → 200 with the same bucket count as
#      today (the backward-compatible clock-anchored path).
#
# Replace the `skip` below with the assertions above when the phase implements.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Phase NN assertions go below. Examples:
#
#   assert_status 200 "$(api_url /healthz)" "healthz returns 200"
#   assert_json_path '.status' 'ok' "$(api_url /readyz)" "readyz reports status=ok"
#   protocol_call 'sessions/create' '{"tenant":"t1","user":"u1"}' "create session"
#
# Until the phase ships, the script can be empty assertions or a single
# `skip "phase NN: not yet implemented"` to keep preflight green.
# ----------------------------------------------------------------------------

skip "phase 172: smoke skeleton — replace with the origin-anchored bucket-grid assertions when the phase implements its surface"

smoke_summary
