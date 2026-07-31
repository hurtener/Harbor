#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 204 — MCP App replay in session-history hydration (D-348), Console-only.
#
# The replay itself is Console-side: `reduceHistoryTurns` folds the durable
# `mcp.app_available` event onto the turn, `hydratePastTurns` sets it on the
# reopened message, and the renderer re-mounts — resolving the persisted tool
# context by its deterministic `toolCallId` BEFORE mounting, so an unresolvable
# context renders an honest placeholder instead of a half-mounted iframe. All
# of that is pinned by the Console vitest suites (history-mcp-app.spec.ts,
# mcp-app-tool-context.spec.ts, mcp-app-replay.spec.ts), which is where the
# behaviour belongs.
#
# This smoke therefore deliberately asserts LITTLE — only that the two
# SERVER-SIDE inputs the replay depends on are mounted and identity-mandatory,
# plus one conditional payload-shape check. Padding it with synthetic runs
# would prove nothing the vitest does not already prove:
#   1. POST /v1/state/history without a bearer → 401 (the windowed durable read
#      that delivers `mcp.app_available` rows).
#   2. POST /v1/control/mcp.apps.tool_context without a bearer → 401 (the
#      persisted-context read the re-mount performs).
#   3. WHEN the dev session's window carries an `mcp.app_available` event:
#      assert it carries the ServerID / ResourceURI / ToolCallID payload keys
#      the reducer folds. SKIP otherwise (CI has no App-declaring MCP server).
# Done-definition: OK >= 2, FAIL = 0; 404/405/501 → SKIP until the phase ships.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# The dev bearer is resolved through common.sh's `dev_bearer`, never by a raw
# ${HARBOR_DEV_TOKEN} read: the raw read is EMPTY outside preflight, so every
# live leg below degrades to a SKIP while the script still exits 0 — "a SKIP
# that should be an OK is a bug" (AGENTS.md §4.2 item 5, issue #624).
# dev_bearer prefers the exported value and falls back to the dev server log.
HARBOR_DEV_TOKEN="$(dev_bearer)"

STATE_URL="$(api_url /v1/state/history)"
CTX_URL="$(api_url /v1/control/mcp.apps.tool_context)"

TOKEN="${HARBOR_DEV_TOKEN:-dev-token-placeholder}"
ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")

if ! command -v curl >/dev/null 2>&1; then
  skip "phase 204: curl not available"
  smoke_summary
  exit 0
fi

# 1. The durable windowed read that delivers the `mcp.app_available` rows. A
#    no-bearer POST distinguishes a missing route (404) from an auth-rejected
#    (401); 401 means the route is mounted AND identity-mandatory.
set +e
PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
  -X POST -H 'Content-Type: application/json' -d '{}' "${STATE_URL}")
set -e
case "${PROBE:-000}" in
  404 | 405 | 501 | 000)
    skip "phase 204: /v1/state/history route not present (${PROBE:-000})"
    smoke_summary
    exit 0
    ;;
  401)
    ok "phase 204: state.history rejects an identity-less body (401 — the replay read path is mounted)"
    ;;
  *)
    fail "phase 204: state.history no-bearer probe expected 401/404, got ${PROBE}"
    smoke_summary
    exit 0
    ;;
esac

# 2. The persisted tool-context read the re-mount performs by the discovery's
#    deterministic tool_call_id. Identity-mandatory: a cross-identity or
#    unknown id is not_found, which is exactly what the Console turns into the
#    honest "no longer available" placeholder.
set +e
CTX_PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
  -X POST -H 'Content-Type: application/json' -d '{}' "${CTX_URL}")
set -e
case "${CTX_PROBE:-000}" in
  404 | 405 | 501 | 000)
    skip "phase 204: mcp.apps.tool_context route not present (${CTX_PROBE:-000})"
    ;;
  401)
    ok "phase 204: mcp.apps.tool_context rejects an identity-less body (401 — the persisted-context read is mounted)"
    ;;
  *)
    fail "phase 204: mcp.apps.tool_context no-bearer probe expected 401/404, got ${CTX_PROBE}"
    ;;
esac

# 3. Payload-shape check, conditional on the dev runtime having an App-declaring
#    MCP server attached (it does not in CI — then this SKIPs).
if [ -z "${HARBOR_DEV_TOKEN:-}" ] || ! command -v jq >/dev/null 2>&1; then
  skip "phase 204: dev bearer / jq unavailable — the App fold is pinned by the Console vitest"
  smoke_summary
  exit 0
fi

TMP="$(mktemp)"
trap 'rm -f "${TMP}"' EXIT
set +e
ST=$(curl -s -o "${TMP}" -w '%{http_code}' --max-time 10 \
  -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
  -H 'Content-Type: application/json' \
  -d '{"session_id":"dev","before":0,"limit":200}' "${STATE_URL}")
set -e

if [ "${ST}" != "200" ]; then
  skip "phase 204: state.history returned ${ST} (no retained dev history to inspect)"
  smoke_summary
  exit 0
fi

APP_EVENTS=$(jq -r '[.events[] | select(.type == "mcp.app_available")] | length' "${TMP}" 2>/dev/null || echo 0)
if [ "${APP_EVENTS}" -eq 0 ]; then
  skip "phase 204: dev window carries no mcp.app_available event (no App-declaring MCP server attached)"
  smoke_summary
  exit 0
fi

KEYED=$(jq -r '[.events[] | select(.type == "mcp.app_available")
  | select((.payload.ServerID // "") != "")
  | select((.payload.ResourceURI // "") != "")
  | select(.payload | has("ToolCallID"))] | length' "${TMP}" 2>/dev/null || echo 0)
if [ "${KEYED}" -gt 0 ]; then
  ok "phase 204: mcp.app_available read-back carries ServerID + ResourceURI + ToolCallID (${KEYED}) — the keys the replay folds"
else
  fail "phase 204: mcp.app_available events present (${APP_EVENTS}) but none carry ServerID + ResourceURI + ToolCallID"
fi

smoke_summary
