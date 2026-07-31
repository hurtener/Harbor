#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 219 — the memory tiers reach the Protocol run surface (D-364).
#
# `StartRequest.caller_memory` admits caller-supplied content into the run's
# `<read_only_external_memory>` tier under the FIXED runtime-owned
# `caller_supplied` map key. It COMPOSES with the runtime's own recall key
# (`recalled_turns`), it never reaches the trusted system-prompt spine, and it
# never writes the Conversation tier. It is bounded at the Protocol edge
# BEFORE a task exists, and announced by `memory.caller_block_admitted`, which
# carries a SIZE and never content.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# GUARD DISCIPLINE — every ASSERTION below FAILS on breakage. The only two
# skipping arms are the harness preamble (§4.2 item 4): the route probe and the
# `jq` availability check, both of which run BEFORE any assertion and describe
# the harness, not the surface. No assertion in this file can answer SKIP.
#
# An UNRESOLVABLE dev bearer is a FAIL, not a third skip: see the note at the
# `dev_bearer` call below (issue #624).
#
# Counted rather than asserted, so a future edit has a number to reconcile
# against: 18 literal `ok` arms, 24 literal `fail` arms, 2 literal `skip` arms,
# plus two legs delegated to `assert_go_tests_pass` (which emits its own
# ok/fail). Observed counters on the shipping build: OK 20 / SKIP 0 / FAIL 0.
# The header previously claimed "19 ok / 24 fail / 2 skip" against an actual
# 20/25/2 — a stale count in a file whose whole subject is instruments that
# report what they did not measure.
#
# ONE residual, named rather than hidden: if `/v1/control/start` itself
# vanished, the route probe would skip the whole live section and this script
# would still report OK 8 (the static guards) — above the inert-smoke gate's
# OK-0 threshold. That is the §4.2 item 4 convention working as designed and
# is not this phase's to change; a missing `start` route fails a great many
# other smokes loudly first.
#
# The skeleton for this file carried the first grep below as a PHASE GATE with
# a SKIP arm ("not yet shipped"). That arm is deleted. This phase HAS shipped,
# so the field's absence is a REGRESSION, not a forward-compatibility case —
# and the SKIP arm made deleting `StartRequest.CallerMemory`, the single most
# obvious way to break this feature, produce `OK 0 / SKIP 1 / FAIL 0` and an
# exit 0. That is §4.2 item 5 inverted, and it is the exact shape the
# wave-v1.24 checkpoint audit rewrote out of `scripts/smoke/phase-215.sh`.
#
# The grep is still load-bearing, for a reason worth stating because it is the
# OPPOSITE of the intuitive one. `decodeRequest` unmarshals a `start` body with
# plain `json.Unmarshal` and NO `DisallowUnknownFields`
# (internal/protocol/transports/control/control.go), so a build WITHOUT the
# field answers 200 to a request carrying one and silently IGNORES it. Every
# admission assertion below would go green against a runtime where the feature
# does not exist — 200 with a task_id is exactly what an inert runtime returns.
# The static grep is what makes that state distinguishable, so it must fail
# loudly rather than excuse itself.
#
# Each guard below was verified by mutation: breaking the thing it protects
# turns its OK into a FAIL, never into a SKIP.
#
# Done-definition: OK >= 14, FAIL = 0. Observed on the shipping build: 20/0/0.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ---------------------------------------------------------------------------
# (S1) The wire field itself. FAILS on absence — this phase shipped it, so
#      there is no pre-219 build to be forward-compatible with, and the live
#      assertions below cannot tell an inert runtime from a working one on
#      their own (see the header).
# ---------------------------------------------------------------------------
if grep -q 'json:"caller_memory,omitempty"' internal/protocol/types/control.go 2>/dev/null; then
    ok "phase 219 static: StartRequest carries the caller_memory wire field"
else
    fail "phase 219 static: StartRequest has no caller_memory wire field in internal/protocol/types/control.go — this phase shipped it, so its absence is a regression; the runtime would answer 200 and silently DROP every caller-supplied memory block (decodeRequest does not reject unknown fields)"
fi

# ---------------------------------------------------------------------------
# Static trip-wires (run regardless of the live server). Every branch below
# FAILS on breakage.
# ---------------------------------------------------------------------------

