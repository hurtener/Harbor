#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 132 — embed production runner (Stack.RunOnce) + NewRunContext
# factory.
#
# The gate for this phase EXTENDS scripts/smoke/phase-112b.sh (leg 6):
# it compiles the checked-in examples/embed-runonce/ worked example and
# asserts the N>=100 concurrent-reuse -race test + the NewRunContext
# projection-parity test exist and pass. There is no standalone surface
# for this script to hit, so it skips and points at the real coverage.
# See docs/plans/phase-132-embed-runonce.md § "Smoke script additions".

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip 'phase 132: coverage lives in scripts/smoke/phase-112b.sh leg 6 (embed-runonce compile + RunOnce/NewRunContext -race tests)'

smoke_summary
