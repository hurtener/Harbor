#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 200 — Per-user credential injection for receiver-style MCP servers (HA-34).
#
# When the surface lands, assert:
#   - A connection declaring BOTH an injection mapping and a bearer oauth_provider is
#     rejected (one auth mode per connection).
#   - The audit surface never emits an injected credential in cleartext — the extended
#     redaction rules (Basic scheme / declared header keys / _meta credential keys)
#     hold every form to *** (static grep guard + live probe where a dev receiver
#     fixture is reachable) (D-341).
# Until then, SKIP cleanly so preflight stays green (404/405/501 → SKIP).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 200: per-user credential-injection smoke — assertions land with the implementation"

smoke_summary