# (S2) D-223 lockstep: the REGENERATED manifest knows the field. A hand-mirror
#      without `make protocol-ts-gen` fails here.
if grep -A 80 '"StartRequest"' web/console/src/lib/protocol/wire-manifest.gen.json 2>/dev/null \
        | grep -q '"key": "caller_memory"'; then
    ok "phase 219 static: caller_memory is in the REGENERATED wire manifest for StartRequest"
else
    fail "phase 219 static: caller_memory absent from StartRequest in wire-manifest.gen.json — regenerate with 'make protocol-ts-gen' (D-223 lockstep)"
fi

# (S3) The composition key is runtime-owned and is written by EXACTLY ONE
#      producer. The runtime's own recall path must never write it: if it did,
#      the two producers would be indistinguishable in the prompt and the
#      provenance property this phase exists for would be silently dead.
#      The pattern is ANCHORED on the whole declaration, name AND value. The
#      first draft grepped the bare substring `CallerSuppliedKey`, which a
#      rename to `CallerSuppliedKeyRENAMED` still satisfies — mutation-verified
#      as an inert guard and rewritten. Both halves matter: the exported name is
#      what the run loop and the admission event read, and the literal is the
#      key a Console trace reader greps for.
if grep -qE '^const CallerSuppliedKey = "caller_supplied"$' internal/runtime/runctx/caller_memory.go 2>/dev/null; then
    ok "phase 219 static: runctx.CallerSuppliedKey is declared in the composition home, with its documented value"
else
    fail "phase 219 static: internal/runtime/runctx/caller_memory.go does not declare 'const CallerSuppliedKey = \"caller_supplied\"' — the fixed External-tier key is the whole collision-freedom argument, and both its NAME and its VALUE are load-bearing"
fi
if grep -q 'caller_supplied' internal/runtime/runctx/memory_fetch.go 2>/dev/null; then
    fail "phase 219 static: memory_fetch.go writes the caller_supplied key — the runtime's recall producer MUST NOT write the caller's key (two indistinguishable producers on one map key)"
else
    ok "phase 219 static: the runtime recall producer does NOT write caller_supplied (provenance stays separable)"
fi

# (S4) ORDERING GUARD. The composition call site must run AFTER the run loop's
#      identity-stamping emitter exists, or the admission event is emitted
#      through a nil emitter and silently vanishes — a §13 silent degradation
#      that no unit test sees, because the unit tests inject their own emitter.
#      Cheap and mechanical, so it fails preflight rather than review.
# `|| true` on both: `set -o pipefail` makes a no-match grep abort the whole
# script at the assignment, which would turn a REAL regression (the call site
# deleted) into a silent early exit instead of the FAIL below.
EMIT_LINE="$(grep -n 'emit := events.IdentityStampingEmitterContext' internal/runtime/serve/runloop.go 2>/dev/null | head -1 | cut -d: -f1 || true)"
COMPOSE_LINE="$(grep -n 'runctx.ComposeCallerMemory(' internal/runtime/serve/runloop.go 2>/dev/null | head -1 | cut -d: -f1 || true)"
if [ -z "${EMIT_LINE}" ] || [ -z "${COMPOSE_LINE}" ]; then
    fail "phase 219 static: could not locate runloop.go's emitter construction (${EMIT_LINE:-none}) and/or the ComposeCallerMemory call site (${COMPOSE_LINE:-none}) — both must exist"
elif [ "${COMPOSE_LINE}" -gt "${EMIT_LINE}" ]; then
    ok "phase 219 static: ComposeCallerMemory (line ${COMPOSE_LINE}) runs AFTER the emitter exists (line ${EMIT_LINE})"
else
    fail "phase 219 static: ComposeCallerMemory (line ${COMPOSE_LINE}) runs BEFORE the emitter is built (line ${EMIT_LINE}) — the admission event would be emitted through a nil emitter and silently vanish"
fi

# (S5) §17.6 fix, pinned so it cannot regress. `findContextLeak` byte-exempts
#      non-RoleTool text (internal/llm/safety.go), and memory tiers render as
#      RoleSystem — so ErrContextLeak has never backstopped a memory tier. The
#      old comment in memory_fetch.go claimed it did. A future author who
#      re-adds that claim would rebuild this phase's edge bound on a guard that
#      does not exist.
if grep -q 'ErrContextLeak' internal/runtime/runctx/memory_fetch.go 2>/dev/null; then
    fail "phase 219 static: memory_fetch.go claims ErrContextLeak backstops a memory tier — findContextLeak byte-exempts non-RoleTool text (internal/llm/safety.go) and memory tiers render as RoleSystem, so that guard has never covered this path"
