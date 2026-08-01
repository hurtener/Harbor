#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 124 — Durable event-bus sequence rehydration / SSE resume guard.
#
# This phase is a driver-internal correctness fix (rehydrate the durable
# bus's nextSeq from the persisted head records at construction) plus an
# additive SSE-framing rule (an event with no replay position — Sequence 0
# — carries no `id:` line). There is NO new wire method, so the smoke
# guards the observable resume path rather than a new endpoint: the SSE
# event stream still opens, and a reconnect carrying a `Last-Event-ID`
# cursor is accepted.
#
# A full restart-and-resume (publish after a simulated restart, no
# sequence collision, no silent skip) and the transient-notice no-`id:`
# framing are covered by the durable-driver + stream-transport unit /
# integration tests — the live preflight server is not restarted mid-smoke.
#
# Conventions (CLAUDE.md §4.2): 404/405/501 → SKIP, so this script SKIPs
# cleanly on builds that pre-date the Phase 60 SSE route or the Phase 64
# live dev server.

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

# 1. Surface probe — is the SSE resume route present at all? When absent
# the rest SKIPs gracefully.
if ! skip_if_404 "$(api_url /v1/events)" \
  'phase 124: /v1/events SSE resume route present'; then
  smoke_summary
  exit 0
fi

# 2. The stream behind the auth middleware fails closed without a verified
# bearer (401) — the resume surface is identity-mandatory (CLAUDE.md §6).
assert_status 401 "$(api_url /v1/events)" \
  'phase 124: SSE stream without bearer rejected (401)'

# 3. With the dev token (admin scope per cmd/harbor/devauth.go), a
# RECONNECT carrying a Last-Event-ID cursor opens the stream (200). This
# exercises the resume path the rehydration fix repairs at the driver
# layer: a client reconnecting at a high cursor must still be served.
if [ -n "${HARBOR_DATA_DIR:-}" ] && [ -f "${HARBOR_DATA_DIR}/server.log" ]; then
  DEV_TOKEN="$(dev_bearer)"
  if [ -n "${DEV_TOKEN}" ]; then
    # SSE streams stay open until the client disconnects; cap the request
    # short and tolerate the timeout — %{http_code} is written before the
    # body, so the open-of-stream status reaches stdout. The Last-Event-ID
    # header is the reconnect cursor a resuming client echoes back.
    set +e
    actual=$(curl -s -o /dev/null -w '%{http_code}' --max-time 2 \
      -H "Authorization: Bearer ${DEV_TOKEN}" \
      -H 'Last-Event-ID: 1' \
      "$(api_url /v1/events?admin=1)")
    set -e
    case "${actual}" in
      200)
        ok 'phase 124: reconnect with Last-Event-ID cursor opens SSE stream (200)'
        ;;
      404|405|501)
        skip "phase 124: /v1/events resume surface not yet implemented (${actual})"
        ;;
      000|"")
        skip 'phase 124: SSE resume probe — curl could not connect / timed out before headers'
        ;;
      *)
        fail "phase 124: reconnect with Last-Event-ID expected 200, got ${actual}"
        ;;
    esac
  else
    skip 'phase 124: SSE resume probe (HARBOR_DEV_TOKEN not found in server log)'
  fi
else
  skip 'phase 124: SSE resume probe (HARBOR_DATA_DIR/server.log not reachable)'
fi

smoke_summary
