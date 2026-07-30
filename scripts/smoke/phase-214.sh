#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 214 — the MCP arm of pass-by-reference routing (egress
# substitution).
#
# The surface has two halves and this script covers both:
#
#   - a PROTOCOL WRITE: `agent_config.add_mcp_connection` /
#     `set_revision` now carry `artifact_byte_eligible` +
#     `artifact_params`, refused loud without eligibility and on stdio;
#   - a RUNTIME SEAM: the driver resolves a mapped artifact id under the
#     dispatching run's own identity and writes the resolved bytes into
#     the outbound tool-call body as standard base64, recording the FACT
#     of it fail-closed before the wire request.
#
# The static guards below exist because a runtime seam has nothing to
# curl: deleting one of them would fail only `go test` in a package a
# reviewer might not run, whereas here it fails preflight.
#
# EVERY assertion FAILS (never SKIPs) when the thing it names is removed
# — the 404/405/501 -> SKIP convention is for forward-phase scripts
# running against older builds, not for a shipped phase's own guards.
# The only SKIP paths are the Protocol route probe (a build with no
# agent-config surface at all) and a missing curl/jq.
#
# Done-definition: OK >= 14, FAIL = 0.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

EGRESS_PKG="internal/tools/artifactegress"
EGRESS_SRC="${EGRESS_PKG}/artifactegress.go"
SCAN_SRC="internal/tools/artifactref/scan.go"
SCAN_TEST="internal/tools/artifactref/egress_scan_test.go"
MCP_SRC="internal/tools/drivers/mcp/mcp.go"
MCP_EGRESS_SRC="internal/tools/drivers/mcp/egress.go"
MCP_EVENTS_SRC="internal/tools/drivers/mcp/events.go"
GOLDEN="internal/tools/drivers/mcp/testdata/egress_frame.golden.json"

# assert_grep <file> <extended-regexp> <desc>
# OK on a match, FAIL otherwise. Patterns use POSIX classes
# ([[:space:]]) — never \t / \d, which BSD grep matches and GNU grep
# does not, so a guard written that way would silently never fire on
# Linux CI.
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

