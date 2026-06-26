#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 128 smoke — the agent-config Protocol capability (agent_config).
#
# Asserts the additive capability surface (RFC §5.2, §5.3, §6.16):
#   - internal/protocol/types/version.go declares CapAgentConfig and the
#     "agent_config" wire string, and registers it in canonicalCapabilities.
#   - internal/protocol/posture.go declares PostureDeps.AgentConfigAvailable
#     and wiredCapabilitiesFor appends CapAgentConfig.
#   - the conformance handshake set is 6 and names CapAgentConfig.
#   - the capability string is defined ONLY in types/version.go (single source).
#   - the manifest lockstep gate AND the generated-docs gate pass.
#   - the protocol + conformance tests + the capability E2E pass under -race.
#   - live: when the dev server exposes runtime.info AND mounts the
#     agent-config surface, runtime.info advertises agent_config in
#     capabilities (skips per 404/405/501 / no dev token).
#
# Conventions (AGENTS.md §4.2): scripts/smoke/common.sh helpers; FAIL = 0.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# -----------------------------------------------------------------------------
# Static: the capability constant + registration (single source).
# -----------------------------------------------------------------------------
assert_grep_present 'CapAgentConfig Capability = "agent_config"' \
    "internal/protocol/types/version.go" \
    "phase 128: CapAgentConfig constant declared"
assert_grep_present 'CapAgentConfig:' \
    "internal/protocol/types/version.go" \
    "phase 128: CapAgentConfig registered in canonicalCapabilities"

# Static: the conditional-advertisement wiring (the consumer).
assert_grep_present 'AgentConfigAvailable' "internal/protocol/posture.go" \
    "phase 128: PostureDeps.AgentConfigAvailable present"
assert_grep_present 'CapAgentConfig' "internal/protocol/posture.go" \
    "phase 128: wiredCapabilitiesFor appends CapAgentConfig"

# Static: the conformance trip-wire stayed updated (6 + named).
assert_grep_present 'CapAgentConfig' \
    "internal/protocol/conformance/conformance.go" \
    "phase 128: conformance handshake names CapAgentConfig"
assert_grep_present '!= 6' \
    "internal/protocol/conformance/conformance.go" \
    "phase 128: conformance canonical-set count is 6"

# Single-source defence: the capability string is defined only in version.go.
assert_grep_absent 'Capability = "agent_config"' \
    "internal/protocol/posture.go" \
    "phase 128: agent_config capability not redefined outside types (single-source)"

# -----------------------------------------------------------------------------
# Build/test gates: manifest lockstep (digest regen) + generated-docs gate.
# -----------------------------------------------------------------------------
if make protocol-ts-gen-check >/dev/null 2>&1; then
    ok "phase 128: make protocol-ts-gen-check passes (manifest digest regenerated + in lockstep)"
else
    fail "phase 128: make protocol-ts-gen-check failed (run make protocol-ts-gen and commit the manifest)"
fi
if make protocol-docs-gen-check >/dev/null 2>&1; then
    ok "phase 128: make protocol-docs-gen-check passes (no generated-docs drift)"
else
    fail "phase 128: make protocol-docs-gen-check failed (run make protocol-docs-gen and commit any diff)"
fi
if go test -race ./internal/protocol/... ./internal/protocol/conformance/... >/dev/null 2>&1; then
    ok "phase 128: protocol + conformance tests pass under -race"
else
    fail "phase 128: protocol/conformance tests failed (go test -race ./internal/protocol/...)"
fi
if go test -race -run TestE2E_Phase128 ./test/integration/... >/dev/null 2>&1; then
    ok "phase 128: agent-config capability E2E passes under -race (advertised iff mounted; missing-identity 401)"
else
    fail "phase 128: capability E2E failed (go test -race -run TestE2E_Phase128 ./test/integration/...)"
fi

# -----------------------------------------------------------------------------
# Live (skips per 404/405/501): runtime.info advertises agent_config when the
# agent-config surface is mounted. The dev server runs WITH the auth validator,
# so an unauthenticated call is rejected 401 — discover the dev token from the
# preflight server log (same posture as phase-72f.sh / phase-127.sh), else SKIP.
# -----------------------------------------------------------------------------
if [[ -z "${HARBOR_DEV_TOKEN:-}" ]] && [[ -n "${HARBOR_DATA_DIR:-}" ]] && [[ -f "${HARBOR_DATA_DIR}/server.log" ]]; then
    HARBOR_DEV_TOKEN="$(grep -m1 '^HARBOR_DEV_TOKEN=' "${HARBOR_DATA_DIR}/server.log" 2>/dev/null | sed 's/^HARBOR_DEV_TOKEN=//' || true)"
fi

live_capability_probe() {
    local desc="phase 128: live runtime.info advertises agent_config"
    local url
    url="$(api_url '/v1/control/runtime.info')"
    if ! command -v curl >/dev/null 2>&1; then
        skip "${desc}: curl not available"
        return
    fi
    # An empty body — the posture handler backfills identity from the JWT.
    local resp code payload
    resp=$(curl -s --max-time 5 \
        -X POST -H 'Content-Type: application/json' \
        ${HARBOR_DEV_TOKEN:+-H "Authorization: Bearer ${HARBOR_DEV_TOKEN}"} \
        --data '{}' -w $'\n%{http_code}' "${url}" 2>/dev/null) || resp=$'\n000'
    code="${resp##*$'\n'}"
    payload="${resp%$'\n'*}"
    case "${code}" in
        404|405|501|000|"")
            skip "${desc}: ${code:-000} (route not wired into this build)"
            return
            ;;
        401)
            skip "${desc}: 401 — HARBOR_DEV_TOKEN not discoverable; authenticated path covered by the E2E test"
            return
            ;;
    esac
    if [[ "${code}" != "200" ]]; then
        fail "${desc}: HTTP ${code} (${url})"
        return
    fi
    if ! command -v jq >/dev/null 2>&1; then
        skip "${desc}: jq not available — cannot inspect capabilities"
        return
    fi
    # runtime.info.capabilities is a JSON array of capability strings
    # (`["agent_config", ...]`).
    if printf '%s' "${payload}" | jq -r '.capabilities[]' 2>/dev/null | grep -qx 'agent_config'; then
        ok "${desc}"
    else
        skip "${desc}: agent_config absent (this build did not mount the agent-config surface)"
    fi
}

live_capability_probe

smoke_summary
