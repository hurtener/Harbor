#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 117 smoke skeleton — replace with real assertions when the phase
# implements its surface. Until then this skips to keep preflight green.

set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 117: not yet implemented"
smoke_summary
