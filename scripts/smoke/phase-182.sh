#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 182 — TUI conversation and session experience (D-318).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

help_output="$(./bin/harbor tui --help 2>&1)"
if [[ "${help_output}" == *"--attach"* && "${help_output}" == *"--session"* && "${help_output}" == *"authenticated Harbor Protocol REST"* ]]; then
    ok "phase 182: operational Protocol-only tui help surface"
else
    fail "phase 182: tui help missing attach/auth/session contract"
fi

if [[ "${help_output}" != *"serve --tui"* && "${help_output}" != *"scaffold"* && "${help_output}" != *"worktree"* && "${help_output}" != *"source editor"* ]]; then
    ok "phase 182: deferred distribution and coding-agent surfaces absent"
else
    fail "phase 182: out-of-phase surface leaked into tui help"
fi

if go test -race ./internal/tui/composer ./internal/tui/conversation ./internal/tui/sessionpicker ./internal/tui/app >/dev/null; then
    ok "phase 182: composer/conversation/session/app race suite"
else
    fail "phase 182: focused race suite"
fi

if go test -race ./test/integration -run '^TestE2E_TUI(Attach_|ConversationPTY_)' -count=1 >/dev/null; then
    ok "phase 182: authenticated attach plus real key-driven PTY workflow"
else
    fail "phase 182: authenticated/PTY integration"
fi

assert_grep_present 'Token\(ctx context\.Context, scope types\.IdentityScope\)' internal/tui/conversation/token.go "phase 182: identity-aware lifetime token source"
assert_grep_present '<-oldDone' internal/tui/conversation/controller.go "phase 182: old stream joined before switch"
assert_grep_present 'TestRuntimeModel_KeyDrivenSessionDialogsCallController' internal/tui/app/live_test.go "phase 182: session controls invoke controller paths"
assert_grep_present 'TestRuntimeModel_KeyDrivenComposerAutocompleteSearchAttachmentExportAndPrefs' internal/tui/app/live_test.go "phase 182: composer controls invoke operational workflows"
assert_grep_present 'TestE2E_TUIConversationPTY_KeyDrivenAuthenticatedWorkflow' test/integration/tui_terminal_pty_test.go "phase 182: real PTY keyboard walkthrough"
assert_grep_present 'startPTYCommand\(t, binary' test/integration/tui_terminal_pty_test.go "phase 182: PTY launches the built harbor tui command"
assert_grep_present '"status": "orchestrator-reviewed"' internal/tui/testdata/golden/capture-manifest.json "phase 182: operational captures passed orchestrator review"
assert_grep_present '"reviewed": true' internal/tui/testdata/golden/capture-manifest.json "phase 182: capture review gate is explicit"
assert_grep_absent 'internal/runtime|web/console' internal/tui/conversation/controller.go "phase 182: no Runtime or Console dependency"
assert_file docs/skills/drive-the-harbor-tui/SKILL.md "phase 182: operator TUI skill"

if [[ -n "${HARBOR_DEV_TOKEN:-}" ]] && command -v jq >/dev/null 2>&1; then
    id_headers=(-H 'X-Harbor-Tenant: dev' -H 'X-Harbor-User: dev' -H 'X-Harbor-Session: dev')
    info_body="$(curl -sS -X POST "$(api_url '/v1/control/runtime.info')" \
        -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" "${id_headers[@]}" \
        -H 'Content-Type: application/json' -d '{"identity":{"tenant":"dev","user":"dev","session":"dev"}}')"
    if jq -e '.capabilities | index("state_snapshots")' >/dev/null 2>&1 <<<"${info_body}"; then
        parity_failed=0
        while IFS='|' read -r route payload; do
            response="$(curl -sS -w $'\n%{http_code}' -X POST "$(api_url "${route}")" \
                -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" "${id_headers[@]}" \
                -H 'Content-Type: application/json' -d "${payload}")"
            code="${response##*$'\n'}"
            body="${response%$'\n'*}"
            if [[ "${code}" == "200" ]] || [[ "${code}" == "404" && "${body}" != *"404 page not found"* ]]; then
                continue
            else
                fail "phase 182: advertised state_snapshots route ${route} returned ${code}"
                parity_failed=1
            fi
        done <<'EOF'
/v1/state/history|{"session_id":"dev","limit":1}
/v1/tasks/list|{}
/v1/sessions/inspect|{"session_id":"dev"}
/v1/pause/list|{"page":1,"page_size":1}
EOF
        if [[ "${parity_failed}" == "0" ]]; then
            ok "phase 182: every advertised hydration method responds live"
        fi
    else
        skip "phase 182: Runtime does not advertise state_snapshots"
    fi
else
    skip "phase 182: HARBOR_DEV_TOKEN/jq unavailable for live method-capability parity"
fi

smoke_summary
