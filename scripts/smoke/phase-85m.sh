#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 85m — MCP 2026-07-28 RC adoption (skeleton).
# Stub authored 2026-05-28 alongside D-168. Real assertions land when the
# phase implements; this skeleton keeps preflight green and satisfies the
# drift-audit's "every phase plan has a smoke" check.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Phase 85m assertions go below. Examples (uncomment + tighten when shipping):
#
#   # Session handshake plumbing removed (D-168 / phase plan AC).
#   if grep -rqE "Mcp-Session-Id|\"initialize\"|\"initialized\"" \
#       internal/tools/drivers/mcp; then
#       fail "phase 85m: session handshake plumbing still present"
#   else
#       ok "phase 85m: no session handshake references in MCP driver"
#   fi
#
#   # New Streamable HTTP headers present.
#   if grep -rq "Mcp-Method" internal/tools/drivers/mcp \
#       && grep -rq "Mcp-Name" internal/tools/drivers/mcp; then
#       ok "phase 85m: Mcp-Method / Mcp-Name headers wired"
#   else
#       fail "phase 85m: new transport headers missing"
#   fi
#
#   # W3C trace propagation through MCP _meta.
#   if grep -rq "traceparent" internal/tools/drivers/mcp; then
#       ok "phase 85m: W3C trace propagation referenced"
#   else
#       fail "phase 85m: trace propagation not wired"
#   fi
#
#   # Cache directive plumbing.
#   if [[ -f internal/tools/drivers/mcp/cache.go ]]; then
#       ok "phase 85m: cache directive plumbing exists"
#   else
#       fail "phase 85m: cache.go missing"
#   fi
# ----------------------------------------------------------------------------

skip "phase 85m: smoke skeleton — replace with real assertions when the phase implements its surface (gated on go-sdk RC support, ≈ late Jul–Aug 2026)"

smoke_summary
