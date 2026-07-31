#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 217 — `meta_annotations` honour `_meta` path nesting (D-362).
#
# Asserts:
#   - live: a DOTTED annotation key is still accepted at the wire door — the
#     shape is legal today and stays legal; only its merge meaning changes.
#   - live: a FLAT annotation key is still accepted (no regression).
#   - live: an annotation whose FIRST SEGMENT is reserved (`tenant.foo`) is
#     REFUSED — the newly-tightened per-segment arm.
#   - live: a WHOLE-KEY spec-reserved annotation (`io.modelcontextprotocol/ui`)
#     is STILL refused. THIS IS THE MUTATION GATE: splitting the key on `.`
#     yields ["io", "modelcontextprotocol/ui"], and NEITHER segment carries the
#     `io.modelcontextprotocol/` prefix — so a per-segment-ONLY guard would
#     ADMIT a spec-reserved annotation that is refused today. Dropping the
#     whole-key arm turns this OK into a FAIL.
#   - live: a COLLIDING declaration (`vendor` + `vendor.id`) is refused, and the
#     error names both offending keys.
#   - live: an OVER-DEEP annotation path is refused.
#   - static: the guard is whole-key AND per-segment in the one shared
#     authority, the depth constant is HOISTED (defined once, in
#     `internal/config`), and the annotation merge goes through the SAME
#     `injectMeta` helper the credential-injection path uses — never a fork.
#   - unit-tests: the path predicates, all four doors, the map-type identity
#     guard, determinism, the concurrent-reuse run, the audit over-redaction
#     characterisation, and the cross-subsystem seam — all under -race.
#
# Every assertion FAILS (never SKIPs) when its guard is removed: the static
# guards fail on a missing pattern, the live checks compare an exact code, and
# the `go test` legs fail on a genuine test failure. The only SKIP paths are the
# route probe (a build with no agent-config surface) and a missing curl/jq.
#
# The agent id is per-invocation, so the script is idempotent against a
# long-lived server.
#
# Done-definition: OK >= 12, FAIL = 0.

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

CONFIG_VALIDATE_GO='internal/config/validate.go'
MCP_GO='internal/tools/drivers/mcp/mcp.go'
ATTACH_GO='internal/tools/drivers/mcp/attach.go'
ADDCONN_GO='internal/runtime/agentcfg/protocol/addconnection.go'
WIREINJ_GO='internal/runtime/agentcfg/protocol/wireinjectiondescriptor.go'

# assert_grep <file> <extended-regexp> <desc>
#
# OK when the pattern matches, FAIL when it does not. Deliberately NOT a skip:
# these guard a shipped surface, and the 404/405/501 -> SKIP convention is for
# forward-phase scripts running against older builds. Patterns use POSIX
# classes ([[:space:]]) — never \t / \d, which BSD grep matches and GNU grep
# does not.
assert_grep() {
    local file="$1" pattern="$2" desc="$3"
    if [ ! -f "${file}" ]; then
        fail "${desc}: ${file} does not exist"
        return
    fi
    if grep -qE "${pattern}" "${file}"; then
        ok "${desc}"
    else
        fail "${desc}: no match for /${pattern}/ in ${file}"
    fi
}

# assert_not_grep <file> <extended-regexp> <desc>
assert_not_grep() {
    local file="$1" pattern="$2" desc="$3"
    if [ ! -f "${file}" ]; then
        fail "${desc}: ${file} does not exist"
        return
    fi
    if grep -qE "${pattern}" "${file}"; then
        fail "${desc}: unexpected match for /${pattern}/ in ${file}"
    else
        ok "${desc}"
    fi
}

