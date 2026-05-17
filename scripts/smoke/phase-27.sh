#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 27 smoke — HTTP tool driver (inline + manifest + static auth).
#
# Phase 27 ships the first out-of-process tool transport: an HTTP
# driver with inline registration (`RegisterHTTPTool`), a UTCP-style
# YAML manifest loader, three static auth modes (API key / bearer /
# cookie), and `Retry-After`-aware rate-limit handling. Every HTTP
# tool gets the same `ToolPolicy` reliability shell as in-process
# tools (D-024).
#
# The smoke runs the package test suite (driver + manifest + auth +
# concurrent-reuse test) under -race against `httptest.Server`. No
# HTTP / Protocol surface yet (no boot-time endpoint to probe).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 120s ./internal/tools/drivers/http/... >/dev/null 2>&1; then
    ok 'phase 27: internal/tools/drivers/http tests pass under -race (inline + manifest + auth + retry-after + D-025 concurrent-reuse)'
else
    fail 'phase 27: internal/tools/drivers/http tests failed (run `go test -race ./internal/tools/drivers/http/...` for detail)'
fi

skip "phase 27: HTTP tool driver has no boot-time Protocol surface yet (manifest paths flow through ToolsConfig at later phases)"

smoke_summary
