#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 130 — Session erasure Protocol method (data-lifecycle deletion, D-262).
# The `sessions.delete` method: an identity-scoped, own-session-only erasure
# that deletes a session and CASCADES deletion of its scoped State + Memory +
# Artifacts; refuses fail-loud on a RUNNING task with a distinct
# `session_running` (409) mirroring the GC never-reap-running invariant; emits
# a redacted, content-free `session.erased` audit event under the actor's
# observability scope; advertises a new `CapSessionLifecycle` capability.
#
# Static guards (always run): single-source registration of the method / code /
# capability + the two wire types (Go) and their hand-mirror (TS). Build/test
# gates: the manifest + generated-docs lockstep + the Go tests. Live (skips per
# 404/405/501): a delete→inspect-404 round-trip + the foreign-target 401 + the
# running-task 409.

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

# --- Static: single-source registration of the method / code / capability. ---
assert_grep_present 'MethodSessionsDelete' "internal/protocol/methods/methods.go" \
  "phase 130: sessions.delete method registered"
assert_grep_present '"sessions.delete"' "internal/protocol/methods/methods.go" \
  "phase 130: sessions.delete method string single-sourced"
assert_grep_present 'CodeSessionRunning' "internal/protocol/errors/errors.go" \
  "phase 130: session_running error code registered"
assert_grep_present '"session_running"' "internal/protocol/errors/errors.go" \
  "phase 130: session_running code string single-sourced"
assert_grep_present 'CapSessionLifecycle' "internal/protocol/types/version.go" \
  "phase 130: session_lifecycle capability registered"

# --- Static: the two new wire types exist (Go) and are mirrored (TS). ---
assert_grep_present 'SessionsDeleteRequest' "internal/protocol/types/sessions.go" \
  "phase 130: SessionsDeleteRequest Go wire type present"
assert_grep_present 'SessionsDeleteResponse' "internal/protocol/types/sessions.go" \
  "phase 130: SessionsDeleteResponse Go wire type present"
assert_grep_present 'SessionsDeleteResponse' "web/console/src/lib/sessions/types.ts" \
  "phase 130: TS SessionsDeleteResponse interface mirrored"

# --- Static: StateStore cascade primitive on the interface. ---
assert_grep_present 'DeleteScope' "internal/state/state.go" \
  "phase 130: StateStore.DeleteScope cascade primitive on the interface"

# --- Build/test gates: manifest lockstep + generated-docs gate + the tests. ---
if make protocol-ts-gen-check >/dev/null 2>&1; then
  ok "phase 130: make protocol-ts-gen-check passes (manifest + TS types in lockstep)"
else
  fail "phase 130: make protocol-ts-gen-check failed (regenerate manifest / mirror the TS types)"
fi
if make protocol-docs-gen-check >/dev/null 2>&1; then
  ok "phase 130: make protocol-docs-gen-check passes (methods/errors/types regenerated, D-209)"
else
  fail "phase 130: make protocol-docs-gen-check failed (run make protocol-docs-gen and commit the pages)"
fi
if go test -race ./internal/sessions/... ./internal/state/... >/dev/null 2>&1; then
  ok "phase 130: sessions + state cascade tests pass under -race"
else
  fail "phase 130: sessions/state tests failed (go test -race ./internal/sessions/... ./internal/state/...)"
fi
if go test -race -run TestE2E_Phase130 ./test/integration/... >/dev/null 2>&1; then
  ok "phase 130: session-erasure E2E passes under -race (cascade + scope matrix + running-task refusal)"
else
  fail "phase 130: session-erasure E2E failed (go test -race -run TestE2E_Phase130 ./test/integration/...)"
fi

# --- Live (skips per 404/405/501): a delete→inspect-404 round-trip + the ---
# --- foreign-target 401 + the running-task 409, against the dev server.   ---
DELETE_URL="$(api_url /v1/sessions/delete)"
INSPECT_URL="$(api_url /v1/sessions/inspect)"
START_URL="$(api_url /v1/control/start)"

