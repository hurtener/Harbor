#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 125 — Session state-history windowed event-replay surface (D-254).
# The `state.history` Protocol method: a by-id, identity-scoped, read-only
# TAIL-FIRST windowed read of a session's durable event stream, heavy
# content carried by a routable artifact ref.
#
# Live-server assertions (404/405/501 → SKIP per CLAUDE.md §4.2):
#   1. The route POST /v1/state/history is mounted (a no-bearer POST → 401,
#      not the route-miss 404). The 401 is ALSO the identity-mandatory check.
#   2. After seeding events (control/start), a tail-first windowed read
#      returns 200 + a sane window shape (events non-empty; the last event's
#      sequence equals tail_sequence; head <= next_cursor <= tail).
#   3. A returned artifact ref id (the seeded heavy turn, when present)
#      ROUTES to artifacts.get_ref — 200 (presigned) OR 501 (the default
#      inmem store's typed presign_unsupported — proves the id reached the
#      resolver well-formed).
#   4. A cross-tenant body is rejected 404 EXACTLY (a 403 would green-light
#      an existence leak; D-219).
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

STATE_URL="$(api_url /v1/state/history)"
GETREF_URL="$(api_url /v1/control/artifacts.get_ref)"
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
    -X POST -H 'Content-Type: application/json' -d '{}' "${STATE_URL}")
  set -e
  case "${PROBE:-000}" in
    404 | 405 | 501 | 000)
      skip "phase 125: /v1/state/history route not present (${PROBE:-000})"
      ;;
    401)
      ok "phase 125: state.history rejects identity-less body (401)"

      if [ -n "${HARBOR_DEV_TOKEN:-}" ] && command -v jq >/dev/null 2>&1; then
        # Seed events for the dev session (a start spawns a task and emits
        # task.* lifecycle events into the durable log even if the LLM seam
        # is unconfigured in preflight).
        curl -sS -X POST "${START_URL}" -H "Authorization: Bearer ${TOKEN}" \
          "${ID_HEADERS[@]}" -H 'Content-Type: application/json' \
          -d '{"query":"phase-125 seed","description":"state.history smoke"}' >/dev/null 2>&1 || true
        sleep 1

        TMP="$(mktemp)"
        trap 'rm -f "${TMP}"' EXIT
        set +e
        ST=$(curl -s -o "${TMP}" -w '%{http_code}' --max-time 10 \
          -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
          -H 'Content-Type: application/json' \
          -d '{"session_id":"dev","before":0,"limit":50}' "${STATE_URL}")
        set -e
        case "${ST}" in
          200)
            LEN=$(jq -r '.events | length' "${TMP}" 2>/dev/null || echo 0)
            if [ "${LEN}" -gt 0 ]; then
              ok "phase 125: tail-first window returns ${LEN} events (200)"
              LAST=$(jq -r '.events[-1].sequence' "${TMP}" 2>/dev/null || echo "")
              TAIL=$(jq -r '.tail_sequence' "${TMP}" 2>/dev/null || echo "")
              HEAD=$(jq -r '.head_sequence' "${TMP}" 2>/dev/null || echo "")
              NEXT=$(jq -r '.next_cursor' "${TMP}" 2>/dev/null || echo "0")
              if [ "${LAST}" = "${TAIL}" ]; then
                ok "phase 125: last event sequence (${LAST}) equals tail_sequence (tail-first)"
              else
                fail "phase 125: last event ${LAST} != tail_sequence ${TAIL}"
              fi
              # head <= next_cursor <= tail (next_cursor 0 means head reached).
              if [ "${NEXT}" = "0" ] || { [ "${HEAD}" -le "${NEXT}" ] && [ "${NEXT}" -le "${TAIL}" ]; }; then
                ok "phase 125: scroll-up cursor sane (head=${HEAD} next=${NEXT} tail=${TAIL})"
              else
                fail "phase 125: cursor out of range (head=${HEAD} next=${NEXT} tail=${TAIL})"
              fi
              # Ref routes — pluck any artifacts[0].id from the page.
              REF_ID=$(jq -r 'first(.events[] | select(.artifacts != null and (.artifacts | length) > 0) | .artifacts[0].id) // ""' "${TMP}" 2>/dev/null || echo "")
              assert_json_path_resolves "${REF_ID}" "${GETREF_URL}" "${TOKEN}" "dev" "dev" "dev" \
                "phase 125: heavy-turn artifact ref"
            else
              skip "phase 125: dev session has no retained events to window (seed produced none)"
            fi
            ;;
          404)
            skip "phase 125: dev session has no event history yet (404 ErrNoHistory)"
            ;;
          *)
            fail "phase 125: tail-first window expected 200/404, got ${ST}"
            ;;
        esac

        # Cross-tenant gate — pinned to 404 EXACTLY (never 403; no existence leak).
        set +e
        XT=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
          -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
          -H 'Content-Type: application/json' \
          -d '{"identity":{"tenant":"t-foreign","user":"dev","session":"dev"},"session_id":"dev"}' "${STATE_URL}")
        set -e
        if [ "${XT}" = "404" ]; then
          ok "phase 125: cross-tenant read is 404 exactly (no existence leak)"
        else
          fail "phase 125: cross-tenant read expected 404, got ${XT} (a 403 would leak existence)"
        fi
      else
        skip "phase 125: dev bearer / jq unavailable — windowed round-trip covered by Go tests"
      fi
      ;;
    *)
      fail "phase 125: state.history no-bearer probe expected 401/404, got ${PROBE}"
      ;;
  esac
else
  skip "phase 125: curl not available"
fi

# ---------------------------------------------------------------------------
# Static guards (always run, never skip).
# ---------------------------------------------------------------------------

# Single-source: the method string is defined in exactly one place.
assert_grep_count 'MethodStateHistory Method = "state.history"' \
  internal/protocol/methods/methods.go 1 \
  "phase 125: state.history method string is single-sourced in methods.go"

# The literal "state.history" appears under internal/ ONLY in methods.go
# (the single-source DEFINITION — handlers/types reference the constant)
# and singlesource.go (the single-source CHECKER's own expected-method
# registry, which by design lists every canonical method literal so it can
# cross-check methods.go). Both are sanctioned; any OTHER occurrence is a
# second definition. (Test files legitimately pin the wire string too.)
OTHER=$(grep -rl --include='*.go' '"state\.history"' internal/ 2>/dev/null \
  | grep -v '_test.go' \
  | grep -v 'internal/protocol/methods/methods.go' \
  | grep -v 'internal/protocol/singlesource/singlesource.go' || true)
if [ -z "${OTHER}" ]; then
  ok "phase 125: no second definition of the literal \"state.history\" under internal/"
else
  fail "phase 125: literal \"state.history\" found outside methods.go: ${OTHER}"
fi

# No Console import inside the stream transport handler (the Runtime never
# imports the Console).
assert_grep_absent 'web/console' \
  internal/protocol/transports/stream/state_history_handler.go \
  "phase 125: state-history handler does not import the Console"

smoke_summary
