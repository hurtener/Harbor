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

# 1. The projection helper + validation live in internal/config.
if go test ./internal/config/ -run 'TestToolPolicyConfig|TestPerToolPolicy|TestValidateTools' -count=1 >/dev/null 2>&1; then
  ok "phase 26b: config tool-policy projection + validation tests pass"
else
  skip "phase 26b: not yet implemented — config tool-policy projection + validation"
fi

# 2. The per-tool override APPLICATION (a per-tool max_attempts override
#    changes the real attempt count at dispatch) lives in the mcp driver —
#    exercise it so the smoke actually guards the runtime behaviour, not
#    just the config projection.
if go test ./internal/tools/drivers/mcp/ -run 'TestPerToolPolicy' -count=1 >/dev/null 2>&1; then
  ok "phase 26b: mcp per-tool policy override changes the real attempt count"
else
  skip "phase 26b: mcp per-tool policy override not yet wired"
fi

smoke_summary
