#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 181 — TUI terminal foundation and visual system (D-317).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 181: smoke skeleton — replace with geometry, golden-matrix, and PTY lifecycle assertions"

smoke_summary
