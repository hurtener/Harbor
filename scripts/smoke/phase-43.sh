#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 43 smoke — Trajectory + fail-loudly Serialize contract.
#
# Phase 43 closes the predecessor's silent-context-loss bug
# (RFC §6.2 + §3.4). The contract:
#
#   - Trajectory.Serialize() ([]byte, error) returns
#     (nil, ErrUnserializable{Field:...}) on any non-JSON-encodable
#     leaf — never silently. The reflective walker tracks the field
#     path so the error message is actionable.
#   - ToolContext splits into a serialisable JSON half + an opaque
#     HandleID registry (process-local at V1; distributed driver is a
#     post-V1 RFC concern).
#   - HandleRegistry.Get on a missing handle returns ErrToolContextLost
#     — never (nil, nil).
#   - Serialize → Deserialize → Serialize is byte-stable.
#
# Phase 43 has no Protocol surface. Correctness is gated by the Go
# test suite at internal/planner/trajectory/...; the smoke runs the
# tests under -race, and asserts the sentinel-error types remain
# exported (a regression check against accidental rename / removal).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1) The package's own tests pass under -race. This is the load-bearing
#    gate — the negative-case + round-trip + concurrent-reuse tests live
#    here, and the fail-loudly contract is asserted against the
#    adversarial pack (functions, channels, cyclic graphs).
if go test -race -count=1 -timeout 120s ./internal/planner/trajectory/... >/dev/null 2>&1; then
    ok 'phase 43: internal/planner/trajectory tests pass under -race'
else
    fail 'phase 43: internal/planner/trajectory tests failed (run `go test -race ./internal/planner/trajectory/...` for detail)'
fi

# 2) The legacy planner package still compiles + passes (the type
#    aliases re-exporting from the subpackage must compile cleanly).
if go test -race -count=1 -timeout 60s ./internal/planner/... >/dev/null 2>&1; then
    ok 'phase 43: legacy internal/planner tests still pass (alias re-exports compile)'
else
    fail 'phase 43: legacy internal/planner tests failed (run `go test -race ./internal/planner/...`)'
fi

# 3) ErrUnserializable is exported as a sentinel type. A simple grep on
#    `go doc` proves the public surface is intact; a regression where
#    the type is accidentally unexported or renamed would surface here.
if go doc github.com/hurtener/Harbor/internal/planner/trajectory ErrUnserializable 2>/dev/null | grep -q 'ErrUnserializable'; then
    ok 'phase 43: ErrUnserializable is exported from internal/planner/trajectory'
else
    fail 'phase 43: ErrUnserializable not found in internal/planner/trajectory (sentinel type missing or renamed)'
fi

# 4) ErrToolContextLost is exported as a sentinel type.
if go doc github.com/hurtener/Harbor/internal/planner/trajectory ErrToolContextLost 2>/dev/null | grep -q 'ErrToolContextLost'; then
    ok 'phase 43: ErrToolContextLost is exported from internal/planner/trajectory'
else
    fail 'phase 43: ErrToolContextLost not found in internal/planner/trajectory (sentinel type missing or renamed)'
fi

# 5) HandleRegistry interface is exported.
if go doc github.com/hurtener/Harbor/internal/planner/trajectory HandleRegistry 2>/dev/null | grep -q 'HandleRegistry'; then
    ok 'phase 43: HandleRegistry interface is exported from internal/planner/trajectory'
else
    fail 'phase 43: HandleRegistry not found in internal/planner/trajectory (interface missing or renamed)'
fi

skip "phase 43: trajectory subsystem has no HTTP/Protocol surface (pause-record wire-up lands at Phase 51 with the unified pause/resume coordinator)"

smoke_summary
