#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 108n — Console Memory page rebuilt to the carded, viewport-locked
# master-detail composition + the REAL `memory.strategy_trace` read and the
# admin-gated `memory.put` / `memory.delete` mutation pair (D-186). This smoke
# is live-server (it exercises the three NEW Protocol methods on the booted dev
# mux) AND carries the static Console-side guard.
#
# Live-server assertions (404/405/501 → SKIP per CLAUDE.md §4.2):
#   1. The three new routes (POST /v1/memory/{strategy_trace,put,delete}) are
#      mounted. On a pre-108n build they 404 → SKIP.
#   2. A POST with NO bearer is rejected 401 (Phase 61 auth fail-closed).
#   3. memory.strategy_trace with the dev bearer → 200 + a `trace.strategy`
#      string (a read method — no admin claim).
#   4. memory.put / memory.delete with the dev bearer route + gate correctly:
#      200 (dev token carries admin) OR 403 (identity_scope_required) — never
#      404/500. memory.delete with an empty key → 400 (fail-loud).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

TRACE_URL="$(api_url /v1/memory/strategy_trace)"
PUT_URL="$(api_url /v1/memory/put)"
DELETE_URL="$(api_url /v1/memory/delete)"

if command -v curl >/dev/null 2>&1; then
  # Surface probe — distinguishes a missing route (404) from auth-rejected (401).
  set +e
  PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${TRACE_URL}")
  set -e
  case "${PROBE:-000}" in
    404 | 405 | 501 | 000)
      skip "phase 108n: /v1/memory/strategy_trace route not present (${PROBE:-000})"
      ;;
    *)
      # 1. No-bearer rejection (401) for all three new routes.
      for pair in "strategy_trace ${TRACE_URL}" "put ${PUT_URL}" "delete ${DELETE_URL}"; do
        name="${pair%% *}"
        url="${pair##* }"
        set +e
        NOAUTH=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
          -X POST -H 'Content-Type: application/json' -d '{}' "${url}")
        set -e
        if [ "${NOAUTH}" = "401" ]; then
          ok "phase 108n: memory.${name} without bearer rejected (401)"
        else
          fail "phase 108n: memory.${name} without bearer expected 401, got ${NOAUTH}"
        fi
      done

      # Resolve the dev bearer from the preflight harness's captured log.
      DEV_TOKEN=""
      if [ -n "${HARBOR_DATA_DIR:-}" ] && [ -f "${HARBOR_DATA_DIR}/server.log" ]; then
        DEV_TOKEN="$(grep -m1 '^HARBOR_DEV_TOKEN=' "${HARBOR_DATA_DIR}/server.log" 2>/dev/null | sed 's/^HARBOR_DEV_TOKEN=//' || true)"
      fi

      if [ -n "${DEV_TOKEN}" ]; then
        TMP="$(mktemp)"
        trap 'rm -f "${TMP}"' EXIT

        # 3. strategy_trace happy path — 200 + a trace.strategy string.
        set +e
        ST=$(curl -s -o "${TMP}" -w '%{http_code}' --max-time 10 \
          -X POST -H "Authorization: Bearer ${DEV_TOKEN}" \
          -H 'Content-Type: application/json' -d '{}' "${TRACE_URL}")
        set -e
        if [ "${ST}" = "200" ]; then
          if command -v jq >/dev/null 2>&1; then
            STRAT=$(jq -r '.trace.strategy' "${TMP}" 2>/dev/null || echo "")
            if [ -n "${STRAT}" ] && [ "${STRAT}" != "null" ]; then
              ok "phase 108n: memory.strategy_trace returns trace.strategy (${STRAT})"
            else
              fail "phase 108n: memory.strategy_trace missing trace.strategy"
            fi
          else
            ok 'phase 108n: memory.strategy_trace 200 (jq absent for shape check)'
          fi
        else
          fail "phase 108n: memory.strategy_trace with bearer expected 200, got ${ST}"
        fi

        # 4. put + delete route + gate correctly (200 admin OR 403 scope).
        set +e
        PUTC=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
          -X POST -H "Authorization: Bearer ${DEV_TOKEN}" -H 'Content-Type: application/json' \
          -d '{"turn":{"user_text":"smoke-q","assistant_text":"smoke-a"}}' "${PUT_URL}")
        DELEMPTY=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
          -X POST -H "Authorization: Bearer ${DEV_TOKEN}" -H 'Content-Type: application/json' \
          -d '{"key":""}' "${DELETE_URL}")
        set -e
        case "${PUTC}" in
          200 | 403) ok "phase 108n: memory.put routes + admin-gates (${PUTC})" ;;
          *) fail "phase 108n: memory.put expected 200/403, got ${PUTC}" ;;
        esac
        # An admin caller with an empty key → 400; a non-admin → 403 (gate first).
        case "${DELEMPTY}" in
          400 | 403) ok "phase 108n: memory.delete rejects empty key / gates (${DELEMPTY})" ;;
          *) fail "phase 108n: memory.delete empty-key expected 400/403, got ${DELEMPTY}" ;;
        esac
      else
        skip 'phase 108n: dev bearer unavailable — strategy_trace/put/delete happy paths covered by Go tests'
      fi
      ;;
  esac
