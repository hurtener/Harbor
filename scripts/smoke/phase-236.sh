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
# HA-54 planner-replay gap (governance amendment): the classified outcome must
# survive the runloop's step recording and reach the actual next ReAct prompt,
# and a generic Step.Error must never mask the classified LLMObservation. Pin
# the exact contract sentences the plan/decision carry, not bare words, so the
# guard dies if the requirement is diluted or dropped.
assert_grep_present "survive runloop step recording" docs/plans/phase-236-typed-mcp-errors.md "classification/retry/bounded result survive runloop step recording"
assert_grep_present "appear in the actual next ReAct prompt" docs/plans/phase-236-typed-mcp-errors.md "the next ReAct prompt is the replay destination"
assert_grep_present "Step\.Error.*never masks" docs/plans/phase-236-typed-mcp-errors.md "a generic Step.Error never masks the classified LLMObservation"
assert_grep_present "mcp_error_replay_test.go" docs/plans/phase-236-typed-mcp-errors.md "the full-path replay test is owned by the phase"
assert_grep_present "revision_conflict.*current revision" docs/plans/phase-236-typed-mcp-errors.md "revision_conflict carries the current revision for reread/retry"
assert_grep_present "planner-replay closure" docs/decisions.md "D-410 carries the planner-replay closure amendment"
# D-410's plan cross-reference must name the real plan file — the wrong name
# was a broken link; a regression to it must fail.
assert_grep_present "phase-236-typed-mcp-errors.md" docs/decisions.md "D-410 cross-references the real plan file"
assert_grep_absent "phase-236-typed-mcp-error-classification.md" docs/decisions.md "no D-410 cross-reference to the nonexistent plan name"
smoke_summary
