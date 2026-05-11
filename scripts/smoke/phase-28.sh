#!/usr/bin/env bash
# Phase 28 smoke — MCP southbound driver (stdio + SSE + streamable-HTTP).
#
# Phase 28 ships `internal/tools/drivers/mcp/` as a `ToolProvider`
# driver that wraps `github.com/modelcontextprotocol/go-sdk@v1.6.0`.
# Auto-detect via `MCPTransportMode = Auto | SSE | StreamableHTTP`
# (stdio for Command configs). Resource subscriptions emit
# `mcp.resource_updated` on the canonical event bus. Every Invoke
# runs inside `tools.RunWithPolicy` (D-024). D-025 concurrent-reuse
# tested with N=100 invocations under -race.
#
# The smoke runs the package test suite (unit + integration via
# in-process mock server + concurrent-reuse + transport-fallback)
# under -race. There is no HTTP / Protocol surface yet (lands in
# Phase 60+).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 180s ./internal/tools/drivers/mcp/... >/dev/null 2>&1; then
    ok 'phase 28: internal/tools/drivers/mcp tests pass under -race (unit + integration + D-025 concurrent-reuse + transport-fallback)'
else
    fail 'phase 28: internal/tools/drivers/mcp tests failed (run `go test -race ./internal/tools/drivers/mcp/...` for detail)'
fi

skip "phase 28: MCP driver has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
