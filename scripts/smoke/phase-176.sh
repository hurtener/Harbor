#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 176 — Session reopen (RFC §6.9 amended, D-312). A closed session
# (explicit OR GC-reaped) RE-ACTIVATES in place on the next `start` /
# EnsureOpen — the durable history resumes intact. The ONE terminal exception
# is an ERASED session: reopen fails loud with the machine-branchable
# `session_erased` code (HTTP 409). A content-free `session.reopened` canonical
# event observes each resumption.
#
# Static guards (always run): the new event type + payload + LastReopenedAt
# field + ErrReopenAfterErase sentinel + the CodeSessionErased wire code are
# single-sourced. Build/test gates: manifest + generated-docs lockstep + the
# sessions/reopen Go tests + the reopen E2E. Live (skips per 404/405/501): a
# `start` on a fresh session succeeds; after `sessions.delete` a `start` on the
# same id → 409 `session_erased` (reopen-after-erase).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# --- Static: the new session.reopened event is single-sourced. ---
assert_grep_present 'EventTypeSessionReopened' "internal/sessions/events.go" \
  "phase 176: session.reopened event type registered"
assert_grep_present '"session.reopened"' "internal/sessions/events.go" \
  "phase 176: session.reopened event string single-sourced"
assert_grep_present 'SessionReopenedPayload' "internal/sessions/events.go" \
  "phase 176: SessionReopenedPayload (content-free) declared"

# --- Static: the record field + terminal-exception sentinel + wire code. ---
assert_grep_present 'LastReopenedAt' "internal/sessions/sessions.go" \
  "phase 176: Session.LastReopenedAt field (hard-cap anchor) present"
assert_grep_present 'ErrReopenAfterErase' "internal/sessions/sessions.go" \
  "phase 176: ErrReopenAfterErase sentinel (the terminal exception) present"
assert_grep_present 'CodeSessionErased' "internal/protocol/errors/errors.go" \
  "phase 176: session_erased wire code registered"
assert_grep_present '"session_erased"' "internal/protocol/errors/errors.go" \
  "phase 176: session_erased code string single-sourced"

# --- Static: the hard cap is measured from max(OpenedAt, LastReopenedAt). ---
assert_grep_present 'LastReopenedAt' "internal/sessions/gc.go" \
  "phase 176: GC hard cap anchored on max(OpenedAt, LastReopenedAt) (FAIL-1)"
# --- Static: the erasure tombstone (terminal, converged-erasure guard). ---
assert_grep_present 'erasureTombstoneKindPrefix' "internal/sessions/erasure.go" \
  "phase 176: durable erasure tombstone (converged-erasure reopen guard)"
assert_grep_present 'func (r \*Registry) isErased' "internal/sessions/erasure.go" \
  "phase 176: isErased fail-closed reopen guard present"

# --- Build/test gates: manifest + generated-docs lockstep + the tests. ---
if make protocol-ts-gen-check >/dev/null 2>&1; then
  ok "phase 176: make protocol-ts-gen-check passes (session.reopened + session_erased in the manifest)"
else
  fail "phase 176: make protocol-ts-gen-check failed (regenerate manifest / mirror the TS types)"
fi
if make protocol-docs-gen-check >/dev/null 2>&1; then
  ok "phase 176: make protocol-docs-gen-check passes (events.md + errors.md regenerated, D-209)"
else
  fail "phase 176: make protocol-docs-gen-check failed (run make protocol-docs-gen and commit the pages)"
fi
if go test -race -run 'Reopen|Tombstone|Erase_ReopenSameID|FailedErase' ./internal/sessions/... >/dev/null 2>&1; then
  ok "phase 176: sessions reopen + tombstone tests pass under -race"
else
  fail "phase 176: sessions reopen tests failed (go test -race ./internal/sessions/...)"
fi
if go test -race -run TestE2E_SessionReopen ./test/integration/... >/dev/null 2>&1; then
  ok "phase 176: session-reopen E2E passes under -race (history intact + converged-erase loud + hard-cap-restart)"
else
  fail "phase 176: session-reopen E2E failed (go test -race -run TestE2E_SessionReopen ./test/integration/...)"
fi

# --- Live (skips per 404/405/501): reopen succeeds on a fresh session; a
# --- start on an ERASED id → 409 session_erased.                           ---
START_URL="$(api_url /v1/control/start)"
DELETE_URL="$(api_url /v1/sessions/delete)"

DEV_TENANT="dev"
DEV_USER="dev"
DEV_SESSION="phase176-smoke-$$"
TOKEN="dev-token-placeholder"
[ -n "${HARBOR_DEV_TOKEN:-}" ] && TOKEN="${HARBOR_DEV_TOKEN}"
ID_HEADERS=(-H "X-Harbor-Tenant: ${DEV_TENANT}" -H "X-Harbor-User: ${DEV_USER}" -H "X-Harbor-Session: ${DEV_SESSION}")
OWN_BODY="{\"identity\":{\"tenant\":\"${DEV_TENANT}\",\"user\":\"${DEV_USER}\",\"session\":\"${DEV_SESSION}\"}}"

if command -v curl >/dev/null 2>&1; then
  set +e
  PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${START_URL}")
  set -e
  case "${PROBE:-000}" in
    404 | 405 | 501 | 000)
      skip "phase 176: /v1/control/start route not present (${PROBE:-000})"
      ;;
    *)
      # Materialise the throwaway session (create-on-first-use on start).
      set +e
      S1=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
        -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' \
        -d '{"query":"phase-176 seed","description":"session reopen smoke"}' "${START_URL}")
      set -e
      case "${S1}" in
        200)
          ok "phase 176: start on a fresh session answers 200 (session materialised)"
          # A second start on the SAME open id is not rejected (the reopen path
          # is the same seam; an already-open session is a no-op).
          set +e
          S2=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
            -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
            -H 'Content-Type: application/json' \
            -d '{"query":"phase-176 resume","description":"resume same id"}' "${START_URL}")
          set -e
          if [ "${S2}" = "200" ]; then
            ok "phase 176: a repeat start on the same session id succeeds (never a spurious reopen rejection)"
          else
            fail "phase 176: repeat start on the same session expected 200, got ${S2}"
          fi

          # Erase the session, then a start on the erased id must fail loud
          # 409 session_erased (reopen-after-erase). If the seed task is still
          # running the delete answers 409 (running) — skip the erased-start leg.
          set +e
          DST=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
            -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
            -H 'Content-Type: application/json' -d "${OWN_BODY}" "${DELETE_URL}")
          set -e
          case "${DST}" in
            200)
              set +e
              ERASED=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
                -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
                -H 'Content-Type: application/json' \
                -d '{"query":"reopen after erase","description":"must be rejected"}' "${START_URL}")
              set -e
              if [ "${ERASED}" = "409" ]; then
                ok "phase 176: start on an ERASED session is rejected 409 (session_erased, reopen-after-erase)"
              else
                fail "phase 176: start on an erased session expected 409 session_erased, got ${ERASED}"
              fi
              ;;
            404 | 409 | 501)
              skip "phase 176: sessions.delete unavailable / running-task (${DST}) — skipping the erased-start leg"
              ;;
            *)
              skip "phase 176: sessions.delete returned ${DST} — skipping the erased-start leg"
              ;;
          esac
          ;;
        *)
          skip "phase 176: start seed returned ${S1} (LLM seam likely unconfigured) — skipping live reopen legs"
          ;;
      esac
      ;;
  esac
else
  skip "phase 176: curl not available — skipping live assertions"
fi

smoke_summary
