#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 92i smoke — Console agent-config REVISION UX (derived per-revision
# change summary + diff-before-rollback + atomic multi-section Save-all).
# Planning-stage skeleton: keeps preflight green until the surface lands.
# This phase adds NO Go / Protocol surface — the live gate is the Playwright
# spec in the frontend CI job. The implementing PR replaces the skip below
# with the real static assertions:
#
#   - the Save-all path + staged-payload accumulation symbols in
#     web/console/src/lib/agentconfig/state.svelte.ts
#   - the derived-summary + diff-preview-modal markup in
#     web/console/src/lib/components/agentconfig/RevisionHistoryCard.svelte
#   - the staged-changes indicator (SaveAllBar or panel header)
#   - a tokens-only / no-hand-rolled-fetch grep over the touched .svelte files
#
# Conventions (AGENTS.md §4.2): 404/405/501 -> SKIP; OK >= 1 once shipped;
# use scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 92i: planning-stage skeleton — Console revision-UX surface not yet implemented"

smoke_summary