else
    ok "phase 219 static: memory_fetch.go no longer claims ErrContextLeak backstops a memory tier (§17.6 fix pinned)"
fi
if grep -q 'offloadableText := m.Role == RoleTool' internal/llm/safety.go 2>/dev/null; then
    ok "phase 219 static: the leak guard's RoleTool byte-exemption is where this phase's bound reasoning assumes it is"
else
    fail "phase 219 static: internal/llm/safety.go no longer byte-exempts non-RoleTool text — the Protocol-edge bound's justification changed; re-derive it before editing this guard"
fi

# (S6) CAP-ORDERING GUARD. maxCallerMemoryBytes MUST stay strictly below the
#      control transport's whole-body cap. Both refuse with the SAME
#      CodeInvalidRequest / 400, so if the field cap ever rises to meet the
#      envelope cap the field check becomes UNREACHABLE — dead code that every
#      status-code test keeps passing against, because the transport answers
#      first with an identical code. (`maxOutputSchemaBytes` at
#      internal/protocol/control.go:274 is already in that state at 64 KiB;
#      this guard is what stops this phase joining it.)
CALLER_CAP="$(grep -oE 'maxCallerMemoryBytes = [0-9]+ \* 1024' internal/protocol/control.go 2>/dev/null | grep -oE '^maxCallerMemoryBytes = [0-9]+' | grep -oE '[0-9]+$' || true)"
ENVELOPE_CAP_KIB="$(grep -oE 'maxBodyBytes = [0-9]+ << 10' internal/protocol/transports/control/control.go 2>/dev/null | grep -oE '[0-9]+' | head -1 || true)"
if [ -z "${CALLER_CAP}" ] || [ -z "${ENVELOPE_CAP_KIB}" ]; then
    fail "phase 219 static: could not read maxCallerMemoryBytes (${CALLER_CAP:-none} KiB) and/or the control transport's maxBodyBytes (${ENVELOPE_CAP_KIB:-none} KiB) — the cap-ordering invariant cannot be checked, and an unreachable field cap looks identical to a working one"
elif [ "${CALLER_CAP}" -lt "${ENVELOPE_CAP_KIB}" ]; then
    ok "phase 219 static: maxCallerMemoryBytes (${CALLER_CAP} KiB) is strictly below the control transport's body cap (${ENVELOPE_CAP_KIB} KiB) — the field check is reachable"
else
    fail "phase 219 static: maxCallerMemoryBytes (${CALLER_CAP} KiB) is NOT below the transport body cap (${ENVELOPE_CAP_KIB} KiB) — the transport answers first with the SAME 400, so the field's own cap is unreachable dead code"
fi

# ---------------------------------------------------------------------------
# Live-server assertions.
# ---------------------------------------------------------------------------
START_URL="$(api_url /v1/control/start)"
EVENTS_LIST_URL="$(api_url /v1/events/list)"
TASK_LIST_URL="$(api_url /v1/tasks/list)"

PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${START_URL}" 2>/dev/null || true)
case "${PROBE:-000}" in
    404|405|501|000|'')
        skip "phase 219: control.start route not present (${PROBE:-000})"
        smoke_summary
        exit 0
        ;;
esac

# THE BEARER COMES FROM common.sh's `dev_bearer`, NEVER from a raw
# `${HARBOR_DEV_TOKEN}` read (issue #624, which this script was the reported
# instance of). A raw read resolves to empty outside preflight — where a
# contributor iterating locally runs this — so nine live assertions collapsed
# into one SKIP and the script still exited 0: `OK 8 / SKIP 1 / FAIL 0`
# standalone versus 20/0/0 under preflight. That is §4.2 item 5, and it makes
# the script's honesty a function of how it is invoked. `dev_bearer` falls back
# to `${HARBOR_DATA_DIR}/server.log`, which a hand-booted `harbor dev` has.
#
# An UNRESOLVABLE bearer is a FAIL, not a SKIP: the route probe above already
# proved a server is answering on this port, so a server we can reach but
# cannot authenticate against is a broken harness, and a broken harness that
# reports green is the whole defect class this wave is draining.
TOKEN="$(dev_bearer)"
if ! command -v jq >/dev/null 2>&1; then
    skip 'phase 219: jq not available — live assertions skipped'
    smoke_summary
    exit 0
