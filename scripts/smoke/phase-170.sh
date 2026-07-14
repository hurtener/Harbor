#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 170 — Same-origin MCP OAuth-discovery dial (HA-19).
#
# Class unit-tests: the dial-policy fix is exercised against spec-derived
# RFC 9728/8414 fixtures bound to loopback (reused from Phase 164), which the
# live dev boot does not run. This:
#   - runs `go test -race` on internal/tools/auth (the discovery walker + the
#     new same-origin dial-pin + DNS-rebinding tests);
#   - runs `go test -race` on the phase-170 production-path integration test;
#   - guards the "no production private-network knob" invariant: every
#     `allowPrivate = true` assignment must live inside discovery.go (the
#     test-only WithPrivateNetworkAccessForTest option body), and there must be
#     exactly one. A naive `grep -v WithPrivateNetworkAccessForTest`
#     false-fails, because the assignment line (`d.allowPrivate = true`) does
#     NOT contain the option name — scope by file + count instead.
# The Console rendering of a discovered requirement reaching needs_allowance
# against a private/self-hosted server is the wave's live-verification step,
# not a smoke.
# Done-definition: OK >= 2, FAIL = 0.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1: the same-origin dial-pin + DNS-rebinding + guardrail tests under -race.
if go test -race -count=1 -timeout 240s \
    -run 'TestDiscoverer|TestEvalDialGate|TestAuthServerMetadataURL|TestIsPrivateIP' \
    ./internal/tools/auth/ >/dev/null 2>&1; then
    ok 'phase 170: discovery dial-pin + rebinding + guardrail tests pass under -race'
else
    fail 'phase 170: dial-pin tests failed (run `go test -race -run TestDiscoverer ./internal/tools/auth/`)'
fi

# 2: the production-path self-hosted-posture integration test under -race.
if go test -race -count=1 -timeout 240s \
    -run 'TestE2E_Phase170' ./test/integration/ >/dev/null 2>&1; then
    ok 'phase 170: production-path self-hosted-posture integration test passes under -race'
else
    fail 'phase 170: integration test failed (run `go test -race -run TestE2E_Phase170 ./test/integration/`)'
fi

# 3: "no production private-network knob" invariant — every `allowPrivate = true`
# assignment lives inside internal/tools/auth/discovery.go (the test-only option
# body), and there is EXACTLY one. Scope by file + count (a `grep -v` on the
# option name false-fails: the assignment line does not contain it).
setters="$(grep -RnE 'allowPrivate[[:space:]]*=[[:space:]]*true' internal/ --include='*.go' || true)"
outside="$(printf '%s\n' "${setters}" | grep -v '^[[:space:]]*$' | grep -v 'internal/tools/auth/discovery.go:' || true)"
inside_count="$(printf '%s\n' "${setters}" | grep -c 'internal/tools/auth/discovery.go:' || true)"
if [ -n "${outside}" ]; then
    fail "phase 170: an allowPrivate=true setter exists OUTSIDE discovery.go (no production private-network knob allowed): ${outside}"
elif [ "${inside_count}" != "1" ]; then
    fail "phase 170: expected EXACTLY one allowPrivate=true setter in discovery.go, found ${inside_count}"
else
    ok 'phase 170: no production private-network knob (exactly one test-only allowPrivate setter, in discovery.go)'
fi

# 4: the dial-time backstop uses ControlContext (post-resolution, ctx-aware) and
# disables keep-alives (so every dial — incl. redirects — re-enters the gate).
if grep -q 'ControlContext' internal/tools/auth/discovery.go && \
   grep -q 'DisableKeepAlives: true' internal/tools/auth/discovery.go; then
    ok 'phase 170: dialer uses ControlContext + DisableKeepAlives (per-dial step-gate)'
else
    fail 'phase 170: discovery dialer must use ControlContext AND DisableKeepAlives'
fi

# 5: custody boundary preserved — discovery fetches attach NO credentials.
if grep -q 'Set("Authorization"' internal/tools/auth/discovery.go; then
    fail 'phase 170: discovery.go sets an Authorization header — discovery fetches must carry NO credentials (§7)'
else
    ok 'phase 170: discovery fetches attach no credentials (no Authorization header set)'
fi

smoke_summary
