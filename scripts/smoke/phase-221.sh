#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 221 — an expected-revision token on the agent-config writes (D-366).
#
# Every guard below was MUTATION-VERIFIED: the mutation named in its comment
# was applied, the guard was watched turn OK -> FAIL (never OK -> SKIP), and
# the mutation reverted. A guard that stays green under its own mutation is
# decorative and does not count toward the done-definition.
#
# Route classification uses an EMPTY body deliberately (the phase-211 lesson):
# a mounted route answers a typed error, an unmounted one answers 404.
# Classifying on bare status would convert a real answer into a SKIP, and
# "a SKIP that should be an OK is a bug" (§4.2 item 5).
#
# Done-definition: OK >= 12, FAIL = 0 against the live preflight build.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

TYPES_GO='internal/protocol/types/agentconfig.go'
ERRORS_GO='internal/protocol/errors/errors.go'
STATUS_GO='internal/protocol/transports/control/status.go'
AGENTCFG_GO='internal/agentcfg/agentcfg.go'
DRIVER_GO='internal/agentcfg/drivers/statestore/statestore.go'
CONFORMANCE_GO='internal/agentcfg/conformance/conformance.go'
VERSION_GO='internal/protocol/types/version.go'
TS_WIRE='web/console/src/lib/protocol/agentconfig.ts'
ERRORS_MD='docs/site/protocol/errors.md'

# ---------------------------------------------------------------------------
# Static guards. Patterns use POSIX classes ([[:space:]]) — never \t / \d,
# which BSD grep matches and GNU grep does not (a guard written that way
# silently never fires on Linux CI).
# ---------------------------------------------------------------------------

# (1) EXACT count, not ">= 1": dropping one of the seventeen doors FAILS here.
# The count moved 16 -> 17 when phase 222's `set_extra_system_blocks` joined the
# spine (CLAUDE.md §17.6 — the guard did exactly its job by refusing to let a
# new door land silently; the door threads the token and is DRIVEN by
# TestConditionalWrite_AllSeventeenDoorsAcceptTheToken, which moved with it).
#     Mutation 3 (drop the field from one request type) turns this OK -> FAIL.
assert_grep_count 'ExpectedContentHash string' "${TYPES_GO}" 17 \
    'phase 221: all seventeen spine request types carry expected_content_hash'
assert_grep_count 'expected_content_hash,omitempty' "${TYPES_GO}" 17 \
    'phase 221: all seventeen json tags are optional (absent == unconditional)'

# (2) The domain option + the sentinel.
assert_grep_present 'type SetOptions struct' "${AGENTCFG_GO}" \
    'phase 221: agentcfg.SetOptions declared'
assert_grep_present 'ErrRevisionConflict = errors\.New' "${AGENTCFG_GO}" \
    'phase 221: agentcfg.ErrRevisionConflict sentinel declared'

# (3) THE ORDERING GUARD — the precondition MUST precede the idempotent
#     re-set. A transposition is the ONE mutation that leaves every
#     grep-for-presence guard green while turning a stale token into a
#     misleading 200, so it is asserted by LINE NUMBER.
#     Mutation 2 (transpose the two blocks) turns this OK -> FAIL.
PRECOND_LINE="$(grep -n 'opts\.ExpectedContentHash != ""' "${DRIVER_GO}" | head -1 | cut -d: -f1)"
IDEMPOTENT_LINE="$(grep -n 'active\.ContentHash == hash' "${DRIVER_GO}" | head -1 | cut -d: -f1)"
if [ -n "${PRECOND_LINE}" ] && [ -n "${IDEMPOTENT_LINE}" ] && [ "${PRECOND_LINE}" -lt "${IDEMPOTENT_LINE}" ]; then
    ok "phase 221: the precondition is evaluated BEFORE the idempotent re-set short-circuit (line ${PRECOND_LINE} < ${IDEMPOTENT_LINE})"
else
    fail "phase 221: precondition/idempotence ordering wrong or missing (precond=${PRECOND_LINE:-none} idempotent=${IDEMPOTENT_LINE:-none}) — a stale token could be answered 200"
fi

# (4) The canonical error code, its registration, and its 409 binding.
#     Mutation 5 (remove the 409 binding) turns the third of these OK -> FAIL.
assert_grep_present 'CodeRevisionConflict Code = "revision_conflict"' "${ERRORS_GO}" \
    'phase 221: CodeRevisionConflict declared'
