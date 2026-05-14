#!/usr/bin/env bash
# Phase 50 smoke — Pause/Resume Coordinator + handle registry
# (RFC §6.3 + §3.3; master-plan Phase 50 detail block; D-067).
#
# Phase 50 ships `internal/runtime/pauseresume` — Harbor's ONE
# pause/resume primitive (CLAUDE.md §7 rule 4). The `Coordinator`
# (`Request` / `Resume` / `Status`) is the single runtime-level
# coordination point that HITL approval, tool-side OAuth, A2A
# AUTH_REQUIRED, and operator/Console PAUSE all converge on. `Token` is
# opaque (runtime-owned ULID encoding); durability rides on the
# existing `state.StateStore` (Phase 07) when a checkpoint store is
# configured — a pause survives a Runtime restart only when
# StateStore-backed.
#
# This is a code-only library phase: no Protocol surface lands until
# the `task.pause` / `task.resume` Protocol methods (a later phase), so
# the HTTP/Protocol assertions skip with a reason per the
# 404/405/501 → SKIP convention.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

PR_PKG="internal/runtime/pauseresume"

# Run the pauseresume package tests under -race. Covers unit
# (Request/Resume/Status, restart-survival both ways, fail-loud paths,
# scope mismatch, idempotency) + the D-025 concurrent-reuse test
# (N≥100 against one shared Coordinator).
if go test -race -count=1 -timeout 180s ./${PR_PKG}/... >/dev/null 2>&1; then
    ok 'phase 50: internal/runtime/pauseresume tests pass under -race (Coordinator round-trip + restart-survival + fail-loud + D-025 concurrent-reuse)'
else
    fail 'phase 50: pauseresume tests failed (run `go test -race ./internal/runtime/pauseresume/...` for detail)'
fi

# Run the Phase 50 durability integration test (real state.StateStore
# across in-mem / SQLite / Postgres — Postgres leg skips without
# HARBOR_PG_DSN).
if go test -race -count=1 -timeout 180s -run 'TestE2E_PauseResume' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 50: pause/resume durability integration test passes under -race (in-mem + SQLite + scope-isolation + lost-handle + missing-identity)'
else
    fail 'phase 50: durability integration test failed (run `go test -race -run TestE2E_PauseResume ./test/integration/...` for detail)'
fi

# Static guard: the Coordinator interface must declare Request /
# Resume / Status (the master-plan Phase 50 surface).
PR_FILE="${PR_PKG}/pauseresume.go"
if [[ ! -f "${PR_FILE}" ]]; then
    fail "phase 50: ${PR_FILE} missing"
else
    missing_methods=""
    for method in "Request(ctx context.Context" "Resume(ctx context.Context" "Status(ctx context.Context"; do
        if ! grep -q "${method}" "${PR_FILE}"; then
            missing_methods="${missing_methods} ${method%%(*}"
        fi
    done
    if [[ -n "${missing_methods}" ]]; then
        fail "phase 50: Coordinator interface missing methods:${missing_methods}"
    else
        ok 'phase 50: pauseresume.Coordinator declares Request / Resume / Status (master-plan Phase 50 surface)'
    fi
fi

# Import-graph guard: the pauseresume package must NOT import the
# Console — the Runtime never imports Console code (CLAUDE.md §13).
if grep -rIn --include='*.go' '"github.com/hurtener/Harbor/web/console' "${PR_PKG}/" 2>/dev/null | grep -q .; then
    fail 'phase 50: internal/runtime/pauseresume imports the Console — the Runtime never imports Console code (CLAUDE.md §13)'
else
    ok 'phase 50: internal/runtime/pauseresume does not import the Console (Runtime/Console boundary preserved)'
fi

# §13 / §4.4 guard: durability rides on the existing state.StateStore;
# Phase 50 must NOT mint a parallel persistence-driver tree (that would
# be the §13 two-parallel-implementations smell — see D-067).
if [[ -d "${PR_PKG}/drivers" ]]; then
    fail 'phase 50: internal/runtime/pauseresume/drivers/ exists — durability must ride on state.StateStore, not a parallel persistence seam (D-067)'
else
    ok 'phase 50: no parallel persistence-driver tree under internal/runtime/pauseresume (durability rides on state.StateStore — D-067)'
fi

# Phase 50 ships no Protocol/HTTP surface — the task.pause /
# task.resume Protocol methods land in a later phase. Skip per the
# 404/405/501 → SKIP convention.
skip "phase 50: Pause/Resume Coordinator is a code-only runtime primitive; the task.pause / task.resume Protocol methods land in a later phase"

smoke_summary
