#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 108p — Console Flows page rebuilt to the carded, viewport-locked
# composition + the king files refactored to `FlowsListState` / `FlowDetailState`
# controllers + pure `derive.ts` + the focused `FlowsTable` component, and the
# inaccurate planner-family empty-state copy corrected to the flows-as-tools
# truth (D-188). Phase 108p adds NO Protocol method — the `flows.*` surface
# shipped in Phase 73i — so this smoke is primarily the static Console-side
# guard, plus a best-effort live probe that the shipped `flows.list` route is
# mounted and fails closed without a bearer.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ---------------------------------------------------------------------------
# Live-server probe — the shipped flows.list route (404/405/501 → SKIP per §4.2).
# ---------------------------------------------------------------------------
LIST_URL="$(api_url /v1/flows/list)"
if command -v curl >/dev/null 2>&1; then
  set +e
  PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${LIST_URL}")
  set -e
  case "${PROBE:-000}" in
    404 | 405 | 501 | 000)
      skip "phase 108p: /v1/flows/list route not present (${PROBE:-000})"
      ;;
    401)
      ok "phase 108p: flows.list without bearer fails closed (401)"
      ;;
    *)
      ok "phase 108p: flows.list route mounted (${PROBE})"
      ;;
  esac
else
  skip 'phase 108p: curl not available'
fi

# ---------------------------------------------------------------------------
# Static Console-side guard — the carded rebuild + the king-file refactor +
# the corrected empty-state copy.
# ---------------------------------------------------------------------------
LIST_PAGE="web/console/src/routes/(console)/flows/+page.svelte"
DETAIL_PAGE="web/console/src/routes/(console)/flows/[flow_id]/+page.svelte"
LIST_STATE="web/console/src/lib/flows/state.svelte.ts"
DETAIL_STATE="web/console/src/lib/flows/detail.svelte.ts"
DERIVE="web/console/src/lib/flows/derive.ts"
TABLE="web/console/src/lib/components/flows/FlowsTable.svelte"

assert_file "${LIST_PAGE}" "phase 108p: Flows list route exists"
assert_file "${DETAIL_PAGE}" "phase 108p: Flow detail route exists"

# The per-page PageHeader is gone on both routes (chrome supersedes it — 108b).
assert_grep_absent 'PageHeader' "${LIST_PAGE}" \
  "phase 108p: the list page no longer renders a per-page PageHeader"
assert_grep_absent 'PageHeader' "${DETAIL_PAGE}" \
  "phase 108p: the detail page no longer renders a per-page PageHeader"

# The carded .panel.card viewport-locked vocabulary on both routes.
assert_grep_present 'panel card' "${LIST_PAGE}" \
  "phase 108p: the list page adopts the carded .panel.card vocabulary"
assert_grep_present 'panel card' "${DETAIL_PAGE}" \
  "phase 108p: the detail page adopts the carded .panel.card vocabulary"
assert_grep_present 'data-testid="flows-page"' "${LIST_PAGE}" \
  "phase 108p: the list page keeps the flows-page root testid"
assert_grep_present 'data-testid="flow-detail-page"' "${DETAIL_PAGE}" \
  "phase 108p: the detail page keeps the flow-detail-page root testid"

# The king-file refactor: controllers + derive.ts + FlowsTable exist + are used.
assert_file "${LIST_STATE}" "phase 108p: the FlowsListState controller exists"
assert_grep_present 'export class FlowsListState' "${LIST_STATE}" \
  "phase 108p: the module exports FlowsListState"
assert_grep_present 'FlowsListState' "${LIST_PAGE}" \
  "phase 108p: the list page composes the FlowsListState controller"
assert_file "${DETAIL_STATE}" "phase 108p: the FlowDetailState controller exists"
assert_grep_present 'export class FlowDetailState' "${DETAIL_STATE}" \
  "phase 108p: the module exports FlowDetailState"
assert_grep_present 'FlowDetailState' "${DETAIL_PAGE}" \
  "phase 108p: the detail page composes the FlowDetailState controller"

assert_file "${DERIVE}" "phase 108p: the derive.ts pure-projection module exists"
assert_grep_present 'export function displayStatus' "${DERIVE}" \
  "phase 108p: derive.ts exports displayStatus (ready/empty derived live — D-180)"
assert_grep_present 'export function successKind' "${DERIVE}" \
  "phase 108p: derive.ts exports successKind"
assert_grep_present 'export function health' "${DERIVE}" \
  "phase 108p: derive.ts exports the health pill projection"

assert_file "${TABLE}" "phase 108p: the FlowsTable component exists"
assert_grep_present 'data-testid="catalog-row"' "${TABLE}" \
  "phase 108p: the table keeps the catalog-row marker (row-click → detail)"
assert_grep_present 'data-testid="catalog-run"' "${TABLE}" \
  "phase 108p: the table wires the admin-gated Run flow action"

# The corrected empty-state copy: flows are TOOLS, not planner-bound (D-188).
assert_grep_present 'registered as' "${LIST_PAGE}" \
  "phase 108p: the empty-state copy describes flows as registered tools (D-188)"
assert_grep_absent 'whose planner is Graph' "${LIST_PAGE}" \
  "phase 108p: the inaccurate planner-family empty-state copy is gone (D-188)"

# Run flow stays the only mutating action, admin-gated + view-only (D-063/D-079).
assert_grep_present 'data-testid="detail-run"' "${DETAIL_PAGE}" \
  "phase 108p: the detail page keeps the admin-gated Run this flow action"

# No hand-rolled fetch — HarborClient via the controllers only (§13).
assert_grep_absent 'fetch(' "${LIST_PAGE}" \
  "phase 108p: the list route has no hand-rolled fetch (HarborClient only)"
assert_grep_absent 'fetch(' "${DETAIL_PAGE}" \
  "phase 108p: the detail route has no hand-rolled fetch (HarborClient only)"
assert_grep_absent 'fetch(' "${LIST_STATE}" \
  "phase 108p: the list controller has no hand-rolled fetch (HarborClient only)"
assert_grep_absent 'fetch(' "${DETAIL_STATE}" \
  "phase 108p: the detail controller has no hand-rolled fetch (HarborClient only)"

# Save-view N7 contract (the button label on its own line — phase-83s).
assert_grep_present 'data-testid="flows-save-view"' "${LIST_PAGE}" \
  "phase 108p: the page keeps the save-view button (disconnected-state N7)"

smoke_summary
