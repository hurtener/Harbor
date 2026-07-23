#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 198 — Live-layer idempotent MCP re-attach (HA-33).
#
# When the surface lands, assert:
#   - agent_config.add_mcp_connection is a known method (present in the wire manifest).
#   - Where a dev MCP fixture is reachable: add a connection, then re-add the SAME
#     name, and assert the second call returns a success state (online) rather than a
#     duplicate-tool-name failure — the idempotent same-name replace (D-339).
# Until then, SKIP cleanly so preflight stays green (404/405/501 → SKIP).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 198: idempotent MCP re-attach smoke — assertions land with the implementation"

smoke_summary