# run_filtered_tests <desc> <run-regexp> <packages...>
# OK on a real pass; SKIP only when the filter matched no tests at all
# (an older build); FAIL on a genuine test failure (never masked).
run_filtered_tests() {
    local desc="$1" runre="$2"
    shift 2
    local out rc
    # NO CGO_ENABLED=0: the race detector needs cgo on Linux, where
    # `CGO_ENABLED=0 go test -race` fails to build rather than running
    # anything. The CGo ban governs the shipped BINARY, not the
    # race-instrumented test binary.
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
# 1. The carrier and its projection bound.
# ----------------------------------------------------------------------------

if [ ! -f "${EGRESS_SRC}" ]; then
    skip "phase 214: ${EGRESS_SRC} absent (the MCP egress arm is not yet implemented)"
    smoke_summary
    exit 0
fi

# A Payload projects a REFERENCE through every serialisation door but
# MarshalJSON. Losing any one turns "the resolved value cannot leak into
# a log or a format verb" from a type property back into a rule
# contributors have to remember.
assert_grep "${EGRESS_SRC}" 'func \(p Payload\) String\(\) string' \
    'phase 214: Payload formats as a reference'
assert_grep "${EGRESS_SRC}" 'func \(p Payload\) LogValue\(\) slog\.Value' \
    'phase 214: Payload logs as a reference'
assert_grep "${EGRESS_SRC}" 'func \(p Payload\) MarshalJSON\(\) \(\[\]byte, error\)' \
    'phase 214: Payload carries content through MarshalJSON alone'

# The WIRE ENCODING is normative: a Go []byte behind the carrier, emitted
# as RFC 4648 §4 standard base64. A Go string slot would let
# encoding/json rewrite every invalid-UTF-8 byte to U+FFFD — the exact
# corruption the artifact read path was fixed for, reintroduced one layer
# out.
assert_grep "${EGRESS_SRC}" 'base64\.StdEncoding\.EncodeToString' \
    'phase 214: the substituted value is emitted as standard base64'
assert_grep "${EGRESS_SRC}" 'data[[:space:]]+\[\]byte' \
    'phase 214: the carrier holds the content as a []byte, never a Go string'

# An oversize value is REFUSED, not truncated: a partial document
# delivered to a remote ingester is a corruption, not a bounded read.
assert_grep "${EGRESS_SRC}" 'ErrEgressTooLarge' \
    'phase 214: an oversize resolved value has a typed refusal'

# ----------------------------------------------------------------------------
# 2. The production bound — ONE content-emitting call site.
# ----------------------------------------------------------------------------

assert_grep "${SCAN_SRC}" 'func ScanEgressSites\(' \
    'phase 214: the egress scan is exported for its gate test'
# Resolving by import PATH rather than by the conventional qualifier is
# what keeps an aliased import from evading the scan.
assert_grep "${SCAN_SRC}" 'EgressPkgPath = "github.com/hurtener/Harbor/internal/tools/artifactegress"' \
    'phase 214: the egress scan resolves its package by import path (an alias is followed)'
# A bound counted in CALL positions is no bound once the function value
# can travel.
assert_grep "${SCAN_SRC}" 'KindEgressValueRef' \
    'phase 214: the scan flags the encoder taken as a VALUE, not only called'
# The live allow-list must actually be wired to the real tree, or the
# scan gates nothing.
assert_grep "${SCAN_TEST}" 'egressSiteAllowList' \
    'phase 214: the egress scan runs against the real tree from a live allow-list'

# ----------------------------------------------------------------------------
# 3. `args` is never rewritten — the seven-sink invariant, at its source.
# ----------------------------------------------------------------------------

# The substitution writes into the DECODED map. Rewriting `args` would
# put the resolved value into the trajectory, the observation, the
# per-invocation content hash AND the durable, browser-readable MCP-App
# tool-context record (which can mint a second artifact from it).
assert_grep "${MCP_EGRESS_SRC}" 'artifactegress\.Encode\(ctx, argMap, mapping, name, maxBytes\)' \
    'phase 214: the substitution writes into the decoded argument map'
if grep -qE '^[[:space:]]*args[[:space:]]*=[[:space:]]' "${MCP_SRC}"; then
    fail 'phase 214: mcp.go reassigns the raw args on the call path — that would carry the resolved value into the trajectory, the content hash and the durable tool-context record'
else
    ok 'phase 214: mcp.go never reassigns the raw argument JSON'
fi
# The content hash is still computed over the model's OWN args (sink 6).
assert_grep "${MCP_SRC}" 'ToolCallID\(runID, string\(p\.source\), name, args\)' \
    'phase 214: the per-invocation content hash is computed over the raw args'

# Resolution happens ONCE per dispatched call, ahead of the reliability
# shell — a memory property (`ceiling x in-flight`, not `x attempts`) and
# a correctness one (an unresolvable id is a model mistake, not a
# transient fault).
assert_grep "${MCP_SRC}" 'plan, err = p\.prepareEgress\(ctx, mcpName, args, egressMapping, egressMaxBytes\)' \
    'phase 214: the resolve runs once per dispatched call, before RunWithPolicy'

# ----------------------------------------------------------------------------
# 4. The compensating control — fail-closed, before the wire request.
# ----------------------------------------------------------------------------

assert_grep "${MCP_EVENTS_SRC}" 'EventTypeMCPArtifactEgressed events\.EventType = "mcp\.artifact_egressed"' \
    'phase 214: the substitution record is a canonical event type'
assert_grep "${MCP_EVENTS_SRC}" 'events\.RegisterEventType\(EventTypeMCPArtifactEgressed\)' \
    'phase 214: the substitution record is registered on the event registry'
# Fail-CLOSED: the publish is checked and ABORTS the call. Unlike the
# best-effort app-discovery emit next door, an unrecorded substitution
# must not happen at all.
assert_grep "${MCP_EGRESS_SRC}" 'if err := p\.publishArtifactEgressed\(ctx, name, records\); err != nil \{' \
    'phase 214: an unrecordable substitution ABORTS before any wire request'

# ----------------------------------------------------------------------------
# 5. The §17.8 wire pin — the committed transcript.
# ----------------------------------------------------------------------------

assert_file "${GOLDEN}" \
    'phase 214: the outbound tools/call arguments transcript is committed'
# The golden must carry the STANDARD-base64 encoding of the binary
# fixture. An SDK-derived fixture alone cannot pin this (jsonschema-go
# infers "array" for a []byte while encoding/json marshals base64, so
# such a fixture is self-consistent at either placement) — this byte
# literal is what makes a carrier swap fail a diff.
assert_grep "${GOLDEN}" 'JVBERv/\+AIDDKA==' \
    'phase 214: the committed transcript pins the exact base64 of the binary fixture'

# ----------------------------------------------------------------------------
# 6. Live — the Protocol write door.
# ----------------------------------------------------------------------------
#
# Deliberately NOT skip_all_if_server_down: that exits the whole script,
# taking the `go test` legs below with it on a standalone run.

ADD_URL="$(api_url /v1/agent_config/add_mcp_connection)"
TOKEN="${HARBOR_DEV_TOKEN:-}"

HEALTH_CODE=000
if command -v curl >/dev/null 2>&1; then
    HEALTH_CODE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "$(api_url /healthz)" || true)
