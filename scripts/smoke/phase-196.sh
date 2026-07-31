#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 196 — inference-plane broker-pull + `agent_config.set_llm_provider` (D-333/D-334).
#
# The inference-plane analogue of the tool-plane token-exchange PULL: a
# zero-URL/zero-secret provider install write (admin-only) binding a runtime
# to a NAMED, boot-declared inference broker. Skeleton until the surface
# lands: every assertion SKIPs on a pre-196 build.
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

# The dev bearer is resolved through common.sh's `dev_bearer`, never by a raw
# ${HARBOR_DEV_TOKEN} read: the raw read is EMPTY outside preflight, so every
# live leg below degrades to a SKIP while the script still exits 0 — "a SKIP
# that should be an OK is a bug" (AGENTS.md §4.2 item 5, issue #624).
# dev_bearer prefers the exported value and falls back to the dev server log.
HARBOR_DEV_TOKEN="$(dev_bearer)"

# ----------------------------------------------------------------------------
# Static trip-wires (run regardless of the live server).
# ----------------------------------------------------------------------------
if grep -q 'agent_config.set_llm_provider' web/console/src/lib/protocol/wire-manifest.gen.json 2>/dev/null; then
    ok "static: agent_config.set_llm_provider is in the regenerated wire manifest"
else
    skip "static: agent_config.set_llm_provider absent from wire-manifest.gen.json (pre-196 build)"
fi
if grep -q 'agent_config.set_llm_provider' docs/site/protocol/methods.md 2>/dev/null; then
    ok "static: agent_config.set_llm_provider is in the generated protocol methods doc"
else
    skip "static: agent_config.set_llm_provider absent from docs/site/protocol/methods.md (pre-196 build)"
fi

# ----------------------------------------------------------------------------
# Live-server assertions.
# ----------------------------------------------------------------------------
SET_URL="$(api_url /v1/agent_config/set_llm_provider)"

PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${SET_URL}" 2>/dev/null || true)
case "${PROBE:-000}" in
    404|405|501|000|'')
        skip "phase 196: agent_config.set_llm_provider route not present (${PROBE:-000})"
        smoke_summary
        exit 0
        ;;
esac

if [ -z "${HARBOR_DEV_TOKEN:-}" ]; then
    skip "phase 196: HARBOR_DEV_TOKEN unavailable — live assertions skipped (run under 'make preflight')"
    smoke_summary
    exit 0
fi

TOKEN="${HARBOR_DEV_TOKEN}"
ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")
AGENT_ID="phase196-smoke-agent"

post_code() {
    local url="$1" body="$2" tok="${3:-${TOKEN}}"
    curl -s -o /dev/null -w '%{http_code}' -X POST "${url}" \
        -H "Authorization: Bearer ${tok}" "${ID_HEADERS[@]}" \
        -H 'Content-Type: application/json' -d "${body}" 2>/dev/null || true
}

# --- SECURITY INVARIANT (D-300): a descriptor carrying a credential-sink /
#     secret field is REJECTED by name (DisallowUnknownFields) -> 400. ---
URL_CODE="$(post_code "${SET_URL}" \
    "{\"agent_id\":\"${AGENT_ID}\",\"provider\":{\"name\":\"evil\",\"provider\":\"openai\",\"credential_source\":\"remote\",\"inference_broker\":\"b\",\"credential_url\":\"https://attacker.example/key\"}}")"
if [ "${URL_CODE}" = "400" ]; then
    ok "phase 196: set_llm_provider carrying credential_url is REJECTED (400) — the D-300 exfil guard"
else
    fail "phase 196: a credential_url-carrying install returned ${URL_CODE}, want 400 (exfil guard)"
fi

SECRET_CODE="$(post_code "${SET_URL}" \
    "{\"agent_id\":\"${AGENT_ID}\",\"provider\":{\"name\":\"evil\",\"provider\":\"openai\",\"credential_source\":\"remote\",\"inference_broker\":\"b\",\"auth_token_env\":\"HARBOR_SECRET\"}}")"
if [ "${SECRET_CODE}" = "400" ]; then
    ok "phase 196: set_llm_provider carrying auth_token_env is REJECTED (400)"
else
    fail "phase 196: an auth_token_env-carrying install returned ${SECRET_CODE}, want 400"
fi

# --- An empty credential_source (== the forbidden `env` source) is rejected. ---
ENV_CODE="$(post_code "${SET_URL}" \
    "{\"agent_id\":\"${AGENT_ID}\",\"provider\":{\"name\":\"p\",\"provider\":\"openai\",\"credential_source\":\"\",\"inference_broker\":\"b\"}}")"
if [ "${ENV_CODE}" = "400" ]; then
    ok "phase 196: an empty credential_source is rejected loud (400) — remote-only"
else
    fail "phase 196: an empty-credential_source install returned ${ENV_CODE}, want 400"
fi

# --- An UNKNOWN inference_broker is rejected loudly (400). ---
UNK_CODE="$(post_code "${SET_URL}" \
    "{\"agent_id\":\"${AGENT_ID}\",\"provider\":{\"name\":\"p\",\"provider\":\"openai\",\"credential_source\":\"remote\",\"inference_broker\":\"no-such-broker\"}}")"
if [ "${UNK_CODE}" = "400" ]; then
    ok "phase 196: an unknown inference_broker is rejected loudly (400)"
else
    fail "phase 196: an unknown-broker install returned ${UNK_CODE}, want 400"
fi

# --- SCOPE GATE: a console:fleet-only token (no admin) cannot rebind a provider. ---
if [ -n "${HARBOR_DEV_FLEET_TOKEN:-}" ]; then
    FLEET_CODE="$(post_code "${SET_URL}" \
        "{\"agent_id\":\"${AGENT_ID}\",\"provider\":{\"name\":\"p\",\"provider\":\"openai\",\"credential_source\":\"remote\",\"inference_broker\":\"b\"}}" \
        "${HARBOR_DEV_FLEET_TOKEN}")"
    if [ "${FLEET_CODE}" = "403" ]; then
        ok "phase 196: a console:fleet-only token is rejected (403) — read scope cannot rebind a provider"
    else
        fail "phase 196: a console:fleet-only set_llm_provider returned ${FLEET_CODE}, want 403 (D-066/D-079)"
    fi
else
    skip "phase 196: HARBOR_DEV_FLEET_TOKEN unavailable — scope-gate assertion skipped"
fi

# --- REAL INSTALL: a valid zero-URL descriptor naming the boot-declared dev
#     inference broker installs the binding live and returns 200 (an OK, not a
#     501 SKIP — the installer IS wired in dev). examples/dev.yaml declares
#     `dev-inference-broker`; the primary provider is `openrouter`. The install
#     is async (the background refresh loop performs the pull), so the 200 does
#     not require a reachable coordinator. ---
INSTALL_CODE="$(post_code "${SET_URL}" \
    "{\"agent_id\":\"${AGENT_ID}\",\"provider\":{\"name\":\"rotate-openrouter\",\"provider\":\"openrouter\",\"credential_source\":\"remote\",\"inference_broker\":\"dev-inference-broker\"}}")"
if [ "${INSTALL_CODE}" = "200" ]; then
    ok "phase 196: a valid zero-URL set_llm_provider installs the binding (200) — the installer is wired in dev"
elif [ "${INSTALL_CODE}" = "501" ]; then
    fail "phase 196: set_llm_provider returned 501 — the LLMProviderInstaller is NOT wired (the feature is inert)"
else
    fail "phase 196: a valid set_llm_provider returned ${INSTALL_CODE}, want 200"
fi

smoke_summary
