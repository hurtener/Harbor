#!/usr/bin/env bash
# Phase 48 smoke — Deterministic planner (RFC §6.2 + RFC §11 Q-6;
# master plan Phase 48 detail block; D-057).
#
# Phase 48 ships Harbor's second concrete Planner under
# `internal/planner/deterministic/`. The planner walks a configured
# decision tree per `Next` call and emits typed Decision shapes
# without any LLM call — proving CLAUDE.md §1 property 3 ("the Planner
# is swappable"): the same Runtime drives ReAct (Phase 45) and the
# deterministic planner via the same interface, no changes. WakePoll
# wake declaration (D-032) — each `Next` performs a non-blocking
# receive against the `tasks.WatchGroup` channel for its outstanding
# group; not-ready → emit `AwaitTask`; ready → consume MemberOutcome
# and proceed.
#
# This is a code-only phase; no protocol surface lands until
# Phase 60+.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Run the deterministic package tests under -race. Covers all unit
# tests + integration + D-025 concurrent-reuse + conformance.
if go test -race -count=1 -timeout 180s ./internal/planner/deterministic/... >/dev/null 2>&1; then
    ok 'phase 48: internal/planner/deterministic tests pass under -race (decision-tree walker + Spawn/Await scenario + D-025 + conformance)'
else
    fail 'phase 48: deterministic tests failed (run `go test -race ./internal/planner/deterministic/...` for detail)'
fi

# Static guard: the WakePoll declaration must be wired (D-032; Phase
# 48 master-plan detail block — "Deterministic ships the `poll` wake
# mode").
DET_FILE="internal/planner/deterministic/deterministic.go"
if [[ ! -f "${DET_FILE}" ]]; then
    fail "phase 48: ${DET_FILE} missing"
else
    if grep -q 'WakePoll' "${DET_FILE}"; then
        ok 'phase 48: DeterministicPlanner declares planner.WakePoll wake-on-resolution mode (D-032)'
    else
        fail 'phase 48: DeterministicPlanner missing WakePoll declaration in deterministic.go (D-032 — Phase 48 spec)'
    fi
fi

# §13 import-graph guard for the deterministic package — no runtime
# imports.
if grep -rIn --include='*.go' 'github.com/hurtener/Harbor/internal/runtime' internal/planner/deterministic/ 2>/dev/null | grep -q .; then
    fail 'phase 48: internal/planner/deterministic/ imports internal/runtime/... — violates Phase 42 import-graph contract (§13)'
else
    ok 'phase 48: internal/planner/deterministic/ does not import internal/runtime/... (Phase 42 import-graph contract preserved)'
fi

# §13 import-graph guard — the deterministic planner has no LLM
# dependency by construction (the LLM-edge composition belongs to
# ReAct; a deterministic planner that imported `internal/llm` would
# be a §13 two-parallel-implementations smell).
if grep -rIn --include='*.go' '"github.com/hurtener/Harbor/internal/llm"' internal/planner/deterministic/ 2>/dev/null | grep -q .; then
    fail 'phase 48: internal/planner/deterministic/ imports internal/llm — the deterministic planner has no LLM dependency by construction (§13)'
else
    ok 'phase 48: internal/planner/deterministic/ does not import internal/llm (deterministic planner is LLM-free by construction)'
fi

# Cross-planner coverage assertion: Phase 49's conformance pack will
# use the deterministic planner as the second leg of cross-planner
# scenarios. The deterministic planner MUST exercise each of
# CallTool, SpawnTask, AwaitTask, Finish in at least one scenario so
# Phase 49 has coverage of every emission shape.
SCENARIO_FILE="internal/planner/deterministic/spawn_await_scenario_test.go"
if [[ ! -f "${SCENARIO_FILE}" ]]; then
    fail "phase 48: ${SCENARIO_FILE} missing — the §13 primitive-with-consumer scenario is mandatory"
else
    missing_shapes=""
    for shape in "CallTool" "SpawnTask" "AwaitTask" "Finish"; do
        if ! grep -q "${shape}" "${SCENARIO_FILE}"; then
            missing_shapes="${missing_shapes} ${shape}"
        fi
    done
    if [[ -n "${missing_shapes}" ]]; then
        fail "phase 48: scenario file missing Decision shapes:${missing_shapes} — cross-planner coverage for Phase 49 conformance pack incomplete"
    else
        ok 'phase 48: scenario test exercises CallTool + SpawnTask + AwaitTask + Finish (§13 primitive-with-consumer compliance; Phase 49 cross-planner coverage)'
    fi
fi

skip "phase 48: deterministic planner is a code-only concrete with no protocol surface (lands in Phase 60+ via the planner-step executor)"

smoke_summary
