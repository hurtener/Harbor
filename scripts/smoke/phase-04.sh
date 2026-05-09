#!/usr/bin/env bash
# Phase 04 smoke — slog Logger + standard attribute set.
#
# Phase 04 is a pure Go package (internal/telemetry) with no HTTP /
# Protocol surface; correctness is verified by
# `go test -race ./internal/telemetry/...` under `make test`. The
# preflight surface check has nothing to assert here, so this script
# SKIPs and lets the test gate carry the load.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 04: telemetry/logger — Go package only; validated by go test ./internal/telemetry/..."

smoke_summary
