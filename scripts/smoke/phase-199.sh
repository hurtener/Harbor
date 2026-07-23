#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 199 — Wire-carried OAuth-provider descriptor, dev-gated (HA-32).
#
# When the surface lands, assert the exfil guard (modeled on phase-169.sh):
#   - With the opt-in OFF (default preflight boot), a set_oauth_provider /
#     add_mcp_connection descriptor carrying a credential-sink field (token_url, or a
#     downstream-host list) is REJECTED 400 — the D-303 name-only posture holds by
#     default (D-340).
#   - Both verbs are present in the wire manifest + generated methods.md.
# Until then, SKIP cleanly so preflight stays green (404/405/501 → SKIP).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 199: wire-OAuth-descriptor exfil-guard smoke — assertions land with the implementation"

smoke_summary
