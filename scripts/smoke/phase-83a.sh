#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83a — ReAct prompt structured sections smoke.
#
# The prompt builder has no HTTP surface; correctness is validated by
# `go test ./internal/planner/react/...` (preflight runs `go test`
# separately). This smoke focuses on STATIC invariants of the
# checked-in golden fixture — the normative spec for the rendered
# default prompt (RFC §6.2, brief 13 §2.1).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

GOLDEN="internal/planner/react/testdata/golden_default_prompt.txt"

# The golden fixture must exist.
assert_file "${GOLDEN}" "golden default-prompt fixture exists"

# The XML sections the default fixture renders. Phase 83a's original
# twelve has been reshaped by Phase 107c (D-167):
#   - <output_format> / <action_schema> / <finishing> are DELETED
#     (the prompt-engineered JSON-action shape is retired).
#   - <parallel_execution> is DELETED (parallel emission is native:
#     the runtime accepts multiple ToolCalls in one response and
#     serialises them per AC-19).
#   - <tool_discovery> is ADDED (native tool-calling + deferred-
#     loading meta-tool instructions).
# Sections <additional_guidance> + <planning_constraints> are
# conditional (omitted from the no-extras default fixture); the
# remaining seven always-on sections appear once each as a line-
# leading opener.
for tag in identity tool_discovery tool_usage \
           reasoning tone error_handling available_tools; do
  assert_grep_present "^<${tag}>$" "${GOLDEN}" "golden carries <${tag}> opener"
done

# Deletion guards — the four sections Phase 107c retired must be
# ABSENT from the golden. A regression that re-introduces them
# (e.g. a future change reviving the brief-13 JSON-action shape)
# fails this smoke.
for tag in output_format action_schema finishing parallel_execution; do
  assert_grep_absent "^<${tag}>$" "${GOLDEN}" "golden omits deleted <${tag}> section (Phase 107c)"
done

# No un-rendered template markers survived into the fixture (catches a
# `{{current_date}}` / `{{rendered_tools}}` regression).
assert_grep_absent '{{' "${GOLDEN}" "golden has no '{{' template markers"

# The action JSON drops the `reasoning` field (brief 13 §2.6) — no
# `"reasoning":` JSON key anywhere in the rendered prompt.
assert_grep_absent '"reasoning":' "${GOLDEN}" "golden has no \"reasoning\": JSON field"

# Rich-output finish fields are dropped entirely (brief 13 §5) — no
# reserved `"confidence"` / `"route"` / `"requires_followup"` /
# `"warnings"` JSON keys.
for field in '"confidence"' '"route"' '"requires_followup"' '"warnings"'; do
  assert_grep_absent "${field}" "${GOLDEN}" "golden has no rich-output field ${field}"
done

# The <tone> intermediate-step clamp matches the Phase 107c
# native-tool-calling contract (the brief-13 "JSON action object"
# CRITICAL clamp was retired by D-167 AC-20).
assert_grep_present \
  "Emit only tool calls — keep any narration to the final answer turn." \
  "${GOLDEN}" "golden <tone> carries the native-tool-calling intermediate-step clamp"
assert_grep_absent \
  "produce ONLY the JSON action object" \
  "${GOLDEN}" "golden omits the retired JSON-action CRITICAL clamp"

# The example config documents the new `extra_guidance` key.
assert_grep_present 'extra_guidance' "examples/harbor.yaml" \
  "example config documents planner.extra_guidance"

smoke_summary
