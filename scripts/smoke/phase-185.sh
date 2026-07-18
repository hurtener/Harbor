#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 185 smoke — Batch decision + AC-21 supersession (projector).
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

skip "phase 185: smoke skeleton — replace with the projector partition-table, degenerate-batch-invariant, and reserved-control description assertions when the phase implements its surface"

smoke_summary
