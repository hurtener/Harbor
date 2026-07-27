#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 210 — in-process pass-by-reference tool routing + the substitution
# invariant.
#
# The phase's surface is a RUNTIME SEAM, not an HTTP endpoint: a tool
# declares an artifact-reference parameter, the model supplies an id, and
# the runtime resolves it at dispatch. There is nothing to curl. What the
# preflight gate is for here is the pair of MECHANICAL guards the
# invariant rests on — the one-substitution-call-site bound and the
# widened LLM-edge arrival check — plus the source-level declarations
# each depends on, so deleting one fails preflight rather than only
# `go test` in a package a reviewer might not run.
#
# Every static guard below was verified to turn OK into FAIL when the
# thing it names is removed.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

AREF_DIR="internal/tools/artifactref"
AREF_SRC="${AREF_DIR}/artifactref.go"
AREF_SCAN="${AREF_DIR}/scan.go"
SAFETY_SRC="internal/llm/safety.go"
INPROC_SRC="internal/tools/drivers/inproc/inproc.go"
SCHEMA_SRC="internal/tools/schema/schema.go"
DISPATCH_SRC="internal/runtime/dispatch/dispatch.go"
ARTIFACTS_SRC="internal/protocol/artifacts.go"

# run_phase210_test <run-regex> <package> <description>
# Runs one go test selection and records OK / FAIL, printing the output
# on failure so a preflight log names the assertion that broke.
run_phase210_test() {
    local pattern="$1" pkg="$2" desc="$3" log
    log="$(mktemp -t phase-210-XXXXXX)"
    if go test -run "${pattern}" -count=1 "${pkg}" >"${log}" 2>&1; then
        ok "${desc}"
    else
        fail "${desc}"
        printf -- '--- go test output ---\n'
        cat "${log}"
    fi
    rm -f "${log}"
}

# ----------------------------------------------------------------------------
# 1. The primitive exists.
# ----------------------------------------------------------------------------

if [ ! -f "${AREF_SRC}" ]; then
    skip "phase 210: ${AREF_SRC} absent (pass-by-reference routing not yet implemented)"
    smoke_summary
    exit 0
fi

assert_file "${AREF_SRC}" \
    "phase 210: the artifact-reference parameter carrier is present"
assert_file "${AREF_SCAN}" \
    "phase 210: the substitution scan is present"

# The carrier projects its ID through every serialisation door. Losing any
# one of the three turns "the resolved value cannot be serialised" from a
# type property back into a rule contributors have to remember.
assert_grep_present 'func \(r Ref\) MarshalJSON\(\)' "${AREF_SRC}" \
    "phase 210: Ref serialises to JSON as its reference id"
assert_grep_present 'func \(r Ref\) String\(\) string' "${AREF_SRC}" \
    "phase 210: Ref formats as its reference id"
assert_grep_present 'func \(r Ref\) LogValue\(\) slog\.Value' "${AREF_SRC}" \
    "phase 210: Ref logs as its reference id"

# The resolved value must be unreachable from outside the package.
assert_grep_absent 'func \(r Ref\) Value\(\)' "${AREF_SRC}" \
    "phase 210: the carrier exposes no raw-value accessor beside Bytes"

# ----------------------------------------------------------------------------
# 2. The production bound — ONE substitution call site, held there by the
#    AST scan. This is the primary mechanism; the guard is the scan's own
#    live test, run against the real tree.
# ----------------------------------------------------------------------------

assert_grep_present 'func ScanSubstitutionSites\(' "${AREF_SCAN}" \
    "phase 210: the substitution scan is exported for its gate test"
# Resolving by import PATH rather than by the conventional qualifier is
# what keeps an aliased import from silently evading the scan.
assert_grep_present 'PkgPath = "github.com/hurtener/Harbor/internal/tools/artifactref"' "${AREF_SCAN}" \
    "phase 210: the scan resolves this package by import path (an alias is followed)"
# A bound counted in CALL positions is no bound once the function itself
# can travel; the scan reports the writer named as a value too.
assert_grep_present 'KindSubstitutionValueRef' "${AREF_SCAN}" \
    "phase 210: the scan flags the substitution writer taken as a VALUE, not only called"

# The WHOLE package, not a name prefix. A `TestSubstitution_` selector
# ran the scan tests and nothing else — the carrier's own tests
# (TestSubstitute_*, TestRef_*, TestTypeContainsRef_*, TestWithResolver_*)
# are what pin the projection that keeps resolved bytes out of every JSON
# door, and a selector that skips them is a guard that cannot fail.
run_phase210_test '.' "./${AREF_DIR}/" \
    "phase 210: the artifactref package is green in full — the one-reviewed-call-site scan (with its non-vacuity, alias, lookalike, second-site and stale-registration pins) AND the carrier's projection, resolver and type-walk tests"

# ----------------------------------------------------------------------------
# 3. The arrival check — findContextLeak walks ToolCalls[].Args.
#    A source guard as well as a test guard, because this arm is one loop
#    that a refactor could drop without any other test noticing.
# ----------------------------------------------------------------------------

