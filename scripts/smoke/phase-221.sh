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
assert_grep_count 't\.Run\("ConditionalWrite_' "${CONFORMANCE_GO}" 7 \
    'phase 221: the shared conformance suite RUNS the seven precondition rows'
assert_grep_count 'func testConditional' "${CONFORMANCE_GO}" 7 \
    'phase 221: the shared conformance suite declares the seven precondition rows'

# (5b) The PARTIAL-WRITE atomicity row, in the same shared suite and for the
#      same reason: a second driver must inherit "a write that did not
#      complete leaves NO revision behind", not re-derive it.
#      Deregistering the row COMPILES (an unused func parameter is legal in
#      Go), so the registration is counted, not the declaration — the same
#      lesson the four rows above already carry.
assert_grep_count 't\.Run\("WriteAtomicity_' "${CONFORMANCE_GO}" 1 \
    'phase 221: the shared conformance suite RUNS the partial-write atomicity row'
assert_grep_present 'mkFaulty FaultFactory' "${CONFORMANCE_GO}" \
    'phase 221: conformance.Run TAKES the fault factory, so a second driver cannot compile without arming it'

# (5c) The atomicity row's TWIN (D-380): a write REPORTED as failed may have
#      LANDED, and a cleanup that assumes otherwise deletes the revision the
#      durable pointer names — unrecoverable, because every door reads through
#      that pointer. The two arms model opposite disk states behind one
#      identical error value, so a suite carrying only one of them certifies a
#      cleanup that is either useless or destructive depending on which half is
#      missing. Counted as its OWN prefix rather than folded into the
#      WriteAtomicity_ count: the two rows must both exist, and one count
#      cannot say that.
#      Counts here were settled by grepping the merged conformance.go, NOT by
#      reasoning about the diff — the ConditionalWrite_ pair above is UNCHANGED
#      at 7, which is the answer reasoning got wrong.
assert_grep_count 't\.Run\("CompensationSafety_' "${CONFORMANCE_GO}" 1 \
    'phase 221: the shared conformance suite RUNS the reported-failed-but-landed row'
assert_grep_present 'mkCommitted CommittedFaultFactory' "${CONFORMANCE_GO}" \
    'phase 221: conformance.Run TAKES the committed-fault factory too, so a second driver cannot compile without arming BOTH arms'
# The compensation itself, in the driver. A bare `return err` between the two
# writes is the defect; the call is the fix.
assert_grep_present 'compensateOrphanRevision\(ctx, q, keys\.revPfx, revID, err\)' "${DRIVER_GO}" \
    'phase 221: the failed active-pointer write compensates its orphan revision'
assert_grep_present 'context\.WithoutCancel\(ctx\)' "${DRIVER_GO}" \
    'phase 221: the compensation runs on an un-cancellable context (a cancelled caller ctx is the likeliest cause of the failure it compensates)'

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
            # NOT a skip. `agent_config.set_revision` shipped waves before this
            # phase, so on a REACHABLE server its absence is an un-mounted
            # REGRESSION, not a future surface — the 404 -> SKIP convention
            # (§4.2 item 4) covers a phase-N+1 script against a phase-N build,
            # which this is not. The sibling instruments already do it this way:
            # phase-222.sh:266 and common.sh's `assert_post_status_auth`.
            fail 'phase 221: agent_config.set_revision answered 404 on a reachable server — the route did not mount (it shipped waves ago; an absent route here is a regression, not a future surface)'
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
            # A FAILED SEED IS A FAILURE, NOT A SKIP. This one arm gated the
            # WHOLE live conflict section — including the only over-the-wire
            # 409/revision_conflict assertion this phase has — so a server that
            # answered anything but 200 here reported green having asserted
            # nothing about the feature. A smoke that cannot seed has not run
            # (§4.2 item 5). The bearer comes from `dev_bearer`, which resolves
            # the admin-scoped dev bootstrap token both under preflight and
            # against a hand-booted `harbor dev`; if it does not resolve, the
            # curl/jq preamble above has already skipped this whole block.
            if [ "${P221_STATUS}" != '200' ]; then
                fail "phase 221: could not seed a revision (status ${P221_STATUS}, code ${P221_CODE:-none}) — the live conflict section, including the ONLY over-the-wire 409/revision_conflict assertion, cannot run"
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
# Unit-test legs. Every leg names its tests EXPLICITLY and goes through
# common.sh's `assert_go_tests_pass`, which greps the `-v` output for a
# `--- PASS:` line per name.
#
# The private `p221_go_test` helper this replaced checked only the EXIT CODE.
# `go test -run NoSuchTest` prints `ok <pkg> [no tests to run]` and exits ZERO,
# so all five legs reported OK against a rename or a deletion of the test they
# name — including the seventeen-door table and the end-to-end round trip. Its
# one SKIP arm grepped `no test files`, which is a DIFFERENT string from the
# `no tests to run` a vanished filter actually prints, so it never fired.
# Reproduced before the repair: renaming TestSetRevision_ConditionalWrite_* away
# left `OK: 1 SKIP: 0 FAIL: 0`. Re-verified after: the same rename FAILs.
# (AGENTS.md §4.2 item 5 — a guard whose pass value is also its can't-tell value
# is not a guard.) The helper was written beside the cure: phase 223 added
# `assert_go_tests_pass` in this same wave for exactly this defect.
#
# `-race -count=1` is passed through the go-test-args parameter so these legs
# keep the race detector and the cache bypass the private helper had.
# ---------------------------------------------------------------------------

