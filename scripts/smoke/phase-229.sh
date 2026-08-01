#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 229 — external release oracles.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source scripts/smoke/common.sh

assert_file docs/plans/phase-229-external-release-oracles.md 'phase 229: corrective plan exists'
assert_grep_present '^## D-391 ' docs/decisions.md 'phase 229: external-oracle decision is recorded'

P229_TMP="$(mktemp -d "${TMPDIR:-/tmp}/harbor-phase-229.XXXXXX")"
trap 'rm -rf "${P229_TMP}"' EXIT
P229_GOLOG="${P229_TMP}/go-test.log"

# The event-name oracle is handwritten beside, but not derived from, the
# registry it checks. assert_go_tests_pass additionally proves the named test
# really ran; a deleted/renamed test is red even though go test -run exits 0.
assert_go_tests_pass "${P229_GOLOG}" './cmd/harbor-gen-protocol-docs/' \
    'phase 229: every canonical event name matches the external oracle' \
    TestCanonicalEventNameOracle

# Exercise the production assessor as a black box without booting preflight.
# The function's dependencies are irrelevant on this arm: an exit-0 smoke with
# no summary must increment TOTAL_FAIL before any phase-status lookup. Extract
# the actual function body so this fixture cannot pass against a reimplemented
# copy while production regresses.
P229_NO_SUMMARY="${P229_TMP}/no-summary.out"
printf '%s\n' '[OK] fixture printed a lookalike line but no canonical summary' > "${P229_NO_SUMMARY}"
TOTAL_FAIL=0
INERT_PENDING=()
INERT_BASELINED=()
INERT_SHIPPED=()
UNRESOLVED_PHASE_ROWS=()
INERT_SEEN_BASELINED=''
eval "$(sed -n '/^assess_smoke_output() {/,/^}/p' scripts/preflight.sh)"
assess_smoke_output 'scripts/smoke/phase-fixture.sh' "${P229_NO_SUMMARY}" 0 > "${P229_TMP}/assessor.out"
if [ "${TOTAL_FAIL}" -eq 1 ] && grep -qF 'did not call smoke_summary' "${P229_TMP}/assessor.out"; then
    ok 'phase 229: summary-less exit-0 smoke fails through the production preflight assessor'
else
    fail "phase 229: summary-less smoke did not increment TOTAL_FAIL exactly once (got ${TOTAL_FAIL})"
fi

# docs_concurrency_errors <workflow>
# Prints one line per policy violation. PR docs validation may supersede an
# older run only on the SAME ref; only the main-only deploy job owns the global
# Pages singleton. This is intentionally a structural parser over the workflow,
# not a grep for the one fixed line.
docs_concurrency_errors() {
    local workflow="$1" deploy_block workflow_block
    workflow_block="$(awk '
        /^concurrency:[[:space:]]*$/ { in_concurrency=1 }
        in_concurrency { print }
        in_concurrency && /^[[:alnum:]_-]+:[[:space:]]*$/ && $0 !~ /^concurrency:/ { exit }
    ' "${workflow}")"
    if [ -z "${workflow_block}" ]; then
        printf '%s\n' 'per-ref validation concurrency block is missing'
    else
        printf '%s\n' "${workflow_block}" | grep -qF 'group: docs-${{ github.workflow }}-${{ github.ref }}' \
            || printf '%s\n' 'validation concurrency is not scoped by workflow and ref'
        printf '%s\n' "${workflow_block}" | grep -qF "  cancel-in-progress: \${{ github.event_name == 'pull_request' }}" \
            || printf '%s\n' 'validation cancellation is not restricted to pull requests'
    fi
    deploy_block="$(awk '
        /^  deploy:[[:space:]]*$/ { in_deploy=1 }
        in_deploy { print }
        in_deploy && /^  [[:alnum:]_-]+:[[:space:]]*$/ && $0 !~ /^  deploy:/ { exit }
    ' "${workflow}")"
    if [ -z "${deploy_block}" ]; then
        printf '%s\n' 'deploy job is missing'
        return
    fi
    if ! grep -qE '^    concurrency:[[:space:]]*$' <<< "${deploy_block}"; then
        printf '%s\n' 'deploy concurrency block is missing'
    fi
    if ! grep -qE '^      group:[[:space:]]*pages[[:space:]]*$' <<< "${deploy_block}"; then
        printf '%s\n' 'deploy does not own the pages group'
    fi
    if ! grep -qE '^      cancel-in-progress:[[:space:]]*false[[:space:]]*$' <<< "${deploy_block}"; then
        printf '%s\n' 'deploy cancellation policy drifted'
    fi
}

DOCS_WORKFLOW='.github/workflows/docs.yml'
DOCS_ERRORS="$(docs_concurrency_errors "${DOCS_WORKFLOW}")"
if [ -z "${DOCS_ERRORS}" ]; then
    ok 'phase 229: PR docs validation is per-ref and only Pages deploy is globally serialized'
else
    fail "phase 229: docs workflow concurrency policy failed — ${DOCS_ERRORS}"
fi

# Establish that the policy guard can fail. Reintroduce the exact issue #644
# shape in a throwaway workflow and require the parser to reject it.
MUTATED_WORKFLOW="${P229_TMP}/docs-global-concurrency.yml"
sed 's/group: docs-${{ github\.workflow }}-${{ github\.ref }}/group: pages/' \
    "${DOCS_WORKFLOW}" > "${MUTATED_WORKFLOW}"
if [ -n "$(docs_concurrency_errors "${MUTATED_WORKFLOW}")" ]; then
    ok 'phase 229: docs concurrency guard is LIVE (clean workflow OK; global-group mutation FAIL)'
else
    fail 'phase 229: docs concurrency guard accepted a workflow-global pages group'
fi

# Establish that the guard rejects the subtler cancellation defect too: an
# unconditional workflow-level true cancels an in-flight main deployment
# before the deploy job's non-cancelling Pages group can preserve it.
MUTATED_CANCELLATION="${P229_TMP}/docs-unconditional-cancellation.yml"
sed "s/cancel-in-progress: \${{ github.event_name == 'pull_request' }}/cancel-in-progress: true/" \
    "${DOCS_WORKFLOW}" > "${MUTATED_CANCELLATION}"
if [ -n "$(docs_concurrency_errors "${MUTATED_CANCELLATION}")" ]; then
    ok 'phase 229: docs concurrency guard is LIVE (unconditional workflow cancellation FAIL)'
else
    fail 'phase 229: docs concurrency guard accepted main-deployment cancellation'
fi

smoke_summary