# The preflight dev server is trust-based (no JWT validator): identity travels
# via the X-Harbor-* headers (resolveIdentity reads them when no bearer
# verifies), and the request-body identity is the dev triple for the
# defence-in-depth check. A fresh, throwaway session id keeps the run isolated.
DEV_TENANT="dev"
DEV_USER="dev"
DEV_SESSION="phase130-smoke-$$"
TOKEN="dev-token-placeholder"
[ -n "${HARBOR_DEV_TOKEN:-}" ] && TOKEN="${HARBOR_DEV_TOKEN}"
ID_HEADERS=(-H "X-Harbor-Tenant: ${DEV_TENANT}" -H "X-Harbor-User: ${DEV_USER}" -H "X-Harbor-Session: ${DEV_SESSION}")
OWN_BODY="{\"identity\":{\"tenant\":\"${DEV_TENANT}\",\"user\":\"${DEV_USER}\",\"session\":\"${DEV_SESSION}\"}}"
FOREIGN_BODY="{\"identity\":{\"tenant\":\"other-tenant\",\"user\":\"${DEV_USER}\",\"session\":\"${DEV_SESSION}\"}}"

if command -v curl >/dev/null 2>&1; then
  # Route probe: an identity-less POST distinguishes a missing route (404) from
  # the identity-mandatory 401 (route mounted AND the gate fires).
  set +e
  PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${DELETE_URL}")
  set -e
  case "${PROBE:-000}" in
    404 | 405 | 501 | 000)
      skip "phase 130: /v1/sessions/delete route not present (${PROBE:-000})"
      ;;
    401)
      ok "phase 130: sessions.delete rejects an identity-less body (401)"
      # Materialise the throwaway session (create-on-first-use on start). The
      # session.lifecycle record is written before the planner runs, so the
      # erasure has a target even when the LLM seam is unconfigured.
      curl -sS -X POST "${START_URL}" -H "Authorization: Bearer ${TOKEN}" \
        "${ID_HEADERS[@]}" -H 'Content-Type: application/json' \
        -d '{"query":"phase-130 seed","description":"sessions.delete smoke"}' >/dev/null 2>&1 || true

      # Foreign-target body (tenant != the verified triple) → 401, refused by
      # the body-identity gate BEFORE any erasure logic (own-session-only).
      set +e
      FT=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' -d "${FOREIGN_BODY}" "${DELETE_URL}")
      set -e
      if [ "${FT}" = "401" ]; then
        ok "phase 130: foreign-target sessions.delete rejected 401"
      else
        fail "phase 130: foreign-target sessions.delete expected 401, got ${FT}"
      fi

      # Own-session delete: 200 (eraser wired, session erased) OR 409 (the seed
      # task is still RUNNING — the fail-loud refusal, also a valid live signal)
      # OR 404/501 (no eraser wired on this dev runtime → SKIP).
      set +e
      DST=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
        -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' -d "${OWN_BODY}" "${DELETE_URL}")
      set -e
      case "${DST}" in
        200)
          ok "phase 130: live sessions.delete answers 200 for own session"
          # The erased session is gone: a follow-up inspect → 404 not_found.
          set +e
          IST=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
            -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
            -H 'Content-Type: application/json' \
            -d "{\"session_id\":\"${DEV_SESSION}\",${OWN_BODY#\{}" "${INSPECT_URL}")
          set -e
          if [ "${IST}" = "404" ]; then
            ok "phase 130: post-erasure sessions.inspect returns 404 not_found"
          else
            fail "phase 130: post-erasure sessions.inspect expected 404, got ${IST}"
          fi
          ;;
        409)
          ok "phase 130: live sessions.delete refuses a running-task session 409"
          ;;
        404 | 501)
          skip "phase 130: sessions.delete eraser not wired on this dev runtime (${DST})"
          ;;
        *)
          fail "phase 130: own-session sessions.delete expected 200/409, got ${DST}"
          ;;
      esac
      ;;
    *)
      skip "phase 130: sessions.delete route probe returned ${PROBE} (skipping live)"
      ;;
  esac
else
  skip "phase 130: curl not available — skipping live assertions"
fi

smoke_summary
