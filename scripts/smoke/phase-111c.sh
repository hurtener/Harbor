#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 111c smoke — durable pauses + pause lifecycle (D-200).
# (RFC §3.3 / §6.3 / §6.11; docs/plans/phase-111c-durable-pause-lifecycle.md)
#
# Static + unit-test assertions:
#   1. `WithCheckpointStore` has its production consumer: the ONE
#      Coordinator construction in `assemble.Assemble` (the merged
#      110d assembly site — cmd + devstack inherit as thin callers)
#      passes the StateStore. The audit's regression grep.
#   2. The `Trajectory: nil` gap (+ its "later-phase" comment) is gone
#      from the steering runloop — the run's live trajectory is
#      threaded into the PauseRequest.
#   3. The pause sweeper ships: `pauseresume.RunSweeper` +
#      `WithMaxParkDuration` exported; the sweeper is DecisionTimeout's
#      first producer; the assembly starts it config-gated.
#   4. The config surface is honest: `pauseresume.max_park_duration` +
#      `pauseresume.sweep_interval` in the schema, validated, and
#      documented in the example configs + docs/CONFIG.md.
#   5. The focused unit + integration slices pass under -race
#      (durability E2E, timeout E2E, ErrUnserializable fail-loud,
#      sweeper race + goroutine baseline).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# --- 1. WithCheckpointStore production consumer -----------------------------

assert_grep_present 'pauseresume\.WithCheckpointStore\(stack\.State\)' \
    internal/runtime/assemble/assemble.go 'phase 111c: the assembly wires the checkpoint store into the ONE Coordinator'
assert_grep_present 'pauseresume\.WithMaxParkDuration\(cfg\.PauseResume\.MaxParkDuration\)' \
    internal/runtime/assemble/assemble.go 'phase 111c: the assembly threads the max-park ceiling'
assert_grep_present 'pauseresume\.RunSweeper\(' \
    internal/runtime/assemble/assemble.go 'phase 111c: the assembly starts the sweeper (config-gated)'

# --- 2. Trajectory threading -------------------------------------------------

assert_grep_absent 'Trajectory: nil' internal/runtime/steering/runloop.go \
    'phase 111c: the runloop no longer persists Trajectory: nil'
assert_grep_absent 'later-phase concern; Phase 53' internal/runtime/steering/runloop.go \
    'phase 111c: the stale later-phase comment is gone'
assert_grep_present 'Trajectory: tr' internal/runtime/steering/runloop.go \
    'phase 111c: requestPause threads the run trajectory into the PauseRequest'

# --- 3. The sweeper surface --------------------------------------------------

assert_file internal/runtime/pauseresume/sweeper.go      'phase 111c: sweeper file'
assert_file internal/runtime/pauseresume/sweeper_test.go 'phase 111c: sweeper tests'
assert_grep_present 'func RunSweeper\(ctx context\.Context, coord Coordinator, opts \.\.\.SweeperOption\) error' \
    internal/runtime/pauseresume/sweeper.go 'phase 111c: RunSweeper exported'
assert_grep_present 'func WithMaxParkDuration\(d time\.Duration\) Option' \
    internal/runtime/pauseresume/coordinator.go 'phase 111c: WithMaxParkDuration exported'
assert_grep_present 'DecisionTimeout' internal/runtime/pauseresume/sweeper.go \
    'phase 111c: the sweeper is DecisionTimeout'"'"'s first producer'
assert_grep_absent 'Phase 50 does not yet emit this' internal/runtime/pauseresume/decision.go \
    'phase 111c: DecisionTimeout godoc no longer claims it has no producer'

# --- 4. Honest config surface ------------------------------------------------

assert_grep_present 'max_park_duration' internal/config/config.go    'phase 111c: schema field max_park_duration'
assert_grep_present 'validatePauseResume' internal/config/validate.go 'phase 111c: pauseresume block validated'
assert_grep_present 'max_park_duration' examples/harbor.yaml          'phase 111c: example config documents max_park_duration'
assert_grep_present 'pauseresume:' examples/dev.yaml                  'phase 111c: dev example carries the block'
assert_grep_present 'pauseresume.max_park_duration' docs/CONFIG.md    'phase 111c: CONFIG.md documents the field'

# --- 5. The focused unit + integration slices pass under -race --------------

if go test ./internal/runtime/pauseresume/ ./internal/runtime/steering/ \
    -run 'Sweep|Sweeper|MaxPark|Timeout|ThreadsTrajectory' -race -count=1 -timeout 300s >/dev/null 2>&1; then
    ok 'phase 111c: sweeper + timeout + trajectory-threading unit slices pass under -race'
else
    fail 'phase 111c: unit slices failed (run the go test line in scripts/smoke/phase-111c.sh for detail)'
fi

if go test ./test/integration/ -run 'Phase111c' -race -count=1 -timeout 300s >/dev/null 2>&1; then
    ok 'phase 111c: durability + timeout + fail-loud integration E2E passes under -race'
else
    fail 'phase 111c: integration E2E failed (go test ./test/integration/ -run Phase111c -race)'
fi

smoke_summary
