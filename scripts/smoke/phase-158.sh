#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 158 smoke — session auto-naming (D-289): the `naming` agent-config
# section rides set_revision (round-trip through agent_config.get), an invalid
# section (repeat_every without max_repetitions) → 400, plus static
# single-source assertions and the steering naming-trigger unit tests.
#
# Conventions (AGENTS.md §4.2): 404/405/501 → SKIP; OK >= 2 once shipped;
# FAIL = 0.

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

# --- Static: the domain + wire types + event + registry surface exist. ---
assert_grep_present 'NamingSection' "internal/agentcfg/agentcfg.go" \
  "phase 158: agentcfg.NamingSection present"
assert_grep_present 'AgentConfigNaming' "internal/protocol/types/agentconfig.go" \
  "phase 158: AgentConfigNaming wire type present"
assert_grep_present 'AgentConfigNaming' "web/console/src/lib/protocol/agentconfig.ts" \
  "phase 158: TS AgentConfigNaming interface mirrored"
assert_grep_present 'EventTypeSessionNamingFailed' "internal/runtime/steering/events.go" \
  "phase 158: session.naming_failed event type registered"
assert_grep_present 'SetTitleAuto' "internal/sessions/sessions.go" \
  "phase 158: sessions.SetTitleAuto on the registry interface"
assert_grep_present 'RecordCompletedTurn' "internal/sessions/sessions.go" \
  "phase 158: sessions.RecordCompletedTurn on the registry interface"
assert_grep_present 'ErrManualTitle' "internal/sessions/sessions.go" \
  "phase 158: sessions.ErrManualTitle sentinel present"

# --- Build/test gates: manifest lockstep + generated-docs gate + the tests. ---
if make protocol-ts-gen-check >/dev/null 2>&1; then
  ok "phase 158: make protocol-ts-gen-check passes (manifest + TS types in lockstep)"
else
  fail "phase 158: make protocol-ts-gen-check failed (regenerate manifest / mirror the TS types)"
fi
if make protocol-docs-gen-check >/dev/null 2>&1; then
  ok "phase 158: make protocol-docs-gen-check passes (methods/events/types regenerated, D-209)"
else
  fail "phase 158: make protocol-docs-gen-check failed (run make protocol-docs-gen and commit the pages)"
fi
if go test ./internal/runtime/steering/ -run 'Naming' >/dev/null 2>&1; then
  ok "phase 158: steering naming-trigger unit tests pass"
else
  fail "phase 158: steering naming-trigger tests failed (go test ./internal/runtime/steering -run Naming)"
fi

# --- Live (skips per 404/405/501): naming section round-trips through
# --- set_revision → get, and an invalid section → 400.
SET_REVISION_URL="$(api_url /v1/agent_config/set_revision)"
GET_URL="$(api_url /v1/agent_config/get)"

# Probe the route first so 404/405/501 → SKIP cleanly on a build that does not
# mount it (the §4.2 SKIP convention).
PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
  -X POST "${SET_REVISION_URL}" -H 'Content-Type: application/json' -d '{}' 2>/dev/null || echo 000)
case "${PROBE}" in
  404|405|501|000|'')
    skip "phase 158: agent_config.set_revision route not present (${PROBE:-000})"
    smoke_summary
    exit 0
    ;;
esac

if [ -z "${HARBOR_DEV_TOKEN:-}" ] || ! command -v jq >/dev/null 2>&1; then
  skip "phase 158: HARBOR_DEV_TOKEN/jq unavailable — naming live assertions skipped (run under 'make preflight')"
  smoke_summary
  exit 0
fi

TOKEN="${HARBOR_DEV_TOKEN}"
AGENT_ID="phase158-smoke-$$"
ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: phase158-$$")

post() {
  local url="$1" body="$2"
  curl -sS -X POST "${url}" -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
    -H 'Content-Type: application/json' -d "${body}"
}

# Pin a naming section via set_revision.
SET_BODY=$(post "${SET_REVISION_URL}" \
  "{\"agent_id\":\"${AGENT_ID}\",\"payload\":{\"naming\":{\"auto\":true,\"after_turns\":2,\"repeat_every\":3,\"max_repetitions\":5,\"max_title_len\":100}}}")
SET_AUTO="$(printf '%s' "${SET_BODY}" | jq -r '.revision.payload.naming.auto // empty')"
if [ "${SET_AUTO}" = "true" ]; then
  ok "phase 158: set_revision pinned the naming section"
else
  fail "phase 158: set_revision did not pin the naming section: ${SET_BODY}"
fi

# get reflects the pinned section.
GET_BODY=$(post "${GET_URL}" "{\"agent_id\":\"${AGENT_ID}\"}")
GET_AFTER="$(printf '%s' "${GET_BODY}" | jq -r '.revision.payload.naming.after_turns // empty')"
if [ "${GET_AFTER}" = "2" ]; then
  ok "phase 158: agent_config.get reflects the pinned naming section"
else
  fail "phase 158: agent_config.get did not reflect the naming section: ${GET_BODY}"
fi

# An invalid section (repeat_every without max_repetitions) → 400.
INVALID_CODE=$(curl -s -o /dev/null -w '%{http_code}' -X POST "${SET_REVISION_URL}" \
  -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" -H 'Content-Type: application/json' \
  -d "{\"agent_id\":\"${AGENT_ID}\",\"payload\":{\"naming\":{\"auto\":true,\"repeat_every\":2}}}")
if [ "${INVALID_CODE}" = "400" ]; then
  ok "phase 158: repeat_every without max_repetitions → 400"
else
  fail "phase 158: invalid naming section returned ${INVALID_CODE}, want 400"
fi

smoke_summary
