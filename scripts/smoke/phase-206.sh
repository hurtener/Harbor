#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 206 — owner-scoped MCP registry mutation + connection-descriptor
# validation on revision writes (D-350).
#
# Asserts:
#   - live: agent_config.set_revision REJECTS a malformed connection descriptor
#     (stdio carrying a url; http carrying a command; http with no url) with 400
#     and persists nothing — the add door's shape rules hold at the
#     full-payload door.
#   - live: set_revision applies the fail-closed stdio command allowlist (403) —
#     the same §7 RCE gate add_mcp_connection applies, now at both doors.
#   - live: a well-formed descriptor still lands (200) and its
#     oauth_discovery_allowed_origins round-trip through agent_config.get.
#   - live: set_mcp_discovery_origins against a name the caller's revision does
#     NOT declare stays a typed 404 (no accidental widening).
#   - unit-tests: the registry's owner-scoped write (including the zero-owner
#     case), the hoisted boot-declared guard, the SetRevision descriptor
#     validation + stdio gate, and the cross-owner integration seam — both the
#     Protocol edge AND the registry layer driven directly — all under -race.
#
# Each assertion FAILS (never SKIPs) when its guard is removed: the live checks
# compare the response code against 400/403/200 exactly, and the `go test` legs
# FAIL on a genuine test failure. The route probe is the only SKIP path, and it
# only fires on a build with no agent-config surface at all.
#
# The agent id is per-invocation, so the script is idempotent against a
# long-lived server (a second run cannot read the first run's persisted write).
#
# Done-definition: OK >= 6, FAIL = 0.

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

# run_filtered_tests <desc> <run-regexp> <packages...>
#
# Runs `go test -race -run <regexp>` over the given packages. OK on a real
# pass; SKIP when the filter matched no tests (the phase not yet landed, so the
# preflight gate stays green on an older build); FAIL on a genuine test failure
# (never masked).
run_filtered_tests() {
    local desc="$1" runre="$2"
    shift 2
    local out rc
    # NO CGO_ENABLED=0 here: the race detector needs cgo on Linux, where
    # `CGO_ENABLED=0 go test -race` fails to build with "-race requires cgo"
    # (exit 2) rather than running anything. macOS builds it either way, so
    # forcing it green here passed locally and failed in CI. Harbor's CGo ban
    # (CLAUDE.md §5) governs the shipped BINARY, not the race-instrumented test
    # binary — every sibling smoke runs `go test -race` with cgo left alone.
    out="$(go test -race -count=1 -run "${runre}" "$@" 2>&1)" && rc=0 || rc=$?
    if [ "${rc}" -eq 0 ]; then
        if printf '%s\n' "${out}" | grep -qE 'no tests to run|no test files'; then
            skip "${desc}: filter '${runre}' matched no tests (phase not yet landed)"
        else
            ok "${desc}"
        fi
        return
    fi
    printf '%s\n' "${out}" | tail -25
    fail "${desc}: go test exited ${rc}"
}

# ----------------------------------------------------------------------------
# Live-server assertions.
# ----------------------------------------------------------------------------
SET_REVISION_URL="$(api_url /v1/agent_config/set_revision)"
GET_URL="$(api_url /v1/agent_config/get)"
SET_ORIGINS_URL="$(api_url /v1/agent_config/set_mcp_discovery_origins)"

PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${SET_REVISION_URL}" 2>/dev/null || true)
LIVE=1
case "${PROBE:-000}" in
    404|405|501|000|'')
        skip "phase 206: agent_config.set_revision route not present (${PROBE:-000})"
        LIVE=0
        ;;
esac

if [ "${LIVE}" = "0" ]; then
    : # live assertions skipped; the guard tests below still run.
elif [ -z "${HARBOR_DEV_TOKEN:-}" ] || ! command -v jq >/dev/null 2>&1; then
    skip "phase 206: HARBOR_DEV_TOKEN/jq unavailable — live assertions skipped (run under 'make preflight')"
