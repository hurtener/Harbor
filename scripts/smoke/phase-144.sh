#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 144 smoke — typed embed binding: RunTyped[T] + the shared
# schema-derivation home (D-273). docs/plans/phase-144-typed-embed-binding.md
#
# Assertions:
#   1. go test -race for internal/tools/schema (the promoted deriver +
#      golden corpus).
#   2. go test -race for the RunTyped tests in internal/runtime/assemble
#      (happy path, unsupported-T rejection, WithOutputSchema conflict,
#      unmarshal-mismatch, the mandatory mixed-type D-025 concurrent-
#      reuse stress).
#   3. go test -race for the phase 144 E2E (test/integration/).
#   4. The dependency-direction grep: internal/tools/schema does NOT
#      import a concrete tool driver (driver -> schema, never the
#      reverse), and internal/runtime/flow no longer imports the inproc
#      driver directly (the pre-existing §13 seam violation this phase
#      closes) — it consumes the neutral schema package instead.
#   5. The exact two-func facade allow-list (D-273 amending D-205 item
#      1): sdk/tools/inproc.RegisterFunc + sdk/assemble.RunTyped are the
#      ONLY func-bearing production files under sdk/.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

TMPDIR="$(mktemp -d -t harbor-phase144-XXXXXX)"
trap 'rm -rf "${TMPDIR}"' EXIT

# --- 1. internal/tools/schema — the promoted deriver ------------------------

schema_test_log="${TMPDIR}/schema-test.log"
if go test -race -count=1 ./internal/tools/schema/... >"${schema_test_log}" 2>&1; then
    ok 'phase 144: internal/tools/schema tests pass under -race (golden corpus + unsupported-shape table)'
else
    fail 'phase 144: internal/tools/schema tests FAILED under -race (see tail)'
    tail -40 "${schema_test_log}" | sed 's/^/    /'
fi

# --- 2. RunTyped unit tests + the mandatory D-025 concurrent-reuse stress ---

assert_grep_present 'TestRunTyped_ConcurrentReuse_MixedTypes_NoBleedNoLeak' \
    internal/runtime/assemble/runtyped_test.go \
    'phase 144: N>=100 mixed-type concurrent-reuse RunTyped -race test exists'

runtyped_log="${TMPDIR}/runtyped-test.log"
if go test -race -count=1 -run 'TestRunTyped' ./internal/runtime/assemble/... >"${runtyped_log}" 2>&1; then
    ok 'phase 144: RunTyped unit tests pass under -race'
else
    fail 'phase 144: RunTyped unit tests FAILED under -race (see tail)'
    tail -40 "${runtyped_log}" | sed 's/^/    /'
fi

# --- 3. The §17 integration E2E ----------------------------------------------

assert_file test/integration/phase144_runtyped_test.go 'phase 144: the RunTyped E2E test exists'

e2e_log="${TMPDIR}/e2e-test.log"
if go test -race -count=1 -run 'Phase144' ./test/integration/... >"${e2e_log}" 2>&1; then
    ok 'phase 144: RunTyped E2E (happy path, corrective-retry + identity propagation, exhaustion) passes under -race'
else
    fail 'phase 144: RunTyped E2E FAILED under -race (see tail)'
    tail -40 "${e2e_log}" | sed 's/^/    /'
fi

# --- 4. Dependency-direction grep: schema never imports a concrete driver ---

assert_grep_absent '"github.com/hurtener/Harbor/internal/tools/drivers' \
    internal/tools/schema/schema.go \
    'phase 144: internal/tools/schema imports no concrete tool driver (driver -> schema, never the reverse)'
assert_grep_present 'internal/tools/schema' \
    internal/tools/drivers/inproc/inproc.go \
    'phase 144: the inproc driver re-bases on the shared schema package'

# The pre-existing §13 seam violation this phase closes: the flow engine
# used to import the CONCRETE inproc driver just to reach DeriveSchema.
# It now depends on the neutral schema package instead.
assert_grep_absent '"github.com/hurtener/Harbor/internal/tools/drivers/inproc"' \
    internal/runtime/flow/flow.go \
    'phase 144: internal/runtime/flow no longer imports the concrete inproc driver (the §13 fix)'
assert_grep_present 'internal/tools/schema' \
    internal/runtime/flow/flow.go \
    'phase 144: internal/runtime/flow derives schemas via the shared schema package'

# --- 5. The enumerated facade func allow-list (D-273 amending D-205) --------
#
# FUNC-level, not file-level, driven by ONE spec list (`file|name-regex|
# name|func-count`): the file-set constraint and each file's func count are
# derived from the list, so a decision-gated addition appends ONE entry.

allowed_func_specs='sdk/tools/inproc/inproc.go|^func RegisterFunc\[|RegisterFunc|1
sdk/assemble/runtyped.go|^func RunTyped\[|RunTyped|1
sdk/server/server.go|^func Open\(|Open|1'
allowed_func_files=$(echo "${allowed_func_specs}" | cut -d'|' -f1)
allowed_file_count=$(echo "${allowed_func_specs}" | grep -c '^')
func_files=$(grep -lE '^func ' -r sdk/ --include='*.go' --exclude='*_test.go' | sed 's#^\./##' | sort -u) || func_files=""
unexpected_func_files=$(comm -23 <(echo "${func_files}") <(echo "${allowed_func_files}" | sort -u))
if [ -z "${unexpected_func_files}" ] && [ "$(echo "${func_files}" | grep -c '^')" -eq "${allowed_file_count}" ]; then
    ok "phase 144: EXACTLY the ${allowed_file_count}-entry facade func allow-list under sdk/"
else
    fail "phase 144: the sdk/ facade func allow-list drifted — found: ${func_files}"
fi

phase144_single_func_ok=1
while IFS='|' read -r f want_re want_name want_count; do
    count=$(grep -cE '^func ' "$f") || count=0
    if [ "${count}" -ne "${want_count}" ] || ! grep -qE "${want_re}" "$f"; then
        fail "phase 144: ${f} must declare EXACTLY ${want_count} func(s) incl. the enumerated forward ${want_name} (found ${count} func bodies)"
        phase144_single_func_ok=0
    fi
done <<< "${allowed_func_specs}"
if [ "${phase144_single_func_ok}" -eq 1 ]; then
    ok 'phase 144: each allow-listed file declares its enumerated func count — the generic forwards (func-level gate)'
fi

smoke_summary