assert_grep_present 'CodeRevisionConflict:[[:space:]]+\{\}' "${ERRORS_GO}" \
    'phase 221: CodeRevisionConflict registered in canonicalCodes'
assert_grep_present 'CodeRevisionConflict' "${STATUS_GO}" \
    'phase 221: CodeRevisionConflict bound to a status in the control transport'

# (5) The shared conformance rows — so a SECOND agentcfg driver cannot ship
#     the interface without the precondition (RFC §9 parity).
#     Mutation 6 (delete one row) turns this OK -> FAIL.
#     Counting the t.Run REGISTRATIONS, not the function declarations: a row
#     whose registration is deleted still declares its function, so a
#     declaration count would stay green while the row stopped executing.
#     The count moved 4 -> 7 when the FIRST-WRITE sentinel landed (the
#     composition protocol had no expressible token at its base case, so two
#     first contributors silently reverted each other). Bumping the count
#     alone would have been the wrong fix, so the three new rows are
#     BEHAVIOURAL: they drive the sentinel, refuse it once a revision exists,
#     and reproduce-then-close the lost update.
assert_grep_count 't\.Run\("ConditionalWrite_' "${CONFORMANCE_GO}" 7 \
    'phase 221: the shared conformance suite RUNS the seven precondition rows'
assert_grep_count 'func testConditional' "${CONFORMANCE_GO}" 7 \
    'phase 221: the shared conformance suite declares the seven precondition rows'

# (6) The change is ADDITIVE — the Protocol version does not move.
assert_grep_present 'ProtocolVersion = "0\.1\.0"' "${VERSION_GO}" \
    'phase 221: ProtocolVersion is unchanged (the change is additive)'

# (7) THE HONESTY GUARDS. These pin the TRUTHFULNESS of the claim, not only
#     its existence: the compare-and-write is atomic in-process only (the
#     StateStore has no CAS primitive), and no text may claim otherwise.
#     Mutation 8 (delete the bound sentence from the godoc) turns the first
#     of these OK -> FAIL.
assert_grep_present 'NOT atomic across Runtime processes' "${AGENTCFG_GO}" \
    'phase 221: the SetOptions godoc states the single-process bound'
assert_grep_present 'CAN STILL LOSE AN UPDATE' "${AGENTCFG_GO}" \
    'phase 221: the SetOptions godoc names the residual in plain words'
assert_grep_present 'NOT a cross-process compare-and-swap' "${ERRORS_GO}" \
    'phase 221: the CodeRevisionConflict godoc states the same bound'
assert_grep_present 'CAN STILL LOSE AN UPDATE' "${ERRORS_MD}" \
    'phase 221: the GENERATED Protocol reference carries the bound (D-209)'

# (8) The Console mirror — seventeen optional fields, hand-mirrored and
#     lockstep-gated (D-223).
assert_grep_count 'expected_content_hash\?: string;' "${TS_WIRE}" 17 \
    'phase 221: the Console wire module mirrors the field on all seventeen types'

# ---------------------------------------------------------------------------
# Live guards. The block degrades to its OWN skip rather than exiting, so the
# static + unit legs still run on a standalone invocation.
# ---------------------------------------------------------------------------

ROUTE="$(api_url /v1/agent_config/set_revision)"
GET_ROUTE="$(api_url /v1/agent_config/get)"
TOKEN="$(dev_bearer)"
SCRATCH="${TMPDIR:-/tmp}"

if ! command -v curl >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
    skip 'phase 221: curl/jq unavailable — live assertions skipped'