fi
if [ -z "${TOKEN}" ]; then
    fail 'phase 219: no dev bearer resolved (HARBOR_DEV_TOKEN unset and no ${HARBOR_DATA_DIR}/server.log) — control.start answered on this port, so the live legs SHOULD run; they would prove nothing without a bearer'
    smoke_summary || true
    exit 1
fi
# A distinctive, greppable marker. It is the payload's ONLY content, so any
# appearance of it outside the prompt is a leak we can name precisely.
MARKER="phase219-marker-9f3c1a-do-not-log"
ADMIT_SESSION="phase219-admit-session"
REFUSE_SESSION="phase219-refusal-session"

post_in_session() {
    # post_in_session <session> <url> <body> — echoes the response body.
    curl -sS -X POST "$2" -H "Authorization: Bearer ${TOKEN}" \
        -H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: $1" \
        -H 'Content-Type: application/json' -d "$3" 2>/dev/null || true
}

code_in_session() {
    # code_in_session <session> <url> <body> — echoes the HTTP status only.
    curl -s -o /dev/null -w '%{http_code}' -X POST "$2" \
        -H "Authorization: Bearer ${TOKEN}" \
        -H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: $1" \
        -H 'Content-Type: application/json' -d "$3" 2>/dev/null || true
}

# --- (1) A `start` carrying a valid caller_memory is ACCEPTED. ---
ADMIT_BODY="$(jq -nc --arg m "${MARKER}" \
    '{query:"phase-219 smoke: caller memory", caller_memory:{note:$m}}')"
ADMIT_RESP="$(post_in_session "${ADMIT_SESSION}" "${START_URL}" "${ADMIT_BODY}")"
ADMIT_TASK="$(printf '%s' "${ADMIT_RESP}" | jq -r '.task_id // empty')"
if [ -n "${ADMIT_TASK}" ]; then
    ok "phase 219: start with caller_memory succeeds (task_id=${ADMIT_TASK})"
else
    fail "phase 219: start with caller_memory failed: $(printf '%s' "${ADMIT_RESP}" | head -c 300)"
fi

# --- (2) The admission is OBSERVABLE as a canonical event.
#         Admission happens before planning, so this event fires whether or not
#         the run's LLM call subsequently succeeds — no run-outcome dependency.
#         Bounded poll with an explicit FAIL on timeout: a timeout that skipped
#         would be an inert guard. ---
EVENTS_RAW=''
ADMIT_EVENT=''
for _ in $(seq 1 30); do
    EVENTS_RAW="$(post_in_session "${ADMIT_SESSION}" "${EVENTS_LIST_URL}" '{"limit":200}')"
    ADMIT_EVENT="$(printf '%s' "${EVENTS_RAW}" \
        | jq -c '[.events[]? | select(.type == "memory.caller_block_admitted")] | first // empty' 2>/dev/null || printf '')"
    [ -n "${ADMIT_EVENT}" ] && break
    sleep 0.5
done
if [ -n "${ADMIT_EVENT}" ]; then
    ok "phase 219: memory.caller_block_admitted was emitted for the admitting run"
    ADMIT_BYTES="$(printf '%s' "${ADMIT_EVENT}" | jq -r '.payload.bytes // empty')"
    if [ -n "${ADMIT_BYTES}" ] && [ "${ADMIT_BYTES}" -gt 0 ] 2>/dev/null; then
        ok "phase 219: the admission event reports a positive byte count (bytes=${ADMIT_BYTES})"
    else
        fail "phase 219: the admission event carries no positive 'bytes' field (got [${ADMIT_BYTES}]) — a provenance signal that does not say how much was admitted is not one"
    fi
else
    fail "phase 219: memory.caller_block_admitted never appeared within the poll budget — caller-supplied memory must be observable over the Protocol (RFC §5.2, D-062)"
fi

# --- (3) The event reports a SIZE and never CONTENT (CLAUDE.md §7 rules 6-7).
#         Checked against the WHOLE events response, not just the one event, so
#         a leak into a sibling event or an envelope field is caught too. ---
if [ -z "${EVENTS_RAW}" ]; then
    fail "phase 219: events.list returned nothing — the content-leak assertion below cannot run against a dead read"
elif printf '%s' "${EVENTS_RAW}" | grep -q "${MARKER}"; then
    fail "phase 219: the caller_memory marker appears in the events stream — the admission event must carry a size, never content"
else
    ok "phase 219: the caller's content appears NOWHERE in the events stream (size-only provenance)"
fi

