#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 220 — `extra_instructions` on `RunOverrides` (D-365).
#
# The phase makes the ALREADY-additive `planner.LLMOverrides.ExtraInstructions`
# reachable over the Harbor Protocol as one optional field on `RunOverrides`,
# and pins the two-producer join: the run-level value COMPOSES BELOW the
# tenant-level value and can never clear it.
#
# WHAT THIS SCRIPT SKIPS, precisely. Exactly TWO arms can SKIP, and both are
# harness preamble rather than assertions: the `/healthz` reachability probe
# (a standalone run against no server) and the `curl` / `go` availability
# checks. EVERY assertion below FAILS when the thing it guards is removed —
# there is no phase-gate SKIP arm. The skeleton shipped with one (`the field
# is absent -> skip everything`), and mutation-verifying it produced exactly
# the §4.2-item-5 failure it was meant to prevent: deleting the wire field
# gave OK 0 / SKIP 1 / FAIL 0 and exit 0. The phase has SHIPPED, so the
# field's absence is a regression, not a future surface.
#
# The live legs use the dev bearer resolved through `common.sh`'s `dev_bearer`
# — never a raw `${HARBOR_DEV_TOKEN}` read, which resolves to empty outside
# `make preflight` and turns every live assertion into a silent SKIP while the
# script still exits 0 (issue #624).
#
# The override slot is session-scoped, one-shot and last-write-wins, and every
# live leg targets the dev identity's OWN session, so the script is idempotent
# against a long-lived preflight server.
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
WIRE_MANIFEST='web/console/src/lib/protocol/wire-manifest.gen.json'

# assert_block_grep <file> <sed-range-start> <sed-range-end> <ERE> <desc>
#
# Asserts an extended-regex pattern matches inside a bounded region of a file
# (a function body, a struct, a doc block). OK on a match, FAIL otherwise —
# deliberately never a SKIP: the 404/405/501 convention is for a forward-phase
# script meeting an older build's HTTP surface, not for source guards.
#
# Patterns use POSIX classes ([[:space:]]) — never \t / \d, which BSD grep
# matches and GNU grep does not.
assert_block_grep() {
    local file="$1" from="$2" to="$3" pattern="$4" desc="$5"
    if [ ! -f "${file}" ]; then
        fail "${desc}: ${file} does not exist"
        return
    fi
    if sed -n "/${from}/,/${to}/p" "${file}" | grep -qE -- "${pattern}"; then
        ok "${desc}"
    else
        fail "${desc}: no match for /${pattern}/ between /${from}/ and /${to}/ in ${file}"
    fi
}

# ----------------------------------------------------------------------------
# 1. The wire field. An ANCHORED whole-declaration match: a bare substring grep
#    for `ExtraInstructions` stays green after a rename or a type change, which
#    is an inert guard (the trap D-364 caught by mutation).
# ----------------------------------------------------------------------------

assert_grep_present \
    'ExtraInstructions[[:space:]]+\*string[[:space:]]+`json:"extra_instructions,omitempty"`' \
    "${RUNS_TYPES_GO}" \
    'phase 220: RunOverrides declares the optional extra_instructions wire field'

# ----------------------------------------------------------------------------
# 2. The field must REACH the composition. A wire field accepted at the door
#    and dropped before the prompt is the inert-field shape.
# ----------------------------------------------------------------------------

assert_block_grep "${OVERRIDES_GO}" \
    '^type PendingOverride struct' '^}' \
    'ExtraInstructions[[:space:]]+\*string' \
    'phase 220: PendingOverride carries the validated extra-instructions value'

assert_block_grep "${OVERRIDES_GO}" \
    '^func (s \*Service) validate(' '^}' \
    'o\.ExtraInstructions' \
    'phase 220: validate() copies the wire value into the pending slot'

assert_block_grep "${OVERRIDES_GO}" \
    '^func ComposeLLMOverrides(' '^}' \
    'session\.ExtraInstructions' \
    'phase 220: ComposeLLMOverrides reads the session-level extra instructions (the field is not inert)'