# Pinned to the leak check's OWN loop header: `validateToolCallPairing`
# also ranges over `m.ToolCalls`, so a looser pattern would match the
# wrong arm and pass with the widening deleted.
assert_grep_present 'for ci, tc := range m\.ToolCalls' "${SAFETY_SRC}" \
    "phase 210: the LLM-edge leak check walks tool-call arguments"
assert_grep_present 'ToolCalls\[%d\]\.Args' "${SAFETY_SRC}" \
    "phase 210: the leak site names the offending tool call by index"

run_phase210_test 'TestFindContextLeak_ToolCallArgs_|TestSafety_OversizeToolCallArgs_|TestSafety_OrdinaryToolCallArgs_' './internal/llm/' \
    "phase 210: the widened arm bites above the threshold, at it, and per parallel branch — while an ordinary tool call still completes"

# The widening is exactly one field. If this regressed, the scope claim in
# the phase plan stopped being true.
assert_grep_present 'offloadableText := m\.Role == RoleTool' "${SAFETY_SRC}" \
    "phase 210: the conversation-text exemption is untouched"

# ----------------------------------------------------------------------------
# 4. The seam: the deriver renders a reference as a string, the driver
#    binds it once per invocation, and dispatch seats a triple-scoped
#    resolver.
# ----------------------------------------------------------------------------

# Pinned to the deriver's BRANCH, not the type variable: a declaration
# nothing dispatches on would leave the model reading the carrier struct.
assert_grep_present 'if t == artifactRefType \{' "${SCHEMA_SRC}" \
    "phase 210: the schema deriver has an artifact-reference case"
# Pinned to the REGISTRATION-time computation, not to any use of the
# flag: computing it per invocation would put mutable per-call work where
# the descriptor is supposed to be immutable.
assert_grep_present 'hasArtifactRefs := artifactref\.TypeContainsRef' "${INPROC_SRC}" \
    "phase 210: the in-process driver decides at REGISTRATION whether a tool takes a reference"
assert_grep_present 'artifactref\.Substitute\(ctx, &in\)' "${INPROC_SRC}" \
    "phase 210: the driver binds the resolved value onto the DECODED argument value"
# The argument JSON must reach Invoke unrewritten — rewriting it would put
# the resolved value into the very artefact the trajectory persists.
assert_grep_present 'result, err := desc\.Invoke\(ctx, d\.Args\)' "${DISPATCH_SRC}" \
    "phase 210: dispatch hands the tool the model's own argument JSON, unrewritten"
assert_grep_present 'func \(e \*toolExecutor\) withArtifactResolver' "${DISPATCH_SRC}" \
    "phase 210: dispatch seats the run-scoped artifact resolver"
# The read key is the isolation triple. A TaskID in the resolver's scope
# would reintroduce the enumerate-then-fail divergence the read key closed.
assert_grep_absent 'TaskID:[[:space:]]*rc\.Quadruple' "${DISPATCH_SRC}" \
    "phase 210: the resolver scopes on the isolation triple, never the task"

run_phase210_test 'TestDerive_ArtifactRef' './internal/tools/schema/' \
    "phase 210: an artifact-reference parameter derives to a plain string the model can author"
run_phase210_test 'TestRegisterFunc_ArtifactRefParam|TestInvoke_' './internal/tools/drivers/inproc/' \
    "phase 210: the driver resolves at dispatch, leaves the argument JSON alone, and fails loud without a resolver (incl. the N=128 concurrent-reuse run)"

# ----------------------------------------------------------------------------
# 5. The invariant, end to end against real drivers.
# ----------------------------------------------------------------------------

run_phase210_test 'TestE2E_PassByRef_' './test/integration/' \
    "phase 210: the resolved value reaches no observation, trajectory, event payload or log — and cross-tenant / unknown / store-less references fail loud"

# ----------------------------------------------------------------------------
# 6. The stored-bytes contract stops describing a transform.
# ----------------------------------------------------------------------------

assert_grep_absent 'may[[:space:]]+rewrite or refuse' "${ARTIFACTS_SRC}" \
    "phase 210: handlePut no longer claims the redactor rewrites the stored payload"
# Counted rather than merely present: the contract is stated in three
# places (the surface godoc, handlePut's godoc, and the Redact call
# site), and losing any ONE of them re-opens the reading that a stored
# artifact is a redacted artifact.
assert_grep_count '(ADMISSION|admission) ?(GATE|gate)?' "${ARTIFACTS_SRC}" 3 \
    "phase 210: the redactor is named an admission gate at all three sites"
assert_grep_present 'PutBytes\(ctx, scope, req\.Bytes, opts\)' "${ARTIFACTS_SRC}" \
    "phase 210: the stored bytes are the ones the author supplied"

# ----------------------------------------------------------------------------
# 7. The third-party arm is deferred, not stubbed. A half-built egress
#    surface would be worse than none: the blockers are design questions,
#    and a stub invites a caller before any of them is answered.
# ----------------------------------------------------------------------------

assert_grep_absent 'GrantURL|PresignedGrant|LoanURL|grant_token' "${AREF_DIR}" \
    "phase 210: no third-party grant surface was stubbed alongside the in-process arm"

smoke_summary
