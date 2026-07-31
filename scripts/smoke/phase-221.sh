#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 221 — an expected-revision token on the agent-config writes (D-366).
#
# SKELETON. The phase is not implemented yet; every assertion below is present
# as a commented-out block naming the exact guard it becomes, and the script
# skips so preflight stays green. When the phase lands: uncomment the blocks,
# delete the trailing `skip`, and verify each guard by MUTATION (the plan's
# "Smoke script additions" lists the eight mutations). A guard that stays green
# under its own mutation is decorative and does not count toward the
# done-definition.
#
# Classified `live-server` from the skeleton onward: the finished script hits
# the booted dev server, and misclassifying a server-touching smoke as
# static-only produces nondeterministic flakes (D-104).
#
# Will assert:
#   - static: `expected_content_hash` is declared on ALL SIXTEEN spine-writing
#     request types — an EXACT count, so dropping one door FAILS rather than
#     passing a ">= 1" presence check.
#   - static: agentcfg carries SetOptions + ErrRevisionConflict, and the driver
#     evaluates the precondition BEFORE the idempotent-re-set short-circuit.
#     The ordering is asserted by LINE NUMBER, because a transposition is the
#     one mutation that leaves every grep-for-presence guard green while
#     turning a stale token into a misleading 200.
#   - static: CodeRevisionConflict is registered in canonicalCodes and bound to
#     http.StatusConflict in the control transport's status mapping.
#   - static: the conformance suite carries the four precondition rows, so a
#     second agentcfg driver cannot ship the interface without them (RFC §9).
#   - static: ProtocolVersion is UNCHANGED — the change is additive.
#   - static: the SINGLE-PROCESS BOUND appears verbatim in the SetOptions
#     godoc. This guards the HONESTY of the claim, not only its existence: the
#     compare-and-write is atomic in-process only (the StateStore has no CAS
#     primitive) and no text may claim otherwise.
#   - live: a conditional write carrying a bogus expected_content_hash answers
#     a typed `revision_conflict` with HTTP 409 — and specifically NOT
#     `invalid_request`, which is what a server that silently ignored an
#     unknown field would answer.
#   - live: an unauthenticated conditional write is still refused by the
#     identity/admin gate — the token is a PRECONDITION, never an authority.
#   - unit-tests: the driver's conditional-write tests, the conformance rows,
#     the N=128 exactly-one-wins race, and the sixteen-door table, under -race.
#
# Route classification uses an EMPTY body deliberately (the phase-211 lesson):
# a mounted route answers a typed `invalid_request`, an unmounted one answers
# `unknown_method`. Classifying on bare status would convert a real answer into
# a SKIP, and "a SKIP that should be an OK is a bug" (§4.2 item 5).
#
# Done-definition: OK >= 12, FAIL = 0 against the live preflight build.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# TYPES_GO='internal/protocol/types/agentconfig.go'
# ERRORS_GO='internal/protocol/errors/errors.go'
# STATUS_GO='internal/protocol/transports/control/status.go'
# AGENTCFG_GO='internal/agentcfg/agentcfg.go'
# DRIVER_GO='internal/agentcfg/drivers/statestore/statestore.go'
# CONFORMANCE_GO='internal/agentcfg/conformance/conformance.go'
# VERSION_GO='internal/protocol/types/version.go'

# ---------------------------------------------------------------------------
# Static guards. Signatures: assert_grep_present <pattern> <path> <desc>
#                            assert_grep_count   <pattern> <path> <n> <desc>
# Patterns use POSIX classes ([[:space:]]) — never \t / \d, which BSD grep
# matches and GNU grep does not (a guard written that way silently never fires
# on Linux CI).
# ---------------------------------------------------------------------------
#
# assert_grep_count 'ExpectedContentHash string' "${TYPES_GO}" 16 \
#     'phase 221: all sixteen spine request types carry expected_content_hash'
#
# assert_grep_present 'type SetOptions struct' "${AGENTCFG_GO}" \
#     'phase 221: agentcfg.SetOptions declared'
# assert_grep_present 'ErrRevisionConflict = errors\.New' "${AGENTCFG_GO}" \
#     'phase 221: agentcfg.ErrRevisionConflict sentinel declared'
#
# # The ordering guard — the precondition MUST precede the idempotent re-set.
# PRECOND_LINE="$(grep -n 'opts\.ExpectedContentHash != ""' "${DRIVER_GO}" | head -1 | cut -d: -f1)"
# IDEMPOTENT_LINE="$(grep -n 'active\.ContentHash == hash' "${DRIVER_GO}" | head -1 | cut -d: -f1)"
# if [ -n "${PRECOND_LINE}" ] && [ -n "${IDEMPOTENT_LINE}" ] && [ "${PRECOND_LINE}" -lt "${IDEMPOTENT_LINE}" ]; then
#     ok 'phase 221: the precondition is evaluated BEFORE the idempotent re-set short-circuit'
# else
#     fail 'phase 221: precondition/idempotence ordering wrong or missing — a stale token could be answered 200'
# fi
#
# assert_grep_present 'CodeRevisionConflict Code = "revision_conflict"' "${ERRORS_GO}" \
#     'phase 221: CodeRevisionConflict declared'
# assert_grep_present 'CodeRevisionConflict:[[:space:]]+\{\}' "${ERRORS_GO}" \
#     'phase 221: CodeRevisionConflict registered in canonicalCodes'
# assert_grep_present 'CodeRevisionConflict' "${STATUS_GO}" \
#     'phase 221: CodeRevisionConflict bound to a status in the control transport'
#
# assert_grep_count 'ExpectedContentHash' "${CONFORMANCE_GO}" 4 \
#     'phase 221: the conformance suite carries the four precondition rows'
#
# assert_grep_present 'ProtocolVersion = "0\.1\.0"' "${VERSION_GO}" \
#     'phase 221: ProtocolVersion is unchanged (the change is additive)'
#
# # The honesty guard — the bound must be stated where the option is declared.
# assert_grep_present 'NOT atomic across Runtime processes' "${AGENTCFG_GO}" \
#     'phase 221: the SetOptions godoc states the single-process bound'

# ---------------------------------------------------------------------------
# Live guards. The block degrades to its own SKIP rather than exiting, so the
# static + unit legs still run on a standalone invocation.
# ---------------------------------------------------------------------------
#
# ROUTE="$(api_url /v1/agent_config/set_revision)"
#   1. classify with an EMPTY body (see the header note): a typed
#      `invalid_request` means mounted; `unknown_method` means not mounted.
#   2. bogus expected_content_hash -> code == "revision_conflict", status 409
#      (asserting the CODE, never the status alone — a server that ignored the
#      unknown field would answer a different code at a similar status).
#   3. no bearer -> the identity/admin gate still refuses, proving the token
#      did not become an authority.

# ---------------------------------------------------------------------------
# Unit-test legs.
# ---------------------------------------------------------------------------
#
#   go test -race -run 'TestSetRevision_ConditionalWrite|TestRollback_ConditionalWrite' ./internal/agentcfg/drivers/statestore/
#   go test -race -run 'Conditional' ./internal/agentcfg/ ./internal/agentcfg/drivers/statestore/
#   go test -race -run 'TestSetRevision_Concurrent' ./internal/agentcfg/drivers/statestore/
#   go test -race -run 'TestConditionalWrite' ./internal/runtime/agentcfg/protocol/

skip "phase 221: smoke skeleton — expected-revision token not yet implemented"

smoke_summary
