#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 220 — `extra_instructions` on `RunOverrides` (D-365).
#
# The phase makes the ALREADY-additive `planner.LLMOverrides.ExtraInstructions`
# reachable over the Protocol as one optional field on `RunOverrides`, and pins
# the two-producer join: the run-level value COMPOSES BELOW the tenant-level
# value and can never clear it.
#
# Asserts:
#   - static: the wire field exists (this grep is the PHASE GATE the live legs
#     branch on — see the 400-vs-SKIP note below);
#   - static: the field reaches PendingOverride AND the session arm of
#     ComposeLLMOverrides — a wire field that never reaches the composition is
#     an inert field, which is exactly the shape this guard exists to catch;
#   - static: the composition JOINS rather than assigns (the precedence
#     decision, mechanically);
#   - static: the audit flag is declared and set, and the VALUE is not emitted;
#   - static: the godoc carries the trust statement (verbatim +
#     <additional_guidance>) — a field shipped without it is the documentation
#     gap the phase exists to close;
#   - static: the Console hand-mirror (runs.ts + ALLOWED_OVERRIDE_KEYS) is in
#     lockstep — D-223's obligation, checked at preflight time and not only at
#     `npm run lint`;
#   - live: the field is accepted at the wire door; it composes with
#     system_prompt_override in the same request; and it opens NO new door
#     (missing identity still 401, cross-session still refused);
#   - unit-tests: the join table, the no-clear property, the verbatim render,
#     the survives-a-replace property, the concurrent-reuse run and the
#     two-producer integration seam — all under -race.
#
# WHY A LIVE 400 IS A FAIL, NOT A SKIP. The runtime decodes the override body
# with DisallowUnknownFields(), so a build WITHOUT the field answers 400 to a
# request carrying it — indistinguishable from a validation refusal. The static
# grep (guard 1) resolves the ambiguity: with the field absent from
# runs.go the live legs SKIP; with it present, a 400 means the wire type and
# the handler disagree, and that is a FAIL. This is the phase-gating device
# that keeps the smoke honest on both older and current builds without ever
# letting a real regression read as a SKIP (§4.2 item 5).
#
# Every non-probe assertion FAILS (never SKIPs) when its guard is removed.
#
# The session id is the dev identity's own session, so the script is
# idempotent against a long-lived preflight server.
#
# Done-definition: OK >= 10, FAIL = 0.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

RUNS_TYPES_GO='internal/protocol/types/runs.go'
OVERRIDES_GO='internal/runtime/runs/protocol/overrides.go'
EVENTS_GO='internal/events/events.go'
RUNS_TS='web/console/src/lib/protocol/runs.ts'
RUNS_TS_TEST='web/console/src/lib/protocol/tests/runs-set-overrides.test.ts'

# assert_grep <file> <extended-regexp> <desc>
#
# OK when the pattern matches, FAIL when it does not. Deliberately NOT a skip:
# the 404/405/501 -> SKIP convention is for a forward-phase script running
# against an older build's HTTP surface, not for source guards. Patterns use
# POSIX classes ([[:space:]]) — never \t / \d, which BSD grep matches and GNU
# grep does not.
assert_grep() {
    local file="$1" pattern="$2" desc="$3"
    if [ ! -f "${file}" ]; then
        fail "${desc}: ${file} does not exist"
        return
    fi
    if grep -qE "${pattern}" "${file}"; then
        ok "${desc}"
    else
        fail "${desc}: no match for /${pattern}/ in ${file}"
    fi
}

# run_filtered_tests <desc> <run-regexp> <packages...>
#
# Runs `go test -race -run <regexp>`. OK on a real pass; SKIP only when the
# filter matched no tests at all (an older build); FAIL on a genuine failure.
run_filtered_tests() {
    local desc="$1" runre="$2"
    shift 2
    local out rc
    # NO CGO_ENABLED=0 here: the race detector needs cgo on Linux, where
    # `CGO_ENABLED=0 go test -race` fails to build. Harbor's CGo ban governs
    # the shipped BINARY, not the race-instrumented test binary.
    out="$(go test -race -count=1 -run "${runre}" "$@" 2>&1)" && rc=0 || rc=$?
    if [ "${rc}" -eq 0 ]; then
        if printf '%s\n' "${out}" | grep -qE 'no tests to run|no test files'; then
            skip "${desc}: filter '${runre}' matched no tests (phase not yet landed)"
        else
            ok "${desc}"
        fi
        return
    fi
    printf '%s\n' "${out}" | tail -25
    fail "${desc}: go test exited ${rc}"
}

# ----------------------------------------------------------------------------
# 1. The phase gate: is the wire field declared?
# ----------------------------------------------------------------------------

FIELD_PRESENT=0
if [ -f "${RUNS_TYPES_GO}" ] && grep -qE 'ExtraInstructions[[:space:]]+\*string[[:space:]]+`json:"extra_instructions' "${RUNS_TYPES_GO}"; then
    FIELD_PRESENT=1
    ok 'phase 220: RunOverrides declares the optional extra_instructions wire field'
