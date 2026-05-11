#!/usr/bin/env bash
# Phase 23 smoke — MemoryStore foundation.
#
# Phase 23 is a pure Go package (internal/memory + drivers/inmem +
# conformancetest) with no HTTP / Protocol surface; correctness is
# verified by `go test -race ./internal/memory/...` under `make test`
# and the cross-subsystem integration test in
# `test/integration/memory_state_test.go` (per AGENTS.md §17).
#
# The preflight surface check has nothing to assert here, so this
# script SKIPs and lets the test gate carry the load.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 23: memory store — Go package only; validated by go test ./internal/memory/..."

smoke_summary
