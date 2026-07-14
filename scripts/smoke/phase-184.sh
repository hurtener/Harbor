#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 184 — TUI Runtime distribution and wave E2E (D-320).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 184: smoke skeleton — replace with stock/scaffold distribution and wave PTY E2E assertions"

smoke_summary
