#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83b — react-tool-schema-injection smoke.
# Phase has not shipped yet; this script is a skeleton that auto-skips.
# See docs/plans/phase-83b-react-tool-schema-injection.md § "Smoke script
# additions" for the binding assertion list.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 83b: smoke skeleton — replace with real assertions when the phase implements its surface"

smoke_summary
