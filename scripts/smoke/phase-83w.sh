#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 83w smoke — wire-surface gaps (F5 Console + F6 Runtime).
#
# This script covers the Go-side F6 assertions. The F5 (Console-side)
# assertions land in a separate script Agent B / coordinator adds when
# the Console patch lands.
#
# F6 is wiring-only: the Phase 73k MCPSurface dispatcher already
# existed (internal/protocol/mcp.go); F6 simply constructs it in
# bootDevStack and threads it into transports.NewMux via the
# WithMCPSurface option. The smoke pins the call sites.
#
# Conventions (CLAUDE.md §4.2):
#   - 404/405/501 → SKIP for any live-server checks (not used here).
#   - At least one OK once the phase has shipped.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# F6.1 — cmd_dev.go constructs the MCPSurface from the boot-time
# mcpRegistry.
assert_grep_present 'protocol\.NewMCPSurface\(protocol\.MCPDeps\{' \
    "cmd/harbor/cmd_dev.go" \
    "phase 83w F6: bootDevStack constructs MCPSurface"

# F6.2 — bootDevStack threads the MCPSurface into transports.NewMux.
assert_grep_present 'transports\.WithMCPSurface\(mcpSurface\)' \
    "cmd/harbor/cmd_dev.go" \
    "phase 83w F6: bootDevStack wires MCPSurface into transports.NewMux"

# F6.3 — devstack.Assemble mirrors per D-094.
assert_grep_present 'protocol\.NewMCPSurface\(protocol\.MCPDeps\{' \
    "harbortest/devstack/devstack.go" \
    "phase 83w F6: devstack.Assemble constructs MCPSurface (D-094 mirror)"
assert_grep_present 'transports\.WithMCPSurface\(mcpSurface\)' \
    "harbortest/devstack/devstack.go" \
    "phase 83w F6: devstack.Assemble wires MCPSurface (D-094 mirror)"

# F6.4 — mcpconsole gained a NoOAuthAccessor for the V1 dev posture
# (no OAuth providers configured).
assert_grep_present 'type NoOAuthAccessor struct' \
    "internal/mcpconsole/mcpconsole.go" \
    "phase 83w F6: mcpconsole.NoOAuthAccessor declared"
assert_grep_present 'ErrNoOAuthConfigured' \
    "internal/mcpconsole/mcpconsole.go" \
    "phase 83w F6: mcpconsole.ErrNoOAuthConfigured sentinel declared"

# F6.5 — the integration test exists and pins the wire surface.
assert_file "test/integration/phase83w_mcp_servers_list_test.go" \
    "phase 83w F6: integration test pins mcp.servers.list wire surface"

smoke_summary
