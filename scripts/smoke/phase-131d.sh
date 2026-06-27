#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 131d smoke — `harbor token` bring-your-own-issuer subcommand (D-264).
#
# `harbor token` is the no-IdP on-ramp: keygen self-issues a signing
# keypair + public JWK Set, mint self-issues the JWTs `harbor serve`
# verifies. serve's verifier is UNCHANGED (D-220 preserved) — it trusts
# the key only because the operator points identity.jwks_file at it.
#
# This smoke boots NO shared server (it is classified live-server only to
# guarantee bin/harbor is built). It asserts:
#
#   1. keygen writes private.pem (mode 0600) + a valid RFC-7517 jwks.json.
#   2. mint (matching iss/aud) produces a JWT carrying the parser's claim
#      shape (tenant/user/session/iss/aud/exp).
#   3. a DEFAULT mint (no --scopes) carries NO scopes claim (non-admin /
#      least privilege).
#   4. the REAL production verifier ACCEPTS a matching token and REJECTS a
#      mismatched-iss/aud token (the edge's 401) — proven by the §17.8
#      real-parser round-trip Go test, run here.
#   5. `harbor serve` still advertises "mints no token" (D-220 intact) and
#      cmd_serve.go carries no mint call (serve mints nothing).
#
# Degradation (§4.2): a build without the `token` subcommand SKIPs.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

BIN="${ROOT}/bin/harbor"

if [[ ! -x "${BIN}" ]]; then
    skip "phase 131d: bin/harbor not built (preflight build step skipped)"
    smoke_summary
    exit 0
fi

# Degradation: a build without `harbor token` SKIPs the whole surface.
if ! "${BIN}" token --help >/dev/null 2>&1; then
    skip "phase 131d: harbor token subcommand absent in this build"
    smoke_summary
    exit 0
fi

TMPDIR="$(mktemp -d -t harbor-phase131d-XXXXXX)"
trap 'rm -rf "${TMPDIR}"' EXIT

KEYS="${TMPDIR}/keys"

# perm_of <path> — portable octal permission bits (macOS stat -f / GNU stat -c).
perm_of() {
    if stat -f '%Lp' "$1" >/dev/null 2>&1; then
        stat -f '%Lp' "$1"
    else
        stat -c '%a' "$1"
    fi
}

# --- 1. keygen: private.pem 0600 + valid RFC-7517 jwks.json -------------------

if ! "${BIN}" token keygen --out "${KEYS}" >/dev/null 2>"${TMPDIR}/keygen.err"; then
    fail 'phase 131d: harbor token keygen failed'
    sed 's/^/    /' "${TMPDIR}/keygen.err"
    smoke_summary
    exit 1
fi

assert_file "${KEYS}/private.pem" 'phase 131d: keygen wrote private.pem'
assert_file "${KEYS}/jwks.json" 'phase 131d: keygen wrote jwks.json'

if [[ "$(perm_of "${KEYS}/private.pem")" == "600" ]]; then
    ok 'phase 131d: private.pem is mode 0600'
else
    fail "phase 131d: private.pem mode is $(perm_of "${KEYS}/private.pem"), want 600"
fi

# keygen CREATED ${KEYS}, so its parent dir must be owner-only (0700) — the
# private key's directory must not be world-/group-listable. (A pre-existing
# --out dir is deliberately left untouched; this asserts the created case.)
if [[ "$(perm_of "${KEYS}")" == "700" ]]; then
    ok 'phase 131d: keygen-created key dir is mode 0700'
else
    fail "phase 131d: key dir mode is $(perm_of "${KEYS}"), want 700"
fi

# jwks.json is a valid single-key RFC-7517 JWK Set (kty present).
if command -v jq >/dev/null 2>&1; then
    KTY="$(jq -r '.keys[0].kty' "${KEYS}/jwks.json" 2>/dev/null || echo '')"
    if [[ "${KTY}" == "EC" || "${KTY}" == "RSA" ]]; then
        ok "phase 131d: jwks.json is a valid JWK Set (keys[0].kty=${KTY})"
    else
        fail "phase 131d: jwks.json keys[0].kty is '${KTY}', want EC or RSA"
    fi
    KID="$(jq -r '.keys[0].kid' "${KEYS}/jwks.json" 2>/dev/null || echo '')"
    if [[ -n "${KID}" && "${KID}" != "null" ]]; then
        ok 'phase 131d: jwks.json carries a thumbprint kid'
    else
        fail 'phase 131d: jwks.json missing a kid'
    fi