# ----------------------------------------------------------------------------
# 3. THE PRECEDENCE GUARD (D-365). The session arm must feed the ALREADY-
#    COMPOSED tenant value into the join. A bare
#    `out.ExtraInstructions = session.ExtraInstructions` does not, so mutating
#    the join into an assignment turns this OK into a FAIL — the whole point:
#    an assignment would hand a non-admin session caller a silent delete on an
#    admin-set tenant block.
# ----------------------------------------------------------------------------

assert_block_grep "${OVERRIDES_GO}" \
    '^func ComposeLLMOverrides(' '^}' \
    'out\.ExtraInstructions[[:space:]]*=[[:space:]]*joinAdditiveGuidance\(out\.ExtraInstructions,[[:space:]]*session\.ExtraInstructions\)' \
    'phase 220: the two-producer join READS the tenant value (composition, not last-writer-wins)'

# The join helper exists EXACTLY once — one production composition site, no
# second copy that could drift (CLAUDE.md §17.4).
JOIN_DEFS=$(grep -rlE '^func joinAdditiveGuidance\(' --include='*.go' internal/ cmd/ 2>/dev/null | wc -l | tr -d ' ')
if [ "${JOIN_DEFS}" = "1" ]; then
    ok 'phase 220: joinAdditiveGuidance is defined exactly once (one production composition, no second copy)'
else
    fail "phase 220: found ${JOIN_DEFS} definitions of joinAdditiveGuidance, want exactly 1"
fi

# The join ORDER and the no-clear property, mechanically. `above` first, the
# separator between, `below` after; and an empty `below` returns `above`
# UNTOUCHED rather than an empty string.
assert_block_grep "${OVERRIDES_GO}" \
    '^func joinAdditiveGuidance(' '^}' \
    'TrimSpace\(\*above\)[[:space:]]*\+[[:space:]]*additiveGuidanceSeparator[[:space:]]*\+[[:space:]]*strings\.TrimSpace\(\*below\)' \
    'phase 220: the join renders the tenant text FIRST, separator, then the run-level text'

assert_block_grep "${OVERRIDES_GO}" \
    '^func joinAdditiveGuidance(' '^}' \
    'below == nil \|\| strings\.TrimSpace\(\*below\) == ""' \
    'phase 220: an empty run-level value short-circuits to the tenant value (there is no run-level clear)'

assert_grep_present \
    '^const additiveGuidanceSeparator = "\\n\\n"$' \
    "${OVERRIDES_GO}" \
    'phase 220: the separator is a blank line, matching the prompt builder own guidance join'

# ----------------------------------------------------------------------------
# 4. The audit flag — and ONLY the flag. The VALUE is caller-supplied free text
#    and must never ride the bus (CLAUDE.md §7 / the payload SafePayload rule).
# ----------------------------------------------------------------------------

assert_block_grep "${EVENTS_GO}" \
    '^type RunOverridesSetPayload struct' '^}' \
    'SetExtraInstructions[[:space:]]+bool' \
    'phase 220: RunOverridesSetPayload declares the SetExtraInstructions flag'

assert_block_grep "${OVERRIDES_GO}" \
    '^func (s \*Service) emitAudit(' '^}' \
    'SetExtraInstructions:[[:space:]]*po\.ExtraInstructions[[:space:]]*!=[[:space:]]*nil' \
    'phase 220: emitAudit carries only the boolean flag — the caller-supplied text never reaches the bus'

# ----------------------------------------------------------------------------
# 5. The trust statement. The field renders VERBATIM into an operator-TRUSTED
#    position; a field that ships without saying so is the documentation gap
#    this phase exists to close.
# ----------------------------------------------------------------------------

assert_block_grep "${RUNS_TYPES_GO}" \
    'ExtraInstructions, when non-nil' 'ExtraInstructions \*string' \
    'additional_guidance' \
    'phase 220: the wire godoc names the <additional_guidance> render position'

assert_block_grep "${RUNS_TYPES_GO}" \
    'ExtraInstructions, when non-nil' 'ExtraInstructions \*string' \
    'VERBATIM' \
    'phase 220: the wire godoc states the text is rendered verbatim (operator-trusted, unescaped)'

