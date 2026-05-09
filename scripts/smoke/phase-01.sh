#!/usr/bin/env bash
# Phase 01 smoke — identity foundation.
#
# Phase 01 is a pure Go package (internal/identity) with no HTTP / Protocol
# surface; correctness is verified by `go test -race ./internal/identity/...`
# under `make test`. The preflight surface check has nothing to assert here,
# so this script SKIPs and lets the test gate carry the load.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 01: identity package validated by go test (no HTTP surface)"

smoke_summary
