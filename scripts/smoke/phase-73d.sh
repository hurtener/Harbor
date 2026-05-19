#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 73d — Console Tasks page (kanban + bulk control).
#
# Surface assertions (404/405/501 auto-SKIP per AGENTS.md §4.2):
#   1. tasks.list happy path + filter facets (status / kind / parent-task).
#   2. tasks.list rejects cross-tenant filter without `admin` scope claim.
#   3. tasks.list rejects missing identity (CodeIdentityRequired).
#   4. tasks.get enrichment shape (parent_session / parent_task / cost / planner_snapshot).
#   5. tasks.get cross-tenant id → CodeNotFound (existence never revealed).
#   6. Phase 54 control verbs (cancel / pause / prioritize) reject requests
#      without the `tasks.control` scope claim — verifies the bulk-action
#      toolbar's gating consumes the EXISTING shipped methods (§13 no
#      parallel implementations) and that prioritize honours payload bounds.
#   7. /console/tasks page route — lands with 73m harbor console subcommand.
#
# Until the phase ships, `protocol_call` stubs each method (SKIP); when the
# Protocol layer lands, replace with real assert_status/assert_json_path
# calls per the assertions above.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

TASK_ID_FALLBACK='task-smoke-fixture'
CROSS_TENANT_TASK_ID='task-cross-tenant-fixture'
SPAWN_PARENT_FALLBACK='task-spawn-parent-fixture'

# 1. tasks.list happy path + facet filters.
protocol_call 'tasks/list' '{}' \
  'phase 73d: tasks.list returns paginated rows + aggregates'

protocol_call 'tasks/list' \
  '{"filter": {"statuses": ["running"]}}' \
  'phase 73d: tasks.list honors status facet'

protocol_call 'tasks/list' \
  '{"filter": {"kinds": ["background"]}}' \
  'phase 73d: tasks.list honors kind facet'

protocol_call 'tasks/list' \
  "{\"filter\": {\"parent_task_id\": \"${SPAWN_PARENT_FALLBACK}\"}}" \
  'phase 73d: tasks.list honors SpawnTask parent drill-down'

# 2. Cross-tenant filter without admin claim → 403 (CodeScopeMismatch).
protocol_call 'tasks/list' \
  '{"filter": {"identities": [{"tenant_id": "t1"}, {"tenant_id": "t2"}]}}' \
  'phase 73d: tasks.list rejects cross-tenant filter without admin scope claim'

# 3. Missing identity context → 401 (CodeIdentityRequired).
protocol_call 'tasks/list' '{}' \
  'phase 73d: tasks.list rejects missing identity context (CodeIdentityRequired)'

# 4. tasks.get enrichment round-trip.
protocol_call 'tasks/get' \
  "{\"id\": \"${TASK_ID_FALLBACK}\"}" \
  'phase 73d: tasks.get returns enriched detail (parent_session + parent_task + cost + planner_snapshot)'

# 5. Cross-tenant tasks.get → 404 (existence never revealed).
protocol_call 'tasks/get' \
  "{\"id\": \"${CROSS_TENANT_TASK_ID}\"}" \
  'phase 73d: tasks.get returns CodeNotFound for cross-tenant task id (existence not revealed)'

# 6. Phase 54 shipped control verbs gated by tasks.control claim.
#    The bulk-action toolbar consumes these EXISTING methods (no new method).
protocol_call 'cancel' \
  "{\"id\": \"${TASK_ID_FALLBACK}\"}" \
  'phase 73d: cancel rejects requests without tasks.control claim (Phase 54 gate)'

protocol_call 'pause' \
  "{\"id\": \"${TASK_ID_FALLBACK}\"}" \
  'phase 73d: pause rejects requests without tasks.control claim (Phase 54 gate)'

protocol_call 'resume' \
  "{\"id\": \"${TASK_ID_FALLBACK}\"}" \
  'phase 73d: resume rejects requests without tasks.control claim (Phase 54 gate)'

protocol_call 'prioritize' \
  "{\"id\": \"${TASK_ID_FALLBACK}\", \"payload\": {\"priority\": 9999}}" \
  'phase 73d: prioritize rejects out-of-range priority (CodePayloadInvalid)'

protocol_call 'approve' \
  "{\"id\": \"${TASK_ID_FALLBACK}\"}" \
  'phase 73d: approve rejects requests without tasks.control claim (Phase 54 gate)'

protocol_call 'reject' \
  "{\"id\": \"${TASK_ID_FALLBACK}\"}" \
  'phase 73d: reject rejects requests without tasks.control claim (Phase 54 gate)'

# 7. Page route lands with 73m harbor console subcommand.
skip 'phase 73d: /console/tasks route lands with 73m harbor console subcommand'

# Surface-existence probe — flips from SKIP to OK when the Protocol layer ships.
skip_if_404 "$(api_url /protocol/tasks/list)" \
  'phase 73d: tasks.list route absent until Protocol layer ships' || true

smoke_summary