# --- (4) An over-cap payload is refused AND no task is created. The task-count
#         half is the load-bearing one: a status-code-only assertion would not
#         catch a refusal that happened after Spawn. Run in its OWN session so
#         the count is not perturbed by the admitting run above. ---
count_refusal_session_tasks() {
    # Echoes "<status> <count>". An unreadable body yields an EMPTY count
    # rather than a laundered 0 — the phase-215 counting-honestly fix.
    local out status count
    out="$(mktemp)"
    status=$(curl -sS -o "${out}" -w '%{http_code}' --max-time 10 \
        -X POST "${TASK_LIST_URL}" -H "Authorization: Bearer ${TOKEN}" \
        -H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: ${REFUSE_SESSION}" \
        -H 'Content-Type: application/json' -d '{}' 2>/dev/null || true)
    status="${status:-000}"
    count=''
    if [ "${status}" = "200" ] && jq -e 'has("aggregates") and (.aggregates | type == "object")' "${out}" >/dev/null 2>&1; then
        count="$(jq -r '[.aggregates | to_entries[] | .value] | add // 0' "${out}" 2>/dev/null || printf '')"
    fi
    rm -f "${out}"
    printf '%s %s' "${status}" "${count}"
}

read -r BEFORE_STATUS BEFORE_COUNT <<< "$(count_refusal_session_tasks)"
if [ "${BEFORE_STATUS}" = "200" ] && [ -n "${BEFORE_COUNT}" ]; then
    ok "phase 219: tasks.list answers 200 with an aggregates object in the refusal session (the count below is a real reading)"
else
    fail "phase 219: tasks.list returned ${BEFORE_STATUS} / unreadable aggregates in the refusal session — the no-task-on-refusal check below cannot run against a dead read"
fi

# SIZING IS LOAD-BEARING, AND THE STATUS CODE ALONE PROVES NOTHING.
#
# The control transport caps the WHOLE body at 64 KiB
# (`maxBodyBytes`, internal/protocol/transports/control/control.go:75, applied
# at :456) and answers a MaxBytesReader overflow with the SAME
# CodeInvalidRequest / 400 this phase's edge check answers (:456-464). A 64 KiB
# payload would therefore be refused 400 by the TRANSPORT with the
# `caller_memory` check entirely absent, and a status-only assertion would show
# OK for a guard that does not exist — the inert-guard shape §4.2 item 5 names.
#
# Two things resolve it: the payload is sized to land strictly BETWEEN the
# 32 KiB caller-memory cap and the 64 KiB envelope cap, so it REACHES the
# handler; and the refusal MESSAGE must name the field, which the transport's
# "request body could not be read" never does.
OVERSIZE="$(head -c 40960 /dev/zero | tr '\0' 'x')"
OVER_BODY="$(jq -nc --arg m "${OVERSIZE}" \
    '{query:"phase-219 smoke: over cap", caller_memory:{blob:$m}}')"
OVER_RESP="$(post_in_session "${REFUSE_SESSION}" "${START_URL}" "${OVER_BODY}")"
OVER_CODE="$(code_in_session "${REFUSE_SESSION}" "${START_URL}" "${OVER_BODY}")"
OVER_MSG="$(printf '%s' "${OVER_RESP}" | jq -r '.error.message // .message // empty' 2>/dev/null || printf '')"
if [ "${OVER_CODE}" != "400" ]; then
    fail "phase 219: a 40 KiB caller_memory returned ${OVER_CODE}, want 400 — the Protocol-edge bound is the ONLY bound (the LLM-edge leak guard byte-exempts system-role text, internal/llm/safety.go:360)"
elif printf '%s' "${OVER_MSG}" | grep -q 'caller_memory'; then
    ok "phase 219: an over-cap caller_memory is refused 400 by the FIELD check (the refusal names caller_memory: ${OVER_MSG})"
else
    fail "phase 219: the 400 refusal does not name caller_memory (got [${OVER_MSG}]) — this is the transport's 64 KiB envelope overflow answering, not the field's own cap; the edge check may be absent entirely"
fi

# --- (5) An explicit null and a malformed document are REFUSED, not treated as
#         absent. A silent no-op here is the §13 shape: the caller believes its
#         memory reached the model and it did not. ---
NULL_CODE="$(code_in_session "${REFUSE_SESSION}" "${START_URL}" \
    '{"query":"phase-219 smoke: explicit null","caller_memory":null}')"
if [ "${NULL_CODE}" = "400" ]; then
    ok "phase 219: an explicit caller_memory:null is refused (never silently treated as absent)"
else
    fail "phase 219: caller_memory:null returned ${NULL_CODE}, want 400 — silently treating it as absent is a §13 silent degradation"
fi
BAD_CODE="$(code_in_session "${REFUSE_SESSION}" "${START_URL}" \
    '{"query":"phase-219 smoke: malformed","caller_memory":{"unterminated":}}')"
if [ "${BAD_CODE}" = "400" ]; then
    ok "phase 219: a malformed caller_memory document is refused with 400"
else
    fail "phase 219: a malformed caller_memory document returned ${BAD_CODE}, want 400"
fi

read -r AFTER_STATUS AFTER_COUNT <<< "$(count_refusal_session_tasks)"
if [ "${BEFORE_STATUS}" != "200" ] || [ "${AFTER_STATUS}" != "200" ] || [ -z "${BEFORE_COUNT}" ] || [ -z "${AFTER_COUNT}" ]; then
    fail "phase 219: task count unreadable (tasks.list ${BEFORE_STATUS} → ${AFTER_STATUS}) — a refused start MUST NOT create a task, and this guard cannot say whether it did"
elif [ "${BEFORE_COUNT}" = "${AFTER_COUNT}" ] && [ "${AFTER_COUNT}" = "0" ]; then
    ok "phase 219: three refused starts created NO task in a fresh session (count stayed 0) — refused before the task exists"
else
    fail "phase 219: refusal-session task count ${BEFORE_COUNT} → ${AFTER_COUNT}, want 0 → 0: a refused start MUST NOT create a task"
fi

# ---------------------------------------------------------------------------
# Race-detector gates on the suites that carry the invariants a live HTTP
# assertion cannot reach: the composition matrix, the no-aliasing property, the
# D-025 N=128 run, the byte-for-byte wrapper regression, and the end-to-end
# proof that the marker reached the External tier and NOTHING else.
# ---------------------------------------------------------------------------
if go test -race -count=1 \
        ./internal/protocol/ \
        ./internal/runtime/runctx/ \
        ./internal/planner/react/ >/dev/null 2>&1; then
    ok "phase 219: go test -race passes for the edge, composition and wrapper-regression suites"
else
    fail "phase 219: go test -race FAILED for internal/protocol, internal/runtime/runctx or internal/planner/react"
fi
# THE TWO FILTERED LEGS NAME THEIR TESTS EXPLICITLY, through common.sh's
# `assert_go_tests_pass`. They used to be exit-code-only `go test -run` guards,
# and `go test -run NoSuchTest` prints `ok <pkg> [no tests to run]` and exits
# ZERO — so both reported OK once the tests they name were renamed away. The
# unfiltered whole-package run above is not affected: with no `-run` filter
# there is no filter to go stale. (AGENTS.md §4.2 item 5.)
#
# `-race -count=1` rides in the go-test-args parameter, so these legs keep the
# race detector they had.
GOLOG_219="$(mktemp "${TMPDIR:-/tmp}/phase219-gotest.XXXXXX")"
trap 'rm -f "${GOLOG_219}"' EXIT

assert_go_tests_pass "${GOLOG_219}" '-race -count=1 ./test/integration/' \
    'phase 219: the caller-memory integration suite (real drivers, recording LLM edge)' \
    TestE2E_CallerMemory_ReachesTheExternalTierAndNothingElse \
    TestE2E_CallerMemory_ComposesWithSemanticRecall \
    TestE2E_CallerMemory_OverCapRefusedAndNoTaskCreated \
    TestE2E_CallerMemory_UnauthenticatedRefusedBeforeTheBody \
    TestE2E_CallerMemory_AdmissionEventFiresWhenTheRunFails \
    TestE2E_CallerMemory_ConcurrentAcrossTenantsOverTheWire

# The run loop's OWN refusal branch and its admission emit. Reachable only
# from an in-process caller (the Protocol edge refuses an inadmissible payload
# before a task exists), so nothing above this line exercises it.
assert_go_tests_pass "${GOLOG_219}" '-race -count=1 ./internal/runtime/serve/' \
    'phase 219: the run-loop composition + admission-emit suite' \
    TestRunOne_CallerMemoryCompositionError_FailsRunLoud \
    TestRunOne_CallerMemory_AdmittedAndAnnounced \
    TestSpawn_MalformedCallerMemory_RefusedAtPersist

smoke_summary
