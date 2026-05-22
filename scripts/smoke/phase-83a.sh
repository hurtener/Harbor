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

# All twelve XML section openers from brief 13 §2.1 — each on its own
# line. Sections 11/12 (<additional_guidance> / <planning_constraints>)
# are omitted from the no-extras default fixture; the ten always-on
# sections must each appear exactly once as a line-leading opener.
for tag in identity output_format action_schema finishing tool_usage \
           parallel_execution reasoning tone error_handling available_tools; do
  assert_grep_present "^<${tag}>$" "${GOLDEN}" "golden carries <${tag}> opener"
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

# The <tone> CRITICAL clamp is ported verbatim (brief 13 §2.6).
assert_grep_present \
  "Do not include a 'thought' or 'reasoning' field in the JSON." \
  "${GOLDEN}" "golden <tone> carries the CRITICAL clamp"

# The example config documents the new `extra_guidance` key.
assert_grep_present 'extra_guidance' "examples/harbor.yaml" \
  "example config documents planner.extra_guidance"

smoke_summary
