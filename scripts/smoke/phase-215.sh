#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 215 — caller-named agent selection (D-360).
#
# `StartRequest.agent_id` names which agent's CONFIGURATION a run executes
# under. The two-check rule: accept when the id equals the runtime's
# configured default, OR when a config revision exists for the caller's
# tenant. Phase 232 adds the independent signed-authority condition: selection
# succeeds only when the effective id is also in the bearer's agent_reach.
# Anything else is REFUSED at the Protocol edge before a task exists — never
# substituted with the default.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.

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

# ---------------------------------------------------------------------------
# Static trip-wires (run regardless of the live server).
# ---------------------------------------------------------------------------

# The wire field is single-sourced and the generated artifacts are in
# lockstep (§13, D-209/D-223 — this phase owns both for the wave).
#
# Both guards below used to say `skip "... (pre-215 build)"` on the absent
# branch. This phase is SHIPPED, so there is no pre-215 build to be forward
# compatible with: deleting `StartRequest.AgentID` — the exact regression the
# guard exists to catch — produced a SKIP, and preflight stayed green. That is
# §4.2 item 5 inverted, and the wave-v1.24 checkpoint audit inverted it back.
if grep -q 'AgentID string `json:"agent_id,omitempty"`' internal/protocol/types/control.go 2>/dev/null; then
    ok "phase 215 static: StartRequest carries the agent_id wire field"
else
    fail "phase 215 static: StartRequest has no agent_id wire field in internal/protocol/types/control.go — this phase shipped it, so its absence is a regression"
fi
if grep -A 70 '"StartRequest"' web/console/src/lib/protocol/wire-manifest.gen.json 2>/dev/null \
        | grep -q '"key": "agent_id"'; then
    ok "phase 215 static: agent_id is in the REGENERATED wire manifest for StartRequest"
else
    fail "phase 215 static: agent_id absent from StartRequest in wire-manifest.gen.json — regenerate with 'make protocol-ts-gen' (D-223 lockstep)"
fi

# --- RULING A TRIP-WIRE. A signed pair's immutable registrar remains its
#     assertion/removal/audit identity, but normal data-plane use MUST carry
#     the exact effective agent restored from durable authenticated reach
#     admission. Keep the two channels distinct. ---
if grep -q 'admissionCtx, admittedAgentID, agentReachAdmitted := d.agentReachAdmissions.Restore(d.subCtx, task)' internal/runtime/serve/runloop.go 2>/dev/null \
    && grep -q 'runCtx = tools.WithEffectiveAgentConfig(runCtx, effectiveAgentID)' internal/runtime/serve/runloop.go 2>/dev/null \
    && grep -q 'agentID, ok = tools.EffectiveAgentConfigFrom(ctx)' internal/tools/auth/drivers/tokenexchange/tokenexchange.go 2>/dev/null; then
    ok "phase 215 static: signed-capability use is bound to restored reach admission, not boot provenance"
elif grep -q 'tools.WithEffectiveAgentConfig' internal/runtime/serve/runloop.go 2>/dev/null \
    || grep -q 'EffectiveAgentConfigFrom' internal/tools/auth/drivers/tokenexchange/tokenexchange.go 2>/dev/null; then
    fail "phase 215 static: signed-capability admission is incomplete — restore reach receipt, stamp effective agent, and require it at token exchange"
else
    fail "phase 215 static: signed-capability admission seams absent — this shipped phase must restore reach admission, stamp the effective agent, and enforce it at token exchange"
fi

# --- The run-start ORDERING guard: tasks.Get must precede
#     reconcileConnections, because the reconcile legs are owner-scoped by
#     (tenant, agent) and need the run's EFFECTIVE agent. ---
GET_LINE="$(grep -n 'task, gErr := d.tasks.Get(taskCtx, taskID)' internal/runtime/serve/runloop.go 2>/dev/null | head -1 | cut -d: -f1)"
REC_LINE="$(grep -n 'd.reconcileConnections(taskCtx,' internal/runtime/serve/runloop.go 2>/dev/null | head -1 | cut -d: -f1)"
if [ -z "${GET_LINE}" ] || [ -z "${REC_LINE}" ]; then
    skip "phase 215 static: runOne's tasks.Get / reconcileConnections call sites not found (unexpected build shape)"
