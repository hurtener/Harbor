#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 112a smoke — the public SDK facade (RFC §3.6, D-204/D-205).
# (docs/plans/phase-112a-sdk-facade.md)
#
# Static + unit-test assertions:
#   1. Every RFC §3.6 inventory package exists under sdk/ (plus the
#      tree-level doc.go contract statement).
#   2. The facade-integrity test imports NO internal/ package — the
#      sdk/ tree alone must carry the headless embedding story.
#   3. sdk/drivers/prod parity gate: the public aggregator's ONLY
#      Harbor import is the internal aggregator (parity by init()
#      transitivity), and it exports no identifiers.
#   4. No-behavior guard (advisory): the facade declares no func
#      bodies outside the documented generic forward (inproc's
#      RegisterFunc, which Go cannot express as a var forward).
#   5. The facade-integrity test slice passes under -race.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# --- 1. The RFC §3.6 inventory is present -----------------------------------

assert_file sdk/doc.go 'phase 112a: tree-level facade godoc (the contract statement)'

for p in \
    sdk/identity/identity.go \
    sdk/events/events.go \
    sdk/config/config.go \
    sdk/tools/tools.go \
    sdk/tools/inproc/inproc.go \
    sdk/tools/builtin/builtin.go \
    sdk/llm/llm.go \
    sdk/memory/memory.go \
    sdk/state/state.go \
    sdk/artifacts/artifacts.go \
    sdk/skills/skills.go \
    sdk/planner/planner.go \
    sdk/planner/react/react.go \
    sdk/planner/deterministic/deterministic.go \
    sdk/tasks/tasks.go \
    sdk/steering/steering.go \
    sdk/dispatch/dispatch.go \
    sdk/runctx/runctx.go \
    sdk/assemble/assemble.go \
    sdk/drivers/prod/prod.go
do
    assert_file "$p" "phase 112a: inventory package ${p%/*}"
done

# --- 2. The integrity test is internal-free ---------------------------------

assert_file test/integration/phase112a_sdk_facade_test.go 'phase 112a: facade-integrity test'
assert_grep_absent 'github.com/hurtener/Harbor/internal/' \
    test/integration/phase112a_sdk_facade_test.go \
    'phase 112a: the integrity test imports no internal/ package'
assert_grep_present 'github.com/hurtener/Harbor/sdk/drivers/prod' \
    test/integration/phase112a_sdk_facade_test.go \
    'phase 112a: the integrity test blank-imports the public aggregator'

# --- 3. sdk/drivers/prod parity gate -----------------------------------------

assert_grep_present '_ "github.com/hurtener/Harbor/internal/drivers/prod"' \
    sdk/drivers/prod/prod.go \
    'phase 112a: the public aggregator blank-imports the internal aggregator'
prod_imports=$(grep -cE '^[[:space:]]+(_ )?"github.com/hurtener/Harbor/' sdk/drivers/prod/prod.go) || prod_imports=0
if [ "${prod_imports}" -eq 1 ]; then
    ok 'phase 112a: the public aggregator imports EXACTLY the internal aggregator (parity by construction)'
else
    fail "phase 112a: sdk/drivers/prod should import exactly one Harbor package, found ${prod_imports}"
fi
if grep -qE '^(func|type|var|const) [A-Z]' sdk/drivers/prod/prod.go; then
    fail 'phase 112a: sdk/drivers/prod must export no identifiers'
else
    ok 'phase 112a: the public aggregator exports no identifiers'
fi

# --- 4. No-behavior guard (advisory) -----------------------------------------

func_count=$(grep -rE '^func ' sdk/ --include='*.go' | grep -cv 'sdk/tools/inproc/inproc.go') || func_count=0
if [ "${func_count}" -eq 0 ]; then
    ok 'phase 112a: no func bodies in the facade outside the documented generic forward'
else
    fail "phase 112a: found ${func_count} func declarations outside sdk/tools/inproc — the facade must stay forwards-only"
fi

# --- 5. The integrity test slice passes under -race --------------------------

if go test ./test/integration/ -run 'Phase112a' -race -count=1 -timeout 300s >/dev/null 2>&1; then
    ok 'phase 112a: facade-integrity test slice passes under -race'
else
    fail 'phase 112a: facade-integrity slice failed (run: go test ./test/integration/ -run Phase112a -race)'
fi

smoke_summary