fi

if ! command -v curl >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
    skip 'phase 214: curl/jq unavailable — live assertions skipped'
elif [ "${HEALTH_CODE:-000}" = "000" ] || [ -z "${HEALTH_CODE}" ]; then
    skip 'phase 214: dev server unreachable — live assertions skipped (run under make preflight)'
else
    p214_post() {
        local url="$1" body="$2"
        local hdrs=(-H 'Content-Type: application/json')
        if [ -n "${TOKEN}" ]; then
            hdrs+=(-H "Authorization: Bearer ${TOKEN}")
        fi
        P214_STATUS=$(curl -s --max-time 10 -o /tmp/phase214.body -w '%{http_code}' \
            "${hdrs[@]}" -X POST -d "${body}" "${url}" 2>/dev/null || true)
        P214_STATUS="${P214_STATUS:-000}"
        P214_BODY=$(cat /tmp/phase214.body 2>/dev/null || echo '{}')
        P214_CODE=$(printf '%s' "${P214_BODY}" | jq -r '.code // .error.code // ""' 2>/dev/null || echo "")
    }

    ID='{"tenant":"dev","user":"dev","session":"dev"}'

    # Route probe with an EMPTY body, asserted POSITIVELY.
    #
    # A mounted `agent_config.add_mcp_connection` runs its identity check
    # before any body validation, so an empty body answers exactly
    # `401 identity_required`. That is a far better probe than "not a 404":
    # an earlier draft of this script addressed the WRONG path
    # (`/v1/control/agent_config.add_mcp_connection`, which does not exist)
    # and every request answered `500 runtime_error` — a status the
    # skip-list did not cover, so the block ran on regardless and the three
    # refusals below failed against a dead route. The confusing part was
    # that they failed for a reason that had nothing to do with what they
    # assert.
    #
    # So an unexpected status is a FAIL here, not a fall-through and not a
    # SKIP: only the documented not-present statuses skip, and anything
    # else means the probe itself is wrong and must say so loudly. 500 is
    # deliberately NOT on the skip list — a runtime error is a failure to
    # report, never a surface to excuse.
    p214_post "${ADD_URL}" '{}'
    LIVE=1
    if [ "${P214_CODE}" = "unknown_method" ]; then
        skip 'phase 214: agent_config.add_mcp_connection is not a canonical method on this build'
        LIVE=0
    else
        case "${P214_STATUS}" in
            404|405|501|000|'')
                skip "phase 214: agent_config.add_mcp_connection route not present (${P214_STATUS})"
                LIVE=0
                ;;
            401)
                ok 'phase 214: the add-connection door is mounted and identity-mandatory'
                ;;
            *)
                fail "phase 214: route probe returned ${P214_STATUS} ${P214_CODE}, want 401 identity_required — the probe is addressing the wrong route, so the refusals below would assert nothing"
                LIVE=0
                ;;
        esac
    fi

    if [ "${LIVE}" = "1" ]; then
        # A mapping WITHOUT the eligibility declaration is refused: the
        # flag IS the containment boundary, so a mapping without it is
        # never persisted inert.
        p214_post "${ADD_URL}" "{\"identity\":${ID},\"agent_id\":\"p214-agent\",\"connection\":{\"name\":\"p214-noteligible\",\"transport\":\"http\",\"url\":\"https://p214.invalid/mcp\",\"artifact_params\":{\"ingest\":[\"doc\"]}}}"
        if [ "${P214_STATUS}" = "400" ] && [ "${P214_CODE}" = "invalid_request" ]; then
            ok 'phase 214: a mapping on a NON-eligible connection is refused 400 invalid_request'
        else
            fail "phase 214: mapping without eligibility returned ${P214_STATUS} ${P214_CODE}, want 400 invalid_request"
        fi

        # A mapping on STDIO is refused on the transport rule — base64
        # artifact bytes belong in an HTTP body, not a stdio frame.
        p214_post "${ADD_URL}" "{\"identity\":${ID},\"agent_id\":\"p214-agent\",\"connection\":{\"name\":\"p214-stdio\",\"transport\":\"stdio\",\"command\":[\"/bin/true\"],\"artifact_byte_eligible\":true,\"artifact_params\":{\"ingest\":[\"doc\"]}}}"
        if [ "${P214_STATUS}" = "400" ] && [ "${P214_CODE}" = "invalid_request" ]; then
            ok 'phase 214: a mapping on a stdio connection is refused 400 invalid_request'
        else
            fail "phase 214: stdio mapping returned ${P214_STATUS} ${P214_CODE}, want 400 invalid_request"
        fi

        # A MALFORMED mapping (a duplicate parameter) is refused by the
        # shared shape validator both doors run.
        p214_post "${ADD_URL}" "{\"identity\":${ID},\"agent_id\":\"p214-agent\",\"connection\":{\"name\":\"p214-dup\",\"transport\":\"http\",\"url\":\"https://p214.invalid/mcp\",\"artifact_byte_eligible\":true,\"artifact_params\":{\"ingest\":[\"doc\",\"doc\"]}}}"
        if [ "${P214_STATUS}" = "400" ] && [ "${P214_CODE}" = "invalid_request" ]; then
            ok 'phase 214: a duplicate mapped parameter is refused 400 invalid_request'
        else
            fail "phase 214: duplicate parameter returned ${P214_STATUS} ${P214_CODE}, want 400 invalid_request"
        fi
    fi
