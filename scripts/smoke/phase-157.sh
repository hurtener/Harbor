#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 157 — Session title: record field + `sessions.set_title` +
# Console rename (D-288). The session record gains `Title` +
# `TitleSource` (additive, zero migration). `sessions.set_title` sets or
# clears a title for a session belonging to the caller's verified
# `(tenant, user)` — NOT own-session-only: the target `session_id` may
# name a sibling session. The wire verb ALWAYS writes `manual`
# provenance; an over-bound (>200 runes) or malformed title fails loud
# with a 400, never a silent clamp. A successful set/clear publishes the
# content-free `session.title_changed` event (identity scope + session
# id + source only — never the title string).
#
# Static guards (always run): single-source registration of the method
# (Go + TS mirror) + the new wire types. Build/test gates: the manifest +
# generated-docs lockstep + the Go tests. Live (skips per 404/405/501):
# set -> inspect round-trip, empty-title clear, and an oversize title
# 400.

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

# --- Static: single-source registration of the method. ---
assert_grep_present 'MethodSessionsSetTitle' "internal/protocol/methods/methods.go" \
  "phase 157: sessions.set_title method registered"
assert_grep_present '"sessions.set_title"' "internal/protocol/methods/methods.go" \
  "phase 157: sessions.set_title method string single-sourced"

# --- Static: the new wire types exist (Go) and are mirrored (TS). ---
assert_grep_present 'SessionsSetTitleRequest' "internal/protocol/types/sessions.go" \
  "phase 157: SessionsSetTitleRequest Go wire type present"
assert_grep_present 'SessionsSetTitleResponse' "internal/protocol/types/sessions.go" \
  "phase 157: SessionsSetTitleResponse Go wire type present"
assert_grep_present 'SessionsSetTitleResponse' "web/console/src/lib/sessions/types.ts" \
  "phase 157: TS SessionsSetTitleResponse interface mirrored"
assert_grep_present 'title_source' "internal/protocol/types/sessions.go" \
  "phase 157: SessionRow.title_source field present"

# --- Static: the session record's additive fields + the SetTitle seam. ---
assert_grep_present 'TitleSource' "internal/sessions/sessions.go" \
  "phase 157: sessions.Session gains Title/TitleSource"
assert_grep_present 'ErrInvalidTitle' "internal/sessions/sessions.go" \
  "phase 157: sessions.ErrInvalidTitle sentinel present"
assert_grep_present 'EventTypeSessionTitleChanged' "internal/sessions/events.go" \
  "phase 157: session.title_changed event type registered"

# --- Build/test gates: manifest lockstep + generated-docs gate + the tests. ---
if make protocol-ts-gen-check >/dev/null 2>&1; then
  ok "phase 157: make protocol-ts-gen-check passes (manifest + TS types in lockstep)"
else
  fail "phase 157: make protocol-ts-gen-check failed (regenerate manifest / mirror the TS types)"
fi
if make protocol-docs-gen-check >/dev/null 2>&1; then
  ok "phase 157: make protocol-docs-gen-check passes (methods/events/types regenerated, D-209)"
else
  fail "phase 157: make protocol-docs-gen-check failed (run make protocol-docs-gen and commit the pages)"
fi
if go test -race ./internal/sessions/... ./internal/sessions/protocol/... >/dev/null 2>&1; then
  ok "phase 157: sessions + sessions/protocol tests pass under -race"
else
  fail "phase 157: sessions tests failed (go test -race ./internal/sessions/... ./internal/sessions/protocol/...)"
fi

# --- Live (skips per 404/405/501): set -> inspect round-trip, clear, ---
# --- oversize-400, against the dev server.                             ---
SET_TITLE_URL="$(api_url /v1/sessions/set_title)"
INSPECT_URL="$(api_url /v1/sessions/inspect)"
START_URL="$(api_url /v1/control/start)"

# The preflight dev server is trust-based (no JWT validator): identity
# travels via the X-Harbor-* headers (resolveIdentity reads them when no
# bearer verifies). A fresh, throwaway session id keeps the run isolated.
DEV_TENANT="dev"
DEV_USER="dev"
DEV_SESSION="phase157-smoke-$$"
TOKEN="dev-token-placeholder"
[ -n "${HARBOR_DEV_TOKEN:-}" ] && TOKEN="${HARBOR_DEV_TOKEN}"
ID_HEADERS=(-H "X-Harbor-Tenant: ${DEV_TENANT}" -H "X-Harbor-User: ${DEV_USER}" -H "X-Harbor-Session: ${DEV_SESSION}")

