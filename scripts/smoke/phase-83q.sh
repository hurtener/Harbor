#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83q — playground sidebar nav. Closes Bug F2 (Playground
# unreachable from the sidebar) + Nit N1 (lowercase breadcrumb) from
# the post-83k visual walkthrough. D-159.
#
# Pure static-surface assertions — the live Playwright test in
# `web/console/tests/harness.spec.ts` covers the runtime behaviour.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# The app shell sidebar references "Playground" with the /playground link.
# Once this lands, the same NAV entry powers both (a) the sidebar render
# and (b) the breadcrumb's Title-Case lookup — fixing F2 and N1 in one shot.
# ----------------------------------------------------------------------------
assert_grep_present "label: 'Playground', href: '/playground'" \
    "web/console/src/routes/(console)/+layout.svelte" \
    "app-shell NAV includes the Playground entry (closes F2)"

# ----------------------------------------------------------------------------
# The Playground deep-link page still renders its page header as
# "Playground" — the header copy was correct pre-83q; we assert it here
# so a future copy edit can't silently regress the Title-Case surface.
# ----------------------------------------------------------------------------
assert_grep_present 'PageHeader title="Playground"' \
    "web/console/src/routes/(console)/playground/[session_id]/+page.svelte" \
    "Playground deep-link page header reads 'Playground' (capital P)"

# ----------------------------------------------------------------------------
# Playwright coverage: the harness baseline asserts both the sidebar
# entry and the breadcrumb capital-P render.
# ----------------------------------------------------------------------------
assert_grep_present "the sidebar lists Playground in the Execution cluster" \
    "web/console/tests/harness.spec.ts" \
    "harness spec asserts the Playground sidebar entry"
assert_grep_present "the Playground route renders a capital-P breadcrumb" \
    "web/console/tests/harness.spec.ts" \
    "harness spec asserts the capital-P breadcrumb (closes N1)"

# ----------------------------------------------------------------------------
# The wave13 aggregator's sidebar-link cardinality assertion now counts
# all 14 V1 IA pages (it was 13 pre-83q because Playground was off-nav).
# ----------------------------------------------------------------------------
assert_grep_present "the sidebar lists the 14 V1 IA pages including Playground" \
    "web/console/tests/wave13.spec.ts" \
    "wave13 aggregator's sidebar cardinality bumped to 14"

# ----------------------------------------------------------------------------
# CONVENTIONS.md §2 reflects the supersession of the original D-121
# stance — Playground is now a sidebar entry.
# ----------------------------------------------------------------------------
assert_grep_present "Playground is reachable directly from the sidebar" \
    "docs/design/console/CONVENTIONS.md" \
    "CONVENTIONS.md §2 records the D-159 supersession"

smoke_summary
