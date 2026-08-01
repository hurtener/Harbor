#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
# Phase 232 — Signed agent reach.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file docs/plans/phase-232-signed-agent-reach.md 'phase 232: plan exists'
assert_grep_present '^## D-397 ' docs/decisions.md 'phase 232: signed-reach decision is recorded'
assert_grep_present 'const AgentReachClaim = "agent_reach"' internal/protocol/auth/agent_reach.go 'phase 232: strict signed claim exists'
assert_grep_count 'authorizeAgent\(w, r, req.AgentID\)' internal/protocol/transports/stream/agentconfig_handler.go 13 'phase 232: all agent-config data-plane handlers share the gate'
assert_grep_present 'WithAgentReachAuthorizer\(agentReach\)' internal/runtime/serve/serve.go 'phase 232: production assembly shares one gate'
assert_grep_present 'WithAgentReachAuthorizer\(stack.AgentReach\)' harbortest/devstack/devstack.go 'phase 232: devstack assembly shares one gate'

P232_TMP="$(mktemp -d "${TMPDIR:-/tmp}/harbor-phase-232.XXXXXX")"
trap 'rm -rf "${P232_TMP}"' EXIT

# Live bearer-authenticated proof. The shipped dev token reaches exactly the
# boot-configured agent. This gives the smoke one real allowed control arm and
# real excluded arms without introducing a test-only token minting endpoint.
TOKEN="$(dev_bearer)"
HEALTH=000
if command -v curl >/dev/null 2>&1; then
    HEALTH=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$(api_url /healthz)" || true)
fi

if ! command -v curl >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
    skip 'phase 232: curl/jq unavailable — live assertions skipped'
elif [ "${HEALTH:-000}" = "000" ] || [ -z "${HEALTH}" ]; then
    skip "phase 232: dev server unreachable at $(api_url /healthz) — live assertions skipped (run under make preflight)"
elif [ -z "${TOKEN}" ]; then
    fail 'phase 232: no dev bearer resolved — unauthenticated requests would prove nothing'
