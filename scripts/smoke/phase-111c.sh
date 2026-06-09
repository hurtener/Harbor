#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 111c smoke skeleton. Replace the skip with real assertions when the
# phase implements its surface (see docs/plans/phase-111c-*.md). Reclassify to
# 'live-server' or 'unit-tests' once the assertions hit a built surface.
#
# Conventions (AGENTS.md §4.2): 404/405/501 → SKIP; ≥1 OK once shipped; use
# scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 111c: smoke skeleton — replace with real assertions when the phase implements its surface"

smoke_summary