else
    skip 'phase 131d: jq not available — JWK Set shape check skipped'
fi

# --- 2/3. mint (matching iss/aud) + default-mint-has-no-scopes ----------------

ISS='https://issuer.example.com'
AUD='harbor'

TOKEN="$("${BIN}" token mint --key "${KEYS}/private.pem" \
    --tenant acme --user alice --session sess-1 \
    --issuer "${ISS}" --audience "${AUD}" 2>/dev/null)"

if [[ -n "${TOKEN}" && "${TOKEN}" == *.*.* ]]; then
    ok 'phase 131d: mint produced a three-segment JWT'
else
    fail 'phase 131d: mint did not produce a JWT'
fi

# Decode the payload (base64url) and assert the claim shape + no scopes.
if command -v python3 >/dev/null 2>&1; then
    printf '%s' "${TOKEN}" > "${TMPDIR}/token.txt"
    CLAIMS_OK="$(python3 - "${TMPDIR}/token.txt" "${ISS}" "${AUD}" <<'PY'
import sys, base64, json
tok = open(sys.argv[1]).read().strip()
iss, aud = sys.argv[2], sys.argv[3]
try:
    payload = tok.split('.')[1]
    payload += '=' * (-len(payload) % 4)
    c = json.loads(base64.urlsafe_b64decode(payload))
except Exception:  # noqa
    print("DECODE_FAIL")
    sys.exit(0)
ok = (c.get("tenant") == "acme" and c.get("user") == "alice"
      and c.get("session") == "sess-1" and c.get("iss") == iss
      and c.get("aud") == aud and "exp" in c)
print("SHAPE_OK" if ok else "SHAPE_BAD")
print("NOSCOPES" if "scopes" not in c else "HASSCOPES")
PY
)"
    if printf '%s' "${CLAIMS_OK}" | grep -q 'SHAPE_OK'; then
        ok 'phase 131d: minted token carries the parser claim shape (tenant/user/session/iss/aud/exp)'
    else
        fail "phase 131d: minted token claim shape wrong (${CLAIMS_OK})"
    fi
    if printf '%s' "${CLAIMS_OK}" | grep -q 'NOSCOPES'; then
        ok 'phase 131d: default mint (no --scopes) carries NO scopes claim (non-admin / least privilege)'
    else
        fail 'phase 131d: default mint carried a scopes claim (should be least-privilege)'
    fi
else
    skip 'phase 131d: python3 not available — JWT claim decode skipped'
fi

# --- 4. real-parser round-trip: accept matching, REJECT mismatched (401) ------
# The §17.8 proof — keygen JWK Set loaded by the REAL auth.JWKSKeySet +
# auth.NewValidator; a matching token is accepted and a mismatched iss/aud
# token is rejected (the rejection that becomes the edge's 401).
rt_log="${TMPDIR}/roundtrip.log"
if go test "${ROOT}/cmd/harbor/" -run 'TestTokenRoundTrip|TestTokenMint_DefaultIsNonAdmin' -count=1 >"${rt_log}" 2>&1; then
    ok 'phase 131d: real-parser round-trip green — verifier accepts matching iss/aud, rejects mismatched (401)'
else
    fail 'phase 131d: real-parser round-trip RED (accept/reject contract broken; see tail)'
    tail -30 "${rt_log}" | sed 's/^/    /'
fi

# --- 5. serve is unchanged: "mints no token" + no mint in cmd_serve.go --------

help_out="${TMPDIR}/serve-help.txt"
if "${BIN}" serve --help >"${help_out}" 2>&1; then
    assert_grep_present 'mints no token' "${help_out}" \
        'phase 131d: harbor serve still advertises "mints no token" (D-220 intact)'
else
    skip 'phase 131d: harbor serve --help unavailable in this build'
fi

assert_grep_absent 'SignedString' cmd/harbor/cmd_serve.go \
    'phase 131d: cmd_serve.go carries no JWT-signing call (serve mints nothing)'

# --- Optional: full live serve + runtime.info round-trip (env-gated) ----------
# serve demands a real LLM provider at boot, so the HTTP 401 leg is gated
# behind a HARBOR_LIVE_* env carrying a usable provider key. CI skips it;
# the real-parser test above is the always-on proof of the same contract.
if [[ -z "${HARBOR_LIVE_OPENROUTER_API_KEY:-}" ]]; then
    skip 'phase 131d: live `harbor serve` + runtime.info round-trip (set HARBOR_LIVE_OPENROUTER_API_KEY to run)'
fi

smoke_summary
