#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 167 — Owner-scoped reconcile for runtime-added connections + providers (D-301).
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
# Phase 167 assertions (unit-tests — owner-scoped reconcile, no new Protocol surface):
#
#   - go test -race the owner-tag + owner-scoped-reconcile packages
#     (TestReconcile_OwnerScoped_NeverDetachesBootOrOtherOwner,
#      TestRegistry_BootServerVisibleToEverySession).
#   - Static: grep that the two reconcile NOTEs (projection.go, mcp_detacher.go)
#     now describe the OWNER-SCOPED view (the corrected trip-wire — the design
#     KEEPS a rewritten note about deliberate process-global boot behaviour, so
#     the earlier "the NOTEs are GONE" assertion would INVERT; assert the note
#     MENTIONS the owner-scoped reconcile instead).
#
# Done-definition: OK >= 2, FAIL = 0.

skip "phase 167: smoke skeleton — replace with real assertions when the phase implements its surface"

smoke_summary