# run_filtered_tests <desc> <run-regexp> <packages...>
#
# Runs `go test -race -run <regexp>`. OK on a real pass; SKIP only when the
# filter matched no tests at all (an older build); FAIL on a genuine failure.
run_filtered_tests() {
    local desc="$1" runre="$2"
    shift 2
    local out rc
    # NO CGO_ENABLED=0 here: the race detector needs cgo on Linux, where
    # `CGO_ENABLED=0 go test -race` fails to build. Harbor's CGo ban governs the
    # shipped BINARY, not the race-instrumented test binary.
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
# Static guards.
# ----------------------------------------------------------------------------

# The guard rule is WHOLE-KEY *and* PER-SEGMENT in the one shared authority.
# Both arms must be present in ReservedMCPMetaPathToken's body.
if [ -f "${CONFIG_VALIDATE_GO}" ] && \
   sed -n '/^func ReservedMCPMetaPathToken(/,/^}/p' "${CONFIG_VALIDATE_GO}" \
     | grep -qE 'if IsReservedMCPMetaKey\(k\)'; then
    ok 'phase 217: the path guard keeps the WHOLE-KEY arm (the spec-namespace arm a per-segment-only rule would lose)'
else
    fail 'phase 217: ReservedMCPMetaPathToken lost its whole-key arm — a per-segment-ONLY guard ADMITS io.modelcontextprotocol/* keys'
fi

if [ -f "${CONFIG_VALIDATE_GO}" ] && \
   sed -n '/^func ReservedMCPMetaPathToken(/,/^}/p' "${CONFIG_VALIDATE_GO}" \
     | grep -qE 'for _, seg := range SplitMCPMetaPath\(k\)'; then
    ok 'phase 217: the path guard adds the PER-SEGMENT arm'
else
    fail 'phase 217: ReservedMCPMetaPathToken has no per-segment arm'
fi

# The depth constant is HOISTED, not duplicated: defined once in
# internal/config, consumed at the wire door.
assert_grep "${CONFIG_VALIDATE_GO}" \
    'const MaxMCPMetaKeyDepth = 16' \
    'phase 217: the depth cap is defined once, in internal/config'

assert_not_grep "${WIREINJ_GO}" \
    'const maxInjectionMetaKeyDepth' \
    'phase 217: the wire door no longer defines its own depth constant (hoisted, not duplicated)'

assert_grep "${WIREINJ_GO}" \
    'config\.MaxMCPMetaKeyDepth' \
    'phase 217: the wire door consumes the hoisted depth cap'

# The annotation merge nests through the SAME helper the injection path uses —
# never a second implementation.
if [ -f "${MCP_GO}" ] && \
   sed -n '/^func buildIdentityMeta(/,/^}/p' "${MCP_GO}" | grep -qE 'injectMeta\(meta,'; then
    ok 'phase 217: the annotation merge nests through the shared injectMeta helper'
else
    fail 'phase 217: buildIdentityMeta does not call injectMeta — the nesting was forked instead of shared'
fi

# All three loud doors consult the shared per-key validator.
for f in "${CONFIG_VALIDATE_GO}" "${ADDCONN_GO}" "${ATTACH_GO}"; do
    assert_grep "${f}" \
        'ValidateMCPMetaAnnotationKey' \
        "phase 217: ${f##*/} applies the shared annotation-path rule"
done

assert_grep "${MCP_GO}" \
    'ErrMetaPathCollision' \
    'phase 217: the merge-time re-check fails loud on a legacy path collision'

# ----------------------------------------------------------------------------
# Live-server assertions — the wire door (set_revision serves the same
# validator add_mcp_connection does).
# ----------------------------------------------------------------------------
SET_REVISION_URL="$(api_url /v1/agent_config/set_revision)"

PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' -d '{}' "${SET_REVISION_URL}" 2>/dev/null || true)
LIVE=1
case "${PROBE:-000}" in
    404|405|501|000|'')
        skip "phase 217: agent_config.set_revision route not present (${PROBE:-000})"
        LIVE=0
        ;;
esac

if [ "${LIVE}" = "0" ]; then
    : # live assertions skipped; the guard tests below still run.
elif [ -z "${HARBOR_DEV_TOKEN:-}" ] || ! command -v jq >/dev/null 2>&1; then
    skip "phase 217: HARBOR_DEV_TOKEN/jq unavailable — live assertions skipped (run under 'make preflight')"
