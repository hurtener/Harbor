#!/usr/bin/env bash
# Phase 26a smoke — Flow-as-Tool registration + per-flow Budget.
#
# Phase 26a ships `flow.Definition` (entry/exit + node specs +
# optional intrinsic Budget), `flow.Compose(def) → engine.Engine`
# (the runnable engine reusable across invocations), and
# `flow.RegisterAsTool(catalog, def, eng)` that wires the engine
# into the tool catalog with `Transport: TransportFlow`. Per-flow
# Budget (deadline / hop budget / cost cap) composes with parent
# run + identity-tier ceilings via min(); exceedance emits
# `flow.budget_exceeded` and returns `ErrFlowBudgetExceeded`.
#
# The smoke runs the package test suite (Definition + Compose +
# RegisterAsTool + Budget min() composition math + D-025
# concurrent-reuse) under -race. There is no HTTP / Protocol
# surface yet (lands in Phase 60+).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 120s ./internal/runtime/flow/... >/dev/null 2>&1; then
    ok 'phase 26a: internal/runtime/flow tests pass under -race (Definition + Compose + RegisterAsTool + Budget min() + D-025 concurrent-reuse)'
else
    fail 'phase 26a: internal/runtime/flow tests failed (run `go test -race ./internal/runtime/flow/...` for detail)'
fi

skip "phase 26a: Flow-as-Tool has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
