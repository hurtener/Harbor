#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 166 — Credential-sink hardening (D-300) — shipped-code security fix.
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
# Phase 166 assertions (unit-tests — this phase adds NO live Protocol surface):
#
#   - go test -race the resolveOAuthBinding downstream-host guard, the
#     tokenexchange audience/scope ceiling + hardened-client tests
#     (RefusesPrivateDial / RefusesRedirect), and the handleSetRawHTMLTrust
#     audit-ordering fix (AuditEmitFailure_LeavesTrustUnchanged).
#   - Static: grep that ToolOAuthProviderConfig carries AllowedDownstreamHosts
#     and that the token-exchange client installs a CheckRedirect.
#
# Done-definition: OK >= 2, FAIL = 0.

skip "phase 166: smoke skeleton — replace with real assertions when the phase implements its surface"

smoke_summary
