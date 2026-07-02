#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 151 smoke — runtime loading-mode control on tool exposure (D-281):
# the `server_loading_modes` / `tool_loading_modes` overrides on the
# agent-config ToolExposure section, the `tools.LoadingOverrideView`
# run-start projection, the `Tool.Form` classification, the effective
# `loading_mode` on `tools.describe` (optional `agent_id`), the D-223 TS
# mirror, and the live flip round-trip against the dev server:
#
#   - static greps: ToolExposure map fields (Go + wire + TS mirror),
#     LoadingOverrideView, projection wiring, ToolForm, regenerated docs.
#   - live: bootstrap an admin dev token (phase-114 pattern); resolve the
#     dev agent id via agents.list (SKIP when absent); tools.describe a
#     built-in and pin loading_mode; set_tool_exposure with a
#     tool_loading_modes flip -> re-describe with agent_id asserting the
#     effective loading_mode flipped; an unknown loading value -> 400
#     invalid_request (no revision recorded); restore -> original mode;
#     unauthenticated POST stays non-200.
#
# Conventions (AGENTS.md §4.2): 404/405/501 -> SKIP; OK >= 1 once shipped;
# use scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 151: smoke skeleton — replace with real assertions when the phase implements its surface"

smoke_summary
