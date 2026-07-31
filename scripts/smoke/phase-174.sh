#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 174 — session-projection enrichment / honest counters (HA-22 /
# D-309). `sessions.list` / `sessions.inspect` populate the six false-
# absence counters (`tasks_count`, `events_count`, `total_cost_cents`,
# `total_tokens`, `has_pending_intervention`, `has_failed_task`) at the
# runtime SOURCE via a read-time Enricher seam that SUMS per session from
# the event substrate + task registry + pause registry. The two agent
# fields take the D-311 class rule: nullable (representable absence) +
# `filter.agent_ids` FAILS LOUD (`invalid_request`) over the unpopulated
# binding — never a silent empty page — while a plain `query` still
# succeeds (WARN-4). A truncated per-session scan surfaces the additive
# honest `counters_partial` marker (WARN-1).
#
# Static guards (always): the Enricher seam + WithEnricher option, the
# additive `counters_partial` wire field, the nullable agent fields, the
# loud-reject at the Service edge, and that production ALWAYS wires the
# enricher (WARN-3). Build/test gates: manifest + generated-docs lockstep
# + the package tests. Live (skips per 404/405/501): sessions.list answers,
# filter.agent_ids fails loud (400) while a plain query answers 200, and
# (SKIP if no cost yet) a populated counter reaches the wire.

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

# --- Static: the Enricher seam + production aggregation. ---
assert_grep_present 'type Enricher interface' "internal/sessions/protocol/enricher.go" \
  "phase 174: sessions.Enricher seam present (mirrors tasks.Projector)"
assert_grep_present 'func WithEnricher' "internal/sessions/protocol/lister_projector.go" \
  "phase 174: WithEnricher option present"
assert_grep_present 'CounterEnricher' "internal/sessions/protocol/enricher.go" \
  "phase 174: production CounterEnricher present"
assert_grep_present 'func \(p \*ListerProjector\) CountersAvailable' "internal/sessions/protocol/lister_projector.go" \
  "phase 174: CountersAvailable capability gate present (WARN-3)"

# --- Static: production ALWAYS wires the enricher at the assembly seam. ---
assert_grep_present 'NewCounterEnricher' "internal/runtime/serve/mux.go" \
  "phase 174: mux assembly wires the production counter enricher (WARN-3 prove-always-wired)"

# --- Static: the additive wire signals + nullable agent fields. ---
assert_grep_present 'CountersPartial bool' "internal/protocol/types/sessions.go" \
  "phase 174: additive SessionRow.CountersPartial wire field (WARN-1)"
assert_grep_present 'counters_partial,omitempty' "internal/protocol/types/sessions.go" \
  "phase 174: counters_partial json tag (additive, omitempty)"
assert_grep_present 'AgentID \*string' "internal/protocol/types/sessions.go" \
  "phase 174: agent_id nullable (representable absence, D-311)"
assert_grep_present 'counters_partial' "web/console/src/lib/sessions/types.ts" \
  "phase 174: TS SessionRow mirrors counters_partial"

# --- Static: the loud-reject at the Service edge (WARN-4). ---
assert_grep_present 'filter.agent_ids operates over an unpopulated' "internal/sessions/protocol/protocol.go" \
  "phase 174: filter.agent_ids fails loud over the unpopulated binding"

# --- Build/test gates: manifest lockstep + generated-docs + the tests. ---
if make protocol-ts-gen-check >/dev/null 2>&1; then
  ok "phase 174: make protocol-ts-gen-check passes (manifest + TS in lockstep)"
else
  fail "phase 174: make protocol-ts-gen-check failed (regenerate manifest / mirror the TS types)"
fi
if make protocol-docs-gen-check >/dev/null 2>&1; then
  ok "phase 174: make protocol-docs-gen-check passes (types.md regenerated, D-209)"
else
  fail "phase 174: make protocol-docs-gen-check failed (run make protocol-docs-gen and commit the pages)"
fi
if go test -race ./internal/sessions/protocol/... >/dev/null 2>&1; then
  ok "phase 174: sessions/protocol tests pass under -race"
else
  fail "phase 174: sessions/protocol tests failed (go test -race ./internal/sessions/protocol/...)"
fi

# --- Live (skips per 404/405/501): agent_ids fails loud, query succeeds, ---
# --- a populated counter reaches the wire.                                ---
LIST_URL="$(api_url /v1/sessions/list)"
START_URL="$(api_url /v1/control/start)"

DEV_TENANT="dev"
DEV_USER="dev"
DEV_SESSION="phase174-smoke-$$"
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
    401 | 200)
      ok "phase 174: /v1/sessions/list route mounted (probe ${PROBE})"
      # Seed a throwaway session so the listing has at least one row.
      curl -sS -X POST "${START_URL}" -H "Authorization: Bearer ${TOKEN}" \
        "${ID_HEADERS[@]}" -H 'Content-Type: application/json' \
        -d '{"query":"phase-174 seed","description":"sessions enrichment smoke"}' >/dev/null 2>&1 || true

      # WARN-4: filter.agent_ids over the unpopulated binding must fail loud
      # (400 invalid_request), never a silent empty 200 page.
      set +e
      AST=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
        -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' \
        -d '{"filter":{"agent_ids":["some-agent"]}}' "${LIST_URL}")
      set -e
      if [ "${AST}" = "400" ]; then
        ok "phase 174: sessions.list filter.agent_ids fails loud (400), never a false-empty page"
      else
        fail "phase 174: filter.agent_ids expected 400 (invalid_request), got ${AST}"
      fi

      # WARN-4: a plain query is NEVER failed whole — it answers 200.
      set +e
      QST=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
        -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' \
        -d '{"filter":{"query":"phase174"}}' "${LIST_URL}")
      set -e
      if [ "${QST}" = "200" ]; then
        ok "phase 174: sessions.list plain query answers 200 (never a whole-query reject)"
      else
        fail "phase 174: plain query expected 200, got ${QST}"
      fi

      # A populated counter reaches the wire once a run has produced cost.
      LR=$(curl -sS --max-time 10 -X POST -H "Authorization: Bearer ${TOKEN}" \
        "${ID_HEADERS[@]}" -H 'Content-Type: application/json' \
        -d '{}' "${LIST_URL}" 2>/dev/null || echo '')
      if echo "${LR}" | grep -Eq '"total_cost_cents":[1-9][0-9]*'; then
        ok "phase 174: a populated total_cost_cents reaches the wire"
      else
        skip "phase 174: no session with recorded cost yet (populated-counter assertion skipped)"
      fi
      ;;
    *)
      skip "phase 174: sessions.list route probe returned ${PROBE} (skipping live)"
      ;;
  esac
else
  skip "phase 174: curl not available — skipping live assertions"
fi

smoke_summary
