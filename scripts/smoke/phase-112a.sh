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
#   4. No-behavior guard: the facade declares no func bodies outside
#      the enumerated forward allow-list
#      (including sdk/tools/inproc.RegisterFunc, sdk/assemble.RunTyped — D-273
#      amending D-205 item 1; Go cannot express a generic function as
#      a var forward). FUNC-level: a third func anywhere under sdk/
#      fails, including a second func/method appended inside an
#      allow-listed file.
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

# --- 4. No-behavior guard: the enumerated forward allow-list ----------------

# Scans the SHIPPED facade surface only: production .go files. `_test.go`
# files are exempt — runnable godoc `Example_*` functions legitimately
# carry func bodies in test files (the first landed under sdk/ in phase 134)
# and are not part of the alias-only forwarding surface (CLAUDE.md §5).
#
# D-273 amends D-205 item 1 from "exactly ONE func" to an ENUMERATED
# allow-list. The list below is the SINGLE source: each entry is
# `file|name-regex|name|func-count` — the file-set constraint and the
# per-file func-count are both DERIVED from it, so a later decision-gated
# addition (a new facade package with a named forward set) appends ONE
# entry instead of rewriting the constraints. The gate stays FUNC-level:
# a func declaration in any un-listed file fails, AND extra funcs inside
# an allow-listed file fail — behavior cannot silently accrete.
allowed_func_specs='sdk/tools/inproc/inproc.go|^func RegisterFunc\[|RegisterFunc|1
sdk/assemble/runtyped.go|^func RunTyped\[|RunTyped|1
sdk/server/server.go|^func Open\(|Open|1
sdk/protocolclient/protocolclient.go|^func New\(|Protocol client forwards|3'

allowed_func_files=$(echo "${allowed_func_specs}" | cut -d'|' -f1)
allowed_file_count=$(echo "${allowed_func_specs}" | grep -c '^')

func_files=$(grep -lE '^func ' -r sdk/ --include='*.go' --exclude='*_test.go' | sed 's#^\./##' | sort -u) || func_files=""
unexpected_func_files=$(comm -23 <(echo "${func_files}") <(echo "${allowed_func_files}" | sort -u))
if [ -z "${unexpected_func_files}" ]; then
    ok 'phase 112a: no func bodies in the facade outside the enumerated allow-list'
else
    fail "phase 112a: found func declarations outside the enumerated allow-list — the facade must stay forwards-only (or the allow-list is stale): ${unexpected_func_files}"
fi
if [ "$(echo "${func_files}" | grep -c '^')" -eq "${allowed_file_count}" ]; then
    ok "phase 112a: EXACTLY ${allowed_file_count} func-bearing file(s) under sdk/ (the enumerated generic-forward allow-list)"
else
    fail "phase 112a: expected exactly ${allowed_file_count} func-bearing files under sdk/, found $(echo "${func_files}" | grep -c '^'): ${func_files}"
fi

# Func-level check: each allow-listed file declares EXACTLY its enumerated
# func count (methods start with `^func ` too, so they are counted) and the
# named forward is present. A sneaky helper appended to an allow-listed file
# fails here.
while IFS='|' read -r f want_re want_name want_count; do
    count=$(grep -cE '^func ' "$f") || count=0
    if [ "${count}" -ne "${want_count}" ]; then
        fail "phase 112a: ${f} declares ${count} func bodies, want EXACTLY ${want_count} (the enumerated forward ${want_name}) — behavior must not accrete inside an allow-listed file"
        continue
    fi
    if grep -qE "${want_re}" "$f"; then
        ok "phase 112a: ${f} declares its enumerated func count (${want_count}) incl. the forward ${want_name}"
    else
        fail "phase 112a: ${f} does NOT declare the enumerated forward ${want_name}"
    fi
done <<< "${allowed_func_specs}"

assert_grep_present '^func StaticToken\(' sdk/protocolclient/protocolclient.go \
    'phase 112a: Protocol client facade forwards StaticToken'
assert_grep_present '^func WithHTTPClient\(' sdk/protocolclient/protocolclient.go \
    'phase 112a: Protocol client facade forwards WithHTTPClient'

# --- 5. The integrity test slice passes under -race --------------------------

if go test ./test/integration/ -run 'Phase112a' -race -count=1 -timeout 300s >/dev/null 2>&1; then
    ok 'phase 112a: facade-integrity test slice passes under -race'
else
    fail 'phase 112a: facade-integrity slice failed (run: go test ./test/integration/ -run Phase112a -race)'
fi

smoke_summary
