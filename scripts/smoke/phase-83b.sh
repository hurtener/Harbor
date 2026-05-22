#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83b — react-tool-schema-injection smoke.
#
# The prompt builder has no HTTP surface; correctness is validated by
# `go test ./internal/planner/react/...` (preflight runs `go test`
# separately). This smoke focuses on STATIC invariants of the
# checked-in golden fixture — the normative spec for the enriched
# <available_tools> rendering (RFC §6.2 + §6.4, brief 13 §2.4, D-144).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

GOLDEN="internal/planner/react/testdata/golden_tools_prompt.txt"

# The golden fixture must exist.
assert_file "${GOLDEN}" "golden enriched-tools fixture exists"

# The enriched per-tool block carries each new field at least once.
assert_grep_present 'args_schema:' "${GOLDEN}" "golden carries an args_schema: line"
assert_grep_present 'side_effects:' "${GOLDEN}" "golden carries a side_effects: line"
assert_grep_present 'examples:' "${GOLDEN}" "golden carries an examples: line"

# The section openers/closers are present.
assert_grep_present '^<available_tools>$' "${GOLDEN}" "golden opens <available_tools>"
assert_grep_present '^</available_tools>$' "${GOLDEN}" "golden closes </available_tools>"

# No un-rendered template markers survived into the fixture.
assert_grep_absent '{{' "${GOLDEN}" "golden has no '{{' template markers"

smoke_summary
