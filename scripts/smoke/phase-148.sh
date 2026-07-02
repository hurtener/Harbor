#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 148 smoke — MCP southbound per-identity OAuth bearer + `_meta`
# provenance enrichment (D-278).
#
# Planned legs (flip from skeleton as the surface lands):
#   - go test -race ./internal/tools/drivers/mcp/... (oauth binding + per-call
#     bearer + buildIdentityMeta provenance tests, incl. the D-025 N>=100
#     mixed-triple no-token-bleed test)
#   - go test -race ./internal/tools/ (agent-provenance ctx seam)
#   - go test -race ./internal/config/... (oauth_provider / meta_annotations
#     validation table: unknown provider name lists declared providers;
#     stdio+binding rejected; Authorization-header conflict rejected;
#     reserved/spec-prefixed annotation keys rejected)
#   - go test -race ./test/integration/ -run TestE2E_Phase148 (142 broker
#     fixture + go-sdk streamable-HTTP MCP fixture: rotation, provenance,
#     broker-refusal fail-loud with ZERO unauthenticated calls)
#   - config-validation leg: validator run against an unknown-provider
#     fixture asserts the error names the declared providers
#   - static greps: no concrete tools/auth/drivers/* import inside the MCP
#     driver; wire manifest regenerated (D-223 gate reuse)
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip "phase 148: smoke skeleton — replace with real assertions when the phase implements its surface"

smoke_summary
