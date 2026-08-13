#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source scripts/smoke/common.sh
assert_file docs/plans/phase-236-typed-mcp-errors.md "phase 236 plan exists"
assert_grep_present "D-410" docs/decisions.md "D-410 is recorded"
assert_grep_present "stdio" docs/plans/phase-236-typed-mcp-errors.md "stdio fixture is planned"
assert_grep_present "streamable HTTP" docs/plans/phase-236-typed-mcp-errors.md "streamable HTTP fixture is planned"
assert_grep_present "SSE" docs/plans/phase-236-typed-mcp-errors.md "SSE fixture is planned"
# The plan's Non-goals forbid TEXT PARSING — classification is typed-envelope
# based (D-410), never prose-parsed. Pin the forbidding sentence itself, not a
# bare word: the plan truthfully carries the "text parsing" phrase, and the
# guard dies only if the Non-goals line is ever dropped.
assert_grep_present "No per-server retry override, text parsing" docs/plans/phase-236-typed-mcp-errors.md "text parsing is forbidden (typed MCP error classification, never prose)"
assert_grep_present "Protocol" docs/plans/phase-236-typed-mcp-errors.md "Protocol contract is bounded"
smoke_summary
