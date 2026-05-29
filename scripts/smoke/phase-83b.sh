#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83b — react-tool-schema-injection smoke.
#
# The prompt builder has no HTTP surface; correctness is validated by
# `go test ./internal/planner/react/...` (preflight runs `go test`
# separately). This smoke focuses on STATIC invariants of the
# checked-in golden fixture.
#
# Phase 107c (D-167) NARROWED <available_tools> to name + description
# only — the args_schema / side_effects / examples surface moved into
# the provider-native `req.Tools[]` declaration. The 83b smoke pivots
# from "enriched fields present" to "deleted fields absent" so a
# regression that re-introduces them under the prompt-engineered shape
# fails this gate (CLAUDE.md §17.6 — same-PR fix when the test surfaces
# the bug).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

GOLDEN="internal/planner/react/testdata/golden_tools_prompt.txt"

# The golden fixture must exist.
assert_file "${GOLDEN}" "golden enriched-tools fixture exists"

# Phase 107c (D-167) narrowed the rendering: the enriched per-tool
# block now carries name + description ONLY. The args_schema /
# side_effects / examples surface moved into the provider-native
# `req.Tools[]` declaration — they MUST NOT appear in the prompt.
assert_grep_absent 'args_schema:' "${GOLDEN}" "golden omits args_schema: (now in req.Tools[].Schema)"
assert_grep_absent 'side_effects:' "${GOLDEN}" "golden omits side_effects: (Phase 107c narrowed available_tools)"
assert_grep_absent 'examples:' "${GOLDEN}" "golden omits examples: (Phase 107c narrowed available_tools)"
# Tool name + description still render as a quick reference.
assert_grep_present '^- ' "${GOLDEN}" "golden carries at least one tool entry line"

# The section openers/closers are present.
assert_grep_present '^<available_tools>$' "${GOLDEN}" "golden opens <available_tools>"
assert_grep_present '^</available_tools>$' "${GOLDEN}" "golden closes </available_tools>"

# No un-rendered template markers survived into the fixture.
assert_grep_absent '{{' "${GOLDEN}" "golden has no '{{' template markers"

smoke_summary
