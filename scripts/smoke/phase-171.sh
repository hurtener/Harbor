#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 171 — events.aggregate durable-driver parity + conformance-matrix
# closure (HA-18 + HA-20, D-305). The aggregator sources its snapshot from the
# HistoryReplayer cross-session windowed fan-in (the same substrate events.list
# uses), threading the handler's server-derived `widened` decision, so
# events.aggregate returns on the durable driver instead of 500ing. One additive
# wire field (EventAggregateResponse.Truncated) — the DATA-not-500 partial signal.
#
# Live-server assertions (404/405/501 → SKIP per CLAUDE.md §4.2):
#   1. The route POST /v1/events/aggregate is mounted (a no-bearer POST → 401,
#      not the route-miss 404). 401 is ALSO the identity-mandatory check.
#   2. A structurally-valid windowed aggregate (dev token) → 200 with a
#      `buckets` array of the expected length. NOTE: preflight boots the INMEM
#      events driver, so this is the regression guard that the method still
#      works; the durable-parity proof (200 not 500 on durable) is the
#      integration test test/integration/events_aggregate_durable_test.go.
#   3. A cross-tenant aggregate body WITHOUT an elevated scope → 403
#      CodeIdentityScopeRequired.
# Static guards (always run, never skip): the additive-field invariant — the
# aggregate surface added ONLY EventAggregateResponse.Truncated (no method/error/
# event, no request-shape field) on internal/protocol/types/events.go; single-
# source method string; no Console import in the stream package.
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

skip "phase 171: smoke skeleton — replace with the events.aggregate durable-parity assertions when the phase implements its surface"

smoke_summary
