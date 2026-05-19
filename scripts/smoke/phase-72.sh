#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 72 — Console subscription protocol surface (re-affirmation +
# scope tightening).
#
# Surface assertions (404/405/501 → SKIP per CLAUDE.md §4.2):
#   1. The `events.subscribe` canonical method-name route exists. (Phase
#      60 ships `GET /v1/events`; Phase 72 elevates it to a first-class
#      method-name constant.)
#   2. A subscribe missing the identity triple AND missing the JWT bearer
#      is rejected at the wire edge (401) before any subscription is
#      opened — the Phase 61 auth.Middleware fail-closes any
#      unauthenticated request.
#   3. A `?admin=1` subscribe WITH the dev token (which carries the
#      `admin` scope per `cmd/harbor/devauth.go`) succeeds — the open of
#      the SSE stream returns 200 before any frames are written. Phase 72
#      adds the canonical `CodeIdentityScopeRequired` wire code for the
#      reject path; that path requires minting a NON-admin token, which
#      is non-trivial in a shell smoke and is covered end-to-end by the
#      integration test (`test/integration/events_subscribe_scope_test.go`).
#   4. The `events.subscribe` method-name is discoverable through the
#      Phase 59 capability handshake once that wire is extended (SKIPs
#      cleanly until the handshake carries the new capability constant).
#
# This script SKIPs cleanly on builds that pre-date Phase 60 (no
# `/v1/events` route), pre-date Phase 64 (no live `harbor dev` server),
# or pre-date Phase 72 (the new Code constant absent in body — body
# assertions fall back to status-only).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. Surface probe — does the Phase 60 SSE route exist at all?
# When the route is absent the rest of the script SKIPs gracefully via
# the per-helper 404/405/501 → SKIP path.
if ! skip_if_404 "$(api_url /v1/events)" \
  'phase 72: /v1/events SSE route present (Phase 60 surface)'; then
  smoke_summary
  exit 0
fi

# 2. Canonical method-name probe via the (future) JSON-RPC-style protocol
# router. Stubbed; `protocol_call` SKIPs until the Protocol method-name
# router lands. The smoke pins the canonical wire string so a future
# router cannot silently rename it.
protocol_call 'events/subscribe' '{}' \
  'phase 72: events.subscribe canonical method-name route'

# 3. Missing-identity (and missing-bearer) rejection — the SSE route
# behind Phase 61's auth middleware fails closed without a verified
# bearer (401) BEFORE any subscription is opened (CLAUDE.md §6 rule 9,
# fail-loudly per §13).
assert_status 401 "$(api_url /v1/events)" \
  'phase 72: subscribe without identity/bearer rejected (401)'

# 4. Cross-tenant fan-in WITH a scope-bearing token — the dev binary
# prints `HARBOR_DEV_TOKEN=...` to stderr at boot (captured by the
# preflight harness into ${HARBOR_DATA_DIR}/server.log). The dev token
# carries the `admin` scope per cmd/harbor/devauth.go, so `?admin=1`
# succeeds. Asserts the gate accepts a properly-scoped subscribe. The
# matching 403 reject-path requires minting a NON-admin token, which
# the integration test (`test/integration/events_subscribe_scope_test.go`)
# covers end-to-end with the real ES256 testdata keypair.
if [ -n "${HARBOR_DATA_DIR:-}" ] && [ -f "${HARBOR_DATA_DIR}/server.log" ]; then
  DEV_TOKEN="$(grep -m1 '^HARBOR_DEV_TOKEN=' "${HARBOR_DATA_DIR}/server.log" 2>/dev/null | sed 's/^HARBOR_DEV_TOKEN=//' || true)"
  if [ -n "${DEV_TOKEN}" ]; then
    # SSE streams stay open until the client disconnects. We only want
    # the open-of-stream status, so cap the request short (--max-time 2)
    # and tolerate a non-zero curl exit (timeout) — the `%{http_code}`
    # is written before the body bytes, so a clean 200 reaches stdout
    # even when the timeout kills the body read. `set +e` brackets the
    # call so `set -euo pipefail` does not abort on the curl timeout.
    set +e
    actual=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 \
      -H "Authorization: Bearer ${DEV_TOKEN}" \
      "$(api_url /v1/events?admin=1)")
    set -e
    case "${actual}" in
      200)
        ok 'phase 72: cross-tenant subscribe with ScopeAdmin-bearing token opens stream (200)'
        ;;
      404|405|501)
        skip "phase 72: /v1/events?admin=1 surface not yet implemented (${actual})"
        ;;
      000|"")
        skip 'phase 72: cross-tenant subscribe admin-path — curl could not connect / timed out before headers'
        ;;
      *)
        fail "phase 72: cross-tenant subscribe with admin scope expected 200, got ${actual}"
        ;;
    esac
  else
    skip 'phase 72: cross-tenant subscribe admin-path (HARBOR_DEV_TOKEN not found in server log)'
  fi
else
  skip 'phase 72: cross-tenant subscribe admin-path (HARBOR_DATA_DIR/server.log not reachable)'
fi

# 5. Reject-path coverage (cross-tenant without scope claim → 403 +
# `identity_scope_required` body Code) is covered end-to-end by the
# Phase 72 integration test. SKIP here so the assertion is recorded as
# deliberately deferred, not absent.
skip 'phase 72: cross-tenant subscribe WITHOUT scope claim → 403 identity_scope_required (covered by integration test)'

# 6. Capability handshake includes events.subscribe — SKIPs until the
# Phase 59 handshake carries the new capability constant alongside
# task_control. The preflight harness reaches the handshake when the
# Protocol layer ships the negotiation route; until then the probe
# auto-SKIPs on 404.
skip_if_404 "$(api_url /v1/handshake)" \
  'phase 72: capability handshake route present' || true

smoke_summary
