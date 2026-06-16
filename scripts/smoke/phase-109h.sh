#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 109h smoke — MCP Apps UI-host capability advertisement.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# This phase makes the MCP southbound driver advertise the host's renderable
# MCP App (`io.modelcontextprotocol/ui`) display modes during the initialize
# handshake (closing brief 14's "ClientCapabilities.Extensions never
# populated" gap), while PRESERVING the current roots advertisement. The
# surface is an OUTBOUND client→server capability — there is no inbound
# Protocol method to probe — so the assertions are static: the
# capability-advertisement code, the roots-preservation guard, the config
# field, and its validation all exist.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

MCP_GO="internal/tools/drivers/mcp/mcp.go"
ATTACH_GO="internal/tools/drivers/mcp/attach.go"
ASSEMBLE_GO="internal/runtime/assemble/assemble.go"
CONFIG_GO="internal/config/config.go"
VALIDATE_GO="internal/config/validate.go"

# ----------------------------------------------------------------------------
# 1. The driver advertises the UI extension with the host's display modes.
# ----------------------------------------------------------------------------
if grep -q 'func hostCapabilities' "${MCP_GO}" &&
    grep -q 'AddExtension(uiExtensionKey' "${MCP_GO}"; then
    ok 'phase 109h: mcp driver advertises the io.modelcontextprotocol/ui host capability'
else
    fail 'phase 109h: hostCapabilities / UI-extension advertisement missing from mcp.go'
fi

# ----------------------------------------------------------------------------
# 2. The roots advertisement is PRESERVED (setting any capability overrides the
#    SDK default; RootsV2 must be replicated or roots silently drops).
# ----------------------------------------------------------------------------
if grep -q 'RootsV2: &mcpsdk.RootCapabilities{ListChanged: true}' "${MCP_GO}"; then
    ok 'phase 109h: roots capability preserved when opting into the UI extension'
else
    fail 'phase 109h: roots-preservation guard (RootsV2 ListChanged) missing — roots would silently drop'
fi

# ----------------------------------------------------------------------------
# 3. HostDisplayModes is threaded config → AttachDeps → driver Config.
# ----------------------------------------------------------------------------
if grep -q 'HostDisplayModes' "${ATTACH_GO}" &&
    grep -q 'HostDisplayModes' "${MCP_GO}" &&
    grep -q 'MCPAppHostDisplayModes()' "${ASSEMBLE_GO}"; then
    ok 'phase 109h: HostDisplayModes threaded config → AttachDeps → driver'
else
    fail 'phase 109h: HostDisplayModes threading is broken (config → AttachDeps → mcp.Config)'
fi

# ----------------------------------------------------------------------------
# 4. The deployment-level config field + its validation exist.
# ----------------------------------------------------------------------------
if grep -q 'MCPAppHost \*MCPAppHostConfig' "${CONFIG_GO}" &&
    grep -q 'allowedMCPAppDisplayModes' "${VALIDATE_GO}"; then
    ok 'phase 109h: tools.mcp_app_host config field + display-mode validation present'
else
    fail 'phase 109h: tools.mcp_app_host config field or its validation missing'
fi

# ----------------------------------------------------------------------------
# 5. The example config documents the new field.
# ----------------------------------------------------------------------------
if grep -q 'mcp_app_host' examples/harbor.yaml; then
    ok 'phase 109h: examples/harbor.yaml documents tools.mcp_app_host.display_modes'
else
    fail 'phase 109h: examples/harbor.yaml does not document the mcp_app_host block'
fi

smoke_summary
