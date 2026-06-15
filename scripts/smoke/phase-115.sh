#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 115 smoke — `harbor serve` (production JWKS verifier).
#
# `harbor serve` is the production sibling of `harbor dev`: it boots the
# headless Runtime + Protocol surface behind a JWKS-backed JWT verifier
# (the operator's IdP signing keys), mints no dev token, and embeds no
# Console. This smoke is a BINARY probe — it runs the built `bin/harbor`
# but never against a network endpoint (the fail-loud config check exits
# before any listener binds), so it cannot collide with the live
# preflight server. It is classified `live-server` only to guarantee the
# binary is already built when it runs. It asserts:
#
#   1. `harbor serve --help` exists and advertises the production posture
#      (a build without the subcommand SKIPs — the 404/405/501 → SKIP
#      convention applied to a CLI surface).
#   2. A boot whose config carries no JWKS verifier source fails NON-ZERO
#      with an error naming the missing `jwks_url` / `jwks_file` field
#      (the §13 fail-loud-at-boot contract).
#   3. `harbor serve --help` does NOT advertise any Console-serving flag
#      (D-091 — only `harbor console` serves the Console).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

BIN="${ROOT}/bin/harbor"

if [ ! -x "${BIN}" ]; then
    skip "harbor serve: bin/harbor not built"
    smoke_summary
    exit 0
fi

help_out="$(mktemp -t harbor-serve-help-XXXXXX)"

# ----------------------------------------------------------------------
# Assertion 1 — `harbor serve --help` exists (production subcommand).
# ----------------------------------------------------------------------
if "${BIN}" serve --help >"${help_out}" 2>&1; then
    if grep -q -i "jwks\|production jwt verifier" "${help_out}"; then
        ok "harbor serve: --help present and advertises the production JWKS verifier"
    else
        fail "harbor serve: --help present but does not mention the JWKS/production verifier posture"
    fi
else
    rm -f "${help_out}"
    skip "harbor serve: subcommand absent (Phase 115 not yet shipped)"
    smoke_summary
    exit 0
fi

# ----------------------------------------------------------------------
# Assertion 2 — fail-loud at boot when no JWKS verifier is configured.
# Build a config from the example with the jwks_* lines stripped; serve
# must exit non-zero and name the missing field.
# ----------------------------------------------------------------------
if [ -f "${ROOT}/examples/serve.yaml" ]; then
    nojwks="$(mktemp -t harbor-serve-nojwks-XXXXXX)"
    grep -v -i 'jwks' "${ROOT}/examples/serve.yaml" > "${nojwks}"
    serve_out="$(mktemp -t harbor-serve-out-XXXXXX)"
    set +e
    "${BIN}" serve --config "${nojwks}" >"${serve_out}" 2>&1
    rc=$?
    set -e
    if [ "${rc}" -ne 0 ] && grep -q -i 'jwks' "${serve_out}"; then
        ok "harbor serve: boot without a JWKS verifier fails non-zero with a jwks-naming error (rc=${rc})"
    else
        fail "harbor serve: missing-verifier boot did not fail loud (rc=${rc}); output: $(tr '\n' ' ' < "${serve_out}")"
    fi
    rm -f "${nojwks}" "${serve_out}"
else
    skip "harbor serve: examples/serve.yaml absent (cannot run the fail-loud probe)"
fi

# ----------------------------------------------------------------------
# Assertion 3 — `harbor serve` does NOT serve the Console (D-091). The
# help text must carry no console-serving flag.
# ----------------------------------------------------------------------
if grep -q -i -- "--serve-console" "${help_out}"; then
    fail "harbor serve: advertises a Console-serving flag (D-091 — only \`harbor console\` serves the Console)"
else
    ok "harbor serve: no Console-serving flag (D-091 deployment posture)"
fi
rm -f "${help_out}"

smoke_summary
