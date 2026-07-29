#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 212 — artifact read-path byte correctness + a classified
# reference-resolution observation.
#
# The surfaces this phase changes are a BUILTIN TOOL and a RUNTIME SEAM.
# There is no `tools.invoke` Protocol method — `internal/protocol/methods`
# has none — so there is nothing to curl. What the preflight gate is for
# here is that the guards exist IN SOURCE and that the behavioural suites
# pass, so deleting one fails preflight rather than only a package test a
# reviewer might not run.
#
# Every static guard below was mutation-verified: the thing it names was
# removed from the real tree and the guard was observed to turn OK into
# FAIL — never into a SKIP.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

FETCH_SRC="internal/tools/builtin/artifact_fetch.go"
CLASS_SRC="internal/planner/observation_class.go"
PARALLEL_OBS_SRC="internal/planner/parallel_observation.go"
DISPATCH_SRC="internal/runtime/dispatch/dispatch.go"
RUNLOOP_SRC="internal/runtime/steering/runloop.go"
PROMPT_SRC="internal/planner/react/prompt.go"
PROTO_SRC="internal/protocol/artifacts.go"
GLOSSARY="docs/glossary.md"

# run_phase212_test <run-regex> <package> <description>
# Runs one go test selection and records OK / FAIL, printing the output
# on failure so a preflight log names the assertion that broke.
run_phase212_test() {
    local pattern="$1" pkg="$2" desc="$3" log
    log="$(mktemp -t phase-212-XXXXXX)"
    if go test -race -run "${pattern}" -count=1 "${pkg}" >"${log}" 2>&1; then
        ok "${desc}"
    else
        fail "${desc}"
        printf -- '--- go test output ---\n'
        cat "${log}"
    fi
    rm -f "${log}"
}

# ----------------------------------------------------------------------------
# 1. The admissibility gate exists at all.
# ----------------------------------------------------------------------------

if [ ! -f "${CLASS_SRC}" ]; then
    skip "phase 212: ${CLASS_SRC} absent (read-path correctness not yet implemented)"
    smoke_summary
    exit 0
fi

# NOT a second assertion on CLASS_SRC: the skip gate above already
# decided that, so an assert_file on the same path could only ever report
# OK — a guard nobody has watched fail is not evidence (§4.2 item 5).
# The integration test is the file this asserts, because nothing else
# here would notice its deletion.
assert_file "test/integration/artifact_readpath_test.go" \
    "phase 212: the end-to-end read-path suite is present"

# The gate's import. Without it there is no rune discipline at all.
assert_grep_present '"unicode/utf8"' "${FETCH_SRC}" \
    "phase 212: the read path imports unicode/utf8 — the admissibility gate's basis"

# The floor that kills the paging livelock. Pinned to the ASSIGNMENT
# inside effectiveMax, not to any mention of the constant: a comment
# naming utf8.UTFMax with the branch deleted would leave the livelock in
# place.
assert_grep_present 'requested = utf8\.UTFMax' "${FETCH_SRC}" \
    "phase 212: effectiveMax floors its result at utf8.UTFMax (4)"

# The unguarded conversion is gone. This is the defect itself: a string
# built straight from raw bytes is what encoding/json rewrites to U+FFFD.
assert_grep_absent 'Content:[[:space:]]+string\(window\)' "${FETCH_SRC}" \
    "phase 212: the unguarded raw-bytes-to-string conversion is gone"

# The guarded replacement, and the refusal that stands in for it.
assert_grep_present 'func admissibleWindow\(' "${FETCH_SRC}" \
    "phase 212: the admissibility gate is its own reviewable function"
assert_grep_present 'is not readable as text' "${FETCH_SRC}" \
    "phase 212: an inadmissible window refuses with a model-readable message"
assert_grep_present 'artifact-reference parameter' "${FETCH_SRC}" \
    "phase 212: the refusal names the by-reference route, so the next decision has a destination"

# The truthful offset. Pinned to the RESPONSE field assignment: echoing
# the requested offset is exactly the shape this phase replaces.
assert_grep_present 'Offset:[[:space:]]+win\.start' "${FETCH_SRC}" \
    "phase 212: the response reports where the window ACTUALLY begins, not the requested offset"

# ----------------------------------------------------------------------------
# 2. The classification — and the sentinel's first consumer.
# ----------------------------------------------------------------------------

assert_grep_present 'ObservationClassArtifactRefNotFound[[:space:]]+ObservationClass = "artifact_ref_not_found"' \
    "${CLASS_SRC}" \
    "phase 212: the model-recoverable class is declared with its wire value"
assert_grep_present 'ObservationClassArtifactResolverUnavailable[[:space:]]+ObservationClass = "artifact_resolver_unavailable"' \
    "${CLASS_SRC}" \
    "phase 212: the operator-misconfiguration class is declared with its wire value"
assert_grep_present 'ObservationClassKey = "error_class"' "${CLASS_SRC}" \
    "phase 212: the observation key the class travels under is single-sourced"
assert_grep_present 'ErrorClass ObservationClass `json:"error_class,omitempty"`' "${PARALLEL_OBS_SRC}" \
    "phase 212: a parallel branch carries the class, omitted when there is none"

# THE point of the phase's second half: the sentinel finally has an
# errors.Is on it. Pinned to the sentinel BY NAME inside the classifier.
assert_grep_present 'errors\.Is\(err, ErrArtifactRefNotFound\)' "${DISPATCH_SRC}" \
    "phase 212: dispatch classifies on ErrArtifactRefNotFound — the sentinel's first consumer"
