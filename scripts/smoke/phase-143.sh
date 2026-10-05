#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 143 smoke — run-level structured output (WithOutputSchema +
# answer_payload, D-272).
#
# There is no new HTTP / Protocol surface: the option threads through
# RunOnce and the additive answer_payload rides the existing envelope as
# opaque JSON. Correctness is verified by the race-enabled Go test suite
# (the option validation + runtime-edge validation + stream suppression +
# the D-025 mixed-traffic stress + the §13 consumer E2E). A static grep
# pins that answer_payload appears in exactly one wire-shape definition.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. Require the actual output-schema contracts under the existing race/bound.
# The full assembler now also owns cumulative-memory recovery; its complete
# package suite belongs to make test and the retained-context smoke. Including
# it in this two-minute schema gate timed out on unrelated rollover tests.
test_log="$(mktemp -t harbor-phase143-XXXXXX)"
trap 'rm -f "$test_log"' EXIT
assert_go_tests_pass "$test_log" \
    '-race -count=1 -timeout=120s ./internal/runtime/assemble ./internal/runtime/runctx ./internal/planner' \
    'phase 143: run output-schema, envelope and validator contracts' \
    TestRunOnce_WithOutputSchema_EmptySchema_FailsLoud \
    TestRunOnce_WithOutputSchema_HappyPath_ValidatedPayload \
    TestRunOnce_WithOutputSchema_ToolsEnvelopeUnwrapped_LandsInAnswerPayload \
    TestRunOnce_WithOutputSchema_InvalidOutput_ErrOutputInvalid \
    TestRunOnce_WithOutputSchema_DowngradeExhausted_ErrOutputInvalid \
    TestRunOnce_WithOutputSchema_NonGoalFinish_NoOutputInvalid \
    TestRunOnce_WithOutputSchema_StreamSuppressesTokens \
    TestRunOnce_WithOutputSchema_ConcurrentMixedTraffic_NoBleed \
    TestNewRunContext_OutputSchema_CompiledOnceAndThreaded \
    TestFinishAnswerEnvelope_Table \
    TestFinishAnswerEnvelope_AbsentSchema_ByteIdenticalGolden \
    TestFinishAnswerEnvelope_WithPayload_Golden \
    TestAnswerEnvelope_GoldenJSON_Phase106ByteCompat \
    TestAnswerEnvelope_GoldenJSON_WithPayload \
    TestAnswerEnvelope_RoundTrip \
    TestAnswerEnvelope_TaskErrorCodeForFinish \
    TestCompileOutputSchema_RejectsEmpty \
    TestCompileOutputSchema_RejectsInvalid \
    TestOutputSchemaValidator_ValidateTable \
    TestOutputSchemaValidator_RawIsImmutableCopy \
    TestOutputSchemaValidator_NilIsNoOp \
    TestOutputSchemaValidator_ConcurrentValidate

# 2. The §13 primitive-with-consumer E2E (happy / corrective-retry /
#    exhaustion / planner-agnostic deterministic leg) under -race.
if go test -race -count=1 -timeout 120s -run '^TestE2E_Phase143_' ./test/integration/... >/dev/null 2>&1; then
	ok 'phase 143: structured-output consumer E2E passes (TestE2E_Phase143_*)'
else
	fail 'phase 143: consumer E2E failed (run `go test -race -run TestE2E_Phase143_ ./test/integration/...`)'
fi

# 3. Static: answer_payload is defined in EXACTLY ONE wire-shape (the
#    AnswerEnvelope). A second `json:"answer_payload..."` tag would mean a
#    duplicate wire definition (§13: single source for a wire shape).
payload_tag_count="$(grep -rn 'json:"answer_payload' internal/ | grep -c 'AnswerPayload' || true)"
if [ "${payload_tag_count}" = "1" ]; then
	ok 'phase 143: answer_payload wire tag defined in exactly one place (AnswerEnvelope)'
else
	fail "phase 143: answer_payload wire tag appears ${payload_tag_count} times, want exactly 1 (single wire-shape source)"
fi

# 4. Static: a schema-invalid answer maps to the typed ErrOutputInvalid at
#    the runtime edge (no silent text fallback, §13). Assert the sentinel
#    exists and the runner references it.
if grep -q 'ErrOutputInvalid' internal/planner/output_schema.go &&
	grep -q 'planner.ErrOutputInvalid' internal/runtime/assemble/runonce.go; then
	ok 'phase 143: schema-invalid answer maps to the typed ErrOutputInvalid (no silent text fallback)'
else
	fail 'phase 143: ErrOutputInvalid wiring missing from the runtime edge'
fi

smoke_summary
