#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 183 — TUI Runtime control and inspection (D-319).

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

if go test -race ./internal/tui/renderers ./internal/tui/tasks ./internal/tui/tools ./internal/tui/artifacts ./internal/tui/events ./internal/tui/interventions ./internal/tui/posture ./internal/tui/app -run 'Test(Registry|Inbox|Derive|State|ActionIntent|ActionMatrix|RuntimeModel_Runtime)' -count=1 >/dev/null; then
    ok "phase 183: renderer/control derivations and N>=100 reuse pass under race"
else
    fail "phase 183: focused renderer/control race suite"
fi

if go test -race ./test/integration -run '^TestE2E_TUIRuntimeControlPTY_MultiplePauseTokensAndReconciliation$' -count=1 >/dev/null; then
    ok "phase 183: built authenticated harbor tui controls production drivers through a real PTY"
else
    fail "phase 183: built authenticated PTY Runtime control walkthrough"
fi

assert_grep_present '"status": "candidate-generated-pending-orchestrator-review"' internal/tui/testdata/golden/capture-manifest.json "phase 183: regenerated capture matrix awaits orchestrator"
assert_grep_present '"reviewed": false' internal/tui/testdata/golden/capture-manifest.json "phase 183: capture review was not self-approved"
assert_grep_present 'runtime-action-matrix' internal/tui/app/golden_test.go "phase 183: action matrix capture exists"
assert_grep_present 'runtime-interventions' internal/tui/app/golden_test.go "phase 183: intervention capture exists"
assert_grep_present 'TestActionIntent_N128MixedIdentityGenerationAndTargets' internal/tui/app/actions_test.go "phase 183: N>=100 mixed intent isolation fence"
for package_dir in tasks tools artifacts events interventions renderers posture; do
    assert_grep_absent 'internal/runtime|web/console' "internal/tui/${package_dir}" "phase 183: ${package_dir} remains Protocol-client-only"
done
assert_file docs/site/skills/drive-the-harbor-tui/SKILL.md "phase 183: docs-site TUI skill stub"

if [[ -n "${HARBOR_DEV_TOKEN:-}" ]] && command -v jq >/dev/null 2>&1; then
    headers=(-H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" -H 'X-Harbor-Tenant: dev' -H 'X-Harbor-User: dev' -H 'X-Harbor-Session: dev' -H 'Content-Type: application/json')
    check_shape() {
        local route="$1" payload="$2" expression="$3" label="$4" response code body
        response="$(curl -sS -w $'\n%{http_code}' -X POST "$(api_url "${route}")" "${headers[@]}" -d "${payload}")"
        code="${response##*$'\n'}"
        body="${response%$'\n'*}"
        if [[ "${code}" == "404" || "${code}" == "405" || "${code}" == "501" ]]; then
            skip "phase 183: ${label} unavailable (${code})"
        elif [[ "${code}" == "200" ]] && jq -e "${expression}" >/dev/null 2>&1 <<<"${body}"; then
            ok "phase 183: ${label}"
        else
            fail "phase 183: ${label} status/shape (${code})"
        fi
    }
    check_shape '/v1/tasks/list' '{}' '.rows and .aggregates' 'live tasks.list shape'
    check_shape '/v1/tools/list' '{"page":1,"page_size":1}' '.tools and .aggregates' 'live tools.list shape'
    check_shape '/v1/control/artifacts.list' '{"scope":{"tenant":"dev","user":"dev","session":"dev"},"limit":1}' '.rows and has("total_matched")' 'live artifacts.list shape'
    check_shape '/v1/events/list' '{"filter":{},"limit":1}' '(.events | type == "array") and has("has_more")' 'live events.list shape'
    check_shape '/v1/events/aggregate' '{"filter":{},"window":3600000000000,"bucket":300000000000}' '(.buckets | type == "array") and (.protocol_version | type == "string")' 'live events.aggregate shape'
    check_shape '/v1/control/runtime.health' '{"identity":{"tenant":"dev","user":"dev","session":"dev"}}' '.subsystems' 'live runtime.health shape'
    check_shape '/v1/pause/list' '{"page":1,"page_size":1}' '(.snapshots | type == "array") and has("total_rows")' 'live pause.list PauseToken shape'
else
    skip "phase 183: HARBOR_DEV_TOKEN/jq unavailable for live canonical shape assertions"
fi

smoke_summary
