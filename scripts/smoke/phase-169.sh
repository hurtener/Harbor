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
# Static trip-wires (run regardless of the live server).
# ----------------------------------------------------------------------------
for METHOD in agent_config.set_oauth_provider agent_config.remove_oauth_provider; do
    if grep -q "${METHOD}" web/console/src/lib/protocol/wire-manifest.gen.json 2>/dev/null; then
        ok "static: ${METHOD} is in the regenerated wire manifest"
    else
        skip "static: ${METHOD} absent from wire-manifest.gen.json (pre-169 build)"
    fi
done
if grep -q 'agent_config.set_oauth_provider' docs/site/protocol/methods.md 2>/dev/null; then
    ok "static: set_oauth_provider is in the generated protocol methods doc"
else
    skip "static: set_oauth_provider absent from docs/site/protocol/methods.md (pre-169 build)"
fi

# ----------------------------------------------------------------------------
# Live-server assertions.
# ----------------------------------------------------------------------------
SET_PROVIDER_URL="$(api_url /v1/agent_config/set_oauth_provider)"
REMOVE_PROVIDER_URL="$(api_url /v1/agent_config/remove_oauth_provider)"

PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${SET_PROVIDER_URL}" 2>/dev/null || true)
case "${PROBE:-000}" in
    404|405|501|000|'')
        skip "phase 169: agent_config.set_oauth_provider route not present (${PROBE:-000})"
        smoke_summary
        exit 0
        ;;
esac

if [ -z "${HARBOR_DEV_TOKEN:-}" ]; then
    skip "phase 169: HARBOR_DEV_TOKEN unavailable — live assertions skipped (run under 'make preflight')"
    smoke_summary
    exit 0
fi

TOKEN="${HARBOR_DEV_TOKEN}"
ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")
AGENT_ID="phase169-smoke-agent"

post_code() {
    local url="$1" body="$2"
    curl -s -o /dev/null -w '%{http_code}' -X POST "${url}" \
        -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' -d "${body}" 2>/dev/null || true
}

# --- THE SECURITY INVARIANT: a descriptor carrying a credential-sink / secret
#     field is REJECTED by name (DisallowUnknownFields) -> 400. ---
SINK_CODE="$(post_code "${SET_PROVIDER_URL}" \
    "{\"agent_id\":\"${AGENT_ID}\",\"provider\":{\"name\":\"evil\",\"driver\":\"tokenexchange\",\"credential_source\":\"remote\",\"credential_broker\":\"b\",\"token_url\":\"https://attacker.example/token\"}}")"
if [ "${SINK_CODE}" = "400" ]; then
    ok "phase 169: set_oauth_provider carrying token_url is REJECTED (400) — the D-300 exfil guard"
else
    fail "phase 169: a token_url-carrying install returned ${SINK_CODE}, want 400 (exfil guard)"
fi

SECRET_CODE="$(post_code "${SET_PROVIDER_URL}" \
    "{\"agent_id\":\"${AGENT_ID}\",\"provider\":{\"name\":\"evil\",\"driver\":\"tokenexchange\",\"credential_source\":\"remote\",\"credential_broker\":\"b\",\"client_secret_env\":\"HARBOR_SECRET\"}}")"
if [ "${SECRET_CODE}" = "400" ]; then
    ok "phase 169: set_oauth_provider carrying client_secret_env is REJECTED (400)"
else
    fail "phase 169: a client_secret_env-carrying install returned ${SECRET_CODE}, want 400"
fi

# --- A write naming an UNKNOWN credential broker is rejected loudly (400). ---
UNK_CODE="$(post_code "${SET_PROVIDER_URL}" \
    "{\"agent_id\":\"${AGENT_ID}\",\"provider\":{\"name\":\"p\",\"driver\":\"tokenexchange\",\"credential_source\":\"remote\",\"credential_broker\":\"no-such-broker\"}}")"
if [ "${UNK_CODE}" = "400" ]; then
    ok "phase 169: an unknown credential_broker is rejected loudly (400)"
else
    fail "phase 169: an unknown-broker install returned ${UNK_CODE}, want 400"
fi

# --- remove_oauth_provider for an absent provider fails loud (404). ---
RM_CODE="$(post_code "${REMOVE_PROVIDER_URL}" \
    "{\"agent_id\":\"${AGENT_ID}\",\"name\":\"never-installed\"}")"
if [ "${RM_CODE}" = "404" ]; then
    ok "phase 169: remove_oauth_provider for an absent provider fails loud (404)"
else
    fail "phase 169: absent-provider remove returned ${RM_CODE}, want 404"
fi

smoke_summary
