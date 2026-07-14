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
#   - guards the "no production private-network knob" invariant: every
#     `allowPrivate = true` assignment must live inside discovery.go (the
#     test-only WithPrivateNetworkAccessForTest option body), and there must be
#     exactly one. NOTE: a naive `grep -v WithPrivateNetworkAccessForTest`
#     false-fails, because the assignment line (`d.allowPrivate = true`) does
#     NOT contain the option name — scope by file + count instead.
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
#     -run 'TestDiscoverer_SameOriginPrivateResourceHop_CompletesOnProductionPath|TestDiscoverer_SameOriginRebindToDifferentPrivateIP_StillRefused|TestDiscoverer_SameOriginRedirectToDifferentPort_StillRefused|TestDiscoverer_AuthServerHopToPinnedIP_StillRefused|TestDiscoverer_CrossOriginPrivateHop_StillRefused|TestDiscoverer_NonPinnedPlainHTTP_StillNotHTTPS|TestDiscoverer_AuthServerHop_StillNeedsAllowance'
#   go test -race ./test/integration/ -run 'Test.*Phase170'
#   # invariant: allowPrivate=true only ever set inside discovery.go's test-only
#   # option body — scope by FILE + COUNT (a `grep -v <option-name>` false-fails
#   # because the assignment line does not contain the option name).
#   hits=$(grep -RnE 'allowPrivate[[:space:]]*=[[:space:]]*true' internal/ --include='*.go' || true)
#   printf '%s\n' "$hits" | grep -v 'internal/tools/auth/discovery.go:' | grep -q . && exit 1  # any setter outside discovery.go
#   [ "$(printf '%s\n' "$hits" | grep -c 'internal/tools/auth/discovery.go:')" = "1" ]         # exactly one, the option body
# ----------------------------------------------------------------------------

skip "phase 170: same-origin discovery-dial fix — smoke skeleton; real assertions land with the implementation PR"

smoke_summary