assert_block_grep "${RUNS_TYPES_GO}" \
    'ExtraInstructions, when non-nil' 'ExtraInstructions \*string' \
    'caller_memory' \
    'phase 220: the wire godoc points recalled/retrieved content at the untrusted-framed tier instead (the binding non-goal)'

# ----------------------------------------------------------------------------
# 6. Console hand-mirror lockstep (D-223 / §4.5 item 5), checked here so a
#    missed mirror fails preflight and not only `npm run lint`.
# ----------------------------------------------------------------------------

assert_grep_present \
    'extra_instructions\?:[[:space:]]*string' \
    "${RUNS_TS}" \
    'phase 220: the Console typed client mirrors extra_instructions by hand'

assert_grep_present \
    "'extra_instructions'" \
    "${RUNS_TS_TEST}" \
    'phase 220: ALLOWED_OVERRIDE_KEYS admits extra_instructions (the strict-decoder key set)'

assert_grep_present \
    '"key": "extra_instructions"' \
    "${WIRE_MANIFEST}" \
    'phase 220: the committed wire manifest carries the field (make protocol-ts-gen was run)'

# ----------------------------------------------------------------------------
# 7. Live-wire assertions against the preflight-booted dev server.
#
#    `runs.set_overrides` is an ALREADY-SHIPPED route, so a 404/405/501 is an
#    un-mounted regression and a FAIL — not a future surface. Only an
#    unreachable server (000) skips, and only the live block.
# ----------------------------------------------------------------------------

SET_OVERRIDES_URL="$(api_url /v1/runs/set_overrides)"
GUIDANCE='Cite every source you use.'

LIVE=1
if ! command -v curl >/dev/null 2>&1; then
    skip 'phase 220: curl not available — live assertions skipped'
    LIVE=0
else
    HEALTH=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$(api_url /healthz)" || true)
    case "${HEALTH}" in
        000|'')
            skip "phase 220: dev server unreachable at $(api_url /healthz) — live assertions skipped (run under 'make preflight')"
            LIVE=0
            ;;
    esac
fi

if [ "${LIVE}" = "1" ]; then
    TOKEN="$(dev_bearer)"
    if [ -z "${TOKEN}" ]; then
        # NOT a skip: an unauthenticated request answers 401 and proves
        # nothing, so a smoke that cannot authenticate has not run (#624).
        fail 'phase 220: no dev bearer resolved (HARBOR_DEV_TOKEN unset and no ${HARBOR_DATA_DIR}/server.log) — the live legs would prove nothing'
    else
        post_code() {
            curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
                -X POST "${SET_OVERRIDES_URL}" \
                -H "Authorization: Bearer ${TOKEN}" \
                -H 'Content-Type: application/json' -d "$1" 2>/dev/null || true
        }

        # assert_live <expected> <body> <desc>
        # 404/405/501 is a FAIL (the route is already shipped); nothing skips.
        assert_live() {
            local want="$1" body="$2" desc="$3" got
            got=$(post_code "${body}")
            if [ "${got}" = "${want}" ]; then
                ok "${desc} (${got})"
            else
                fail "${desc}: got ${got}, want ${want}"
            fi
        }

        # The field is accepted at the wire door. A 400 here is a FAIL, never a
        # SKIP: the handler decodes with DisallowUnknownFields(), so a 400 with
        # the field declared in runs.go means the wire type and the handler
        # disagree. `session_id` is omitted so the handler defaults it to the
        # verified session — the caller's own, so the leg is idempotent.
        assert_live 200 \
            "{\"overrides\":{\"extra_instructions\":\"${GUIDANCE}\"}}" \
            'phase 220: extra_instructions is accepted at the wire door'

        # Additive and REPLACE are not mutually exclusive: the additive block
        # survives a whole-spine replace, so both may ride one request.
        assert_live 200 \
            "{\"overrides\":{\"extra_instructions\":\"${GUIDANCE}\",\"system_prompt_override\":\"You are terse.\"}}" \
            'phase 220: extra_instructions and system_prompt_override compose in one request — additive survives replace'

        # A present-but-empty value is a no-op, not an error and not a clear.
        assert_live 200 \
            '{"overrides":{"extra_instructions":""}}' \
            'phase 220: an empty extra_instructions is accepted as a no-op'

        # The new field opens NO new door: identity stays mandatory.
        UNAUTH=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
            -X POST "${SET_OVERRIDES_URL}" -H 'Content-Type: application/json' \
            -d "{\"overrides\":{\"extra_instructions\":\"${GUIDANCE}\"}}" 2>/dev/null || true)
        if [ "${UNAUTH}" = "401" ]; then
            ok 'phase 220: a request carrying extra_instructions without identity is refused 401 — the field opens no new door'
        else
            fail "phase 220: unauthenticated extra_instructions returned ${UNAUTH}, want 401 (CLAUDE.md §6 — identity is mandatory)"
        fi

        # Cross-session scope still refuses with the new field set.
        assert_live 403 \
            "{\"overrides\":{\"session_id\":\"phase220-someone-elses-session\",\"extra_instructions\":\"${GUIDANCE}\"}}" \
            'phase 220: a cross-session extra_instructions set is refused — a new field must not widen the session boundary'

        # Leave the slot clean: the one-shot slot is last-write-wins and this
        # script never sends a message, so a stale override would otherwise
        # ride the next real run in the dev session.
        post_code '{"overrides":{"extra_instructions":""}}' >/dev/null
    fi
