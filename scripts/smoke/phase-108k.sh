#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 108k — Console Tools page rebuilt to the carded, viewport-locked
# Events-108h / Background-Jobs-108j composition (D-183; supersedes the Phase 73f /
# pre-chrome layout). The page drops the per-page PageHeader (the breadcrumb is
# app-shell chrome), adopts the carded `.panel.card` vocabulary, viewport-locks
# (TABLE-primary + a right-rail detail), deepens the rail to full mock fidelity
# (Manifest / Inputs / Outputs / Recent invocations / Approval + Statistics /
# Content size / Source provenance / Run history), and refactors the ~847-line
# king file into a ToolsPageState controller + pure derive.ts projections + the
# focused ToolCatalogTable / ToolDetailRail components. No new Protocol method — a
# pure consumer of the shipped seven tools.* methods (Phase 73f / D-116).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

PAGE="web/console/src/routes/(console)/tools/+page.svelte"
STATE="web/console/src/lib/tools/state.svelte.ts"
DERIVE="web/console/src/lib/tools/derive.ts"
TABLE="web/console/src/lib/components/tools/ToolCatalogTable.svelte"
RAIL="web/console/src/lib/components/tools/ToolDetailRail.svelte"
OLD_TABS="web/console/src/lib/components/tools/ToolDetailTabs.svelte"

# ----------------------------------------------------------------------------
# 1. Route exists.
# ----------------------------------------------------------------------------
assert_file "${PAGE}" "phase 108k: Tools route exists"

# ----------------------------------------------------------------------------
# 2. REMOVED — the pre-chrome per-page PageHeader + the superseded ToolDetailTabs.
# ----------------------------------------------------------------------------
assert_grep_absent 'PageHeader' "${PAGE}" \
    "phase 108k: Tools page no longer renders a per-page PageHeader (breadcrumb is chrome)"
if [[ -f "${OLD_TABS}" ]]; then
    fail "phase 108k: the superseded ToolDetailTabs.svelte should be deleted (replaced by ToolDetailRail.svelte)"
else
    ok "phase 108k: the pre-chrome ToolDetailTabs.svelte is deleted (replaced by ToolDetailRail.svelte)"
fi

# ----------------------------------------------------------------------------
# 3. PRESENT — the carded, viewport-locked composition + the page root.
# ----------------------------------------------------------------------------
assert_grep_present 'data-testid="tools-page"' "${PAGE}" \
    "phase 108k: page keeps the tools-page root testid"
assert_grep_present 'panel card' "${PAGE}" \
    "phase 108k: page adopts the carded .panel.card vocabulary"
assert_grep_present 'ToolsPageState' "${PAGE}" \
    "phase 108k: page composes the ToolsPageState controller (king-file refactor)"

# ----------------------------------------------------------------------------
# 4. The controller — the king-file refactor target.
# ----------------------------------------------------------------------------
assert_file "${STATE}" "phase 108k: ToolsPageState controller exists"
assert_grep_present 'export class ToolsPageState' "${STATE}" \
    "phase 108k: the controller exports the ToolsPageState class"
# Wires the shipped admin write (no new Protocol method; §13 / D-079).
assert_grep_present 'setApprovalPolicy' "${STATE}" \
    "phase 108k: the controller wires the shipped tools.set_approval_policy admin write"
assert_grep_present 'revokeOAuth' "${STATE}" \
    "phase 108k: the controller wires the shipped tools.revoke_oauth admin write"

# ----------------------------------------------------------------------------
# 5. The pure projections — unit-testable module.
# ----------------------------------------------------------------------------
assert_file "${DERIVE}" "phase 108k: the derive.ts pure-projection module exists"
assert_grep_present 'export function lastUsed' "${DERIVE}" \
    "phase 108k: derive.ts exports lastUsed (Go zero-time → honest 'never')"
assert_grep_present 'export function displayStatus' "${DERIVE}" \
    "phase 108k: derive.ts exports displayStatus (ready/empty derived live — D-180)"
assert_grep_present 'export function toPageError' "${DERIVE}" \
    "phase 108k: derive.ts exports toPageError (no silent swallow — §13)"

# ----------------------------------------------------------------------------
# 6. The catalog table — DataTable wrapper with the scoped column override.
# ----------------------------------------------------------------------------
assert_file "${TABLE}" "phase 108k: the ToolCatalogTable component exists"
assert_grep_present 'data-testid="tools-catalog-row"' "${TABLE}" \
    "phase 108k: ToolCatalogTable renders the tools-catalog-row rows"
# N13 (carried from Phase 83x): the Reliability column keeps its width token so
# long tier values like `production-tested` no longer truncate mid-glyph.
assert_grep_present 'var\(--size-col-reliability\)' "${TABLE}" \
    "phase 108k: ToolCatalogTable keeps the Reliability column width token (N13)"
# The header-padding override is SCOPED under the .tools-table wrapper so it never
# leaks to another page's DataTable (the unscoped-:global leak that bit BG).
assert_grep_present '\.tools-table :global\(\.data-table thead th\)' "${TABLE}" \
    "phase 108k: ToolCatalogTable scopes its DataTable header override (no :global leak)"

# ----------------------------------------------------------------------------
# 7. The deepened right rail — full mock fidelity.
# ----------------------------------------------------------------------------
assert_file "${RAIL}" "phase 108k: the ToolDetailRail component exists"
assert_grep_present 'data-testid="tools-right-rail"' "${RAIL}" \
    "phase 108k: ToolDetailRail renders the per-tool detail surface"
# The five rail tabs are rendered from a TABS array via a shared template testid
# (`tools-detail-tab` + a data-tab key) — anchor on the tab keys, never a per-tab
# literal testid (which the shared template never emits).
for key in manifest inputs outputs recent approval; do
    assert_grep_present "'${key}'" "${RAIL}" \
        "phase 108k: ToolDetailRail declares the ${key} rail tab"
done
# The Approval Approve / Reject are the REAL admin method, not a fake string (§13).
assert_grep_present 'data-testid="tools-approve"' "${RAIL}" \
    "phase 108k: ToolDetailRail renders the real Approve control (tools.set_approval_policy)"
assert_grep_present 'onsetpolicy' "${RAIL}" \
    "phase 108k: ToolDetailRail wires Approve/Reject to the real admin write (no fabricated success)"

# ----------------------------------------------------------------------------
# 8. The Save-view contract (phase-83s / disconnected-state N7) is preserved.
# ----------------------------------------------------------------------------
assert_grep_present 'data-testid="tools-save-filter"' "${PAGE}" \
    "phase 108k: page keeps the tools-save-filter button (disconnected-state N7 contract)"
assert_grep_present 'placeholder="Save current as…"' "${PAGE}" \
    "phase 108k: page keeps the 'Save current as…' saved-view input (phase-83s N7)"
if grep -qE '^[[:space:]]+Save view[[:space:]]*$' "${PAGE}"; then
    ok "phase 108k: page renders the 'Save view' button label on its own line (phase-83s N7)"
else
    fail "phase 108k: page must render a 'Save view' button on its own line (phase-83s N7)"
fi
assert_grep_present 'DISCONNECTED_TOOLTIP' "${PAGE}" \
    "phase 108k: page gates disconnected controls with the canonical tooltip (phase-83r)"

smoke_summary
