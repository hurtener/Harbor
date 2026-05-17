#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 07 smoke — StateStore foundation.
#
# Phase 07 is a pure Go package (internal/state + drivers/inmem +
# conformancetest) with no HTTP / Protocol surface; correctness is
# verified by `go test -race ./internal/state/...` under `make test`.
# The preflight surface check has nothing to assert here, so this
# script SKIPs and lets the test gate carry the load.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 07: state store — Go package only; validated by go test ./internal/state/..."

smoke_summary
