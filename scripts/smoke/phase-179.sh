#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 179 — Go Protocol client foundation (D-315).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 179: smoke skeleton — replace with client, inspect conversion, and external-facade assertions"

smoke_summary
