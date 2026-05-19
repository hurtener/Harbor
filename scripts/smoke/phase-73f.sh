#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 73f — Console Tools page (Protocol + UI).
#
# Surface assertions (404/405/501 auto-SKIP per AGENTS.md §4.2):
#   1. tools.list / get / describe / metrics / content_stats round-trip.
#   2. tools.set_approval_policy + tools.revoke_oauth require tools.admin claim.
#   3. Page route /console/tools returns 200 (lands with 73m's harbor console subcommand — SKIPped here).
#
# Until the phase ships, `protocol_call` stubs each method (SKIP); when the
# Protocol layer lands, replace with real assert_status/assert_json_path
# calls per the assertions above.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

TOOL_ID_FALLBACK='tool-smoke-fixture'

# 1. tools.list happy path + filter.
protocol_call 'tools/list' '{}' \
  'phase 73f: tools.list returns catalog'
protocol_call 'tools/list' \
  '{"filter": {"transports": ["MCP"]}}' \
  'phase 73f: tools.list honors transport facet'

# 2. tools.get / describe / metrics / content_stats round-trip.
for method in get describe metrics content_stats; do
  protocol_call "tools/${method}" \
    "{\"id\": \"${TOOL_ID_FALLBACK}\"}" \
    "phase 73f: tools.${method} round-trips"
done

# 3. tools.metrics window arithmetic + status pill.
protocol_call 'tools/metrics' \
  "{\"id\": \"${TOOL_ID_FALLBACK}\", \"window\": \"1h\"}" \
  'phase 73f: tools.metrics returns status pill'

# 4. Admin methods rejected without tools.admin claim.
protocol_call 'tools/set_approval_policy' \
  "{\"id\": \"${TOOL_ID_FALLBACK}\", \"policy\": \"gated\"}" \
  'phase 73f: tools.set_approval_policy rejects requests without tools.admin claim'
protocol_call 'tools/revoke_oauth' \
  "{\"id\": \"${TOOL_ID_FALLBACK}\"}" \
  'phase 73f: tools.revoke_oauth rejects requests without tools.admin claim'

# 5. Page route — lands with 73m's harbor console subcommand.
skip 'phase 73f: /console/tools route lands with 73m harbor console subcommand'

# Surface-existence probe — flips from SKIP to OK when the Protocol layer ships.
skip_if_404 "$(api_url /protocol/tools/list)" \
  'phase 73f: tools.list route absent until Protocol layer ships' || true

smoke_summary