else
    skip 'phase 220: RunOverrides has no extra_instructions field yet — the phase has not landed; every downstream guard skips with it'
fi

if [ "${FIELD_PRESENT}" = "0" ]; then
    smoke_summary
    exit 0
fi

# ----------------------------------------------------------------------------
# 2. Static guards — the field must REACH the composition, and the composition
#    must JOIN rather than assign.
# ----------------------------------------------------------------------------

assert_grep "${OVERRIDES_GO}" \
    'ExtraInstructions[[:space:]]+\*string' \
    'phase 220: PendingOverride carries the validated extra-instructions value'

# The session arm of ComposeLLMOverrides must mention the field at all. A wire
# field that never reaches the composition is inert — accepted at the door,
# dropped before the prompt.
if sed -n '/^func ComposeLLMOverrides(/,/^}/p' "${OVERRIDES_GO}" \
     | grep -qE 'session\.ExtraInstructions'; then
    ok 'phase 220: ComposeLLMOverrides reads the session-level extra instructions (the field is not inert)'
else
    fail 'phase 220: ComposeLLMOverrides never reads session.ExtraInstructions — the wire field is accepted and then dropped'
fi

# THE PRECEDENCE GUARD. The join must READ the already-composed tenant value
# inside the session arm. A bare `out.ExtraInstructions = session.ExtraInstructions`
# assignment does not, so mutating the join into an assignment turns this OK
# into a FAIL — which is the whole point: an assignment would hand a non-admin
# session caller a silent delete on an admin-set tenant block.
if sed -n '/^func ComposeLLMOverrides(/,/^}/p' "${OVERRIDES_GO}" \
     | grep -qE 'joinAdditiveGuidance|out\.ExtraInstructions[,)]|\*out\.ExtraInstructions'; then
    ok 'phase 220: the two-producer join READS the tenant value (composition, not last-writer-wins)'
else
    fail 'phase 220: ComposeLLMOverrides assigns the session value over the tenant value — a run-level caller could erase an admin-set block (D-365 chose composition)'
fi

assert_grep "${EVENTS_GO}" \
    'SetExtraInstructions[[:space:]]+bool' \
    'phase 220: RunOverridesSetPayload declares the SetExtraInstructions flag'

assert_grep "${OVERRIDES_GO}" \
    'SetExtraInstructions:' \
    'phase 220: emitAudit sets the SetExtraInstructions flag'

# The VALUE must never ride the bus (CLAUDE.md §7 — the payload's own
# SafePayload rule). The emit must carry a boolean, never the string.
if sed -n '/^func (s \*Service) emitAudit(/,/^}/p' "${OVERRIDES_GO}" \
     | grep -qE 'SetExtraInstructions:[[:space:]]*po\.ExtraInstructions[[:space:]]*!=[[:space:]]*nil'; then
    ok 'phase 220: the audit payload carries only the boolean flag — the caller-supplied text never reaches the bus'
else
    fail 'phase 220: emitAudit does not derive SetExtraInstructions as a nil-check — the override VALUE may be leaking into the payload (CLAUDE.md §7)'
fi

# The trust statement. The field renders VERBATIM into an operator-TRUSTED
# position; a field that ships without saying so is the documentation gap this
# phase exists to close.
if sed -n '/ExtraInstructions, when non-nil/,/ExtraInstructions \*string/p' "${RUNS_TYPES_GO}" \
     | grep -qE 'additional_guidance'; then
    ok 'phase 220: the wire godoc names the <additional_guidance> render position'
else
    fail 'phase 220: the ExtraInstructions godoc does not name <additional_guidance> — the trust statement is missing'
fi

if sed -n '/ExtraInstructions, when non-nil/,/ExtraInstructions \*string/p' "${RUNS_TYPES_GO}" \
     | grep -qE 'VERBATIM|verbatim'; then
    ok 'phase 220: the wire godoc states the text is rendered verbatim (operator-trusted, unescaped)'
else
    fail 'phase 220: the ExtraInstructions godoc does not state the verbatim/trusted property'
fi

# ----------------------------------------------------------------------------
# 3. Console hand-mirror lockstep (D-223 / §4.5 item 5).
# ----------------------------------------------------------------------------

assert_grep "${RUNS_TS}" \
    'extra_instructions\?:[[:space:]]*string' \
    'phase 220: the Console typed client mirrors extra_instructions by hand'

assert_grep "${RUNS_TS_TEST}" \
    "'extra_instructions'" \
    'phase 220: ALLOWED_OVERRIDE_KEYS admits extra_instructions (the strict-decoder key set)'

# ----------------------------------------------------------------------------
# 4. Live-wire assertions against the preflight-booted dev server.
# ----------------------------------------------------------------------------

SET_OVERRIDES_URL="$(api_url /v1/runs/set_overrides)"

PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${SET_OVERRIDES_URL}" 2>/dev/null || true)
LIVE=1
case "${PROBE:-000}" in
    404|405|501|000|'')
        skip "phase 220: runs.set_overrides route not present (${PROBE:-000})"
        LIVE=0
        ;;
esac

if [ "${LIVE}" = "0" ]; then
    : # live assertions skipped; the guard tests below still run.
elif [ -z "${HARBOR_DEV_TOKEN:-}" ]; then
    skip "phase 220: HARBOR_DEV_TOKEN unavailable — live assertions skipped (run under 'make preflight')"
elif ! command -v curl >/dev/null 2>&1; then
    skip 'phase 220: curl not available; cannot exercise live wire'
else
    TOKEN="${HARBOR_DEV_TOKEN}"
    ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")

    post_code() {
        curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
            -X POST "${SET_OVERRIDES_URL}" \
            -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
            -H 'Content-Type: application/json' -d "$1" 2>/dev/null || true
    }

    # --- The field is accepted at the wire door. A 400 here means the strict
    #     DisallowUnknownFields() decoder rejected the key even though
    #     runs.go declares it — the wire type and the handler disagree.
    CODE=$(post_code '{"overrides":{"session_id":"dev","extra_instructions":"Answer in the imperative mood."}}')
    if [ "${CODE}" = "200" ]; then
        ok 'phase 220: extra_instructions is accepted at the wire door (200)'
    else
        fail "phase 220: extra_instructions returned ${CODE}, want 200 — the field is declared in ${RUNS_TYPES_GO} but the handler does not accept it"
    fi

    # --- Additive AND replace are not mutually exclusive: the additive block
    #     survives a whole-spine replace, so both may ride one request. A
    #     refusal here would contradict the property the phase is built on.
    CODE=$(post_code '{"overrides":{"session_id":"dev","extra_instructions":"Cite sources.","system_prompt_override":"You are terse."}}')
    if [ "${CODE}" = "200" ]; then
        ok 'phase 220: extra_instructions and system_prompt_override compose in one request (200) — additive survives replace'
    else
        fail "phase 220: the additive + replace pair returned ${CODE}, want 200 — the two are not mutually exclusive"
    fi

    # --- A present-but-empty value is accepted and contributes nothing. It is
    #     NOT an error, and specifically NOT a channel for clearing a
    #     tenant-level block (there is no run-level clear).
    CODE=$(post_code '{"overrides":{"session_id":"dev","extra_instructions":""}}')
    if [ "${CODE}" = "200" ]; then
        ok 'phase 220: an empty extra_instructions is accepted as a no-op (200) — not an error, and not a clear'
    else
        fail "phase 220: an empty extra_instructions returned ${CODE}, want 200"
    fi

    # --- The new field opens NO new door: identity stays mandatory.
    CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
        -X POST "${SET_OVERRIDES_URL}" -H 'Content-Type: application/json' \
        -d '{"overrides":{"session_id":"dev","extra_instructions":"x"}}' 2>/dev/null || true)
    if [ "${CODE}" = "401" ]; then
        ok 'phase 220: a request carrying extra_instructions without identity is refused 401 — the field opens no new door'
    else
        fail "phase 220: unauthenticated extra_instructions returned ${CODE}, want 401 (CLAUDE.md §6 — identity is mandatory)"
    fi

    # --- Cross-session scope still refuses with the new field set.
    CODE=$(post_code '{"overrides":{"session_id":"some-other-session","extra_instructions":"x"}}')
    case "${CODE}" in
        403)
            ok 'phase 220: a cross-session extra_instructions set is refused 403 (ErrCrossSessionScope) with the new field present'
            ;;
        *)
            fail "phase 220: cross-session extra_instructions returned ${CODE}, want 403 — a new field must not widen the session boundary"
            ;;
    esac
fi

# ----------------------------------------------------------------------------
# 5. Guard tests (each FAILS, never SKIPs, when its guard is removed).
# ----------------------------------------------------------------------------

run_filtered_tests \
    "phase 220: the two-producer join table, the no-clear property, the audit flag and the concurrent-reuse run (runs/protocol)" \
    'TestComposeLLMOverrides_ExtraInstructions|TestComposeLLMOverrides_ConcurrentReuse_NoCrossTalk|TestSetOverrides_ExtraInstructions|TestSetOverrides_CrossSessionRefusedWithExtraInstructions' \
    ./internal/runtime/runs/protocol/

run_filtered_tests \
    "phase 220: the composed value renders VERBATIM and survives SystemPromptOverride (planner/react)" \
    'TestComposition_ExtraInstructionsStillAdditive|TestComposition_ExtraInstructionsRenderedVerbatim|TestComposition_AbsentExtraInstructionsIsByteIdentical' \
    ./internal/planner/react/

run_filtered_tests \
    "phase 220: the two-producer seam end to end, with identity propagation and three failure modes (integration)" \
    'TestE2E_RunExtraInstructions_' \
    ./test/integration/

smoke_summary
