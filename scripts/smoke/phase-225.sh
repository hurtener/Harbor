#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 225 — strict run decoding and byte-faithful prompt tiers.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source scripts/smoke/common.sh
assert_file docs/plans/phase-225-run-prompt-fidelity.md 'phase 225: corrective plan exists'
assert_grep_present '^## D-387 ' docs/decisions.md 'phase 225: prompt-fidelity decision is recorded'

P225_TMP="$(mktemp -d "${TMPDIR:-/tmp}/harbor-phase-225.XXXXXX")"
trap 'rm -rf "${P225_TMP}"' EXIT
P225_GOLOG="${P225_TMP}/go-test.log"

assert_go_tests_pass "${P225_GOLOG}" './internal/protocol/transports/stream/' \
    'phase 225: runs.set_overrides accepts one JSON document and only trailing whitespace' \
    TestRunsHandler_SetOverrides_RejectsTrailingJSON \
    TestRunsHandler_SetOverrides_AllowsTrailingWhitespace

assert_go_tests_pass "${P225_GOLOG}" './internal/runtime/serve/' \
    'phase 225: caller-memory telemetry reports pre-redaction wire bytes without leaking content' \
    TestRunOne_CallerMemory_AdmittedAndAnnounced

assert_go_tests_pass "${P225_GOLOG}" './internal/runtime/runs/protocol/' \
    'phase 225: session personalization has one producer and cannot clear tenant guidance' \
    TestComposeLLMOverrides_ExtraInstructionsAuthorityTable \
    TestComposeLLMOverrides_UserPersonalizationHasOnlySessionProducer \
    TestComposeLLMOverrides_ConcurrentReuse_NoCrossTalk \
    TestSetOverrides_ExtraInstructionsCopiedByValue

assert_go_tests_pass "${P225_GOLOG}" './internal/planner/react/' \
    'phase 225: personalization is contained and operator blocks preserve admitted bytes' \
    TestComposition_ExtraInstructionsAuthoritySeparated_TwoProducers \
    TestComposition_UserPersonalizationCannotForgeSectionBoundary \
    TestRenderExtraSystemBlocks_BodyIsVerbatim \
    TestRenderExtraSystemBlocks_PreservesSurroundingWhitespace \
    TestRenderExtraSystemBlocks_UserLayerStaysEscaped
smoke_summary
