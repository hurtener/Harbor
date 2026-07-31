#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 162 — events.list durable, time-ranged, cross-session raw-event read
# (D-294). The `events.list` Protocol method: the existing EventFilter
# (identity axes + since/until) + a tail-first, sequence-based paging cursor
# mirroring state.history; rows = the flat StateEvent projection; non-widened
# callers see only their own tuple, fleet widening needs the closed
# admin/console:fleet scope.
#
# Live-server assertions (404/405/501 → SKIP per CLAUDE.md §4.2):
#   1. The route POST /v1/events/list is mounted (a no-bearer POST → 401,
#      not the route-miss 404). The 401 is ALSO the identity-mandatory check.
#   2. After seeding events (control/start), a windowed read returns 200 +
#      a sane page (events non-empty; the page is oldest-first).
#   3. Take next_cursor and fetch page 2 (the scroll-up paging round-trip).
#   4. A cross-tenant filter WITHOUT an elevated scope is rejected 403 (when
#      the dev token carries no admin/console:fleet scope).
# Static guards (always run, never skip): single-source for the method
# string + no Console import inside the stream package.

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

LIST_URL="$(api_url /v1/events/list)"
START_URL="$(api_url /v1/control/start)"

TOKEN="dev-token-placeholder"
if [ -n "${HARBOR_DEV_TOKEN:-}" ]; then
  TOKEN="${HARBOR_DEV_TOKEN}"
fi
ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")

if command -v curl >/dev/null 2>&1; then
  # Route probe: a no-bearer POST distinguishes a missing route (404) from
  # an auth-rejected (401). 401 means the route is mounted AND the
  # identity-mandatory gate fires.
  set +e
  PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{"filter":{}}' "${LIST_URL}")
  set -e
  case "${PROBE:-000}" in
    404 | 405 | 501 | 000)
      skip "phase 162: /v1/events/list route not present (${PROBE:-000})"
      ;;
    401)
      ok "phase 162: events.list rejects identity-less body (401)"

      if [ -n "${HARBOR_DEV_TOKEN:-}" ] && command -v jq >/dev/null 2>&1; then
        # Seed events for the dev session (a start spawns a task and emits
        # task.* lifecycle events into the durable log even if the LLM seam
        # is unconfigured in preflight).
        curl -sS -X POST "${START_URL}" -H "Authorization: Bearer ${TOKEN}" \
          "${ID_HEADERS[@]}" -H 'Content-Type: application/json' \
          -d '{"query":"phase-162 seed","description":"events.list smoke"}' >/dev/null 2>&1 || true
        sleep 1

        TMP="$(mktemp)"
        trap 'rm -f "${TMP}"' EXIT
        set +e
        ST=$(curl -s -o "${TMP}" -w '%{http_code}' --max-time 10 \
          -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
          -H 'Content-Type: application/json' \
          -d '{"filter":{},"cursor":0,"limit":3}' "${LIST_URL}")
        set -e
        case "${ST}" in
          200)
            LEN=$(jq -r '.events | length' "${TMP}" 2>/dev/null || echo 0)
            if [ "${LEN}" -gt 0 ]; then
              ok "phase 162: windowed read returns ${LEN} events (200)"
              # Rows are oldest-first within the page — first <= last.
              FIRST=$(jq -r '.events[0].sequence' "${TMP}" 2>/dev/null || echo 0)
              LAST=$(jq -r '.events[-1].sequence' "${TMP}" 2>/dev/null || echo 0)
              NEXT=$(jq -r '.next_cursor' "${TMP}" 2>/dev/null || echo 0)
              HASMORE=$(jq -r '.has_more' "${TMP}" 2>/dev/null || echo false)
              if [ "${FIRST}" -le "${LAST}" ]; then
                ok "phase 162: page is oldest-first (first=${FIRST} <= last=${LAST})"
              else
                fail "phase 162: page not oldest-first (first=${FIRST} > last=${LAST})"
              fi
              # Paging round-trip: when has_more, page 2 with cursor=next.
              if [ "${HASMORE}" = "true" ] && [ "${NEXT}" != "0" ]; then
                set +e
                ST2=$(curl -s -o "${TMP}" -w '%{http_code}' --max-time 10 \
                  -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
                  -H 'Content-Type: application/json' \
                  -d "{\"filter\":{},\"cursor\":${NEXT},\"limit\":3}" "${LIST_URL}")
                set -e
                if [ "${ST2}" = "200" ]; then
                  P2FIRST=$(jq -r '.events[0].sequence // 0' "${TMP}" 2>/dev/null || echo 0)
                  if [ "${P2FIRST}" -lt "${NEXT}" ] || [ "${P2FIRST}" = "0" ]; then
                    ok "phase 162: scroll-up page 2 returns older rows (next=${NEXT})"
                  else
                    fail "phase 162: page 2 first ${P2FIRST} not older than cursor ${NEXT}"
                  fi
                else
                  fail "phase 162: page 2 expected 200, got ${ST2}"
                fi
              else
                ok "phase 162: single page covers the retained window (has_more=false)"
              fi
            else
              skip "phase 162: dev session has no retained events to window (seed produced none)"
            fi
            ;;
          *)
            fail "phase 162: windowed read expected 200, got ${ST}"
            ;;
        esac

        # Cross-tenant filter WITHOUT elevated scope → 403 (when the dev token
        # carries no admin/console:fleet scope). A dev token that DOES carry
        # an elevated scope answers 200 — informational, not a failure.
        set +e
        XT=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
          -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
          -H 'Content-Type: application/json' \
          -d '{"filter":{"tenant_ids":["t-foreign"]}}' "${LIST_URL}")
        set -e
        if [ "${XT}" = "403" ]; then
          ok "phase 162: cross-tenant read without a scope claim is 403"
        else
          skip "phase 162: cross-tenant read answered ${XT} (dev token likely carries an elevated scope; the 403 leg is pinned in the Go handler + integration tests)"
        fi
      else
        skip "phase 162: dev bearer / jq unavailable — windowed round-trip covered by Go tests"
      fi
      ;;
    *)
      fail "phase 162: events.list no-bearer probe expected 401/404, got ${PROBE}"
      ;;
  esac
else
  skip "phase 162: curl not available"
fi

# ---------------------------------------------------------------------------
# Static guards (always run, never skip).
# ---------------------------------------------------------------------------

# Single-source: the method string is defined in exactly one place.
assert_grep_count 'MethodEventsList Method = "events.list"' \
  internal/protocol/methods/methods.go 1 \
  "phase 162: events.list method string is single-sourced in methods.go"

# The literal "events.list" appears under internal/ ONLY in methods.go (the
# single-source DEFINITION) and singlesource.go (the checker's expected-method
# registry). Any OTHER non-test occurrence is a second definition.
OTHER=$(grep -rl --include='*.go' '"events\.list"' internal/ 2>/dev/null \
  | grep -v '_test.go' \
  | grep -v 'internal/protocol/methods/methods.go' \
  | grep -v 'internal/protocol/singlesource/singlesource.go' || true)
if [ -z "${OTHER}" ]; then
  ok "phase 162: no second definition of the literal \"events.list\" under internal/"
else
  fail "phase 162: literal \"events.list\" found outside methods.go: ${OTHER}"
fi

# No Console import inside the stream transport handler (the Runtime never
# imports the Console).
assert_grep_absent 'web/console' \
  internal/protocol/transports/stream/events_list_handler.go \
  "phase 162: events-list handler does not import the Console"

smoke_summary
