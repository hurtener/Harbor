#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 170 — Same-origin MCP OAuth-discovery dial (HA-19).
#
# Class unit-tests: the dial-policy fix is exercised against spec-derived
# RFC 9728/8414 fixtures bound to loopback (reused from Phase 164), which the
# live dev boot does not run. When the phase lands, this:
#   - runs `go test -race` on internal/tools/auth (the discovery walker + the
#     new same-origin dial-pin + DNS-rebinding tests) and the phase-170
#     integration test;
#   - greps that NO production code path sets `allowPrivate` outside the
#     test-only WithPrivateNetworkAccessForTest option (the "no production
#     private-network knob" invariant, mechanically guarded).
# The Console rendering of a discovered requirement reaching needs_allowance
# against a private/self-hosted server is the wave's live-verification step,
# not a smoke.
# Done-definition: OK >= 2, FAIL = 0 once the phase ships.
# Until then it SKIPs. Real assertions land with the implementation PR.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Phase 170 assertions land with the implementation PR. Planned shape:
#
#   go test -race ./internal/tools/auth/... \
#     -run 'TestDiscoverer_SameOriginPrivateResourceHop_CompletesOnProductionPath|TestDiscoverer_SameOriginRebindToDifferentPrivateIP_StillRefused|TestDiscoverer_CrossOriginPrivateHop_StillRefused|TestDiscoverer_AuthServerHop_StillNeedsAllowance'
#   go test -race ./test/integration/ -run 'Test.*Phase170'
#   # invariant grep: allowPrivate is only ever set via the test-only option
#   ! grep -RnE 'allowPrivate\s*=\s*true' internal/ --include='*.go' \
#       | grep -v 'WithPrivateNetworkAccessForTest'
# ----------------------------------------------------------------------------

skip "phase 170: same-origin discovery-dial fix — smoke skeleton; real assertions land with the implementation PR"

smoke_summary