fi

# ----------------------------------------------------------------------------
# 8. Guard tests. `assert_go_tests_pass` greps the -v output for a `--- PASS:`
#    line PER NAME, so a renamed or deleted test is a FAIL — `go test -run`
#    with a filter that matches nothing exits ZERO, and an exit-code-only check
#    would report OK after the guard disappeared.
# ----------------------------------------------------------------------------

if ! command -v go >/dev/null 2>&1; then
    skip 'phase 220: go toolchain not available — guard tests skipped'
else
    # Trailing X's are MANDATORY: GNU mktemp (the Linux CI runner) rejects a
    # template with fewer than three trailing `X`s outright, while BSD mktemp
    # (macOS, where contributors run preflight) accepts it — so a template
    # without them passes locally and dies in CI. The explicit
    # `${TMPDIR:-/tmp}` path form is preferred over `-t`, which GNU documents
    # as deprecated.
    GOLOG="$(mktemp "${TMPDIR:-/tmp}/phase220-gotest.XXXXXX")"
    trap 'rm -f "${GOLOG}"' EXIT

    assert_go_tests_pass "${GOLOG}" './internal/runtime/runs/protocol/' \
        'phase 220: the join table, the no-clear property, the by-value copy, the audit flag and the concurrent-reuse run' \
        TestComposeLLMOverrides_ExtraInstructionsJoinTable \
        TestComposeLLMOverrides_ExtraInstructionsNoRunLevelClear \
        TestComposeLLMOverrides_ConcurrentReuse_NoCrossTalk \
        TestSetOverrides_ExtraInstructionsCopiedByValue \
        TestSetOverrides_ExtraInstructionsAuditFlag \
        TestSetOverrides_IdentityAndCrossSessionRefusedWithExtraInstructions

    assert_go_tests_pass "${GOLOG}" './internal/planner/react/' \
        'phase 220: the composed value renders VERBATIM, survives a SystemPromptOverride, and is absent-safe' \
        TestComposition_ExtraInstructionsRenderedVerbatim_TwoProducers \
        TestComposition_ComposedExtraInstructionsSurviveSystemPromptOverride \
        TestComposition_AbsentExtraInstructionsRendersNoGuidanceSection

    assert_go_tests_pass "${GOLOG}" './internal/runtime/serve/' \
        'phase 220: the run loop run-start resolution carries the join (not just the composer in isolation)' \
        TestResolveLLMOverrides_ExtraInstructionsJoin_TenantThenSession

    assert_go_tests_pass "${GOLOG}" './test/integration/' \
        'phase 220: the two-producer seam end to end, with identity propagation and three failure modes' \
        TestE2E_RunExtraInstructions_TwoProducerSeam \
        TestE2E_RunExtraInstructions_TenantBlockSurvivesEverySessionShape \
        TestE2E_RunExtraInstructions_FailureModes \
        TestE2E_RunExtraInstructions_IdentityIsolationUnderConcurrency
fi

smoke_summary