else
    P232_HEADERS=(-H 'X-Harbor-Tenant: dev' -H 'X-Harbor-User: dev')

    # p232_call <session> <path> <bearer> <body>
    # Exposes P232_STATUS/P232_BODY/P232_CODE in the current shell.
    p232_call() {
        local session="$1" path="$2" bearer="$3" body="$4"
        local out="${P232_TMP}/live-body.json"
        P232_STATUS=$(curl -sS -o "${out}" -w '%{http_code}' --max-time 10 \
            -X POST "$(api_url "${path}")" \
            -H "Authorization: Bearer ${bearer}" "${P232_HEADERS[@]}" \
            -H "X-Harbor-Session: ${session}" -H 'Content-Type: application/json' \
            -d "${body}" 2>/dev/null || true)
        P232_STATUS="${P232_STATUS:-000}"
        P232_BODY=$(cat "${out}" 2>/dev/null || printf '{}')
        P232_CODE=$(printf '%s' "${P232_BODY}" | jq -r '.code // .error.code // ""' 2>/dev/null || printf '')
    }

    p232_expect() {
        local want="$1" desc="$2"
        if [ "${P232_STATUS}" = "${want}" ]; then
            ok "${desc} (${P232_STATUS})"
        else
            fail "${desc}: got ${P232_STATUS}, want ${want} (code=${P232_CODE}, body=$(printf '%s' "${P232_BODY}" | head -c 240))"
        fi
    }

    DEFAULT_AGENT='harbor-dev-agent'
    EXCLUDED_AGENT='phase232-out-of-reach-agent'

    p232_call phase232-default /v1/control/start "${TOKEN}" \
        '{"identity":{"tenant":"dev","user":"dev","session":"phase232-default"},"query":"phase 232 omitted target"}'
    p232_expect 200 'phase 232: omitted control.start target binds the default agent and passes signed reach'

    p232_call phase232-explicit /v1/control/start "${TOKEN}" \
        "{\"identity\":{\"tenant\":\"dev\",\"user\":\"dev\",\"session\":\"phase232-explicit\"},\"agent_id\":\"${DEFAULT_AGENT}\",\"query\":\"phase 232 explicit target\"}"
    p232_expect 200 'phase 232: explicit in-reach control.start target passes the shared gate'

    # Make the excluded id genuinely tenant-local and resolvable through the
    # pre-existing control-plane revision verb. This proves the 403 below is
    # authority, not an unknown-selection refusal, and does not rely on smoke
    # ordering or Phase 215's fixture.
    p232_call phase232-denied /v1/agent_config/set_revision "${TOKEN}" \
        "{\"agent_id\":\"${EXCLUDED_AGENT}\",\"payload\":{\"prompt_layers\":{\"base\":\"phase 232 resolvable but unauthorized\"}}}"
    EXCLUDED_REVISION=$(printf '%s' "${P232_BODY}" | jq -r '.revision.revision_id // empty' 2>/dev/null || printf '')
    if [ "${P232_STATUS}" = 200 ] && [ -n "${EXCLUDED_REVISION}" ]; then
        ok "phase 232: excluded agent is tenant-local and resolvable (revision=${EXCLUDED_REVISION}) without bearer authority"
    else
        fail "phase 232: could not prove excluded agent resolvable before reach checks (status=${P232_STATUS}, revision=${EXCLUDED_REVISION:-empty})"
    fi

    # Read a real, fresh-session task count before and after the denied starts.
    # A 200 plus aggregates object is mandatory; an unreadable count is never
    # laundered into zero.
    p232_count_tasks() {
        local session="$1" count=''
        p232_call "${session}" /v1/tasks/list "${TOKEN}" '{}'
        if [ "${P232_STATUS}" = "200" ] && printf '%s' "${P232_BODY}" | jq -e 'has("aggregates") and (.aggregates | type == "object")' >/dev/null 2>&1; then
            count=$(printf '%s' "${P232_BODY}" | jq -r '[.aggregates | to_entries[] | .value] | add // 0' 2>/dev/null || printf '')
        fi
        printf '%s %s' "${P232_STATUS}" "${count}"
    }

    read -r BEFORE_STATUS BEFORE_COUNT <<< "$(p232_count_tasks phase232-denied)"
    p232_call phase232-denied /v1/control/start "${TOKEN}" \
        "{\"identity\":{\"tenant\":\"dev\",\"user\":\"dev\",\"session\":\"phase232-denied\"},\"agent_id\":\"${EXCLUDED_AGENT}\",\"query\":\"must not spawn\"}"
    p232_expect 403 'phase 232: explicit out-of-reach control.start target is refused before resolution/spawn'

    TOKEN_PREFIX="${TOKEN%.*}"
    TOKEN_SIGNATURE="${TOKEN##*.}"
    FIRST_SIGNATURE_CHAR="${TOKEN_SIGNATURE:0:1}"
    if [ "${FIRST_SIGNATURE_CHAR}" = 'A' ]; then REPLACEMENT='B'; else REPLACEMENT='A'; fi
    BAD_TOKEN="${TOKEN_PREFIX}.${REPLACEMENT}${TOKEN_SIGNATURE:1}"
    p232_call phase232-denied /v1/control/start "${BAD_TOKEN}" \
        '{"identity":{"tenant":"dev","user":"dev","session":"phase232-denied"},"query":"malformed bearer must not spawn"}'
    p232_expect 401 'phase 232: malformed signed bearer is refused at authentication'
    read -r AFTER_STATUS AFTER_COUNT <<< "$(p232_count_tasks phase232-denied)"
    if [ "${BEFORE_STATUS}" = 200 ] && [ "${AFTER_STATUS}" = 200 ] && \
            [ -n "${BEFORE_COUNT}" ] && [ "${BEFORE_COUNT}" = "${AFTER_COUNT}" ] && [ "${AFTER_COUNT}" = 0 ]; then
        ok 'phase 232: excluded and malformed starts created no task in the fresh denial session (0 -> 0)'
    else
        fail "phase 232: denied-start task count was not a real stable 0 (status ${BEFORE_STATUS}->${AFTER_STATUS}, count ${BEFORE_COUNT:-?}->${AFTER_COUNT:-?})"
    fi

    p232_call phase232-config /v1/agent_config/session/set_user_prompt "${TOKEN}" \
        "{\"identity\":{\"tenant\":\"dev\",\"user\":\"dev\",\"session\":\"phase232-config\"},\"agent_id\":\"${DEFAULT_AGENT}\",\"user_prompt\":\"phase 232 live\"}"
    p232_expect 200 'phase 232: in-reach agent-config mutation passes the same shared gate'
    p232_call phase232-config /v1/agent_config/session/set_user_prompt "${TOKEN}" \
        "{\"identity\":{\"tenant\":\"dev\",\"user\":\"dev\",\"session\":\"phase232-config\"},\"agent_id\":\"${EXCLUDED_AGENT}\",\"user_prompt\":\"must not persist\"}"
    p232_expect 403 'phase 232: out-of-reach agent-config mutation is refused'

    p232_call phase232-user-config /v1/agent_config/user/skills/list "${TOKEN}" \
        "{\"agent_id\":\"${DEFAULT_AGENT}\"}"
    case "${P232_STATUS}" in
        200|501) ok "phase 232: in-reach claim-free user agent-config call passes the gate to its service (${P232_STATUS})" ;;
        *) fail "phase 232: in-reach claim-free user agent-config call stopped before its service (status=${P232_STATUS}, code=${P232_CODE})" ;;
    esac
    p232_call phase232-user-config /v1/agent_config/user/skills/list "${TOKEN}" \
        "{\"agent_id\":\"${EXCLUDED_AGENT}\"}"
    p232_expect 403 'phase 232: claim-free user agent-config family refuses an out-of-reach target'

    # The stock dev config has an intentionally empty tool catalog. A missing
    # tool therefore provides a stable downstream 404 control arm: 404 proves
    # the in-reach/omitted requests passed the reach gate into catalog lookup,
    # while the same lookup with an excluded projection must stop at 403.
    LIVE_TOOL='phase232-intentionally-missing-tool'
    p232_call phase232-tools /v1/tools/describe "${TOKEN}" \
        "{\"id\":\"${LIVE_TOOL}\",\"agent_id\":\"${DEFAULT_AGENT}\"}"
    p232_expect 404 'phase 232: explicit in-reach tools.describe projection passes the gate to catalog lookup'
    p232_call phase232-tools /v1/tools/describe "${TOKEN}" \
        "{\"id\":\"${LIVE_TOOL}\",\"agent_id\":\"${EXCLUDED_AGENT}\"}"
    p232_expect 403 'phase 232: explicit out-of-reach tools.describe projection is refused'
    p232_call phase232-tools /v1/tools/describe "${TOKEN}" "{\"id\":\"${LIVE_TOOL}\"}"
    p232_expect 404 'phase 232: omitted optional tools.describe agent projection remains compatible through catalog lookup'
