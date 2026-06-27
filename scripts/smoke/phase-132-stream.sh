#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 132-stream — WithStream sink on Stack.RunOnce.
#
# The gate for this phase EXTENDS scripts/smoke/phase-112b.sh (the
# streaming leg): it grep-asserts the ordered-chunks-before-envelope
# end-to-end test exists, asserts the N>=100 concurrent-reuse -race test
# carries the no-cross-run-chunk-bleed assertion, and runs both under
# -race. There is no standalone network surface for this script to hit,
# so it skips and points at the real coverage. See
# docs/plans/phase-132-stream-withstream.md § "Smoke script additions".

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip 'phase 132-stream: coverage lives in scripts/smoke/phase-112b.sh (WithStream ordered-chunk + no-cross-run-bleed -race tests)'

smoke_summary
