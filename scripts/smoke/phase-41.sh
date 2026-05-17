#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 41 — In-runtime skill generator with persistence.
#
# Phase 41 ships the planner-callable `skill_propose` tool that
# validates an LLM-drafted skill, stamps `Origin=Generated`, scopes by
# operator-supplied `Scope` (default `project`), upserts via the Phase
# 37 `SkillStore`, and emits a mandatory `skill.proposed` audit event
# on every persist.
#
# Phase 41 has no Protocol surface — the catalog is in-process at this
# wave. Smoke runs the Go-level test surface (unit + integration +
# D-025 concurrent-reuse + audit-emit failure mode) under -race and
# pins the `skill_propose` tool-name constant so a silent rename
# surfaces here, not in Phase 60+.
#
# Conventions (CLAUDE.md §4.2):
#   - 404/405/501 → SKIP (no Protocol surface yet).
#   - The Go test surface MUST pass — that's the smoke-observable boundary at this phase.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 180s ./internal/skills/generator/... >/dev/null 2>&1; then
    ok 'phase 41: internal/skills/generator tests pass under -race (propose + conflict + audit + Promote + D-025 + integration)'
else
    fail 'phase 41: internal/skills/generator tests failed (run `go test -race ./internal/skills/generator/...` for detail)'
fi

# Pin the skill_propose constant — a silent rename here would break
# every planner prompt downstream. Grep on the registration constant.
if grep -q 'ToolNameSkillPropose *= *"skill_propose"' internal/skills/generator/generator.go; then
    ok 'phase 41: tool-name constant pinned (skill_propose)'
else
    fail 'phase 41: tool-name constant drifted — expected ToolNameSkillPropose = "skill_propose" in internal/skills/generator/generator.go'
fi

# Phase 41 ships within the skills subsystem; the Phase 37 + Phase 38
# surfaces must continue to pass once the generator's audit + conflict
# additions sit on top. Re-run the whole subtree.
if go test -race -count=1 -timeout 180s ./internal/skills/... >/dev/null 2>&1; then
    ok 'phase 41: internal/skills (Phase 37 + Phase 38) tests still green with Phase 41 generator present'
else
    fail 'phase 41: internal/skills regression detected (run `go test -race ./internal/skills/...` for detail)'
fi

skip "phase 41: generator catalog has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
