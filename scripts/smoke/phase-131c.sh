#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 131c smoke — worked OIDC client + serve round-trip (reaffirms D-263).
#
# The binding PRODUCTION LEG of the v1.8.0 Adopter-Path Protocol on-ramp:
# an OAuth2 client-credentials flow obtains a JWT from a hermetic
# ES256/JWKS mock-OIDC issuer, `harbor serve` boots pointed at that
# issuer's published JWKS, and the worked example
# (examples/protocol-clients/oidc-client-example) attaches and calls
# runtime.info. Asserts:
#
#  1. Static: the worked example exists and is SDK-FREE — pure stdlib
#     against the Protocol wire, no `sdk/` and no `internal/` Harbor
#     import (the SDK-free guarantee the production-identity guide makes).
#  2. The example compiles.
#  3. The binding round-trip Go test passes: the mock-OIDC issuer mints a
#     token the REAL parser accepts (CLAUDE.md §17.8 — the fixture signs
#     with the same golang-jwt library and publishes the same JWK shape
#     internal/protocol/auth/jwks.go parses, so a wrong field fails the
#     verifier), serve verifies it, runtime.info returns OK with the
#     granted scope, AND an unpublished-key token is rejected (failure
#     mode). On a build that predates `harbor serve` the Go test self-
#     SKIPs (the 404/405/501 → SKIP convention applied to a CLI surface)
#     and `go test` still exits zero.
#
# Conventions (AGENTS.md §4.2): 404/405/501 → SKIP; ≥1 OK once shipped;
# use scripts/smoke/common.sh helpers.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

EXAMPLE_DIR="examples/protocol-clients/oidc-client-example"
EXAMPLE_PKG="./${EXAMPLE_DIR}"

# ── 1. Static: the worked example exists ────────────────────────────────
assert_file "${EXAMPLE_DIR}/main.go" \
    "worked OIDC client example present"

# ── 1b. SDK-free: no Harbor sdk/ or internal/ import in the dep graph ───
if deps_out="$(go list -deps "${EXAMPLE_PKG}" 2>/dev/null)"; then
    deps_tmp="$(mktemp -t harbor-131c-deps-XXXXXX)"
    printf '%s\n' "${deps_out}" > "${deps_tmp}"
    assert_grep_absent 'github.com/hurtener/Harbor/sdk' "${deps_tmp}" \
        "oidc-client-example is SDK-free (no github.com/hurtener/Harbor/sdk import)"
    assert_grep_absent 'github.com/hurtener/Harbor/internal' "${deps_tmp}" \
        "oidc-client-example pulls no Harbor internal package (stdlib-only against the wire)"
    rm -f "${deps_tmp}"
else
    fail "go list -deps ${EXAMPLE_PKG} failed — example does not compile"
fi

# ── 2. The example compiles (discard the artifact — never pollute the
#       working tree with a stray binary). ─────────────────────────────
if go build -o /dev/null "${EXAMPLE_PKG}" >/dev/null 2>&1; then
    ok "oidc-client-example compiles"
else
    fail "oidc-client-example failed to compile — run: go build ${EXAMPLE_PKG}"
fi

# ── 3. The binding round-trip (mock-OIDC → serve → runtime.info) ────────
# The Go test self-SKIPs on a pre-`harbor serve` build; `go test` exits
# zero either way, so a clean run is the gate.
if go test -race -count=1 -run 'TestE2E_OIDCClient' ./test/integration/ >/dev/null 2>&1; then
    ok "OIDC client-credentials → serve JWKS-verify → runtime.info round-trip passes (+ unpublished-key rejection)"
else
    fail "OIDC serve round-trip failed — run: go test -race -run 'TestE2E_OIDCClient' ./test/integration/"
fi

smoke_summary
