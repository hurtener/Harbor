#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 169 — Protocol-installed OAuth provider + binding (D-303).
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
# Phase 169 assertions (live-server + a unit-tests companion):
#
#   - agent_config.set_oauth_provider + agent_config.remove_oauth_provider
#     present on the booted method surface (404/405/501 -> SKIP on pre-169).
#   - A set_oauth_provider carrying token_url (or client_secret_env) is REJECTED
#     with a field-naming decode error — the security invariant, over the wire.
#   - A write naming an UNKNOWN credential broker is rejected loudly.
#   - A call WITHOUT admin scope -> CodeScopeMismatch.
#   - Static: both methods in wire-manifest.gen.json + the regenerated
#     docs/site/protocol/methods.md.
#   - go test -race the internal/tools/auth ProviderSet + the sink/secret-field
#     rejection package (TestSetOAuthProvider_RejectsSinkAndSecretFields).
#
# Done-definition: OK >= 3, FAIL = 0.

skip "phase 169: smoke skeleton — replace with real assertions when the phase implements its surface"

smoke_summary
