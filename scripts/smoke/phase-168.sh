#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 168 — Live MCP OAuth discovery-allowance write (D-302).
#
# Skeleton: the surface does not exist yet, so every assertion SKIPs.
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
# Phase 168 assertions (live-server + a unit-tests companion):
#
#   - agent_config.set_mcp_discovery_origins present on the booted method
#     surface (404/405/501 -> SKIP on pre-168 builds).
#   - Grant against an UNKNOWN connection -> typed loud error.
#   - Call WITHOUT admin scope -> CodeScopeMismatch.
#   - Malformed origin (http://, path-bearing, IP literal) -> CodeInvalidRequest.
#   - Static: the method appears in wire-manifest.gen.json + the regenerated
#     docs/site/protocol/methods.md.
#   - go test -race the registry mutator + the discovery dial-guard.
#
# Done-definition: OK >= 3, FAIL = 0.

skip "phase 168: smoke skeleton — replace with real assertions when the phase implements its surface"

smoke_summary
