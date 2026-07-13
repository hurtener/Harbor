#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 166 — MCP OAuth discovery-allowance write (D-300).
#
# Skeleton: the surface does not exist yet, so every assertion below SKIPs.
# The implementing PR replaces the `skip` with the real assertions.
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
# Phase 166 assertions (live-server + a unit-tests companion leg):
#
#   - `agent_config.set_mcp_discovery_origins` is present on the booted method
#     surface (404/405/501 -> SKIP keeps this green on pre-166 builds).
#   - A grant against an UNKNOWN connection name returns the typed loud error
#     (never a silent 200).
#   - A call WITHOUT the admin scope is rejected with CodeScopeMismatch.
#   - A malformed origin (http://, an origin with a path, an IP literal) is
#     rejected with CodeInvalidRequest.
#   - Static: the new method appears in wire-manifest.gen.json and in the
#     regenerated docs/site/protocol/methods.md (the D-223 trip-wire).
#   - `go test -race` the registry mutator + the discovery dial-guard packages
#     (TestDiscovery_RuntimeGrantedOrigin_StillRefusesPrivateDial).
#
# Done-definition: OK >= 3, FAIL = 0.

skip "phase 166: smoke skeleton — replace with real assertions when the phase implements its surface"

smoke_summary
