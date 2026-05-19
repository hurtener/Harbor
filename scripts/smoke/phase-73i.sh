#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 73i — Console Flows page (Protocol + UI).
#
# Surface assertions (404/405/501 auto-SKIP per AGENTS.md §4.2):
#   1. 6 NEW flows.* Protocol methods round-trip.
#   2. flows.run requires flows.run scope claim — rejection without claim.
#   3. Cross-tenant flows.list without admin → 403.
#   4. Page route /console/flows — SKIPped until 73m harbor console lands.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

FLOW_ID_FALLBACK='flow-smoke-fixture'
RUN_ID_FALLBACK='run-smoke-fixture'

# 1. 6 new flows.* methods.
protocol_call 'flows/list' '{}' \
  'phase 73i: flows.list returns paginated catalog'
protocol_call 'flows/describe' \
  "{\"id\": \"${FLOW_ID_FALLBACK}\"}" \
  'phase 73i: flows.describe returns engine graph'
protocol_call 'flows/runs/list' \
  "{\"flow_id\": \"${FLOW_ID_FALLBACK}\"}" \
  'phase 73i: flows.runs.list returns run history'
protocol_call 'flows/runs/describe' \
  "{\"run_id\": \"${RUN_ID_FALLBACK}\"}" \
  'phase 73i: flows.runs.describe returns per-node timeline + ArtifactRef'
protocol_call 'flows/metrics' \
  "{\"flow_id\": \"${FLOW_ID_FALLBACK}\"}" \
  'phase 73i: flows.metrics returns sparkline aggregates'
protocol_call 'flows/run' \
  "{\"flow_id\": \"${FLOW_ID_FALLBACK}\", \"inputs\": {}}" \
  'phase 73i: flows.run rejects requests without flows.run scope claim'

# 2. Cross-tenant flows.list without admin → 403.
protocol_call 'flows/list' \
  '{"filter": {"tenants": ["t1", "t2"]}}' \
  'phase 73i: flows.list rejects cross-tenant filter without admin claim'

# 3. Page route.
skip 'phase 73i: /console/flows route lands with 73m harbor console subcommand'

skip_if_404 "$(api_url /protocol/flows/list)" \
  'phase 73i: flows.list route absent until Protocol layer ships' || true

smoke_summary
