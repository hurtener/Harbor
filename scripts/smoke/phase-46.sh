#!/usr/bin/env bash
# Phase 46 smoke — Trajectory compression / summariser (RFC §6.2;
# master plan Phase 46 detail block; D-055).
#
# Phase 46 ships the runtime-side trajectory summariser invoked when
# the planner-step's token estimate exceeds RunContext.Budget.TokenBudget.
# The contract:
#
#   - `Summariser` interface (internal/planner) — fail-loudly
#     on error; (nil, nil) is a contract violation surfaced as
#     ErrEmptySummary.
#   - `TrajectorySummary` (alias on Phase 43's Summary struct) carries
#     {Goals, Facts, Pending, LastOutputDigest, Note}.
#   - `CompressionRunner.MaybeCompress(ctx, rc, tr)` checks the
#     trajectory's chars/4 estimate against rc.Budget.TokenBudget; when
#     exceeded, invokes the summariser and stamps tr.Summary.
#   - `trajectory.compressed` + `trajectory.compression_failed` events
#     are the load-bearing fail-loudly observability surfaces (§13).
#   - The Phase 45 ReAct prompt builder reads tr.Summary when present
#     and skips raw step history — compression is a runtime concern;
#     the planner sees only the compacted view.
#
# Phase 46 is a code-only phase; no protocol surface lands until the
# runtime engine consumes the runner (Phase 47+).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1) The planner package (top-level) tests pass under -race. Covers
#    the Phase 42 finish-planner surface + Phase 44 repair + the new
#    Phase 46 CompressionRunner + the D-025 N=128 stress.
if go test -race -count=1 -timeout 180s ./internal/planner -run . >/dev/null 2>&1; then
    ok 'phase 46: internal/planner top-level tests pass under -race (Phase 46 compression primitive + Phase 42 surface + D-025)'
else
    fail 'phase 46: internal/planner top-level tests failed (run `go test -race ./internal/planner` for detail)'
fi

# 1b) The trajectory subpackage tests still pass (Phase 43 fail-loudly
#     contract surface unchanged by Phase 46).
if go test -race -count=1 -timeout 120s ./internal/planner/trajectory/... >/dev/null 2>&1; then
    ok 'phase 46: internal/planner/trajectory tests pass under -race (Phase 43 fail-loudly Serialize surface unchanged)'
else
    fail 'phase 46: internal/planner/trajectory tests failed (Phase 43 regression)'
fi

# 2) The react subpackage tests pass under -race — re-asserts the
#    Phase 45 surface AND the Phase 46 consumer wire-up
#    (prompt_test.go::TestDefaultBuilder_WithSummary_SkipsStepHistory
#    + compression_integration_test.go).
if go test -race -count=1 -timeout 180s ./internal/planner/react/... >/dev/null 2>&1; then
    ok 'phase 46: internal/planner/react tests pass under -race (Phase 45 surface + Phase 46 consumer integration)'
else
    fail 'phase 46: react tests failed (run `go test -race ./internal/planner/react/...` for detail)'
fi

# 3) Exported-surface assertions via go doc. A regression where any of
#    these is accidentally unexported or renamed surfaces here.
if go doc github.com/hurtener/Harbor/internal/planner Summariser 2>/dev/null | grep -q 'Summariser'; then
    ok 'phase 46: Summariser interface is exported from internal/planner'
else
    fail 'phase 46: Summariser not exported from internal/planner (interface missing or renamed)'
fi

if go doc github.com/hurtener/Harbor/internal/planner TrajectorySummary 2>/dev/null | grep -q 'TrajectorySummary'; then
    ok 'phase 46: TrajectorySummary alias is exported from internal/planner'
else
    fail 'phase 46: TrajectorySummary not exported from internal/planner (alias missing or renamed)'
fi

if go doc github.com/hurtener/Harbor/internal/planner CompressionRunner 2>/dev/null | grep -q 'CompressionRunner'; then
    ok 'phase 46: CompressionRunner is exported from internal/planner'
else
    fail 'phase 46: CompressionRunner not exported from internal/planner (type missing or renamed)'
fi

if go doc github.com/hurtener/Harbor/internal/planner DefaultTokenEstimator 2>/dev/null | grep -q 'DefaultTokenEstimator'; then
    ok 'phase 46: DefaultTokenEstimator is exported from internal/planner'
else
    fail 'phase 46: DefaultTokenEstimator not exported from internal/planner (function missing or renamed)'
fi

if go doc github.com/hurtener/Harbor/internal/planner NewCompressionRunner 2>/dev/null | grep -q 'NewCompressionRunner'; then
    ok 'phase 46: NewCompressionRunner constructor is exported from internal/planner'
else
    fail 'phase 46: NewCompressionRunner not exported from internal/planner'
fi

# 4) Event-registry assertions — fail-loudly observability surface (§13).
if grep -q 'EventTypeTrajectoryCompressed' internal/planner/events.go 2>/dev/null; then
    ok 'phase 46: trajectory.compressed event type registered (success-path observability)'
else
    fail 'phase 46: trajectory.compressed event type missing from internal/planner/events.go'
fi

if grep -q 'EventTypeTrajectoryCompressionFailed' internal/planner/events.go 2>/dev/null; then
    ok 'phase 46: trajectory.compression_failed event type registered (fail-loudly observability surface; §13)'
else
    fail 'phase 46: trajectory.compression_failed event type missing from internal/planner/events.go (silent-degradation risk; §13)'
fi

# 5) Budget.TokenBudget field present — the runner's threshold input.
if grep -q 'TokenBudget' internal/planner/planner.go 2>/dev/null; then
    ok 'phase 46: Budget.TokenBudget field present in internal/planner/planner.go (compression threshold input)'
else
    fail 'phase 46: Budget.TokenBudget field missing from internal/planner/planner.go (runner has no threshold input)'
fi

# 6) §13 import-graph guard for the planner + react packages — no
#    runtime imports. Phase 42's importgraph_test.go covers this
#    properly in Go (it parses imports via go/parser); the smoke
#    script is the static second gate, intentionally narrow to
#    actual import-line shape (`"github.com/.../internal/runtime/...`)
#    so the conformance test's literal reference (used as the
#    walker's lint target) is not a false positive.
if grep -rIn --include='*.go' '"github.com/hurtener/Harbor/internal/runtime' internal/planner/ 2>/dev/null | grep -v 'importgraph_test.go' | grep -q .; then
    fail 'phase 46: internal/planner/ imports internal/runtime/... — violates Phase 42 import-graph contract (§13)'
else
    ok 'phase 46: internal/planner/ does not import internal/runtime/... (Phase 42 import-graph contract preserved)'
fi

skip "phase 46: compression primitive has no Protocol surface; the runtime-engine invocation lands at Phase 47+ (planner-runtime stitch)"

smoke_summary
