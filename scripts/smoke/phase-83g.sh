#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83g — wire the Phase 28 MCP southbound driver into the dev
# binary's boot path. The catalog-side wiring is exercised end-to-end
# by `test/integration/phase83g_mcp_dev_consumer_test.go` (spawns a
# real stdio subprocess via the `cmd/harbor-mcptest-stdio` test
# fixture). This static smoke asserts the wiring surfaces exist so
# they cannot silently disappear.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Production consumer wiring.
# ----------------------------------------------------------------------------
# Phase 110d (D-197): the per-server attach helper was PROMOTED from
# cmd/harbor's attachDevMCPServer (and devstack's drifted mirror) to
# `mcpdrv.Attach`, invoked from the ONE assembly fan-out
# (internal/runtime/assemble) both callers wrap.
assert_grep_present 'func Attach\(ctx context\.Context, ms config\.MCPServerConfig' "internal/tools/drivers/mcp/attach.go" \
    "mcpdrv.Attach is the per-server wiring entry point (D-150 → D-197 promotion)"
assert_grep_present 'mcpdrv\.NewRegistry\b' "internal/runtime/assemble/assemble.go" \
    "the assembly constructs the MCP Registry (D-150 via D-197)"
assert_grep_present 'mcpdrv\.Attach\(' "internal/runtime/assemble/assemble.go" \
    "the assembly attaches each cfg.Tools.MCPServers entry via mcpdrv.Attach"
assert_grep_present 'MCPRegistry \*mcpdrv.Registry' "harbortest/devstack/devstack.go" \
    "DevStack exposes the MCPRegistry for tests to inspect"

# ----------------------------------------------------------------------------
# Test fixture binary lives at the documented path.
# ----------------------------------------------------------------------------
assert_file "cmd/harbor-mcptest-stdio/main.go" \
    "harbor-mcptest-stdio test fixture binary present"
assert_file "cmd/harbor-mcptest-stdio/README.md" \
    "harbor-mcptest-stdio carries an explanatory README"
assert_grep_present 'mcp.AddTool' "cmd/harbor-mcptest-stdio/main.go" \
    "test fixture registers at least one tool via mcp.AddTool"

# ----------------------------------------------------------------------------
# Operator-facing documentation.
# ----------------------------------------------------------------------------
assert_grep_present 'mcp_servers:' "examples/harbor.yaml" \
    "examples/harbor.yaml documents the tools.mcp_servers[] block"
assert_grep_present 'transport_mode:' "examples/harbor.yaml" \
    "examples/harbor.yaml shows the per-server transport_mode field"

smoke_summary
