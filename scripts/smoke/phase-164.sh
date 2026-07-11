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

skip "phase 164: smoke skeleton — MCP OAuth discovery not yet implemented; replace with the discovery/SSRF go-test leg + wire-manifest grep when the phase lands"

smoke_summary
