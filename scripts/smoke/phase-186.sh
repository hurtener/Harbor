#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 186 smoke — Batch executor: heterogeneous dispatch, auto-grouping, ordered observations.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 186: smoke skeleton — replace with the dispatch-table, auto-group, breadth-cap rejection, and reply-ordering-invariant test assertions when the phase implements its surface"

smoke_summary
