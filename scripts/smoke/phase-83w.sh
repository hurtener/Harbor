#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 83w — wire-surface gaps from the round-2 walkthrough.
# D-164. Two distinct fixes integrated by coordinator from two agents:
#   - F5 (Console-side, Agent B): friendly unknown_method on
#     topology.snapshot. The pre-83w-F5 Live Runtime + Playground
#     pages routed the unknown_method error through PageState's red
#     ERROR branch with a Retry button that would always fail. The
#     fix special-cases unknown_method to render an info banner
#     instead.
#   - F6 (Go-side, Agent A): adds `mcp.servers.list` to the Runtime's
#     wire surface. The handler reads the *mcp.Registry already
#     constructed at bootDevStack.
#
# Conventions (CLAUDE.md §4.2):
#   - 404/405/501 → SKIP for any live-server checks (not used here).
#   - At least one OK once the phase has shipped.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# F5 — friendly unknown_method on topology.snapshot.
# ----------------------------------------------------------------------------

# The shared PageState gains the `info` branch (option (a) of the plan —
# additive to disconnected/loading/error/empty/ready). Reusable across
# the two affected pages (live-runtime + playground/[session_id]).
assert_grep_present "'info'" \
    "web/console/src/lib/components/ui/PageState.svelte" \
    "PageState.PageStatus union includes the 'info' branch"
assert_grep_present "status === 'info'" \
    "web/console/src/lib/components/ui/PageState.svelte" \
    "PageState renders the info branch when status is 'info'"
assert_grep_present 'data-testid="page-state-info"' \
    "web/console/src/lib/components/ui/PageState.svelte" \
    "info branch carries the page-state-info testid"

# The shared error-mapping helper that detects unknown_method on the
# typed client — the single sanctioned read of "this Runtime answered
# with the not-applicable shape" code.
assert_grep_present 'export function isUnknownMethod' \
    "web/console/src/lib/protocol/errors.ts" \
    'isUnknownMethod helper exported from $lib/protocol/errors.js'
assert_grep_present "code === 'unknown_method'" \
    "web/console/src/lib/protocol/errors.ts" \
    "isUnknownMethod matches the 'unknown_method' canonical code"

# The Live Runtime page routes topology.snapshot's unknown_method to the
# friendly state — NOT the red ERROR state. Phase 108e reframed the page into
# the capability cockpit: topology is a CAPABILITY-GATED panel, so the page
# only fetches the snapshot when `topology_snapshot` is advertised (the D-164
# short-circuit) and the friendly "Topology view not available" copy moved into
# the gated topology panel. The page still imports + uses isUnknownMethod as
# the defence-in-depth wire catch in loadTopology().
assert_grep_present 'isUnknownMethod' \
    "web/console/src/routes/(console)/live-runtime/+page.svelte" \
    "Live Runtime page imports + uses isUnknownMethod"
assert_grep_present 'Topology view not available' \
    "web/console/src/lib/components/live-runtime/topology-panel.svelte" \
    "Live Runtime topology panel renders the friendly headline on unknown_method (108e)"
assert_grep_present 'planner/RunLoop' \
    "web/console/src/lib/components/live-runtime/topology-panel.svelte" \
    "Live Runtime topology panel's friendly detail names the runtime shape (108e)"

# The Playground session_id page routes topology.snapshot's
# unknown_method to the friendly info banner above the chat surface,
# AND degrades to empty/ready (the chat is still functional on a
# planner/RunLoop runtime).
assert_grep_present 'isUnknownMethod' \
    "web/console/src/routes/(console)/playground/[session_id]/+page.svelte" \
    "Playground session_id page imports + uses isUnknownMethod"
assert_grep_present 'Topology view not available on this Runtime' \
    "web/console/src/routes/(console)/playground/[session_id]/+page.svelte" \
    "Playground page renders the friendly headline on unknown_method"
assert_grep_present 'data-testid="playground-topology-info"' \
    "web/console/src/routes/(console)/playground/[session_id]/+page.svelte" \
    "Playground info-banner carries a stable testid"

# Negative-shape assertion: the Playground main index page does NOT
# call topology.snapshot (it redirects to the session_id deep-link).
# Only the [session_id] page calls topology.snapshot; the main index
# is not in scope for F5.
if grep -q 'topology\.snapshot' "web/console/src/routes/(console)/playground/+page.svelte" 2>/dev/null; then
    fail "Playground main index page should NOT call topology.snapshot directly"
else
    ok "Playground main index page does not call topology.snapshot (only [session_id] does)"
fi

# ----------------------------------------------------------------------------
# F6 — Runtime exposes mcp.servers.list.
# ----------------------------------------------------------------------------

# F6.1 — cmd_dev.go constructs the MCPSurface from the boot-time
# mcpRegistry.
assert_grep_present 'protocol\.NewMCPSurface\(protocol\.MCPDeps\{' \
    "cmd/harbor/cmd_dev.go" \
    "phase 83w F6: bootDevStack constructs MCPSurface"

# F6.2 — bootDevStack threads the MCPSurface into transports.NewMux.
assert_grep_present 'transports\.WithMCPSurface\(mcpSurface\)' \
    "cmd/harbor/cmd_dev.go" \
    "phase 83w F6: bootDevStack wires MCPSurface into transports.NewMux"

# F6.3 — devstack.Assemble mirrors per D-094.
assert_grep_present 'protocol\.NewMCPSurface\(protocol\.MCPDeps\{' \
    "harbortest/devstack/devstack.go" \
    "phase 83w F6: devstack.Assemble constructs MCPSurface (D-094 mirror)"
assert_grep_present 'transports\.WithMCPSurface\(mcpSurface\)' \
    "harbortest/devstack/devstack.go" \
    "phase 83w F6: devstack.Assemble wires MCPSurface (D-094 mirror)"

# F6.4 — mcpconsole gained a NoOAuthAccessor for the V1 dev posture
# (no OAuth providers configured).
assert_grep_present 'type NoOAuthAccessor struct' \
    "internal/mcpconsole/mcpconsole.go" \
    "phase 83w F6: mcpconsole.NoOAuthAccessor declared"
assert_grep_present 'ErrNoOAuthConfigured' \
    "internal/mcpconsole/mcpconsole.go" \
    "phase 83w F6: mcpconsole.ErrNoOAuthConfigured sentinel declared"

# F6.5 — the integration test exists and pins the wire surface.
assert_file "test/integration/phase83w_mcp_servers_list_test.go" \
    "phase 83w F6: integration test pins mcp.servers.list wire surface"

smoke_summary
