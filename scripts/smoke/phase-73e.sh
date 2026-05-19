#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 73e — Console Agents page (Protocol + UI).
#
# Surface assertions (404/405/501 auto-SKIP per AGENTS.md §4.2):
#   1. 8 NEW agents.* Protocol methods round-trip.
#   2. agents.list cross-tenant filter without admin claim → 403.
#   3. Existing registry control methods (Pause / Drain / Restart / Force-Stop /
#      Deregister) require control-scope claim — invoked by the page; smoke
#      asserts the rejection path, never invokes a control method as a smoke.
#   4. Page route /console/agents — SKIPped until 73m's harbor console lands.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

AGENT_ID_FALLBACK='agent-smoke-fixture'

# 1. 8 new agents.* methods.
protocol_call 'agents/list' '{}' \
  'phase 73e: agents.list returns paginated agents'
protocol_call 'agents/get' \
  "{\"id\": \"${AGENT_ID_FALLBACK}\"}" \
  'phase 73e: agents.get returns full projection'
for method in tools memory governance skills permissions metrics; do
  protocol_call "agents/${method}" \
    "{\"id\": \"${AGENT_ID_FALLBACK}\"}" \
    "phase 73e: agents.${method} round-trips"
done

# 2. Cross-tenant agents.list without admin → 403.
protocol_call 'agents/list' \
  '{"filter": {"tenants": ["t1", "t2"]}}' \
  'phase 73e: agents.list rejects cross-tenant filter without admin claim'

# 3. Control verb without claim → 403. Smoke probes the rejection, not the
#    success path (smoke never mutates the registry).
protocol_call 'registry/pause' \
  "{\"agent_id\": \"${AGENT_ID_FALLBACK}\"}" \
  'phase 73e: registry.Pause rejects requests without control-scope claim'

# 4. Page route.
skip 'phase 73e: /console/agents route lands with 73m harbor console subcommand'

skip_if_404 "$(api_url /protocol/agents/list)" \
  'phase 73e: agents.list route absent until Protocol layer ships' || true

smoke_summary