elif [ "${GET_LINE}" -lt "${REC_LINE}" ]; then
    ok "phase 215 static: runOne reads the task (line ${GET_LINE}) BEFORE reconcileConnections (line ${REC_LINE})"
else
    fail "phase 215 static: reconcileConnections (line ${REC_LINE}) runs before tasks.Get (line ${GET_LINE}) — the reconcile would use the boot agent, not the run's"
fi

# ---------------------------------------------------------------------------
# Live-server assertions.
# ---------------------------------------------------------------------------
START_URL="$(api_url /v1/control/start)"
TASK_GET_URL="$(api_url /v1/tasks/get)"
TASK_LIST_URL="$(api_url /v1/tasks/list)"
SET_REVISION_URL="$(api_url /v1/agent_config/set_revision)"

PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${START_URL}" 2>/dev/null || true)
case "${PROBE:-000}" in
    404|405|501|000|'')
        skip "phase 215: control.start route not present (${PROBE:-000})"
        smoke_summary
        exit 0
        ;;
esac

if [ -z "${HARBOR_DEV_TOKEN:-}" ] || ! command -v jq >/dev/null 2>&1; then
    skip "phase 215: HARBOR_DEV_TOKEN/jq unavailable — live assertions skipped (run under 'make preflight')"
    smoke_summary
    exit 0
fi

TOKEN="${HARBOR_DEV_TOKEN}"
ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")
# The dev runtime's configured default agent id (serve.go's devAgentConfigID).
DEFAULT_AGENT="harbor-dev-agent"
SECOND_AGENT="phase215-smoke-agent"

post_body() {
    local url="$1" body="$2"
    curl -sS -X POST "${url}" -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' -d "${body}" 2>/dev/null
}

post_code() {
    local url="$1" body="$2"
    curl -s -o /dev/null -w '%{http_code}' -X POST "${url}" \
        -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' -d "${body}" 2>/dev/null || true
}

# task_agent_id <task_id> — the agent_id the task read-back reports
# ("" when the caller named none: DEFAULTED, never "unknown").
task_agent_id() {
    post_body "${TASK_GET_URL}" "{\"id\":\"$1\"}" | jq -r '.task.agent_id // ""'
}

# --- (1) A `start` with NO agent_id succeeds and reads back empty — the
#         unchanged path. ---
PLAIN_RESP="$(post_body "${START_URL}" '{"query":"phase-215 smoke: no agent named"}')"
PLAIN_TASK="$(printf '%s' "${PLAIN_RESP}" | jq -r '.task_id // empty')"
if [ -n "${PLAIN_TASK}" ]; then
    ok "phase 215: start with no agent_id succeeds (task_id=${PLAIN_TASK})"
    PLAIN_AGENT="$(task_agent_id "${PLAIN_TASK}")"
    if [ -z "${PLAIN_AGENT}" ]; then
        ok "phase 215: an unnamed run reads back an EMPTY agent_id (defaulted, never fabricated)"
    else
        fail "phase 215: an unnamed run reported agent_id=${PLAIN_AGENT}, want empty"
    fi
else
    fail "phase 215: start with no agent_id failed: $(printf '%s' "${PLAIN_RESP}" | head -c 200)"
fi

# --- (2) CHECK (i): the runtime's CONFIGURED DEFAULT id is accepted with
#         NO config revision written for it. This is the case an
#         agent-registry-membership rule would have refused, because the
#         boot agent is never registered as a fleet entity. ---
DEF_RESP="$(post_body "${START_URL}" "{\"query\":\"phase-215 smoke: default agent\",\"agent_id\":\"${DEFAULT_AGENT}\"}")"
DEF_TASK="$(printf '%s' "${DEF_RESP}" | jq -r '.task_id // empty')"
if [ -n "${DEF_TASK}" ]; then
    DEF_AGENT="$(task_agent_id "${DEF_TASK}")"
    if [ "${DEF_AGENT}" = "${DEFAULT_AGENT}" ]; then
        ok "phase 215: check (i) — the configured default id is accepted with no revision and persists on the task"
    else
        fail "phase 215: a run naming ${DEFAULT_AGENT} read back agent_id=${DEF_AGENT}"
    fi
else
    fail "phase 215: start naming the configured default was REFUSED: $(printf '%s' "${DEF_RESP}" | head -c 300)"
fi

