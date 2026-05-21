#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 85a — mcp-client-core-compliance smoke.
# Phase has not shipped yet; skeleton auto-skips so preflight stays green.
# See docs/plans/phase-85a-mcp-client-core-compliance.md § "Smoke script
# additions" for the binding assertion list.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 85a: smoke skeleton — replace with real assertions when the phase implements its surface"

smoke_summary
