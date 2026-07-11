#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 164 — MCP OAuth requirement discovery, surfaced as data.
#
# Class unit-tests: the discovery edge needs the OAuth-challenging fixture
# server (spec-derived RFC 9728/8414 documents), which the live dev boot
# does not run. When the phase lands, this:
#   - runs `go test -race` on the discovery chain walker + SSRF guardrail
#     packages (internal/tools/auth discovery tests + the mcp driver's
#     challenge-capture tests);
#   - greps that the additive oauth_requirement view types are present in
#     the regenerated wire manifest (wire-manifest.gen.json).
# The Console rendering of a discovered requirement is the wave's
# live-verification step, not a smoke.
# Done-definition: OK >= 2, FAIL = 0 once the phase ships.
# Until then it SKIPs. Real assertions land with the implementation PR.
#
#   cp scripts/smoke/_template.sh scripts/smoke/phase-NN.sh
#   chmod +x scripts/smoke/phase-NN.sh
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# Classification (D-104 — the `# PREFLIGHT_REQUIRES:` header above):
#   - static-only — pure file/text greps, golden compares, file-existence
#     assertions. Runs in the parallel batch BEFORE the dev server boots.
#   - live-server — hits the booted dev server over HTTP (`api_url`,
#     `assert_status`, `skip_if_404`, `assert_json_path`) or reads the
#     preflight server log. Runs serially against the booted instance.
#   - unit-tests — runs `go test` for one or more packages. Parallelisable;
#     `go test` schedules its own internal parallelism.
#
# Pick `live-server` whenever the smoke depends on `HARBOR_BIND` /
# `HARBOR_BASE_URL` / `HARBOR_DEV_TOKEN` / `${HARBOR_DATA_DIR}/server.log`
# or invokes the built `bin/harbor` against a network endpoint. When in
# doubt, `live-server` is the safe default — misclassifying a
# server-touching smoke as `static-only` produces nondeterministic flakes.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Phase NN assertions go below. Examples:
#
#   assert_status 200 "$(api_url /healthz)" "healthz returns 200"
#   assert_json_path '.status' 'ok' "$(api_url /readyz)" "readyz reports status=ok"
#   protocol_call 'sessions/create' '{"tenant":"t1","user":"u1"}' "create session"
#
# Until the phase ships, the script can be empty assertions or a single
# `skip "phase NN: not yet implemented"` to keep preflight green.
# ----------------------------------------------------------------------------

# 1: the discovery chain walker + SSRF guardrail tests under -race
# (internal/tools/auth), driven by the committed spec-derived fixtures.
if go test -race -count=1 -timeout 240s \
    -run 'TestDiscoverer|TestAuthServerMetadataURL' \
    ./internal/tools/auth/... >/dev/null 2>&1; then
    ok 'phase 164: OAuth discovery walker + SSRF guardrail tests pass under -race'
else
    fail 'phase 164: discovery walker tests failed (run `go test -race -run TestDiscoverer ./internal/tools/auth/...`)'
fi

# 2: the challenge-capture edge tests under -race (internal/tools/drivers/mcp).
if go test -race -count=1 -timeout 240s \
    -run 'WWWAuth|Challenge|BuildHTTPClient|OAuthDiscovery' \
    ./internal/tools/drivers/mcp/... >/dev/null 2>&1; then
    ok 'phase 164: WWW-Authenticate capture + registry-record tests pass under -race'
else
    fail 'phase 164: challenge-capture tests failed (run `go test -race -run Challenge ./internal/tools/drivers/mcp/...`)'
fi

# 3: the cross-subsystem discovery integration test under -race.
if go test -race -count=1 -timeout 240s \
    -run 'TestE2E_MCPOAuthDiscovery' ./test/integration/... >/dev/null 2>&1; then
    ok 'phase 164: MCP OAuth discovery integration test passes under -race'
else
    fail 'phase 164: discovery integration test failed (run `go test -race -run TestE2E_MCPOAuthDiscovery ./test/integration/...`)'
fi

# 4: the additive oauth_requirement view type is registered in the wire
# manifest (D-223 lockstep — the Console client mirrors it).
assert_grep_present \
    'MCPOAuthRequirementView' \
    web/console/src/lib/protocol/wire-manifest.gen.json \
    'phase 164: oauth_requirement view type present in the wire manifest (D-223)'

# 5: custody boundary — the discovery fetch code attaches NO credentials
# (no Authorization header is ever set on a discovery request).
if grep -q 'Set("Authorization"' internal/tools/auth/discovery.go; then
    fail 'phase 164: discovery.go sets an Authorization header — discovery fetches must carry NO credentials (D-297 / §7)'
else
    ok 'phase 164: discovery fetches attach no credentials (no Authorization header set)'
fi

smoke_summary
