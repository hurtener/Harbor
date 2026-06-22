#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 92k smoke — the auth.Provider runtime config registration seam.
#
# This phase's surface is a Go API (auth.Provider.RegisterConfig /
# UnregisterConfig), exercised by `go test ./internal/tools/auth/...`, not an
# HTTP endpoint — hence the `unit-tests` classification (D-104). When the phase
# ships, replace the skip below with the package test invocation; parked, the
# single skip keeps preflight green.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Phase 92k assertions go below when the seam lands, e.g.:
#
#   go test -race ./internal/tools/auth/... || fail "auth package tests"
#
# Until the phase ships, a single skip keeps preflight green.
# ----------------------------------------------------------------------------

skip "phase 92k: auth.Provider runtime config registration seam — not yet implemented"

smoke_summary
