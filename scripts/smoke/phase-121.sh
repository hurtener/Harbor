#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 121 smoke — surface the Phase 120 runtime gauges in the Console
# Live Runtime health panel. NO new Protocol method: metrics.snapshot +
# the MetricsSnapshot/NamedGauge wire types shipped at Phase 72f; 121
# catches the hand-maintained TS client up to the existing manifest and
# renders the gauges. Static-only: the rendering is exercised by vitest +
# the frontend CI job; these assertions pin the load-bearing source facts.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

CONSOLE="web/console/src/lib"

# Skip cleanly on a checkout without the Console.
if [ ! -d "web/console/src" ]; then
    skip "phase 121: web/console absent"
    smoke_summary
    exit 0
fi

# 1. The typed client gained the metrics.snapshot surface (standard
#    namespace + control route — not a hand-rolled fetch).
assert_grep_present 'class MetricsNamespace' "${CONSOLE}/protocol/client.ts" \
    "phase 121: MetricsNamespace on the typed client"
assert_grep_present "/v1/control/metrics.snapshot" "${CONSOLE}/protocol/client.ts" \
    "phase 121: metrics.snapshot routes through the control transport"

# 2. The wire types are now DECLARED (lockstep), no longer allowlisted.
assert_grep_present 'export interface MetricsSnapshot' "${CONSOLE}/protocol/posture.ts" \
    "phase 121: MetricsSnapshot TS interface declared"
assert_grep_present 'export interface NamedGauge' "${CONSOLE}/protocol/posture.ts" \
    "phase 121: NamedGauge TS interface declared"
assert_grep_absent '"NamedGauge"' "web/console/scripts/protocol-ts-untyped-allow.json" \
    "phase 121: NamedGauge dropped from the untyped-allow list (now typed)"
assert_grep_absent '"MetricsSnapshot"' "web/console/scripts/protocol-ts-untyped-allow.json" \
    "phase 121: MetricsSnapshot dropped from the untyped-allow list (now typed)"

# 3. The projection surfaces ONLY harbor_runtime_* (raw collectors stay
#    off the Protocol), via the unit-tested module — not inline in the .svelte.
assert_grep_present "harbor_runtime_" "${CONSOLE}/live-runtime/health-gauges.ts" \
    "phase 121: gauge projection scopes to the harbor_runtime_ family"
assert_grep_present 'runtimeGaugesFrom' "${CONSOLE}/components/live-runtime/health-panel.svelte" \
    "phase 121: health panel renders gauges via the tested projection"

# 4. Console discipline: the panel reads through the typed client (the page
#    loader), never a hand-rolled fetch in the component.
assert_grep_absent 'fetch(' "${CONSOLE}/components/live-runtime/health-panel.svelte" \
    "phase 121: no hand-rolled fetch in the health panel"

# 5. (live-server) metrics.snapshot is reachable on the dev server once up.
if skip_if_404 "$(api_url /v1/control/metrics.snapshot)" 'phase 121: metrics.snapshot endpoint'; then
    ok "phase 121: metrics.snapshot endpoint reachable on the dev server"
fi

smoke_summary
