#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 72c — search.* cluster (5 methods, one phase).
#
# Surface assertions (executed only when the surface is live; 404/405/501
# auto-SKIP per AGENTS.md §4.2):
#   1. Five happy-path round-trips: search.query / search.sessions /
#      search.tasks / search.events / search.artifacts each return a
#      paginated response with a `rows` array.
#   2. Five cross-tenant rejections: each method, called with a
#      filter listing multiple tenants WITHOUT the auth.ScopeAdmin
#      scope claim, returns 403.
#   3. Five missing-identity rejections: each method, called without an
#      identity triple in context, returns 401.
#
# Until the phase ships, `protocol_call` stubs the calls (SKIP); when the
# phase lands, replace `protocol_call` invocations with real `curl` /
# `assert_status` / `assert_json_path` calls per the assertions above.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# 1. Happy-path round-trips — one per method.
# ----------------------------------------------------------------------------

protocol_call 'search/query' \
  '{"query": "hello", "indexes": ["sessions", "tasks", "events", "artifacts"]}' \
  'phase 72c: search.query palette dispatcher round-trips'

protocol_call 'search/sessions' \
  '{"query": "agent-a"}' \
  'phase 72c: search.sessions round-trips'

protocol_call 'search/tasks' \
  '{"query": "in-progress"}' \
  'phase 72c: search.tasks round-trips'

protocol_call 'search/events' \
  '{"query": "tool.failed"}' \
  'phase 72c: search.events round-trips'

protocol_call 'search/artifacts' \
  '{"query": "report.pdf"}' \
  'phase 72c: search.artifacts round-trips'

# ----------------------------------------------------------------------------
# 2. Cross-tenant rejection — each method must 403 when the filter lists
#    multiple tenants and the caller lacks the auth.ScopeAdmin claim.
# ----------------------------------------------------------------------------

protocol_call 'search/query' \
  '{"query": "x", "filter": {"tenant_ids": ["t1", "t2"]}}' \
  'phase 72c: search.query rejects cross-tenant filter without auth.ScopeAdmin claim'

protocol_call 'search/sessions' \
  '{"query": "x", "filter": {"tenant_ids": ["t1", "t2"]}}' \
  'phase 72c: search.sessions rejects cross-tenant filter without auth.ScopeAdmin claim'

protocol_call 'search/tasks' \
  '{"query": "x", "filter": {"tenant_ids": ["t1", "t2"]}}' \
  'phase 72c: search.tasks rejects cross-tenant filter without auth.ScopeAdmin claim'

protocol_call 'search/events' \
  '{"query": "x", "filter": {"tenant_ids": ["t1", "t2"]}}' \
  'phase 72c: search.events rejects cross-tenant filter without auth.ScopeAdmin claim'

protocol_call 'search/artifacts' \
  '{"query": "x", "filter": {"tenant_ids": ["t1", "t2"]}}' \
  'phase 72c: search.artifacts rejects cross-tenant filter without auth.ScopeAdmin claim'

# ----------------------------------------------------------------------------
# 3. Missing-identity rejection — each method must 401 when the caller's
#    context lacks the (tenant, user, session) triple.
# ----------------------------------------------------------------------------

protocol_call 'search/query' \
  '{"query": "x"}' \
  'phase 72c: search.query rejects missing identity context'

protocol_call 'search/sessions' \
  '{"query": "x"}' \
  'phase 72c: search.sessions rejects missing identity context'

protocol_call 'search/tasks' \
  '{"query": "x"}' \
  'phase 72c: search.tasks rejects missing identity context'

protocol_call 'search/events' \
  '{"query": "x"}' \
  'phase 72c: search.events rejects missing identity context'

protocol_call 'search/artifacts' \
  '{"query": "x"}' \
  'phase 72c: search.artifacts rejects missing identity context'

# ----------------------------------------------------------------------------
# Surface-existence probes — until the Protocol layer ships these routes,
# each probe SKIPs via 404. Once the phase lands, these flip to OK and
# the protocol_call stubs above are replaced with real assertions.
# ----------------------------------------------------------------------------

skip_if_404 "$(api_url /protocol/search/query)" \
  'phase 72c: search.query route absent until Protocol layer ships' || true
skip_if_404 "$(api_url /protocol/search/sessions)" \
  'phase 72c: search.sessions route absent until Protocol layer ships' || true
skip_if_404 "$(api_url /protocol/search/tasks)" \
  'phase 72c: search.tasks route absent until Protocol layer ships' || true
skip_if_404 "$(api_url /protocol/search/events)" \
  'phase 72c: search.events route absent until Protocol layer ships' || true
skip_if_404 "$(api_url /protocol/search/artifacts)" \
  'phase 72c: search.artifacts route absent until Protocol layer ships' || true

smoke_summary