else
    TOKEN="${HARBOR_DEV_TOKEN}"
    ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")
    AGENT_ID="phase217-smoke-agent-$$-$(date +%s)"

    # A 17-segment annotation key — one past config.MaxMCPMetaKeyDepth (16).
    DEEP="a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.a.leaf"

    conn_payload() {
        printf '{"agent_id":"%s","payload":{"connections":{"servers":[{"name":"p217","transport":"http","url":"https://x.invalid/rpc","meta_annotations":%s}]}}}' \
            "${AGENT_ID}" "$1"
    }
    conn_code() {
        curl -s -o /dev/null -w '%{http_code}' -X POST "${SET_REVISION_URL}" \
            -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
            -H 'Content-Type: application/json' -d "$(conn_payload "$1")" 2>/dev/null || true
    }
    conn_body() {
        curl -sS -X POST "${SET_REVISION_URL}" \
            -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
            -H 'Content-Type: application/json' -d "$(conn_payload "$1")" 2>/dev/null
    }

    # --- A flat annotation is unchanged, and a DOTTED annotation stays LEGAL.
    #     (The dotted key is a supported shape on the shipped surface; this
    #     phase changes its MERGE meaning, not its legality.)
    CODE=$(conn_code '{"deployment":"prod"}')
    if [ "${CODE}" = "200" ]; then
        ok 'phase 217: a flat annotation key is still accepted (200)'
    else
        fail "phase 217: flat annotation returned ${CODE}, want 200"
    fi

    CODE=$(conn_code '{"vendor.tag":"blue","vendor.account_id":"acct-42"}')
    if [ "${CODE}" = "200" ]; then
        ok 'phase 217: a dotted annotation path is accepted (200)'
    else
        fail "phase 217: dotted annotation returned ${CODE}, want 200"
    fi

    # --- The newly-tightened PER-SEGMENT arm: a reserved FIRST segment.
    CODE=$(conn_code '{"tenant.foo":"x"}')
    if [ "${CODE}" = "400" ]; then
        ok 'phase 217: a reserved FIRST SEGMENT (tenant.foo) is refused (400)'
    else
        fail "phase 217: tenant.foo returned ${CODE}, want 400 — the per-segment arm is not wired"
    fi

    # --- THE MUTATION GATE: the WHOLE-KEY arm must survive the tightening.
    #     Reverting it (leaving per-segment only) turns this OK into a FAIL,
    #     because neither segment of io.modelcontextprotocol/ui carries the
    #     spec prefix.
    CODE=$(conn_code '{"io.modelcontextprotocol/ui":"x"}')
    if [ "${CODE}" = "400" ]; then
        ok 'phase 217: a WHOLE-KEY spec-reserved annotation is STILL refused (400) — the whole-key arm survived'
    else
        fail "phase 217: io.modelcontextprotocol/ui returned ${CODE}, want 400 — a per-segment-ONLY guard LOOSENED a security control"
    fi

    # --- A colliding declaration is refused and NAMES both offending keys.
    CODE=$(conn_code '{"vendor":"x","vendor.id":"y"}')
    if [ "${CODE}" = "400" ]; then
        ok 'phase 217: a colliding annotation declaration is refused (400)'
    else
        fail "phase 217: colliding declaration returned ${CODE}, want 400"
    fi
    BODY="$(conn_body '{"vendor":"x","vendor.id":"y"}')"
    if printf '%s' "${BODY}" | grep -q 'vendor.id' && printf '%s' "${BODY}" | grep -q 'collide'; then
        ok 'phase 217: the collision error names the offending keys and the rule'
    else
        fail "phase 217: the collision error does not name both keys: ${BODY}"
    fi

    # --- An over-deep annotation path is refused.
    CODE=$(conn_code "{\"${DEEP}\":\"x\"}")
    if [ "${CODE}" = "400" ]; then
        ok 'phase 217: an over-deep annotation path is refused (400)'
    else
        fail "phase 217: over-deep annotation returned ${CODE}, want 400"
    fi
fi

# ----------------------------------------------------------------------------
# Guard tests (each FAILS, never SKIPs, when its guard is removed).
# ----------------------------------------------------------------------------
run_filtered_tests \
    "phase 217: path predicates + the boot door + the injection depth-cap parity (config)" \
    'TestReservedMCPMetaPathToken_WholeKeyAndPerSegment|TestValidateMCPMetaAnnotationKey_Table|TestValidateMCPMetaPathCollisions_|TestValidate_MCPMetaAnnotationPaths_|TestValidate_InjectionMetaKeyDepthCap_BootDoor|TestIsReservedMCPMetaKey_GoldenSet|TestValidate_MCPSouthboundOAuth_' \
    ./internal/config/

run_filtered_tests \
    "phase 217: nesting + map-type identity + determinism + concurrent reuse + the attach door (tools/mcp)" \
    'TestBuildIdentityMeta_|TestInjectMeta_|TestAttachDoor_MetaAnnotationPaths|TestConfigValidate_InjectionMetaKeyDepthCap|TestResolveOAuthBinding' \
    ./internal/tools/drivers/mcp/

run_filtered_tests \
    "phase 217: the wire door — add_mcp_connection + set_revision (agentcfg/protocol)" \
    'TestSetRevision_Connections_MalformedRejectedNothingPersisted|TestSetRevision_Connections_ValidPersistsAndRoundTrips|TestWireInjection_AddMCPConnection_' \
    ./internal/runtime/agentcfg/protocol/

run_filtered_tests \
    "phase 217: the over-redaction characterisation (audit, test-only — no rule change)" \
    'TestInjectionCredentialRule_' \
    ./internal/audit/

run_filtered_tests \
    "phase 217: the cross-subsystem seam against a spec-derived MCP fixture (integration)" \
    'TestE2E_MCPMetaNesting_' \
    ./test/integration/

smoke_summary
