#!/usr/bin/env bash
# Phase 38 — Skill planner tools (skill_search, skill_get, skill_list).
#
# Phase 38 ships three planner-callable Tools that wrap the Phase 37
# `SkillStore` with capability filtering (`RequiredTools` /
# `RequiredNS` / `RequiredTags` ⊆ allowed), tool-name + PII redaction
# at injection, and a tiered budgeter (full → drop optional → cap
# steps to 3 → `ErrSkillTooLarge`).
#
# Phase 38 has no Protocol surface — the catalog is in-process at this
# wave. Smoke runs the Go-level test surface (unit + integration +
# D-025 concurrent-reuse) under -race and pins the three tool-name
# constants so a silent rename surfaces here, not in Phase 60+.
#
# Conventions (CLAUDE.md §4.2):
#   - 404/405/501 → SKIP (no Protocol surface yet).
#   - The Go test surface MUST pass — that's the smoke-observable boundary at this phase.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 120s ./internal/skills/tools/... >/dev/null 2>&1; then
    ok 'phase 38: internal/skills/tools tests pass under -race (filter + redactor + budgeter + handlers + D-025 + integration)'
else
    fail 'phase 38: internal/skills/tools tests failed (run `go test -race ./internal/skills/tools/...` for detail)'
fi

# Pin the three tool-name constants — a silent rename here would break
# every planner prompt downstream. Grep on the registration constants.
if grep -q 'ToolNameSkillSearch *= *"skill_search"' internal/skills/tools/tools.go \
    && grep -q 'ToolNameSkillGet *= *"skill_get"' internal/skills/tools/tools.go \
    && grep -q 'ToolNameSkillList *= *"skill_list"' internal/skills/tools/tools.go; then
    ok 'phase 38: tool-name constants pinned (skill_search / skill_get / skill_list)'
else
    fail 'phase 38: tool-name constants drifted — expected skill_search / skill_get / skill_list in internal/skills/tools/tools.go'
fi

# Regression check: Phase 37's surface still passes once Phase 38's
# wrapper sits on top. Pre-Phase-38 this lived in scripts/smoke/phase-37.sh as
# a skip; flipped to an assertion in this PR.
if go test -race -count=1 -timeout 120s ./internal/skills/... >/dev/null 2>&1; then
    ok 'phase 38: internal/skills (Phase 37) tests still green with Phase 38 wrapper present'
else
    fail 'phase 38: internal/skills regression detected (run `go test -race ./internal/skills/...` for detail)'
fi

skip "phase 38: planner-tool catalog has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