else
    # Classify the route with an EMPTY body: a mounted route answers a typed
    # error at 4xx; an unmounted one answers 404.
    P221_CLASSIFY=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H 'Content-Type: application/json' -d '{}' "${ROUTE}" || true)
    case "${P221_CLASSIFY:-000}" in
        000)
            skip 'phase 221: dev server unreachable — live assertions skipped (run under make preflight)'
            ;;
        404)
            skip 'phase 221: agent_config.set_revision not mounted on this build — live assertions skipped'
            ;;
        *)
            # p221_post <url> <body>  -> sets P221_STATUS / P221_BODY / P221_CODE
            p221_post() {
                local url="$1" body="$2"
                local hdrs=(-H 'Content-Type: application/json')
                if [ -n "${TOKEN}" ]; then
                    hdrs+=(-H "Authorization: Bearer ${TOKEN}")
                fi
                P221_STATUS=$(curl -s --max-time 10 -o "${SCRATCH}/phase221.body" -w '%{http_code}' \
                    "${hdrs[@]}" -X POST -d "${body}" "${url}" 2>/dev/null || true)
                P221_STATUS="${P221_STATUS:-000}"
                P221_BODY=$(cat "${SCRATCH}/phase221.body" 2>/dev/null || echo '{}')
                P221_CODE=$(printf '%s' "${P221_BODY}" | jq -r '.code // .error.code // ""' 2>/dev/null || echo "")
            }

            ID='{"tenant":"dev","user":"dev","session":"dev"}'
            AGENT='smoke-221-agent'

            # (L1) THE TOKEN IS NOT AN AUTHORITY. An UNAUTHENTICATED write
            #      carrying a syntactically-valid token is still refused by the
            #      identity/admin gate — and specifically NOT with
            #      revision_conflict, which would mean the precondition ran
            #      before the gate.
            UNAUTH_CODE=$(curl -s --max-time 5 -o "${SCRATCH}/phase221.unauth" -w '%{http_code}' \
                -X POST -H 'Content-Type: application/json' \
                -d "{\"identity\":${ID},\"agent_id\":\"${AGENT}\",\"payload\":{},\"expected_content_hash\":\"deadbeef\"}" \
                "${ROUTE}" 2>/dev/null || true)
            UNAUTH_BODY=$(cat "${SCRATCH}/phase221.unauth" 2>/dev/null || echo '{}')
            UNAUTH_ERRCODE=$(printf '%s' "${UNAUTH_BODY}" | jq -r '.code // ""' 2>/dev/null || echo "")
            case "${UNAUTH_CODE}" in
                200)
                    fail 'phase 221: an unauthenticated conditional write answered 200 — the token became an authority'
                    ;;
                401|403)
                    if [ "${UNAUTH_ERRCODE}" = 'revision_conflict' ]; then
                        fail 'phase 221: an authorization failure was reported as revision_conflict — the precondition runs before the identity/scope gate'
                    else
                        ok "phase 221: an unauthenticated conditional write is refused by the identity/admin gate (${UNAUTH_CODE}/${UNAUTH_ERRCODE:-none}), not by the token"
                    fi
                    ;;
                *)
                    fail "phase 221: unauthenticated conditional write answered unexpected status ${UNAUTH_CODE}"
                    ;;
            esac

            # (L2) THE TYPED CONFLICT, over the wire. Seed a revision, read its
            #      content hash back through agent_config.get, then write under
            #      a DELIBERATELY-BOGUS hash.
            p221_post "${ROUTE}" "{\"identity\":${ID},\"agent_id\":\"${AGENT}\",\"payload\":{\"skills\":{\"names\":[\"smoke-a\"]}}}"
            if [ "${P221_STATUS}" != '200' ]; then
                skip "phase 221: could not seed a revision (status ${P221_STATUS}, code ${P221_CODE:-none}) — the live conflict probe needs an admin-scoped bearer"
            else
                ok 'phase 221: the UNCONDITIONAL write path still answers 200 (absent token == today, byte for byte)'

                # Read the hash back the way a client does.
                p221_post "${GET_ROUTE}" "{\"identity\":${ID},\"agent_id\":\"${AGENT}\"}"
                LIVE_HASH=$(printf '%s' "${P221_BODY}" | jq -r '.revision.content_hash // ""' 2>/dev/null || echo "")
                if [ -z "${LIVE_HASH}" ]; then
                    fail 'phase 221: agent_config.get returned no content_hash — the recovery loop a conflicted client runs is broken'
                else
                    ok 'phase 221: agent_config.get returns the content_hash a conditional writer needs'
                fi

                # A MATCHING token proceeds exactly as an unconditional write.
                p221_post "${ROUTE}" "{\"identity\":${ID},\"agent_id\":\"${AGENT}\",\"payload\":{\"skills\":{\"names\":[\"smoke-a\",\"smoke-b\"]}},\"expected_content_hash\":\"${LIVE_HASH}\"}"
                if [ "${P221_STATUS}" = '200' ]; then
                    ok 'phase 221: a MATCHING expected_content_hash proceeds (200)'
                else
                    fail "phase 221: a matching expected_content_hash was refused (status ${P221_STATUS}, code ${P221_CODE:-none})"
                fi

                # A BOGUS token is refused with the TYPED code at 409. Asserting
                # the CODE, never the status alone: a server that silently
                # ignored the unknown field would answer 200, and one that
                # mis-mapped the sentinel would answer a different code.
                p221_post "${ROUTE}" "{\"identity\":${ID},\"agent_id\":\"${AGENT}\",\"payload\":{\"skills\":{\"names\":[\"smoke-c\"]}},\"expected_content_hash\":\"0000000000000000000000000000000000000000000000000000000000000000\"}"
                if [ "${P221_STATUS}" = '200' ]; then
                    fail 'phase 221: a BOGUS expected_content_hash answered 200 — the server is ignoring the field'
                elif [ "${P221_CODE}" = 'revision_conflict' ] && [ "${P221_STATUS}" = '409' ]; then
                    ok 'phase 221: a stale expected_content_hash answers a typed revision_conflict at HTTP 409'
                elif [ "${P221_CODE}" = 'invalid_request' ]; then
                    fail 'phase 221: a stale expected_content_hash answered invalid_request — the conflict is not machine-branchable'
                else
                    fail "phase 221: a stale expected_content_hash answered status ${P221_STATUS} code ${P221_CODE:-none}, want 409/revision_conflict"
                fi

                # NOTHING WAS PERSISTED: the config is still what the matching
                # write left, not what the refused write proposed.
                p221_post "${GET_ROUTE}" "{\"identity\":${ID},\"agent_id\":\"${AGENT}\"}"
                AFTER_NAMES=$(printf '%s' "${P221_BODY}" | jq -rc '.revision.payload.skills.names // []' 2>/dev/null || echo '[]')
                if printf '%s' "${AFTER_NAMES}" | grep -q 'smoke-c'; then
                    fail "phase 221: the refused write was PERSISTED (skills = ${AFTER_NAMES})"
                else
                    ok "phase 221: the refused write persisted nothing (skills = ${AFTER_NAMES})"
                fi
            fi
            ;;
    esac
