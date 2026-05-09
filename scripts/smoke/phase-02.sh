#!/usr/bin/env bash
# Phase 02 — configuration loader smoke.
# The config package has no HTTP surface; correctness is validated by go test.
# This script exists so the drift-audit's plan↔smoke pairing rule is satisfied
# and so `make preflight` accounting includes phase 02.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 02: config package validated by go test (no HTTP surface)"

smoke_summary
