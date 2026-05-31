#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83r — disconnected-state hygiene (W1/W2/W3/N4/N5/N8/N9/N10).
#
# Static-source assertions only. The live behaviour (disabled buttons,
# desaturated chips, single empty-state message) is covered by the
# Playwright spec `web/console/tests/disconnected-state.spec.ts` per the
# §17 contract. This smoke is a tripwire that pins the load-bearing
# strings + helper exports so a refactor cannot silently delete them.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# -----------------------------------------------------------------------------
# Shared connection helpers (the single sanctioned read).
# -----------------------------------------------------------------------------
assert_grep_present \
    'export function isDisconnected' \
    'web/console/src/lib/connection.ts' \
    'connection.ts exports the shared isDisconnected predicate'

assert_grep_present \
    "export const DISCONNECTED_TOOLTIP = 'Attach a Runtime to enable'" \
    'web/console/src/lib/connection.ts' \
    'connection.ts pins the canonical disconnected tooltip string'

# -----------------------------------------------------------------------------
# Per-page wiring: each page imports the shared tooltip + routes its
# controls through a `disconnected` predicate. Pages without filter /
# action surfaces (the Settings + Playground variants that work
# disconnected by design — D-158) are exempt.
# -----------------------------------------------------------------------------
for page in overview live-runtime sessions tasks agents tools events background-jobs flows memory mcp-connections artifacts; do
    file="web/console/src/routes/(console)/${page}/+page.svelte"
    # Phase 108c: the Overview rebuild moved its only disconnected-gated control
    # (the Refresh button) out of the page and into the ContextAuditRow
    # component, which imports the canonical tooltip — assert there for overview.
    if [ "${page}" = "overview" ]; then
        file="web/console/src/lib/components/overview/ContextAuditRow.svelte"
    fi
    assert_grep_present \
        'DISCONNECTED_TOOLTIP' \
        "${file}" \
        "${page} disconnected controls use the canonical tooltip"
done

# -----------------------------------------------------------------------------
# W1 — Overview Cost Rollup card stops rendering synthetic `$0.00` data
# when disconnected.
# -----------------------------------------------------------------------------
assert_grep_present \
    'cost-rollup-disconnected' \
    'web/console/src/lib/components/overview/CostRollupCard.svelte' \
    'Overview CostRollupCard exposes a disconnected branch (W1)'

# -----------------------------------------------------------------------------
# W2 — Live Runtime composer accepts the disconnected prop + every verb
# routes through tipFor() (which prefers the disconnected tooltip).
# -----------------------------------------------------------------------------
assert_grep_present \
    'disconnected\?: boolean' \
    'web/console/src/lib/components/live-runtime/composer/run-composer.svelte' \
    'RunComposer accepts the disconnected prop (W2)'
assert_grep_present \
    'composer-textarea' \
    'web/console/src/lib/components/live-runtime/composer/run-composer.svelte' \
    'RunComposer textarea is the canonical composer-textarea testid'

# -----------------------------------------------------------------------------
# N5 — Tools page collapses ToolDetailTabs when disconnected (one empty
# message, not two).
# -----------------------------------------------------------------------------
assert_grep_present \
    '#if !disconnected' \
    'web/console/src/routes/(console)/tools/+page.svelte' \
    'Tools page collapses the secondary empty when disconnected (N5)'

# -----------------------------------------------------------------------------
# N8 — StatusChip accepts the desaturated prop + MCP Connections threads
# the page's disconnected predicate into the chip.
# -----------------------------------------------------------------------------
assert_grep_present \
    'desaturated' \
    'web/console/src/lib/components/ui/StatusChip.svelte' \
    'StatusChip accepts the desaturated prop (N8)'
assert_grep_present \
    'desaturated={disconnected}' \
    'web/console/src/routes/(console)/mcp-connections/+page.svelte' \
    'MCP Connections threads disconnected into the status chips (N8)'

# -----------------------------------------------------------------------------
# N9 — Artifacts page subtitle reads "no Runtime attached" when
# disconnected, not "— 0 artifacts".
# -----------------------------------------------------------------------------
assert_grep_present \
    'no Runtime attached' \
    'web/console/src/routes/(console)/artifacts/+page.svelte' \
    'Artifacts subtitle has the disconnected variant (N9)'

# -----------------------------------------------------------------------------
# N10 — PageState centres vertically in the disconnected / empty / error
# branches.
# -----------------------------------------------------------------------------
assert_grep_present \
    '.page-state.disconnected' \
    'web/console/src/lib/components/ui/PageState.svelte' \
    'PageState applies vertical centring to the disconnected branch (N10)'

smoke_summary
