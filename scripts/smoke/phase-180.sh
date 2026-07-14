#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 180 — TUI projection and reconciliation core (D-316).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 180: smoke skeleton — replace with projection, shared-fixture, and reconciliation assertions"

smoke_summary