else
  skip 'phase 108n: curl not available'
fi

# ---------------------------------------------------------------------------
# Static Console-side guard — the carded master-detail rebuild + the king-file
# refactor + the live event feed + the mutation surface wiring.
# ---------------------------------------------------------------------------
PAGE="web/console/src/routes/(console)/memory/+page.svelte"
STATE="web/console/src/lib/memory/state.svelte.ts"
DERIVE="web/console/src/lib/memory/derive.ts"
TABLE="web/console/src/lib/components/memory/MemoryTable.svelte"
EVENTS="web/console/src/lib/components/memory/MemoryEventsCard.svelte"
TRACE="web/console/src/lib/components/memory/StrategyTraceCard.svelte"
COMPOSER="web/console/src/lib/components/memory/AddMemoryComposer.svelte"
CLIENT="web/console/src/lib/protocol/client.ts"

assert_file "${PAGE}" "phase 108n: Memory page route exists"
assert_grep_absent 'PageHeader' "${PAGE}" \
  "phase 108n: the page no longer renders a per-page PageHeader"
assert_grep_present 'panel card' "${PAGE}" \
  "phase 108n: the page adopts the carded .panel.card vocabulary"
assert_grep_present 'data-testid="memory-page"' "${PAGE}" \
  "phase 108n: the page keeps the memory-page root testid"

assert_file "${STATE}" "phase 108n: the MemoryPageState controller exists"
assert_grep_present 'export class MemoryPageState' "${STATE}" \
  "phase 108n: the module exports MemoryPageState"
assert_grep_present 'MemoryPageState' "${PAGE}" \
  "phase 108n: the page composes the MemoryPageState controller"
for verb in strategyTrace addTurn 'evict' evictSelected; do
  assert_grep_present "${verb}" "${STATE}" \
    "phase 108n: the controller wires ${verb}"
done
assert_grep_present 'EventsSubscription' "${STATE}" \
  "phase 108n: the controller opens a live memory.* event subscription"
assert_grep_present 'hasScope' "${STATE}" \
  "phase 108n: the controller derives the admin claim (hasScope)"

assert_file "${DERIVE}" "phase 108n: the derive.ts pure-projection module exists"
assert_grep_present 'export function decodeMemoryValue' "${DERIVE}" \
  "phase 108n: derive.ts exports decodeMemoryValue (the base64 wire-shape fix)"
assert_grep_present 'export function projectMemoryEvents' "${DERIVE}" \
  "phase 108n: derive.ts exports projectMemoryEvents (live feed projection)"

assert_file "${TABLE}" "phase 108n: the MemoryTable component exists"
assert_file "${EVENTS}" "phase 108n: the live MemoryEventsCard exists"
assert_file "${TRACE}" "phase 108n: the StrategyTraceCard exists"
assert_file "${COMPOSER}" "phase 108n: the AddMemoryComposer exists"
assert_grep_present 'data-testid="memory-events-feed"' "${EVENTS}" \
  "phase 108n: the live event feed replaces the deferred placeholder"
assert_grep_present 'data-testid="memory-strategy-trace"' "${TRACE}" \
  "phase 108n: the strategy-trace card renders"
assert_grep_present 'data-testid="memory-evict-selected"' "${PAGE}" \
  "phase 108n: the bulk bar wires the real admin Evict selected"

# The client namespace exposes the three new methods.
assert_grep_present 'strategy_trace' "${CLIENT}" \
  "phase 108n: the memory namespace exposes strategy_trace"
assert_grep_present '/v1/memory/put' "${CLIENT}" \
  "phase 108n: the memory namespace exposes the put route"
assert_grep_present '/v1/memory/delete' "${CLIENT}" \
  "phase 108n: the memory namespace exposes the delete route"

# Save-view N7 contract + no hand-rolled fetch.
assert_grep_present 'data-testid="memory-save-view"' "${PAGE}" \
  "phase 108n: the page keeps the memory-save-view button (disconnected-state N7)"
if grep -qE '^[[:space:]]+Save view[[:space:]]*$' "${PAGE}"; then
  ok "phase 108n: the page renders the 'Save view' button label on its own line (phase-83s N7)"
else
  fail "phase 108n: the page must render a 'Save view' button on its own line (phase-83s N7)"
fi
assert_grep_absent 'fetch\(' "${PAGE}" \
  "phase 108n: the page route has no hand-rolled fetch (HarborClient only)"
assert_grep_absent 'fetch\(' "${STATE}" \
  "phase 108n: the controller has no hand-rolled fetch (HarborClient only)"

smoke_summary
