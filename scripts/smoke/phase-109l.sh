#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 109l — MCP Apps host theme + data delivery (re-land, handshake-safe).
#
# When the surface lands, assert (static, against the TS source):
#   - app-bridge-host.ts constructs the ui/initialize host-context with
#     styles/variables (theme + design tokens) and has a post-init setHostContext
#     theme relay + the sendToolInput/sendToolResult delivery.
#   - the lifecycle $effect in mcp-app.svelte does NOT depend on the theme store
#     (guard against the reverted teardown-mid-handshake pattern).
#   - flip the re-land SKIP guards in scripts/smoke/phase-109j.sh to OK-on-presence.
# The real gates are the real-iframe Playwright handshake test + the binding
# live-render (analytics-widgets under a real gpt-5.6-luna agent, screenshot).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 109l: MCP Apps theme + data-delivery — assertions land with the implementation"

smoke_summary
