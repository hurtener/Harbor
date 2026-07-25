#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 108o — Console Artifacts page rebuilt to the carded, viewport-locked
# master-detail composition + the king file refactored to a controller, AND
# the real admin-gated `artifacts.delete` mutation landed (D-187). This smoke
# is live-server (it exercises the NEW method on the booted control surface)
# AND carries the static Console-side guard.
#
# Live-server assertions (404/405/501 → SKIP per CLAUDE.md §4.2):
#   1. The new route (POST /v1/control/artifacts.delete) is mounted. On a
#      pre-108o build it 404s → SKIP.
#   2. A POST with NO bearer is rejected 401 (Phase 61 auth fail-closed).
#   3. artifacts.delete with the dev bearer routes + gates: an idempotent
#      delete of an absent id returns 200 {deleted:false} (admin) OR 403
#      (identity_scope_required, non-admin) — never 404/500.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

DELETE_URL="$(api_url /v1/control/artifacts.delete)"

if command -v curl >/dev/null 2>&1; then
  set +e
  PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${DELETE_URL}")
  set -e
  case "${PROBE:-000}" in
    404 | 405 | 501 | 000)
      skip "phase 108o: /v1/control/artifacts.delete route not present (${PROBE:-000})"
      ;;
    *)
      set +e
      NOAUTH=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H 'Content-Type: application/json' -d '{}' "${DELETE_URL}")
      set -e
      if [ "${NOAUTH}" = "401" ]; then
        ok "phase 108o: artifacts.delete without bearer rejected (401)"
      else
        fail "phase 108o: artifacts.delete without bearer expected 401, got ${NOAUTH}"
      fi

      DEV_TOKEN=""
      if [ -n "${HARBOR_DATA_DIR:-}" ] && [ -f "${HARBOR_DATA_DIR}/server.log" ]; then
        DEV_TOKEN="$(grep -m1 '^HARBOR_DEV_TOKEN=' "${HARBOR_DATA_DIR}/server.log" 2>/dev/null | sed 's/^HARBOR_DEV_TOKEN=//' || true)"
      fi
      if [ -n "${DEV_TOKEN}" ]; then
        set +e
        DELC=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
          -X POST -H "Authorization: Bearer ${DEV_TOKEN}" -H 'Content-Type: application/json' \
          -d '{"scope":{"tenant":"dev","user":"dev","session":"dev"},"id":"inmem_deadbeefdead"}' "${DELETE_URL}")
        set -e
        case "${DELC}" in
          200 | 403) ok "phase 108o: artifacts.delete routes + admin-gates (${DELC})" ;;
          *) fail "phase 108o: artifacts.delete expected 200/403, got ${DELC}" ;;
        esac
      else
        skip 'phase 108o: dev bearer unavailable — artifacts.delete happy path covered by Go tests'
      fi
      ;;
  esac
else
  skip 'phase 108o: curl not available'
fi

# ---------------------------------------------------------------------------
# Static Console-side guard — the carded master-detail rebuild + the
# king-file refactor + the real admin delete wiring.
# ---------------------------------------------------------------------------
PAGE="web/console/src/routes/(console)/artifacts/+page.svelte"
STATE="web/console/src/lib/artifacts/state.svelte.ts"
DERIVE="web/console/src/lib/artifacts/derive.ts"
TABLE="web/console/src/lib/components/artifacts/ArtifactsTable.svelte"
CLIENT="web/console/src/lib/protocol/client.ts"

assert_file "${PAGE}" "phase 108o: Artifacts page route exists"
assert_grep_absent 'PageHeader' "${PAGE}" \
  "phase 108o: the page no longer renders a per-page PageHeader"
assert_grep_present 'panel card' "${PAGE}" \
  "phase 108o: the page adopts the carded .panel.card vocabulary"
assert_grep_present 'data-testid="artifacts-page"' "${PAGE}" \
  "phase 108o: the page keeps the artifacts-page root testid"

assert_file "${STATE}" "phase 108o: the ArtifactsPageState controller exists"
assert_grep_present 'export class ArtifactsPageState' "${STATE}" \
  "phase 108o: the module exports ArtifactsPageState"
assert_grep_present 'ArtifactsPageState' "${PAGE}" \
  "phase 108o: the page composes the ArtifactsPageState controller"
for verb in deleteArtifact deleteSelected uploadFile resolvePreview; do
  assert_grep_present "${verb}" "${STATE}" \
    "phase 108o: the controller wires ${verb}"
done
assert_grep_present 'hasScope' "${STATE}" \
  "phase 108o: the controller derives the admin claim (hasScope)"

assert_file "${DERIVE}" "phase 108o: the derive.ts pure-projection module exists"
assert_grep_present 'export function fmtSize' "${DERIVE}" \
  "phase 108o: derive.ts exports fmtSize"
assert_grep_present 'export function displayStatus' "${DERIVE}" \
  "phase 108o: derive.ts exports displayStatus (ready/empty derived live — D-180)"

assert_file "${TABLE}" "phase 108o: the ArtifactsTable component exists"
assert_grep_present 'data-testid="artifact-row-delete"' "${TABLE}" \
  "phase 108o: the table wires the real admin row Delete"
assert_grep_present 'data-testid="bulk-delete"' "${PAGE}" \
  "phase 108o: the bulk bar wires the real admin Delete selected"
assert_grep_present 'data-testid="action-delete"' "${PAGE}" \
  "phase 108o: the rail wires the real admin Delete"

# The client namespace exposes the new method.
assert_grep_present '/v1/control/artifacts.delete' "${CLIENT}" \
  "phase 108o: the artifacts namespace exposes the delete route"

# Save-view N7 contract + no hand-rolled fetch.
assert_grep_present 'data-testid="save-view"' "${PAGE}" \
  "phase 108o: the page keeps the save-view button (disconnected-state N7)"
if grep -qE '^[[:space:]]+Save view[[:space:]]*$' "${PAGE}"; then
  ok "phase 108o: the page renders the 'Save view' button label on its own line (phase-83s N7)"
else
  fail "phase 108o: the page must render a 'Save view' button on its own line (phase-83s N7)"
fi
assert_grep_absent 'fetch\(' "${PAGE}" \
  "phase 108o: the page route has no hand-rolled fetch (HarborClient only)"
assert_grep_absent 'fetch\(' "${STATE}" \
  "phase 108o: the controller has no hand-rolled fetch (HarborClient only)"

smoke_summary