if command -v curl >/dev/null 2>&1; then
  # Route probe: an identity-less POST distinguishes a missing route (404)
  # from the identity-mandatory 401 (route mounted AND the gate fires).
  set +e
  PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${SET_TITLE_URL}")
  set -e
  case "${PROBE:-000}" in
    404 | 405 | 501 | 000)
      skip "phase 157: /v1/sessions/set_title route not present (${PROBE:-000})"
      ;;
    401)
      ok "phase 157: sessions.set_title rejects an identity-less body (401)"
      # Materialise the throwaway session (create-on-first-use on start).
      curl -sS -X POST "${START_URL}" -H "Authorization: Bearer ${TOKEN}" \
        "${ID_HEADERS[@]}" -H 'Content-Type: application/json' \
        -d '{"query":"phase-157 seed","description":"sessions.set_title smoke"}' >/dev/null 2>&1 || true

      # Set a title on the dev session.
      SET_BODY="{\"session_id\":\"${DEV_SESSION}\",\"title\":\"Phase 157 smoke title\"}"
      set +e
      SST=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
        -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' -d "${SET_BODY}" "${SET_TITLE_URL}")
      set -e
      case "${SST}" in
        200)
          ok "phase 157: live sessions.set_title answers 200"
          INSPECT_BODY="{\"session_id\":\"${DEV_SESSION}\"}"
          IR=$(curl -sS --max-time 5 -X POST -H "Authorization: Bearer ${TOKEN}" \
            "${ID_HEADERS[@]}" -H 'Content-Type: application/json' \
            -d "${INSPECT_BODY}" "${INSPECT_URL}")
          if echo "${IR}" | grep -q '"title":"Phase 157 smoke title"' \
            && echo "${IR}" | grep -q '"title_source":"manual"'; then
            ok "phase 157: sessions.inspect reflects the title + title_source=manual"
          else
            fail "phase 157: sessions.inspect did not reflect the set title: ${IR}"
          fi

          # Clear round-trip: empty title clears both fields.
          CLEAR_BODY="{\"session_id\":\"${DEV_SESSION}\",\"title\":\"\"}"
          set +e
          CST=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
            -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
            -H 'Content-Type: application/json' -d "${CLEAR_BODY}" "${SET_TITLE_URL}")
          set -e
          if [ "${CST}" = "200" ]; then
            ok "phase 157: sessions.set_title clear (empty title) answers 200"
            IR2=$(curl -sS --max-time 5 -X POST -H "Authorization: Bearer ${TOKEN}" \
              "${ID_HEADERS[@]}" -H 'Content-Type: application/json' \
              -d "${INSPECT_BODY}" "${INSPECT_URL}")
            if echo "${IR2}" | grep -q '"title":""' || ! echo "${IR2}" | grep -q '"title":"Phase 157 smoke title"'; then
              ok "phase 157: post-clear sessions.inspect no longer shows the title"
            else
              fail "phase 157: post-clear sessions.inspect still shows the old title: ${IR2}"
            fi
          else
            fail "phase 157: sessions.set_title clear expected 200, got ${CST}"
          fi

          # Oversize title (> 200 runes) -> 400, never a silent clamp.
          OVERSIZE_TITLE=$(printf 'x%.0s' $(seq 1 201))
          OVERSIZE_BODY="{\"session_id\":\"${DEV_SESSION}\",\"title\":\"${OVERSIZE_TITLE}\"}"
          set +e
          OST=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
            -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
            -H 'Content-Type: application/json' -d "${OVERSIZE_BODY}" "${SET_TITLE_URL}")
          set -e
          if [ "${OST}" = "400" ]; then
            ok "phase 157: oversize (>200-rune) sessions.set_title rejected 400"
          else
            fail "phase 157: oversize sessions.set_title expected 400, got ${OST}"
          fi
          ;;
        404 | 501)
          skip "phase 157: sessions.set_title title-setter not wired on this dev runtime (${SST})"
          ;;
        *)
          fail "phase 157: own-session sessions.set_title expected 200, got ${SST}"
          ;;
      esac
      ;;
    *)
      skip "phase 157: sessions.set_title route probe returned ${PROBE} (skipping live)"
      ;;
  esac
else
  skip "phase 157: curl not available — skipping live assertions"
fi

smoke_summary
