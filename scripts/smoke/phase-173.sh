#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 173 — events.aggregate per-tenant attribution for admin-widened reads
# (HA-17, D-307). Opt-in `by_tenant` on EventAggregateRequest adds
# `counts_by_tenant` (tenant → event_type → count) to the response FOR admin-
# widened reads only, for tenants the caller is already entitled to. Additive
# wire fields (D-223/D-209 lockstep).
#
# Live-server assertions (404/405/501 → SKIP per CLAUDE.md §4.2):
#   1. The route POST /v1/events/aggregate is mounted (no-bearer POST → 401).
#   2. With an admin/console:fleet dev token: an aggregate POST with
#      `by_tenant: true` + a cross-tenant filter → 200 with `counts_by_tenant`
#      present, and the per-tenant counts summing to the bucket totals.
#   3. An aggregate POST WITHOUT `by_tenant` → 200 with NO `counts_by_tenant`
#      key (backward compatible).
#   4. A non-elevated dev token with `by_tenant: true` + a cross-tenant filter
#      → 403 (the widening gate fires before attribution; attribution never
#      bypasses it).
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

skip "phase 173: smoke skeleton — replace with the per-tenant attribution assertions when the phase implements its surface"

smoke_summary