if ! command -v go >/dev/null 2>&1; then
    skip 'phase 221: go toolchain unavailable — unit-test legs skipped'
else
    P221_GOLOG="$(mktemp "${TMPDIR:-/tmp}/phase221-gotest.XXXXXX")"
    trap 'rm -f "${P221_GOLOG}"' EXIT

    assert_go_tests_pass "${P221_GOLOG}" '-race -count=1 ./internal/agentcfg/drivers/statestore/' \
        'phase 221: the driver conditional-write tests pass under -race (incl. the ordering pin)' \
        TestSetRevision_ConditionalWrite_MatchingHashProceeds \
        TestSetRevision_ConditionalWrite_MismatchRefused \
        TestSetRevision_ConditionalWrite_NoActiveRevisionRefused \
        TestSetRevision_ConditionalWrite_RefusalPersistsNothing \
        TestSetRevision_ConditionalWrite_PreconditionPrecedesIdempotentReset \
        TestRollback_ConditionalWrite_MatchMismatchAndNoActive

    assert_go_tests_pass "${P221_GOLOG}" '-race -count=1 ./internal/agentcfg/drivers/statestore/' \
        'phase 221: the shared conformance suite (incl. the seven precondition rows) passes under both scope arms' \
        TestStateStore_Conformance

    # The suite over the DURABLE driver too (D-380 W5). The atomicity rows were
    # armed over the in-memory store alone, which is the weakest possible
    # witness for a residue assertion — "the record survived" and "the record
    # was removed" are answered by a map under the same lock as the writes.
    # Named separately from TestStateStore_Conformance because
    # assert_go_tests_pass anchors its -run pattern, so the parent name does
    # NOT drag this one in.
    assert_go_tests_pass "${P221_GOLOG}" '-race -count=1 ./internal/agentcfg/drivers/statestore/' \
        'phase 221: the conformance suite (incl. BOTH fault arms) also runs over the durable SQLite state driver' \
        TestStateStore_Conformance_SQLite

    # THE PARTIAL-WRITE RESIDUE. The write path is two Saves with no
    # transaction between them, so a store error after the first left a
    # revision that EXISTS and that nothing references — inert to readers
    # (the pointer is the source of truth) but visible in `list_revisions`,
    # where it reads to an operator as a lost write. These name the tests
    # explicitly because "the caller got an error" stays green through the
    # whole defect: the residue is the assertion.
    assert_go_tests_pass "${P221_GOLOG}" '-race -count=1 ./internal/agentcfg/drivers/statestore/' \
        'phase 221: a failed active-pointer write leaves no unreferenced revision behind' \
        TestSetRevision_PointerWriteFailure_SurfacesTheStoreError \
        TestSetRevision_PointerWriteFailure_RemovesTheOrphanRevision \
        TestSetRevision_PointerWriteFailure_CompensatesOnACancelledContext \
        TestSetRevision_CompensationFailure_IsReportedNotSwallowed \
        TestSetRevision_SuccessfulWritePathIssuesNoDelete

    # ...and that compensation deletes ONLY when the pointer DISOWNS the
    # revision (D-380, correcting D-373). "The write failed" is what the store
    # SAID; a deadline firing after commit, a dropped ack, a proxy timeout or a
    # reset connection all report that over a write that LANDED, and deleting
    # then manufactures a dangling pointer that no later write can repair,
    # because every door reads through the pointer. The unknown answer (the
    # re-read itself fails) RETAINS, which is the deliberate half. These name
    # the tests explicitly because the leg above stays green through the whole
    # regression: it only ever arms the write that did NOT land.
    assert_go_tests_pass "${P221_GOLOG}" '-race -count=1 ./internal/agentcfg/drivers/statestore/' \
        'phase 221: a pointer write that COMMITTED and then errored keeps its revision, stays writable, and is announced' \
        TestSetRevision_PointerWriteCommittedThenErrored_KeepsTheRevision \
        TestSetRevision_PointerWriteCommittedThenErrored_LeavesTheAgentWritable \
        TestSetRevision_PointerWriteCommittedThenErrored_StillAnnouncesTheRevision \
        TestSetRevision_CompensationCannotReadThePointer_RetainsTheRevision

    # The conformance rows' REGISTRATION guard, in Go rather than only in the
    # greps above: deregistering a `t.Run` COMPILES (an unused func parameter
    # is legal), so the suite stays green while the invariant stops being
    # asserted. It lives in the conformance package, not the driver's.
    assert_go_tests_pass "${P221_GOLOG}" '-race -count=1 ./internal/agentcfg/conformance/' \
        'phase 221: both fault rows are REGISTERED (deregistering one compiles, so Go asserts it too)' \
        TestConformance_FaultRowsAreRegistered

    assert_go_tests_pass "${P221_GOLOG}" '-race -count=1 ./internal/agentcfg/drivers/statestore/' \
        'phase 221: N=128 racing writers yield exactly one winner, and the cross-process residual is pinned AS ABSENT' \
        TestSetRevision_ConcurrentConditionalWriters_ExactlyOneWins \
        TestSetRevision_ConcurrentUnconditionalWriters_AllSucceed \
        TestConditionalWrite_ConcurrentReuse_NoCrossOwnerBleed \
        TestConditionalWrite_CrossProcessBoundIsDocumented

    assert_go_tests_pass "${P221_GOLOG}" '-race -count=1 ./internal/runtime/agentcfg/protocol/' \
        'phase 221: all seventeen doors thread the token, and the token is never an authority' \
        TestConditionalWrite_AllSeventeenDoorsAcceptTheToken \
        TestConditionalWrite_SeventeenRequestTypesDeclareTheField \
        TestConditionalWrite_TokenIsPreconditionNotAuthority

    assert_go_tests_pass "${P221_GOLOG}" '-race -count=1 ./test/integration/' \
        'phase 221: the end-to-end conditional-write round trip passes over real drivers' \
        TestE2E_AgentConfig_ConditionalWrite \
        TestE2E_AgentConfig_ConditionalWrite_IdentityPropagation \
        TestE2E_AgentConfig_ConditionalWrite_FailureModes \
        TestE2E_AgentConfig_ConditionalWrite_ConcurrencyStress

    # A refusal must be side-effect free (D-370). The 409 arrives on a door that
    # has ALREADY dialled, handshaken and registered a live MCP server, so the
    # conditional write's guarantee is only true if the attach is compensated.
    # These name the tests explicitly because the defect shipped past a suite
    # that asserted the refusal and never the residue: deleting the
    # compensation leaves "caller gets a 409" and "no revision persisted" green.
    assert_go_tests_pass "${P221_GOLOG}" '-race -count=1 ./internal/runtime/agentcfg/protocol/' \
        'phase 221: a revision_conflict compensates the live attach instead of stranding it' \
        TestAddMCPConnection_RevisionConflict_CompensatesTheAttach \
        TestAddMCPConnection_RevisionConflict_UninstallsInlineWireProvider \
        TestAddMCPConnection_AuthRequiredConflict_CompensatesTheAttach \
        TestAddMCPConnection_UnconditionalAddIsUnchanged \
        TestAddMCPConnection_FailedAttachDoesNotDetach \
        TestAddMCPConnection_ConflictWithNoDetacherFailsLoud \
        TestAddMCPConnection_CompensatingDetachFailureIsNotSwallowed

    # ...and preserves pre-existing live state across a refused re-add (D-381).
    # Before the transactional redesign, compensation had to undo only what
    # THIS call created: an unconditional teardown turned a refused re-add of
    # an ALREADY-LIVE connection into a destructive operation. The transaction
    # now refuses a stale request before it prepares or publishes either plane,
    # so its replacement tests pin that no preparation, lifecycle, or detach
    # occurs while the existing connection/provider stays live. These names are
    # explicit because the fresh-name tests above stay green through the
    # pre-existing-state regression.
    assert_go_tests_pass "${P221_GOLOG}" '-race -count=1 ./internal/runtime/agentcfg/protocol/' \
        'phase 221: a stale re-add preserves the existing connection/provider and never prepares a rejected attempt' \
        TestAddMCPConnection_RevisionConflict_KeepsAStillDeclaredConnection \
        TestAddMCPConnection_RevisionConflict_KeepsAStillDeclaredWireProvider \
        TestAddMCPConnection_RevisionConflict_DoesNotPrepareProviderOrConnection \
        TestAddMCPConnection_FailedAttach_KeepsAStillDeclaredWireProvider \
        TestAddMCPConnection_RevisionConflict_EmitsNoLifecycleForUnstartedAttempt \
        TestAddMCPConnection_StaleExpectation_NeverNeedsCompensatingDetach

    # ...and the THREE disk states one identical error value can hide (D-381
    # closing D-380's finding against this door). "The write failed" is what the
    # store SAID: a deadline firing after commit reports failure over a write
    # that landed, and the pre-D-381 door detached on it, leaving a config entry
    # naming a server nothing serves. The declared-ness re-read answers it with
    # no second mechanism, because "did the write land?" and "does the pointer
    # name it?" are the same question. The third row pins the UNKNOWN answer:
    # the current transactional path retains and reports it rather than
    # destructively guessing, so that safety boundary cannot change silently.
    assert_go_tests_pass "${P221_GOLOG}" '-race -count=1 ./internal/runtime/agentcfg/protocol/' \
        'phase 221: a write REPORTED failed but LANDED keeps its connection; a partial landing is still detached; the unknown answer is loud' \
        TestAddMCPConnection_WriteLandedThenErrored_DoesNotDetach \
        TestAddMCPConnection_RecordLandedPointerStuck_DetachesAndIsCorrect \
        TestAddMCPConnection_WriteLandedButPointerUnreadable_RetainsLoudly
fi

# The OWNER-SCOPING guard, asserted on the SOURCE because its consequence is
# invisible at runtime: a compensating detach addressed to the wrong owner is a
# SILENT no-op (Registry.Deregister answers a foreign owner with
# ErrServerNotFound; detachSource swallows it as idempotent), so the leak
# returns with every test still green. The double now models owner scoping
# structurally, and this pins the production call passing the CALLER's tenant.
#     Mutation (pass a literal tenant to DetachConnection) turns this OK -> FAIL.
ADDCONN_GO='internal/runtime/agentcfg/protocol/addconnection.go'
assert_grep_present 'DetachConnection\(ctx, id\.TenantID, agentID, desc\.Name\)' "${ADDCONN_GO}" \
    'phase 221: the compensating detach is addressed to the CALLER tenant + agent (a foreign owner is a silent no-op)'
assert_grep_present 'wireProviderIsNew && !providerDeclared' "${ADDCONN_GO}" \
    'phase 221: the compensating uninstall skips a wire provider the ACTIVE revision still declares'

smoke_summary
