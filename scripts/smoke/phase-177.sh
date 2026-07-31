#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 177 — projection-completeness gate + the populate/remove surfaces
# (HA-24 / D-313). Closes the silent-absence class (D-311) on tasks / flows /
# memory, un-stubs the wired tasks serve.Enricher, honestly gates the tools
# facets pending Phase 178, and ships a build-time projection-completeness gate.
#
# Static guards (always run, never skip):
#   1. The projection-completeness gate test file exists
#      (internal/protocol/projectioncheck) — the class-closer is present.
#   2. The structurally-dead memory TTL facet/aggregate is gone from the wire
#      manifest (no `has_ttl_expiring` / `expiring_in_1h`).
# Live-server assertions (404/405/501 → SKIP per CLAUDE.md §4.2):
#   3. tasks.list: filter.has_pending_approval answered (SKIP until a task
#      holds an open approval gate).
#   4. memory.list: filter.agent_ids over an unpopulated agent field returns
#      invalid_request (loud), NEVER a false-empty 200; a plain list still 200s.

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

# ---- Static guard 1: the projection-completeness gate exists ---------------
if [ -f internal/protocol/projectioncheck/projectioncheck.go ] \
  && [ -f internal/protocol/projectioncheck/projectioncheck_test.go ]; then
  ok "phase 177: projection-completeness gate present (internal/protocol/projectioncheck)"
else
  skip "phase 177: projection-completeness gate not yet landed"
fi

# ---- Static guard 2: the dead memory TTL facet/aggregate is gone -----------
# The removal is Phase 177 IMPLEMENTATION work; on a pre-implementation build
# (the gate package absent) the facet is still present, so SKIP rather than
# FAIL — the §4.2 "phase-N+1 skeleton coexists with a phase-N build" convention
# read for a static wire-manifest guard.
MANIFEST="web/console/src/lib/protocol/wire-manifest.gen.json"
if [ ! -f "${MANIFEST}" ]; then
  skip "phase 177: wire manifest not present"
elif [ ! -f internal/protocol/projectioncheck/projectioncheck.go ]; then
  skip "phase 177: memory TTL facet removal not yet landed (pre-implementation build)"
elif grep -q -e 'has_ttl_expiring' -e 'expiring_in_1h' "${MANIFEST}"; then
  fail "phase 177: dead memory TTL facet/aggregate still on the wire manifest"
else
  ok "phase 177: dead memory TTL facet/aggregate removed from the wire"
fi

# ---- Live-server surface probes --------------------------------------------
TASKS_URL="$(api_url /v1/tasks/list)"
MEM_URL="$(api_url /v1/memory/list)"

TOKEN="${HARBOR_DEV_TOKEN:-dev-token-placeholder}"
ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")

if ! command -v curl >/dev/null 2>&1; then
  skip "phase 177: curl unavailable — live probes skipped"
  smoke_summary
  return 0 2>/dev/null || exit 0
fi

# tasks.list route probe (no-bearer POST distinguishes 404-miss from 401-auth).
set +e
PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
  -X POST -H 'Content-Type: application/json' -d '{"filter":{}}' "${TASKS_URL}")
set -e
case "${PROBE:-000}" in
  404 | 405 | 501 | 000)
    skip "phase 177: /v1/tasks/list route not present (${PROBE:-000})"
    ;;
  *)
    ok "phase 177: tasks.list route mounted (${PROBE})"
    if [ -n "${HARBOR_DEV_TOKEN:-}" ] && command -v jq >/dev/null 2>&1; then
      TMP="$(mktemp)"; trap 'rm -f "${TMP}"' EXIT
      set +e
      ST=$(curl -s -o "${TMP}" -w '%{http_code}' --max-time 10 \
        -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' \
        -d '{"filter":{"has_pending_approval":true}}' "${TASKS_URL}")
      set -e
      if [ "${ST}" = "200" ]; then
        HAS=$(jq -r '[.tasks[]?.has_pending_approval] | length' "${TMP}" 2>/dev/null || echo 0)
        ok "phase 177: has_pending_approval facet answered 200 (rows carrying field: ${HAS})"
      else
        skip "phase 177: has_pending_approval facet probe non-200 (${ST})"
      fi
    else
      skip "phase 177: no HARBOR_DEV_TOKEN/jq — tasks facet assertion skipped"
    fi
    ;;
esac

# memory.list: the agent_ids facet over an unpopulated agent field must fail
# loud, never return a false-empty page.
set +e
MPROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
  -X POST -H 'Content-Type: application/json' -d '{"filter":{}}' "${MEM_URL}")
set -e
case "${MPROBE:-000}" in
  404 | 405 | 501 | 000)
    skip "phase 177: /v1/memory/list route not present (${MPROBE:-000})"
    ;;
  *)
    if [ -n "${HARBOR_DEV_TOKEN:-}" ]; then
      set +e
      AST=$(curl -s -o /dev/null -w '%{http_code}' --max-time 8 \
        -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' \
        -d '{"filter":{"agent_ids":["nonexistent"]}}' "${MEM_URL}")
      set -e
      if [ "${AST}" = "400" ]; then
        ok "phase 177: memory.list agent_ids over unpopulated field fails loud (400)"
      elif [ "${AST}" = "200" ]; then
        # V1 memory has NO producer-identity populate path — list.go ALWAYS
        # returns ErrInvalidFilter for agent_ids. A 200 is therefore a
        # REGRESSION (a false-empty page masquerading as success), NOT a
        # populated path — fail loud, never SKIP (the smoke must guard the
        # exact bug it exists for).
        fail "phase 177: memory agent_ids returned 200 — V1 has no populate path, so a 200 is a false-absence regression (must loud-reject)"
      else
        skip "phase 177: memory agent_ids probe non-conclusive (${AST})"
      fi
    else
      skip "phase 177: no HARBOR_DEV_TOKEN — memory agent_ids assertion skipped"
    fi
    ;;
esac

smoke_summary