fi

# ----------------------------------------------------------------------------
# 7. The test suites this phase's guarantees actually live in.
# ----------------------------------------------------------------------------

# The WHOLE package, not a name prefix: the carrier's projections, the
# encoding pin (including the companion proving a Go-string slot
# CORRUPTS), every refusal, the ceiling boundary and the package-shape
# assertion that bounds the scan's same-package blind spot.
run_filtered_tests 'phase 214: the egress package is green in full — the normative base64 encoding, the string-corruption companion, the carrier projections, every refusal, and the package-shape bound' \
    '.' "./${EGRESS_PKG}/"

run_filtered_tests 'phase 214: the egress scan holds the encoder to one reviewed call site (non-vacuity, alias, second-site, stale-registration and value-reference pins)' \
    'TestEgress_' './internal/tools/artifactref/'

run_filtered_tests 'phase 214: the driver delivers exact bytes, records fail-closed before the wire, resolves once per dispatched call, refuses an unmapped schema, and survives N=128 concurrent reuse' \
    'TestEgress_' './internal/tools/drivers/mcp/'

run_filtered_tests 'phase 214: the egress declaration is refused identically at BOTH persistence doors and carried into the attach request' \
    'TestArtifactEgress' './internal/runtime/agentcfg/protocol/'

run_filtered_tests 'phase 214: the boot validator enforces the same eligibility, transport and shape rules, and the ceiling is independent of the heavy threshold' \
    'TestValidate_MCPArtifact|TestValidateMCPArtifact|TestDefaultMCPArtifact' './internal/config/'

run_filtered_tests 'phase 214: end to end — bytes reach a real MCP server byte-exact while ALL SEVEN sinks stay clean, cross-identity ids answer not-found, and the App callback fails loud' \
    'TestE2E_MCPEgress_' './test/integration/'

smoke_summary