assert_grep_present 'errors\.Is\(err, ErrArtifactStoreUnavailable\), errors\.Is\(err, artifactref\.ErrNoResolver\)' \
    "${DISPATCH_SRC}" \
    "phase 212: the resolver-unavailable class is computed over BOTH existing sentinels"
# No second sentinel: two declarations for one fact is the §13 shape.
assert_grep_absent 'ErrResolveFailed|ErrArtifactResolveFailed' "${DISPATCH_SRC}" \
    "phase 212: no sibling sentinel was declared for a fact ErrArtifactRefNotFound already carries"

# Both dispatch paths stamp it.
assert_grep_present 'classify\(fmt\.Errorf\("tool %q invoke: %w"' "${DISPATCH_SRC}" \
    "phase 212: the single-call path classifies its invoke error"
assert_grep_present 'ErrorClass: observationClassOf\(r\.Err\)' "${DISPATCH_SRC}" \
    "phase 212: the parallel path stamps the same class onto the failing branch"

# The run loop renders it onto the step's observation.
assert_grep_present 'errPayload\[planner\.ObservationClassKey\] = string\(class\)' "${RUNLOOP_SRC}" \
    "phase 212: the run loop writes the class onto the step's error observation"

# ----------------------------------------------------------------------------
# 3. The two model-facing descriptions of one tool agree.
# ----------------------------------------------------------------------------

assert_grep_present 'offset \+ returned_bytes' "${PROMPT_SRC}" \
    "phase 212: <heavy_results> states the concrete paging rule (it was silent about offset)"
assert_grep_present 'mime field before calling' "${PROMPT_SRC}" \
    "phase 212: <heavy_results> names the mime discriminator to read BEFORE calling"
# Scoped to the SECTION's own phrasing, not to the words "full payload":
# the field-aware preview footer legitimately still says "retrieve the
# full payload", because a field-aware preview is JSON and JSON is text.
assert_grep_absent 'retrieve the full payload of a stored result' "${PROMPT_SRC}" \
    "phase 212: the stale full-payload promise is gone from <heavy_results>"
assert_grep_present 'This tool returns TEXT' "${FETCH_SRC}" \
    "phase 212: the tool's own <available_tools> description carries the text-only rule"

# ----------------------------------------------------------------------------
# 4. The twin divergence is argued where a future reader will find it.
# ----------------------------------------------------------------------------

assert_grep_present 'SHORT-READ an operator' "${FETCH_SRC}" \
    "phase 212: the tool-side godoc names which invariant is shared and which is not"
assert_grep_present 'deliberate divergence from the tool-side twin' "${PROTO_SRC}" \
    "phase 212: the Protocol-side godoc carries the mirror argument"

# ----------------------------------------------------------------------------
# 5. The reconciled docs. `eof` is a field no response has ever carried.
# ----------------------------------------------------------------------------

assert_grep_absent '`eof`' "${GLOSSARY}" \
    "phase 212: the glossary stops naming an eof field on the artifact read response"

# ----------------------------------------------------------------------------
# 6. The behavioural suites. A static guard proves a line exists; these
#    prove it does what it claims.
# ----------------------------------------------------------------------------

run_phase212_test 'TestArtifactFetch_|TestFetchBounds_|TestAdmissibleWindow_|TestArtifactWindow_|TestResolveFetchBounds_' \
    './internal/tools/builtin/' \
    "phase 212: the admissibility matrix, the reassembly + strict-progress invariants over the full (offset x max_bytes) cross-product, the terminating paging loop, the rune floor, the refusal shape, and the N=128 concurrent-reuse run"

run_phase212_test 'TestObservationClass|TestParallelBranchObservation_Class' './internal/planner/' \
    "phase 212: the class reads through a three-deep wrap chain, preserves the underlying message, and round-trips through JSON with and without the key"

run_phase212_test 'TestExecutor_CallTool_UnresolvableRef|TestExecutor_CallTool_NoStoreWired|TestObservationClassOf_|TestClassify_|TestExecutor_CallTool_ToolsOwnError|TestExecutor_CallParallel_Branch|TestExecutor_CallParallel_ToolsOwn' \
    './internal/runtime/dispatch/' \
    "phase 212: both dispatch paths classify against a real reference-consuming tool and a real store, the class set is closed at two, and a tool's own error is byte-identical to the pre-phase shape"

run_phase212_test 'TestRunLoop_UnresolvableArtifactRef|TestRunLoop_NoArtifactStoreWired|TestRunLoop_ToolsOwnError|TestRunLoop_ClassifiedObservation' \
    './internal/runtime/steering/' \
    "phase 212: the class lands on Step.Observation AND Step.LLMObservation independently, survives persistence, and is absent for an ordinary tool error"

run_phase212_test 'TestBuildSystemContent_HeavyResults|TestRenderObservationForLLM_CarriesTheErrorClass|TestDefaultBuilder_GoldenDefaultPrompt' \
    './internal/planner/react/' \
    "phase 212: <heavy_results> carries the paging rule, the binary caveat and the mime discriminator, and the class reaches the RENDERED prompt text"

run_phase212_test 'TestArtifactsGetHandler_Binary|TestArtifactsGetHandler_OffsetWindows|TestArtifactsGetHandler_PagesTheWholeArtifact' \
    './internal/protocol/' \
    "phase 212: the Protocol byte read is still byte-exact for binary at every offset and bound — the trip-wire for a consistency refactor that would short-read an operator's PDF"

run_phase212_test 'TestE2E_ArtifactReadPath' './test/integration/' \
    "phase 212: end to end across the inmem and sqlite drivers — binary refuses, text pages byte-exactly, an unresolvable reference reaches the next planner turn classified, identity propagates on every leg, and three failure modes are covered"

smoke_summary