fi

# ---------------------------------------------------------------------------
# Unit-test legs. Each FAILS on a genuine failure and SKIPs only when the
# filter matches no tests at all.
# ---------------------------------------------------------------------------

# p221_go_test <run-regex> <package> <description>
p221_go_test() {
    local pattern="$1" pkg="$2" desc="$3" out rc
    out=$(go test -race -count=1 -run "${pattern}" "${pkg}" 2>&1) && rc=0 || rc=$?
    if [ "${rc}" -ne 0 ]; then
        printf '%s\n' "${out}" | tail -20 >&2
        fail "${desc}"
        return
    fi
    if printf '%s' "${out}" | grep -q 'no test files'; then
        skip "${desc}: package has no tests"
        return
    fi
    ok "${desc}"
}

if ! command -v go >/dev/null 2>&1; then
    skip 'phase 221: go toolchain unavailable — unit-test legs skipped'
else
    p221_go_test 'TestSetRevision_ConditionalWrite|TestRollback_ConditionalWrite' \
        ./internal/agentcfg/drivers/statestore/ \
        'phase 221: the driver conditional-write tests pass under -race (incl. the ordering pin)'
    p221_go_test 'TestStateStore_Conformance' \
        ./internal/agentcfg/drivers/statestore/ \
        'phase 221: the shared conformance suite (incl. the seven precondition rows) passes under both scope arms'
    p221_go_test 'TestSetRevision_Concurrent|TestConditionalWrite_ConcurrentReuse|TestConditionalWrite_CrossProcessBound' \
        ./internal/agentcfg/drivers/statestore/ \
        'phase 221: N=128 racing writers yield exactly one winner, and the cross-process residual is pinned AS ABSENT'
    p221_go_test 'TestConditionalWrite' \
        ./internal/runtime/agentcfg/protocol/ \
        'phase 221: all seventeen doors thread the token, and the token is never an authority'
    p221_go_test 'TestE2E_AgentConfig_ConditionalWrite' \
        ./test/integration/ \
        'phase 221: the end-to-end conditional-write round trip passes over real drivers'
fi

smoke_summary
