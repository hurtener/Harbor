#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 73j — Console Memory page (Protocol + UI).
#
# Surface assertions (404/405/501 auto-SKIP per AGENTS.md §4.2):
#   1. memory.list / memory.get / memory.health round-trip.
#   2. memory.list with foreign tenant filter requires the auth.ScopeAdmin
#      claim — rejected without it.
#   3. memory.* requests with an incomplete identity triple fail loudly per
#      D-033 (the runtime emits memory.identity_rejected and the Protocol
#      handler returns CodeIdentityRequired).
#   4. memory.get on a heavy-value key returns ValueArtifact (NOT inline
#      bytes) per D-026.
#   5. Page route /console/memory returns 200 (lands with 73m's harbor
#      console subcommand — SKIPped here).
#
# Until the phase ships, `protocol_call` stubs each method (SKIP); when the
# Protocol layer lands, replace with real assert_status / assert_json_path
# calls per the assertions above.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

MEMORY_KEY_FALLBACK='memory-smoke-fixture'
FOREIGN_TENANT_ID='t-other'

# 1. memory.list happy path + facet honour.
protocol_call 'memory/list' '{}' \
  'phase 73j: memory.list returns paginated items for caller identity scope'
protocol_call 'memory/list' \
  '{"filter": {"scopes": ["session"]}}' \
  'phase 73j: memory.list honors scope facet'
protocol_call 'memory/list' \
  '{"filter": {"drivers": ["inmem"]}}' \
  'phase 73j: memory.list honors driver facet'
protocol_call 'memory/list' \
  '{"filter": {"strategies": ["truncation"]}}' \
  'phase 73j: memory.list honors strategy facet'
protocol_call 'memory/list' \
  '{"filter": {"has_ttl_expiring": true}}' \
  'phase 73j: memory.list honors has_ttl_expiring filter'
protocol_call 'memory/list' \
  '{"filter": {"content_search": "fixture"}}' \
  'phase 73j: memory.list honors content_search (runtime-side per brief 11 §CC-4)'

# 2. memory.get round-trip — assert exactly one of value / value_artifact populated.
protocol_call 'memory/get' \
  "{\"key\": \"${MEMORY_KEY_FALLBACK}\"}" \
  'phase 73j: memory.get returns full item detail (light value path)'

# 3. memory.get on heavy-content key returns ValueArtifact, NEVER inline bytes (D-026).
protocol_call 'memory/get' \
  "{\"key\": \"${MEMORY_KEY_FALLBACK}-heavy\"}" \
  'phase 73j: memory.get returns ArtifactStub for heavy values (D-026 closure)'

# 4. memory.health aggregate counters + driver-by-scope.
protocol_call 'memory/health' '{}' \
  'phase 73j: memory.health returns aggregate counters + driver_by_scope'

# 5. Cross-tenant filter rejected without auth.ScopeAdmin claim (D-079 pattern).
protocol_call 'memory/list' \
  "{\"filter\": {\"tenant_ids\": [\"${FOREIGN_TENANT_ID}\"]}}" \
  'phase 73j: memory.list rejects foreign-tenant filter without auth.ScopeAdmin claim'

# 6. Identity-required failure-loud — D-033 closure at the Protocol edge.
#    A request whose identity scope is missing session_id MUST fail loudly with
#    CodeIdentityRequired (401 / 403 are both acceptable canonical codes for the
#    Phase 61 auth posture; the runtime ALSO emits memory.identity_rejected on the
#    bus per D-033 — the integration test asserts that emission, this smoke only
#    asserts the wire-level rejection.)
protocol_call 'memory/list' '{}' \
  'phase 73j: memory.list rejects requests with incomplete identity triple (D-033 / D-001)'

# 7. Identity-rejection event MUST NOT be masked — surfaced via the Phase 72a-extended
#    events.subscribe filter. The Console renders the event verbatim; NO "view rejected
#    memory anyway" affordance (§13 forbidden-practice). The smoke probes the subscription
#    surface; the Playwright spec asserts the UI rendering.
protocol_call 'events/subscribe' \
  '{"filter": {"event_types": ["memory.identity_rejected"]}}' \
  'phase 73j: events.subscribe filter for memory.identity_rejected surfaces (D-033)'

# 8. Recovery-dropped event surface — D-035 wire string is memory.recovery_dropped.
#    (page-memory.md §12 mockup-refinements names it memory.overflow_drop_oldest — that
#    naming drift is a docs(design) follow-up; the smoke and runtime use the shipped
#    constant.)
protocol_call 'events/subscribe' \
  '{"filter": {"event_types": ["memory.recovery_dropped"]}}' \
  'phase 73j: events.subscribe filter for memory.recovery_dropped surfaces (D-035)'

# 9. Page route — lands with 73m's harbor console subcommand.
skip 'phase 73j: /console/memory route lands with 73m harbor console subcommand'

# Surface-existence probe — flips from SKIP to OK when the Protocol layer ships.
skip_if_404 "$(api_url /protocol/memory/list)" \
  'phase 73j: memory.list route absent until Protocol layer ships' || true

smoke_summary
