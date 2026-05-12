#!/usr/bin/env bash
# Phase 37 — Skill store + LocalDB driver + FTS5 ladder.
#
# Phase 37 ships the SkillStore interface + the SQLite-backed
# `localdb` driver + the FTS5 → regex → exact ranking ladder. None of
# these surface yet over the Protocol — the planner-facing tools
# (`skill_search`, `skill_get`, `skill_list`) land in Phase 38 and
# wrap the storage layer with capability filtering + redaction +
# tiered injection budgeter. That phase's smoke script will flip
# this skeleton's `skip` to real assertions.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N
#     builds).
#   - At least one OK once the phase has shipped — Phase 37 is
#     persistence-only, so the skip is the correct surface and the
#     phase-38 PR flips it.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 37: persistence-only — SkillStore + localdb driver + FTS5 ladder ship as Go-level surface only. Phase 38 will land the planner-tool smoke assertions."

smoke_summary
