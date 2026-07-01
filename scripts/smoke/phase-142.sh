#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 142 smoke — external tool-credential provisioning: the `tokenexchange`
# OAuth driver (D-271).
#
# When the surface lands this runs `go test -race` for
# internal/tools/auth/drivers/tokenexchange + the phase-142 integration test,
# and greps: (a) internal/drivers/prod carries the driver's blank import
# (D-196 — never cmd/harbor); (b) no non-test file in the driver package
# references TokenStore.Put (brokered tokens are never persisted).
# Parked (planning-only) for now.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 142: smoke skeleton — replace with real assertions when the tokenexchange driver lands"

smoke_summary
