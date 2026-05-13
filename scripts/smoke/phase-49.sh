#!/usr/bin/env bash
# Phase 49 smoke — Planner conformance pack (RFC §6.2; master plan
# Phase 49 detail block; D-058).
#
# Phase 49 fills the Phase 42 conformance harness skeleton with the
# real scenario suite both Wave 8 concretes (ReAct + Deterministic)
# must pass. The pack is a test-only surface (no Protocol method, no
# REST endpoint); the smoke script asserts:
#
#   1. The conformance package tests pass under -race.
#   2. Both ReAct + Deterministic per-package conformance tests pass.
#   3. The load-bearing wake-mode round-trip scenario fires (subtest
#      name pinned in the test source — D-032 binding).
#   4. The §13 import-graph lint still passes (no new
#      internal/runtime/... imports introduced).
#
# This is a code-only phase; no protocol surface lands until later.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. Conformance package itself passes under -race.
if go test -race -count=1 -timeout 180s ./internal/planner/conformance/... >/dev/null 2>&1; then
    ok 'phase 49: internal/planner/conformance tests pass under -race (importgraph lint + scenario fixtures)'
else
    fail 'phase 49: conformance tests failed (run `go test -race ./internal/planner/conformance/...` for detail)'
fi

# 2. ReAct's full conformance run passes.
if go test -race -count=1 -timeout 180s -run 'TestReact_Conformance' ./internal/planner/react/... >/dev/null 2>&1; then
    ok 'phase 49: ReAct passes the full conformance pack under -race (WakePush round-trip + top-prompt fixtures)'
else
    fail 'phase 49: ReAct conformance failed (run `go test -race -run TestReact_Conformance ./internal/planner/react/...` for detail)'
fi

# 3. Deterministic's full conformance run passes.
if go test -race -count=1 -timeout 180s -run 'TestDeterministic_Conformance' ./internal/planner/deterministic/... >/dev/null 2>&1; then
    ok 'phase 49: Deterministic passes the full conformance pack under -race (WakePoll round-trip + DecisionTreeStep fixtures)'
else
    fail 'phase 49: Deterministic conformance failed (run `go test -race -run TestDeterministic_Conformance ./internal/planner/deterministic/...` for detail)'
fi

# 4. Wave 8 wave-end E2E passes.
if go test -race -count=1 -timeout 300s -run 'TestE2E_Wave8' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 49: Wave 8 wave-end E2E passes under -race (Skills + Planner + Tools + Tasks + Memory + LLM real-drivers seam)'
else
    fail 'phase 49: Wave 8 E2E failed (run `go test -race -run TestE2E_Wave8 ./test/integration/...` for detail)'
fi

# 5. The wake-mode round-trip subtest name MUST exist in the
# conformance source — D-032 binding. A silent rename would hide the
# load-bearing scenario; pin the literal here.
CONF_FILE="internal/planner/conformance/conformance.go"
if [[ ! -f "${CONF_FILE}" ]]; then
    fail "phase 49: ${CONF_FILE} missing"
else
    if grep -q '"WakeMode_RoundTrip"' "${CONF_FILE}"; then
        ok 'phase 49: WakeMode_RoundTrip scenario present in conformance.go (D-032 binding — the load-bearing wake-mode round-trip)'
    else
        fail 'phase 49: WakeMode_RoundTrip scenario missing from conformance.go (D-032 binding — silent rename / drop forbidden)'
    fi
fi

# 6. §13 import-graph lint — the conformance package STILL imports
# zero internal/runtime/... packages. The Phase 42
# importgraph_test.go is the binding gate; this smoke check is the
# fast static guard.
#
# Exclude importgraph_test.go itself: the lint test contains the
# literal forbidden-import string ("github.com/hurtener/Harbor/internal/runtime")
# as the substring it greps for in the planner subtree. A naive
# match would flag the lint test as a violation. The binding gate is
# the test, not this smoke check; the smoke check is the fast
# heuristic.
if grep -rIn --include='*.go' --exclude='importgraph_test.go' 'github.com/hurtener/Harbor/internal/runtime' internal/planner/conformance/ 2>/dev/null | grep -q .; then
    fail 'phase 49: internal/planner/conformance/ imports internal/runtime/... — violates Phase 42 import-graph contract (§13)'
else
    ok 'phase 49: internal/planner/conformance/ does not import internal/runtime/... (Phase 42 import-graph contract preserved)'
fi

# 7. Phase 49 must not regress §13 forbidden-name scan in any new
# source file. The drift-audit covers this globally; this is the
# scoped fast check.
if grep -rIn --include='*.go' -i "penguiflow\|the prior project" internal/planner/conformance/ test/integration/wave8_test.go scripts/smoke/phase-49.sh 2>/dev/null | grep -q .; then
    fail 'phase 49: forbidden predecessor reference found in Phase 49 sources (§13 forbidden-name scan)'
else
    ok 'phase 49: Phase 49 sources clean of predecessor references (§13 forbidden-name scan)'
fi

skip "phase 49: conformance pack is a code-only test asset with no protocol surface (planner-step executor lands in Phase 60+)"

smoke_summary
