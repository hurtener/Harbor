#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 183 — TUI Runtime control and inspection (D-319).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 183: smoke skeleton — replace with task, tool, intervention, posture, and fallback assertions"

smoke_summary
