#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 140 smoke — the v1.8.0 wave-end composing E2E.
#
# This is a STATIC + unit-test guard, not a live-server smoke: the
# surface it gates is a Go integration test (the wave-end E2E), not a
# Protocol endpoint. It pins the EXACT top test name and carries a
# `go test -list` no-match-fails guard so the gate can never silently
# match zero tests — the same false-green hazard phase-136.sh closes.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

TEST_NAME='TestE2E_WaveV18_AdopterPaths'
TEST_FILE='test/integration/wave_v18_test.go'

# (a) The wave-end E2E file exists.
assert_file "${TEST_FILE}" \
    "phase 140: ${TEST_FILE} exists"

# (b) The top test is defined under its EXACT pinned name in the source.
assert_grep_present "func ${TEST_NAME}\(" "${TEST_FILE}" \
    "phase 140: ${TEST_NAME} is defined in ${TEST_FILE}"

# (c) No-match-fails guard: `go test -list` must enumerate the wave-end
# tests under the TestE2E_WaveV18 prefix. A zero count (a renamed/typo'd
# test, or the file deleted) fails LOUD instead of vacuously skipping —
# the whole point of this gate.
list_out=$(go test -list '^TestE2E_WaveV18' ./test/integration 2>/dev/null || true)
match_count=$(printf '%s\n' "${list_out}" | grep -c '^TestE2E_WaveV18' || true)
if [ "${match_count}" -ge 1 ]; then
    ok "phase 140: go test -list enumerates ${match_count} TestE2E_WaveV18 test(s)"
else
    fail "phase 140: go test -list found ${match_count} tests matching ^TestE2E_WaveV18, want >= 1 — the wave-end E2E gate would match nothing (false-green)"
fi

smoke_summary