# --- (3) CHECK (ii): write a revision under a SECOND agent id, then prove
#         tenant-local config existence is selection, NOT authority. The dev
#         bearer reaches only DEFAULT_AGENT, so naming this resolvable second
#         id must now refuse with signed-reach scope_mismatch. ---
REV_PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${SET_REVISION_URL}" 2>/dev/null || true)
case "${REV_PROBE:-000}" in
    404|405|501|000|'')
        skip "phase 215: agent_config.set_revision route not present (${REV_PROBE:-000}) — check (ii) not exercised"
        ;;
    *)
        SET_BODY="$(post_body "${SET_REVISION_URL}" "{\"agent_id\":\"${SECOND_AGENT}\",\"payload\":{\"prompt_layers\":{\"base\":\"phase-215 smoke base layer\"}}}")"
        REV_ID="$(printf '%s' "${SET_BODY}" | jq -r '.revision.revision_id // empty')"
        if [ -z "${REV_ID}" ]; then
            fail "phase 215: could not pin a revision for ${SECOND_AGENT}: $(printf '%s' "${SET_BODY}" | head -c 300)"
        else
            SEC_CODE="$(post_code "${START_URL}" "{\"query\":\"phase-215 smoke: second agent\",\"agent_id\":\"${SECOND_AGENT}\"}")"
            if [ "${SEC_CODE}" = "403" ]; then
                ok "phase 215/232: check (ii) — a config revision makes ${SECOND_AGENT} selectable but does not grant signed bearer authority"
            else
                fail "phase 215/232: start naming configured-but-out-of-reach ${SECOND_AGENT} returned ${SEC_CODE}, want 403"
            fi
        fi
        ;;
esac

# --- (4) The stock bearer cannot reach an UNKNOWN agent_id, so it is refused
#         with 403 before tenant-local selection lookup, and no task is
#         created. Phase 232's recording-resolver test separately proves that
#         an allowed bearer sees unknown and foreign targets as the same 400
#         invalid_request selection refusal. The task-count check is the
#         load-bearing half: a status-code-only assertion would not catch a
#         refusal that happened after Spawn.
#
#         The block runs in its OWN session so the count is not perturbed
#         by the runs steps 1-3 started (or by any child task one of them
#         might spawn while this block runs). A fresh session starts at 0
#         and must still be 0 after two refusals. ---
ISOLATED_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: phase215-refusal-session")
isolated_post() {
    curl -sS -X POST "$1" -H "Authorization: Bearer ${TOKEN}" \
        "${ISOLATED_HEADERS[@]}" -H 'Content-Type: application/json' -d "$2" 2>/dev/null || true
}
#         COUNTING HONESTLY. This counter used to end `... || echo 0` over a
#         `jq '... add // 0'`, so a 401, a 404 and a malformed body ALL
#         yielded "0" — indistinguishable from a genuine empty session. The
#         load-bearing half of this check therefore passed with `tasks.list`
#         entirely dead. The wave-v1.24 checkpoint audit split the two: the
#         probe now asserts `tasks.list` answered 200 with an `aggregates`
#         object BEFORE any count is compared, and a count that cannot be
#         read is an empty string that fails the comparison loudly.
#         The function echoes `<status> <count>` on ONE line rather than
#         setting a global: it is called through `$( )`, which runs it in a
#         subshell, so a global assigned inside it never reaches the caller.
count_isolated_tasks() {
    local out status count
    out="$(mktemp)"
    status=$(curl -sS -o "${out}" -w '%{http_code}' --max-time 10 \
        -X POST "${TASK_LIST_URL}" -H "Authorization: Bearer ${TOKEN}" \
        "${ISOLATED_HEADERS[@]}" -H 'Content-Type: application/json' -d '{}' \
        2>/dev/null || true)
    status="${status:-000}"
    count=''
    # An `aggregates` OBJECT is the proof the body is a real TaskListResponse;
    # `// 0` on a missing key is exactly the laundering this replaces.
    if [ "${status}" = "200" ] && jq -e 'has("aggregates") and (.aggregates | type == "object")' "${out}" >/dev/null 2>&1; then
        count="$(jq -r '[.aggregates | to_entries[] | .value] | add // 0' "${out}" 2>/dev/null || printf '')"
    fi
    rm -f "${out}"
    printf '%s %s' "${status}" "${count}"
}
read -r BEFORE_STATUS BEFORE_COUNT <<< "$(count_isolated_tasks)"
if [ "${BEFORE_STATUS}" = "200" ] && [ -n "${BEFORE_COUNT}" ]; then
    ok "phase 215: tasks.list answers 200 with an aggregates object in the isolated session (the count below is a real reading)"
