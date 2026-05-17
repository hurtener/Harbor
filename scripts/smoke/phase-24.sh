#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 24 smoke — Memory strategies (truncation, rolling_summary).
#
# Phase 24 is a pure Go package extension (internal/memory/strategy +
# internal/memory/drivers/inmem changes) with no HTTP / Protocol
# surface; correctness is verified by `go test -race
# ./internal/memory/...` under `make test` and the cross-subsystem
# integration test in `test/integration/memory_strategies_test.go`
# (per AGENTS.md §17).
#
# The preflight surface check has nothing to assert here, so this
# script SKIPs and lets the test gate carry the load.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 24: memory strategies — Go package only; validated by go test ./internal/memory/..."

smoke_summary
