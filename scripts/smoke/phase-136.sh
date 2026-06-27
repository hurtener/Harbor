#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 136 smoke — MCP agent-calls-tool integration test.
#
# This is a STATIC + unit-test guard, not a live-server smoke: the
# surface it gates is a Go integration test, not a Protocol endpoint.
# It pins the EXACT test name and carries a no-match-fails guard so the
# gate can never silently match zero tests — the precise false-green
# hazard the v1.8.0 wave coordination doc flags (the original
# `…Phase83g.*Call` gate regex matched ZERO tests, a
# SKIP-that-should-be-OK).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

TEST_NAME='TestE2E_Phase83g_MCPAgentCallsTool'
TEST_FILE='test/integration/mcp_agent_call_test.go'

# (a) The test is defined under its EXACT pinned name in the source.
assert_grep_present "func ${TEST_NAME}\(" "${TEST_FILE}" \
    "phase 136: ${TEST_NAME} is defined in ${TEST_FILE}"

# (b) No-match-fails guard: `go test -list` must enumerate EXACTLY the
# pinned test. A zero count (a renamed/typo'd test, or the file
# deleted) fails LOUD instead of vacuously skipping — the whole point
# of this gate.
list_out=$(go test -list "^${TEST_NAME}\$" ./test/integration 2>/dev/null || true)
match_count=$(printf '%s\n' "${list_out}" | grep -c "^${TEST_NAME}\$" || true)
if [ "${match_count}" -eq 1 ]; then
    ok "phase 136: go test -list enumerates exactly ${TEST_NAME} (count=${match_count})"
else
    fail "phase 136: go test -list found ${match_count} tests matching ^${TEST_NAME}\$, want exactly 1 — the agent-call gate would match nothing (false-green)"
fi

smoke_summary