else
    fail "phase 215: tasks.list returned ${BEFORE_STATUS} / unreadable aggregates in the isolated session — the no-task-on-refusal check below cannot run against a dead read"
fi
UNKNOWN_BODY="$(isolated_post "${START_URL}" '{"query":"phase-215 smoke: unknown agent","agent_id":"phase215-no-such-agent"}')"
UNKNOWN_CODE="$(curl -s -o /dev/null -w '%{http_code}' -X POST "${START_URL}" \
    -H "Authorization: Bearer ${TOKEN}" "${ISOLATED_HEADERS[@]}" \
    -H 'Content-Type: application/json' \
    -d '{"query":"phase-215 smoke: unknown agent 2","agent_id":"phase215-no-such-agent"}' 2>/dev/null || true)"
read -r AFTER_STATUS AFTER_COUNT <<< "$(count_isolated_tasks)"
if [ "${UNKNOWN_CODE}" = "403" ]; then
    ok "phase 215/232: an unknown out-of-reach agent_id is refused with 403 before tenant-local selection"
else
    fail "phase 215/232: an unknown out-of-reach agent_id returned ${UNKNOWN_CODE}, want 403"
fi
if [ "${BEFORE_STATUS}" != "200" ] || [ "${AFTER_STATUS}" != "200" ] || [ -z "${BEFORE_COUNT}" ] || [ -z "${AFTER_COUNT}" ]; then
    fail "phase 215: task count unreadable (tasks.list ${BEFORE_STATUS} → ${AFTER_STATUS}) — a refused start MUST NOT create a task, and this guard cannot say whether it did"
elif [ "${BEFORE_COUNT}" = "${AFTER_COUNT}" ] && [ "${AFTER_COUNT}" = "0" ]; then
    ok "phase 215: two refused starts created NO task in a fresh session (count stayed 0) — refused before the task exists"
else
    fail "phase 215: isolated-session task count ${BEFORE_COUNT} → ${AFTER_COUNT}, want 0 → 0: a refused start MUST NOT create a task"
fi

# --- (5) NON-ORACLE (the mechanical half): the refusal text is
#         INDEPENDENT of the rejected id, so a caller cannot learn
#         anything about an id from the way it was refused.
#
#         The cross-TENANT half of this property (an id registered under
#         another tenant refuses identically to one that never existed)
#         cannot be driven from here: the dev token pins the caller's
#         tenant, so a tenant switch is refused by the identity gate
#         BEFORE the agent check runs — which is itself correct. It is
#         asserted end-to-end with real multi-tenant identities in
#         internal/protocol/control_agent_test.go and
#         test/integration/agent_selection_test.go. What IS mechanically
#         checkable here is the property those tests rest on: one refusal
#         constant, naming neither the id nor the reason. Embedding the id
#         in the refusal turns this OK into a FAIL. ---
msg_of() { printf '%s' "$1" | jq -r '.error.message // .message // empty'; }
OTHER_UNKNOWN_BODY="$(post_body "${START_URL}" '{"query":"phase-215 smoke: another unknown agent","agent_id":"phase215-a-completely-different-unknown-agent"}')"
UNKNOWN_MSG="$(msg_of "${UNKNOWN_BODY}")"
OTHER_MSG="$(msg_of "${OTHER_UNKNOWN_BODY}")"
if [ -z "${UNKNOWN_MSG}" ] || [ -z "${OTHER_MSG}" ]; then
    fail "phase 215: a refusal body carried no readable message field (unknown=[${UNKNOWN_MSG}] other=[${OTHER_MSG}])"
elif [ "${UNKNOWN_MSG}" = "${OTHER_MSG}" ]; then
    ok "phase 215: two DIFFERENT unresolvable agent ids produce a byte-identical refusal — the text leaks nothing about the id"
else
    fail "phase 215: the refusal text varies with the rejected id (existence-oracle shape) — a=[${UNKNOWN_MSG}] b=[${OTHER_MSG}]"
fi

smoke_summary
