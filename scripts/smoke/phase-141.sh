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

# --- D-377 / D-378 — bounded model-visible names + the loud collision drop.
COLL="internal/planner/react/tool_name_collision_test.go"
DECL="internal/planner/react/discovered_tools.go"

# The budget is deliberately BELOW the provider's 64-byte ceiling: a tool name
# is paid on every turn, twice per tool. A revert to 64 must not pass silently.
assert_grep_present "const maxToolNameBytes = 44" "${SANI}" \
    "phase 141: the model-visible name budget is bounded below the 64-byte provider ceiling (D-377)"
# Shortening keeps the TAIL (the verb), not the head (the repeated source id).
# `s[:keep]` — the head truncation that collapsed a whole server onto one
# declaration — must never come back.
assert_grep_present "s\[len\(s\)-keep:\]" "${SANI}" \
    "phase 141: over-budget names are shortened tail-first (D-377)"
assert_grep_absent "return s\[:keep\]" "${SANI}" \
    "phase 141: head truncation (the silent-collapse cause) is gone (D-378)"
# Both model-visible surfaces go through ONE transform, or the model is shown
# a name it cannot call.
assert_grep_present "sanitizeToolName\(t\.Name\)" "internal/planner/react/prompt.go" \
    "phase 141: the <available_tools> section renders the DECLARED name (D-377)"
# The drop is announced. A bare `continue` here is the §13 silent degradation.
assert_grep_present "emitToolDeclarationCollision" "${DECL}" \
    "phase 141: a dropped declaration is announced on the bus (D-378)"
assert_grep_present "EventTypePlannerToolDeclarationCollision" "internal/planner/events.go" \
    "phase 141: the collision event type is registered (D-378)"

# The behavioural gate. Static greps pin that the code SHAPE survives; these
# pin that it BEHAVES — and assert_go_tests_pass fails on a rename rather than
# reporting OK for a filter that matched nothing.
assert_go_tests_pass "$(mktemp)" "-race -count=1 ./internal/planner/react/" \
    "phase 141: bounded-name + loud-collision behaviour (D-377 / D-378)" \
    TestBuildToolDeclarations_LongSourceIDDoesNotCollapse \
    TestSanitizeToolName_KeepsTheVerbVisible \
    TestSanitizeToolName_LongNamesStayDistinct \
    TestSanitizeToolName_ShortNamesUnchanged \
    TestSanitizeToolName_Deterministic \
    TestRenderAvailableTools_MatchesDeclaredNames \
    TestBuildToolDeclarations_ResidualCollisionIsLoud \
    TestBuildToolDeclarations_CollisionWithReservedControlIsLoud \
    TestBuildToolDeclarations_BenignRediscoveryStaysQuiet \
    TestBuildToolDeclarations_DeclaredNameCostPerTurn

assert_file "${COLL}" "phase 141: the collision + cost regression suite is present"

smoke_summary
