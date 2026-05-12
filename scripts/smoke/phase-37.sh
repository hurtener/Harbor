#!/usr/bin/env bash
# Phase 37 — Skill store + LocalDB driver + FTS5 ladder.
#
# Phase 37 ships the SkillStore interface + the SQLite-backed
# `localdb` driver + the FTS5 → regex → exact ranking ladder. The
# planner-facing tools (`skill_search`, `skill_get`, `skill_list`)
# wrapping the storage layer land in Phase 38.
#
# Phase 37 has no HTTP/Protocol surface — the SkillStore is in-process
# at this wave; the planner-tool catalog over the Protocol is Phase 60+.
# Smoke runs the Go-level test surface (interface + driver + FTS5
# ladder + D-025 concurrent-reuse + conformance suite) under -race.
#
# Conventions (CLAUDE.md §4.2):
#   - 404/405/501 → SKIP (no Protocol surface yet).
#   - The Go test surface MUST pass — that's the smoke-observable boundary at this phase.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 120s ./internal/skills/... >/dev/null 2>&1; then
    ok 'phase 37: internal/skills tests pass under -race (SkillStore + localdb driver + FTS5/regex/exact ladder + conformance + D-025)'
else
    fail 'phase 37: internal/skills tests failed (run `go test -race ./internal/skills/...` for detail)'
fi

skip "phase 37: skill store has no HTTP/Protocol surface yet (Phase 38 wraps the storage layer in planner tools; Phase 60+ exposes over Protocol)"

smoke_summary
