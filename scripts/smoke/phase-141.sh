#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 141 smoke — native tool-name sanitization for provider-safe
# tool-calling (D-270). Static pins only; the behavioural gate is the
# round-trip unit test (a dotted name through declaration + projection),
# which the scripted-LLM tests lacked (the live bug, §17.8). This pins
# that the sanitizer + its tests exist so the gate cannot silently vanish.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

SANI="internal/planner/react/tool_name_sanitize.go"
TST="internal/planner/react/tool_name_sanitize_test.go"

assert_file "${SANI}" "phase 141: tool-name sanitizer source present"
assert_file "${TST}" "phase 141: tool-name sanitizer round-trip test present"
assert_grep_present "func sanitizeToolName" "${SANI}" "phase 141: sanitizeToolName defined"
assert_grep_present "func resolveDeclaredToolName" "${SANI}" "phase 141: resolveDeclaredToolName defined"
assert_grep_present "func TestProjectResponse_ResolvesSanitizedNameToCatalog" "${TST}" \
    "phase 141: the declaration→projection round-trip regression test exists"
# The declaration builder + history replay must run names through the sanitizer.
assert_grep_present "sanitizeToolName\(t\.Name\)" "internal/planner/react/discovered_tools.go" \
    "phase 141: tool declarations are sanitized"
assert_grep_present "sanitizeToolName\(call\.Tool\)" "internal/planner/react/prompt.go" \
    "phase 141: assistant tool_calls history replay is sanitized"

smoke_summary
