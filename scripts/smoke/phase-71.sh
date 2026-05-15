#!/usr/bin/env bash
# Phase 71 smoke — harbortest test kit self-tests.
#
# The kit has no HTTP / Protocol surface: its self-tests ARE its smoke.
# We invoke `go test -race ./harbortest/...` and assert exit-0. The
# 404/405/501 → SKIP convention does not apply here because there is
# no network surface; instead, missing-binary degradation is "the
# harbortest/ directory doesn't exist yet" — in that case we SKIP per
# CLAUDE.md §4.2 convention 4 so phase-N+1 builds keep this phase's
# script green.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if [ ! -d "${ROOT}/harbortest" ]; then
  skip "phase 71: harbortest package not yet present"
  smoke_summary
  exit 0
fi

# Run the kit self-tests under -race. The kit's own assertions are
# exercised here: round-trip capture, AssertSequence happy + sad,
# AssertNoLeaks happy + the deliberate-cross-session-bug regression,
# SimulateFailure counter behaviour, and the D-025 concurrent-reuse
# stress (N=100 concurrent RunOnce).
if go test -race -count=1 ./harbortest/... >/tmp/harbortest-smoke.log 2>&1; then
  ok "harbortest: go test -race ./harbortest/... passed"
else
  fail "harbortest: go test -race ./harbortest/... failed; tail:"
  tail -50 /tmp/harbortest-smoke.log >&2 || true
fi

smoke_summary
