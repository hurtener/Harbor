#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 119 smoke — runtime retention + ctx hardening.
#
# Static-only: this phase fixes internal correctness (map reaping,
# identity-scoped governance keys, a cancellable recovery ctx, two
# control-flow cleanups) with no new HTTP/Protocol surface. The
# behavioural guarantees are gated by `go test -race` in the touched
# packages; these assertions pin the load-bearing source facts so a
# regression that silently reverts a fix fails preflight.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

ENG="internal/runtime/engine"
GOV="internal/governance"
MEM="internal/memory/strategy"

# 1. Engine capacity reap: the sweeper exists and is launched in Run,
#    mirroring the cancellation sweeper.
assert_grep_present 'func \(e \*engine\) reapIdleCapacities' "${ENG}/streaming.go" \
    "engine: reapIdleCapacities reaper present"
assert_grep_present 'func \(e \*engine\) runCapacitySweeper' "${ENG}/streaming.go" \
    "engine: capacity sweeper present"
assert_grep_present 'runCapacitySweeper\(internalCtx\)' "${ENG}/engine.go" \
    "engine: capacity sweeper launched in Run"
assert_grep_present 'lastActivity' "${ENG}/streaming.go" \
    "engine: runCapacity tracks lastActivity for the idle-TTL reap"

# 2. Governance identity-scoping (RFC §6.15): the helper exists and both
#    subsystems strip RunID before keying / persisting.
assert_grep_present 'func identityScoped' "${GOV}/governance.go" \
    "governance: identityScoped helper present"
assert_grep_present 'identityScoped\(q\)' "${GOV}/cost.go" \
    "governance: cost accumulator strips RunID from its key"
assert_grep_present 'identityScoped\(q\)' "${GOV}/ratelimit.go" \
    "governance: rate limiter strips RunID from its key"

# 3. Rolling-summary recovery loop uses a cancellable ctx, not
#    context.Background(), and Close cancels it.
assert_grep_present 'recoveryCancel' "${MEM}/rolling_summary.go" \
    "rolling-summary: cancellable recovery ctx present"
assert_grep_present 'ctx := e.recoveryCtx' "${MEM}/rolling_summary.go" \
    "rolling-summary: recoverOne uses the cancellable recovery ctx"

# 4. Low cleanups: the MapConcurrent dead select is gone; readAny reuses
#    one timer instead of allocating time.After per poll cycle.
assert_grep_absent 'time.After\(time.Microsecond\)' "${ENG}/engine.go" \
    "engine: readAny no longer allocates time.After per poll cycle"
assert_grep_present 'multiChannelPollInterval' "${ENG}/engine.go" \
    "engine: readAny reuses a single poll timer"

smoke_summary
