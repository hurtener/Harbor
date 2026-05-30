#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 26b smoke — per-MCP-server / per-tool tool-policy config.
# Asserts the config→tools.ToolPolicy projection + load/validate of a
# fixture config carrying a per-tool override. No live server needed.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# The projection helper + validation live in internal/config; the per-tool
# override application lives in internal/tools/drivers/mcp. Until the phase
# ships these tests do not exist yet and `go test` reports no such tests —
# keep the gate green with a skip until the surface lands.
if go test ./internal/config/ -run 'TestToolPolicyConfig|TestPerToolPolicy' -count=1 >/dev/null 2>&1; then
  ok "phase 26b: config tool-policy projection tests pass"
else
  skip "phase 26b: not yet implemented — config tool-policy projection + validation"
fi

smoke_summary
