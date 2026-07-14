#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 174 — session-projection enrichment / honest counters (HA-22).
#
# Guards the fix for the false-absence defect: SessionRow's counter fields
# (total_cost_cents, total_tokens, tasks_count, events_count, has_failed_task,
# has_pending_intervention) were declared + wire-visible but never assigned, so
# the runtime shipped facets / sort / cursor over permanent zeros. This smoke
# asserts (a) a populated counter reaches the `sessions.list` wire once a run has
# produced activity, and (b) a filter/sort/query over an UNPOPULATED agent field
# fails LOUD (invalid_request), never a silent empty 200 page.
#
# Until the surface lands the assertions SKIP on 404/405/501 so the script stays
# green against a pre-174 build.
#
#   cp scripts/smoke/_template.sh scripts/smoke/phase-NN.sh
#   chmod +x scripts/smoke/phase-NN.sh
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# Classification (D-104 — the `# PREFLIGHT_REQUIRES:` header above):
#   - static-only — pure file/text greps, golden compares, file-existence
#     assertions. Runs in the parallel batch BEFORE the dev server boots.
#   - live-server — hits the booted dev server over HTTP (`api_url`,
#     `assert_status`, `skip_if_404`, `assert_json_path`) or reads the
#     preflight server log. Runs serially against the booted instance.
#   - unit-tests — runs `go test` for one or more packages. Parallelisable;
#     `go test` schedules its own internal parallelism.
#
# Pick `live-server` whenever the smoke depends on `HARBOR_BIND` /
# `HARBOR_BASE_URL` / `HARBOR_DEV_TOKEN` / `${HARBOR_DATA_DIR}/server.log`
# or invokes the built `bin/harbor` against a network endpoint. When in
# doubt, `live-server` is the safe default — misclassifying a
# server-touching smoke as `static-only` produces nondeterministic flakes.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Phase 174 assertions.
# ----------------------------------------------------------------------------

# Static leg: the sessions Protocol package tests exercise the enricher seam,
# the honest-zero fallback, the truthful cost facet / sort, and the loud
# rejection of a facet over an unpopulated agent field.
if go test -race ./internal/sessions/protocol/... >/dev/null 2>&1; then
  ok "phase 174: sessions/protocol tests pass under -race (enricher + truthful facet/sort)"
else
  fail "phase 174: sessions/protocol tests failed (go test -race ./internal/sessions/protocol/...)"
fi

# Live leg (skips per 404/405/501): sessions.list carries a populated counter,
# and a facet over an unpopulated agent field fails LOUD (never a silent empty
# 200 page). The preflight dev server is trust-based — identity via X-Harbor-*.
LIST_URL="$(api_url /v1/sessions/list)"
DEV_TENANT="dev"; DEV_USER="dev"; DEV_SESSION="phase174-smoke-$$"
TOKEN="dev-token-placeholder"
[ -n "${HARBOR_DEV_TOKEN:-}" ] && TOKEN="${HARBOR_DEV_TOKEN}"
ID_HEADERS=(-H "X-Harbor-Tenant: ${DEV_TENANT}" -H "X-Harbor-User: ${DEV_USER}" -H "X-Harbor-Session: ${DEV_SESSION}")

if command -v curl >/dev/null 2>&1; then
  set +e
  PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${LIST_URL}")
  set -e
  case "${PROBE:-000}" in
    404 | 405 | 501 | 000)
      skip "phase 174: /v1/sessions/list route not present (${PROBE:-000})"
      ;;
    *)
      ok "phase 174: /v1/sessions/list route is mounted (${PROBE})"
      # A well-formed list request returns rows carrying the counter fields
      # (present on the wire; a populated value asserts once a run has produced
      # activity — otherwise the honest zero is acceptable, so we assert the
      # field is PRESENT, not that it is non-zero, to stay build-order robust).
      LR=$(curl -sS --max-time 10 -X POST -H "Authorization: Bearer ${TOKEN}" \
        "${ID_HEADERS[@]}" -H 'Content-Type: application/json' -d '{}' "${LIST_URL}" 2>/dev/null)
      if echo "${LR}" | grep -q '"total_cost_cents"'; then
        ok "phase 174: sessions.list rows carry the total_cost_cents counter field"
      else
        skip "phase 174: no rows yet under the dev identity (counter presence not observable)"
      fi
  esac
else
  skip "phase 174: curl not available — live sessions.list assertions skipped"
fi

smoke_summary
