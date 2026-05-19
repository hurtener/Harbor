#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83a — react-prompt-structured-sections smoke.
# Phase has not shipped yet; this script is a skeleton that auto-skips so
# the preflight gate stays green. Replace with real assertions when the
# phase lands. See docs/plans/phase-83a-react-prompt-structured-sections.md
# § "Smoke script additions" for the binding assertion list.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 83a: smoke skeleton — replace with real assertions when the phase implements its surface"

smoke_summary