fi

assert_go_tests_pass "${P232_TMP}/go-test.log" '-race -count=1 ./internal/protocol/auth ./internal/protocol ./internal/protocol/transports/stream ./internal/runtime/serve ./cmd/harbor ./harbortest/devstack ./test/integration' \
    'phase 232: signed-reach rejection and assembly regressions execute under race' \
    TestParseAgentReach_StrictBoundedShape \
    TestReachAuthorizer_FailsClosedAndDoesNotBleed \
    TestReachAuthorizer_ConcurrentIsolation_N100 \
    TestValidator_AgentReach_StrictClaimAndVerifiedAuthority \
    TestAgentConfigHandler_AgentReachClosedCensus \
    TestDispatchStart_AgentReach_GatesExplicitAndDefaultBeforeSpawn \
    TestDispatchStart_AgentReach_DirectResolverConstructionFailsClosed \
    TestDispatchStart_AgentReach_BareDirectOmittedTargetFailsClosed \
    TestAgentConfigHandler_UserRoute_WithUserScopeAllowed \
    TestToolsHandler_Describe_ExplicitAgentReachOnly \
    TestSignDevToken_ProducesParseableJWT \
    TestTokenMint_AgentReach_ValidatesAndSignsBoundedClaim \
    TestE2E_AgentReach_AuthenticatedMuxMatrix \
    TestE2E_AgentReach_ClosedAgentConfigCensus \
    TestE2E_AgentReach_SharedMuxConcurrentIsolationCancellationAndLeak
smoke_summary