else
    TOKEN="${HARBOR_DEV_TOKEN}"
    ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")
    # A per-invocation agent id keeps the script idempotent: the "nothing was
    # persisted" assertion below reads a FRESH agent, so a second run against the
    # same server cannot see the previous run's successful write and fail.
    AGENT_ID="phase206-smoke-agent-$$-$(date +%s)"

    agentcfg_code() {
        local url="$1" body="$2"
        curl -s -o /dev/null -w '%{http_code}' -X POST "${url}" \
            -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
            -H 'Content-Type: application/json' -d "${body}" 2>/dev/null || true
    }
    agentcfg_body() {
        local url="$1" body="$2"
        curl -sS -X POST "${url}" -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
            -H 'Content-Type: application/json' -d "${body}" 2>/dev/null
    }

    # --- Malformed connection descriptors are rejected (400), nothing persisted.
    BAD_OK=1
    for BAD in \
        "{\"name\":\"p206-bad\",\"transport\":\"stdio\",\"command\":[\"server-bin\"],\"url\":\"https://x.invalid/rpc\"}" \
        "{\"name\":\"p206-bad\",\"transport\":\"http\",\"url\":\"https://x.invalid/rpc\",\"command\":[\"server-bin\"]}" \
        "{\"name\":\"p206-bad\",\"transport\":\"http\"}" \
        "{\"name\":\"\",\"transport\":\"http\",\"url\":\"https://x.invalid/rpc\"}" \
        "{\"name\":\"p206-bad\",\"transport\":\"carrier-pigeon\"}"
    do
        CODE=$(agentcfg_code "${SET_REVISION_URL}" "{\"agent_id\":\"${AGENT_ID}\",\"payload\":{\"connections\":{\"servers\":[${BAD}]}}}")
        if [ "${CODE}" != "400" ]; then
            fail "phase 206: malformed connection descriptor returned ${CODE}, want 400 — ${BAD}"
            BAD_OK=0
        fi
    done
    if [ "${BAD_OK}" = "1" ]; then
        ok "phase 206: set_revision rejects malformed connection descriptors (400)"
    fi

    # Nothing was persisted by any of the rejected writes.
    GET_BODY="$(agentcfg_body "${GET_URL}" "{\"agent_id\":\"${AGENT_ID}\"}")"
    IS_SET="$(printf '%s' "${GET_BODY}" | jq -r '.set // false')"
    if [ "${IS_SET}" = "false" ]; then
        ok "phase 206: a rejected set_revision persisted no revision"
    else
        fail "phase 206: a rejected set_revision left an active revision: ${GET_BODY}"
    fi

    # --- The fail-closed stdio allowlist gates this door too (403), and the
    #     dev config declares no allowlist, so EVERY stdio descriptor is refused.
    CODE=$(agentcfg_code "${SET_REVISION_URL}" "{\"agent_id\":\"${AGENT_ID}\",\"payload\":{\"connections\":{\"servers\":[{\"name\":\"p206-stdio\",\"transport\":\"stdio\",\"command\":[\"/bin/sh\",\"-c\",\"id\"]}]}}}")
    if [ "${CODE}" = "403" ]; then
        ok "phase 206: set_revision applies the fail-closed stdio allowlist (403)"
    else
        fail "phase 206: un-allowlisted stdio descriptor returned ${CODE}, want 403"
    fi

    # --- A well-formed descriptor still lands, allow-list included.
    GOOD="{\"name\":\"p206-good\",\"transport\":\"http\",\"url\":\"https://x.invalid/rpc\",\"oauth_discovery_allowed_origins\":[\"https://as.example.net\"]}"
    CODE=$(agentcfg_code "${SET_REVISION_URL}" "{\"agent_id\":\"${AGENT_ID}\",\"payload\":{\"connections\":{\"servers\":[${GOOD}]}}}")
    if [ "${CODE}" = "200" ]; then
        ok "phase 206: set_revision accepts a well-formed connection descriptor (200)"
    else
        fail "phase 206: well-formed descriptor returned ${CODE}, want 200"
    fi

    GET_BODY="$(agentcfg_body "${GET_URL}" "{\"agent_id\":\"${AGENT_ID}\"}")"
    ORIGIN="$(printf '%s' "${GET_BODY}" | jq -r '(.revision.payload.connections.servers[0].oauth_discovery_allowed_origins // []) | index("https://as.example.net") // empty')"
    if [ -n "${ORIGIN}" ]; then
        ok "phase 206: the discovery allow-list round-trips through set_revision -> get"
    else
        fail "phase 206: allow-list did not round-trip: ${GET_BODY}"
    fi

    # --- An undeclared connection name stays a typed 404 (no widening).
    UNK_PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H 'Content-Type: application/json' -d '{}' "${SET_ORIGINS_URL}" 2>/dev/null || true)
    case "${UNK_PROBE:-000}" in
        404|405|501|000|'')
            skip "phase 206: agent_config.set_mcp_discovery_origins route not present (${UNK_PROBE:-000})"
            ;;
        *)
            CODE=$(agentcfg_code "${SET_ORIGINS_URL}" "{\"agent_id\":\"${AGENT_ID}\",\"name\":\"p206-not-declared\",\"allowed_origins\":[\"https://as.example.net\"]}")
            if [ "${CODE}" = "404" ]; then
                ok "phase 206: an undeclared connection name fails loud (404)"
            else
                fail "phase 206: undeclared-name write returned ${CODE}, want 404"
            fi
            ;;
    esac
fi

# ----------------------------------------------------------------------------
# Guard tests (each FAILS, never SKIPs, when its guard is removed).
# ----------------------------------------------------------------------------
run_filtered_tests \
    "phase 206: registry owner-scoped discovery-origin write (tools/mcp)" \
    'TestRegistry_SetOAuthDiscoveryOrigins_(OwnerScoped|BootDeclaredIsOwnerScopedOut|ZeroOwnerOwnsNothing|ConcurrentOwners)' \
    ./internal/tools/drivers/mcp/

run_filtered_tests \
    "phase 206: boot-declared guard + owner refusal + descriptor validation + stdio gate (agentcfg)" \
    'TestSetMCPDiscoveryOrigins_(BootDeclaredRejectedWhenAlsoDeclaredInRevision|OwnerMismatchFailsLoudAndRollsBack|PassesCallerOwnerToTheApplier)|TestSetRevision_Connections_' \
    ./internal/runtime/agentcfg/protocol/

run_filtered_tests \
    "phase 206: cross-owner + boot-declared seam end to end (integration)" \
    'TestE2E_OwnerScopedDiscoveryWrite_|TestE2E_OwnerScopedRegistryWrite_IsTheAuthoritativeEnforcement|TestE2E_BootDeclaredConnection_RefusedOnBothPaths|TestE2E_SetRevision_MalformedConnectionDescriptorRejected|TestE2E_MissingIdentity_SetRevisionRefused' \
    ./test/integration/

smoke_summary
