#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 207 smoke — re-land the reverted MCP-Apps host obligations (D-351).
#
# Phase 109k's own guards (the five obligations it claimed and this phase
# re-lands) live in `scripts/smoke/phase-109k.sh`, rewritten in this phase from
# SKIP-tolerant greps to hard `assert_grep_present` assertions. This script does
# NOT duplicate them; it guards what Phase 207 adds ON TOP of the re-land:
#
#   1. The CONFINEMENT property is unconditional — nothing reintroduces an
#      "already qualified?" escape hatch, and the Protocol wire request still
#      carries no app-supplied server scope.
#   2. The typed not-found: an unresolvable app tool-call is a `CodeNotFound`
#      at the Protocol edge and an `MCPAppToolNotFoundError` at the host, so an
#      app can degrade deliberately instead of guessing.
#   3. The self-sizing CLAMP is host-owned: both size tokens exist and the frame
#      is bounded by them (a JS-only clamp would be one edit from unbounded).
#   4. The D-342 handshake-safety property that the teardown re-land could most
#      easily break: the teardown send is gated on the app having initialized.
#   5. The record is corrected honestly (the master plan + D-227 + HA-38/HA-41).
#
# Conventions (AGENTS.md §4.2): helpers from scripts/smoke/common.sh only; every
# assertion here covers a surface that has landed, so absent means FAIL, never
# SKIP (§4.2 item 5 — the bug this whole phase closes).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

BRIDGE='web/console/src/lib/chat/renderers/app-bridge-host.ts'
RENDERER='web/console/src/lib/chat/renderers/mcp-app.svelte'
ADAPTER='web/console/src/lib/mcp-app-host-client.ts'
TOKENS='web/console/src/lib/tokens.css'
WIRE='internal/protocol/types/mcp_apps.go'
EDGE='internal/protocol/mcp.go'

# ----------------------------------------------------------------------------
# 1. Confinement is UNCONDITIONAL and host-derived.
#
#    The qualifier prefixes every name the app can utter. A future "skip the
#    prefix when the name already looks qualified" shortcut would reopen the
#    cross-server hole, so pin the absence of a conditional guard, and pin that
#    the wire request still has no server field an app could aim.
# ----------------------------------------------------------------------------
assert_grep_present 'return .\$\{serverID\}_\$\{name\}' "${BRIDGE}" \
    'phase 207: the server-namespace qualifier is unconditional'
# The qualifier body must be a single unconditional return. An
# "is it already qualified?" shortcut is the one refactor that would reopen the
# cross-server hole while every other assertion here still passed, so pin the
# absence of ANY `startsWith` in this module rather than one spelling of it.
assert_grep_absent 'startsWith' "${BRIDGE}" \
    'phase 207: no already-qualified escape hatch in the qualifier'
# The wire request carries no app-supplied server scope — the namespace is
# host-derived, never something the sandboxed App can aim. The precise guard is
# the reflection test `TestMCPAppCallToolRequest_CarriesNoServerScope`
# (internal/protocol/types); this pins its existence so the guard cannot be
# deleted alongside the field it protects.
assert_grep_present 'func TestMCPAppCallToolRequest_CarriesNoServerScope' \
    internal/protocol/types/mcp_apps_test.go \
    'phase 207: mcp.apps.call_tool takes no app-supplied server scope (guarded)'

# ----------------------------------------------------------------------------
# 2. A name that does not resolve inside the app's own server is a TYPED,
#    distinguishable rejection — not the generic "MCP read failed".
# ----------------------------------------------------------------------------
assert_grep_present 'containsMarker\(msg, "tool not found"\)' "${EDGE}" \
    'phase 207: an unresolvable tool maps to CodeNotFound at the Protocol edge'
assert_grep_present 'class MCPAppToolNotFoundError' "${BRIDGE}" \
    'phase 207: the host declares a typed app-tool not-found'
assert_grep_present 'new MCPAppToolNotFoundError' "${ADAPTER}" \
    'phase 207: the adapter raises the typed not-found on a Runtime not_found'

# ----------------------------------------------------------------------------
# 3. The self-sizing clamp is HOST-owned and token-driven — an app reports a
#    height, the host decides the envelope. Both bounds must exist as tokens
#    AND be applied to the frame.
# ----------------------------------------------------------------------------
assert_grep_present '--size-app-inline-min:' "${TOKENS}" \
    'phase 207: the inline-app minimum height token exists'
assert_grep_present '--size-app-inline-max:' "${TOKENS}" \
    'phase 207: the host-owned inline-app maximum height token exists'
assert_grep_present '^ +max-height: var\(--size-app-inline-max\);' "${RENDERER}" \
    'phase 207: the app frame is clamped to the host-owned maximum'

# ----------------------------------------------------------------------------
# 4. D-342 handshake safety on the new teardown path: the graceful
#    `ui/resource-teardown` is sent ONLY for a bridge the app has finished
#    `ui/initialize` on. Posting a request onto a mid-handshake transport is the
#    exact shape that got the Console MCP-Apps surface reverted.
# ----------------------------------------------------------------------------
assert_grep_present 'if \(wasInitialized\) \{' "${BRIDGE}" \
    'phase 207: teardown is gated on the app having completed ui/initialize'

# ----------------------------------------------------------------------------
# 5. The record is corrected, not quietly patched over.
# ----------------------------------------------------------------------------
assert_grep_present 'D-351' docs/decisions.md \
    'phase 207: the decision is recorded'
assert_grep_present 'D-351' docs/plans/README.md \
    'phase 207: the master plan names the re-land decision'
# Pinned to the HA-38 ROW, not to the phrase — HA-41's row and the prose notes
# also name phase 207, so a bare phrase grep would keep passing after HA-38 was
# reverted to Filed.
assert_grep_present '\| HA-38 \|.*\| Shipped — phase 207' docs/notes/downstream-asks.md \
    'phase 207: HA-38 is flipped to Shipped in the downstream-asks record'

smoke_summary
