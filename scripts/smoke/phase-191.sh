#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 191 smoke — OAuth broker legs: step-up visibility, resource-bound exchange, actor chain + wave E2E.
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

skip "phase 191: smoke skeleton — replace with the step-up envelope, exchange resource/actor field, per-tool binding, and wave-v116 E2E assertions when the phase implements its surface"

smoke_summary
