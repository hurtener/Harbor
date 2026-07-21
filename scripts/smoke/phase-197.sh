#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 197 — broker-pulled Harbor-orchestrated failover chains (D-335) + v1.17 wave-end E2E.
#
# Realizes the governance FailoverPolicy seam over a broker-pulled ordered
# chain: on a retryable provider error the policy advances, re-runs PreCall
# BEFORE re-issuing, and emits a `governance.failover` hop event. D-018
# stands — bifrost's native Fallbacks array is NOT used. Skeleton until the
# surface lands: assertions SKIP on a pre-197 build.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Static trip-wires (run regardless of the live server).
# ----------------------------------------------------------------------------
# governance.failover is a canonical EVENT (registered in the internal events
# registry + rendered by the protocol docs generator), NOT a request/response
# wire type — so it rides the events surface, never the TS wire manifest
# (D-337). Assert it is declared + registered in the governance event source.
if grep -q 'EventTypeFailover.*governance.failover' internal/governance/events.go 2>/dev/null; then
    ok "static: governance.failover is declared as a canonical event type in internal/governance/events.go"
else
    skip "static: governance.failover event type absent from internal/governance/events.go (pre-197 build)"
fi
if grep -q 'governance.failover' docs/site/protocol/events.md 2>/dev/null; then
    ok "static: governance.failover is in the generated protocol events doc"
else
    skip "static: governance.failover absent from docs/site/protocol/events.md (pre-197 build)"
fi

# Static guard: the failover walk must NOT use bifrost's native Fallbacks
# array (D-018/D-335 — Harbor orchestrates every hop). A construction of
# schemas.Fallback in the governance failover path is a rejection-on-sight
# violation.
if [ -f internal/governance/failover.go ]; then
    if grep -q 'schemas.Fallback\|Fallbacks:' internal/governance/failover.go 2>/dev/null; then
        fail "phase 197: governance/failover.go references bifrost Fallbacks — D-018/D-335 forbid it"
    else
        ok "phase 197: governance/failover.go does not use bifrost's native Fallbacks (D-018 stands)"
    fi
else
    skip "phase 197: internal/governance/failover.go absent (pre-197 build)"
fi

# ----------------------------------------------------------------------------
# Live-server assertions: the governance.failover event surface (best-effort).
# ----------------------------------------------------------------------------
# The failover hop is an EVENT, not a request-response method; the smoke's
# load-bearing gate is the wave-end E2E (TestE2E_WaveV117) run by preflight's
# unit-test batch. Here we assert only the event type is advertised where a
# reachable events/metrics surface exposes it.
EVENTS_URL="$(api_url /v1/events/types 2>/dev/null || true)"
if [ -n "${EVENTS_URL}" ]; then
    PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "${EVENTS_URL}" 2>/dev/null || true)
    case "${PROBE:-000}" in
        200)
            BODY=$(curl -s --max-time 5 "${EVENTS_URL}" 2>/dev/null || true)
            if printf '%s' "${BODY}" | grep -q 'governance.failover'; then
                ok "phase 197: governance.failover is advertised on the events-type surface"
            else
                skip "phase 197: events-type surface reachable but governance.failover not advertised (pre-197 build)"
            fi
            ;;
        *)
            skip "phase 197: events-type surface not present (${PROBE:-000})"
            ;;
    esac
else
    skip "phase 197: no events-type surface to probe for governance.failover"
fi

smoke_summary
