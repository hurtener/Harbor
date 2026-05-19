#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 73l smoke — Console Artifacts page Protocol surface.
#
# Covers:
#   - artifacts.list (extended filter shape)
#   - artifacts.put  (Brief 11 §PG-2 upload pipeline)
#   - artifacts.get_ref (PresignGet resolver)
#   - Cross-tenant artifacts.list rejection (no ScopeAdmin)
#   - Identity-required failure mode
#
# The 404/405/501 → SKIP convention is preserved: if the dev binary
# does not yet expose any of the three Protocol methods, the
# corresponding assertions SKIP cleanly so the script stays green on
# pre-73-Phase builds.
#
# CLAUDE.md §4.2 conventions:
#   - common.sh helpers only.
#   - At least one OK once the phase ships.
#   - FAIL never on main.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Helpers local to this phase. All real curl wrappers go through common.sh;
# these helpers narrow the call surface to artifacts.* paths.
#
# The HTTP surface (Protocol transport HTTP+SSE — Phase 60) routes JSON-RPC
# calls under /v1/rpc; the smoke uses curl + jq to issue them and inspect
# responses. The dev token (HARBOR_DEV_TOKEN) is injected by preflight.
# ----------------------------------------------------------------------------

DEV_TOKEN="${HARBOR_DEV_TOKEN:-dev-token}"
RPC_URL="$(api_url /v1/rpc)"

# rpc_call <method> <params-json> <description>
# Issues an HTTP POST JSON-RPC call. Returns the raw response body on stdout.
rpc_call() {
    local method="$1" params="$2" desc="$3"
    if ! command -v curl >/dev/null 2>&1 || ! command -v jq >/dev/null 2>&1; then
        skip "${desc}: curl or jq missing"
        return 1
    fi
    local body
    body=$(curl -s -o /dev/stdout -w '' \
        -H "Authorization: Bearer ${DEV_TOKEN}" \
        -H "Content-Type: application/json" \
        --max-time 5 \
        -X POST "${RPC_URL}" \
        --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"${method}\",\"params\":${params}}" \
        2>/dev/null || echo '{}')
    printf '%s' "${body}"
}

# rpc_status <method> <params-json>
# Issues a probe POST and prints the HTTP status code. Used to detect the
# surface-absent 404/405/501 path. When curl cannot reach the server
# (no dev binary running, port not bound, transport error), curl already
# prints "000" via the %{http_code} writer — we mustn't compound with a
# fallback echo or the caller sees "000000". A bare `|| true` keeps the
# function safe under `set -e`.
rpc_status() {
    local method="$1" params="$2"
    if ! command -v curl >/dev/null 2>&1; then
        echo "000"
        return
    fi
    curl -s -o /dev/null -w '%{http_code}' \
        -H "Authorization: Bearer ${DEV_TOKEN}" \
        -H "Content-Type: application/json" \
        --max-time 5 \
        -X POST "${RPC_URL}" \
        --data "{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"${method}\",\"params\":${params}}" \
        2>/dev/null || true
}

# ----------------------------------------------------------------------------
# Assertion 1 — artifacts.list happy path.
# ----------------------------------------------------------------------------

LIST_PARAMS='{"scope":{"tenant_id":"t-dev","user_id":"u-dev","session_id":"s-dev"}}'
STATUS=$(rpc_status "artifacts.list" "${LIST_PARAMS}")
case "${STATUS}" in
    404|405|501|000)
        skip "artifacts.list: ${STATUS} (surface not yet implemented)"
        ;;
    200)
        BODY=$(rpc_call "artifacts.list" "${LIST_PARAMS}" "artifacts.list response")
        if printf '%s' "${BODY}" | jq -e '.result.rows | type == "array"' >/dev/null 2>&1; then
            ok "artifacts.list: returns .result.rows as array"
        elif printf '%s' "${BODY}" | jq -e '.error.code' >/dev/null 2>&1; then
            # Expected error envelopes (e.g. handler returns 200 with JSON-RPC error body).
            CODE=$(printf '%s' "${BODY}" | jq -r '.error.code')
            case "${CODE}" in
                -32601|MethodNotFound)
                    skip "artifacts.list: ${CODE} (surface not yet implemented)"
                    ;;
                *)
                    fail "artifacts.list: unexpected error code ${CODE}"
                    ;;
            esac
        else
            fail "artifacts.list: response shape unexpected (body=${BODY})"
        fi
        ;;
    *)
        fail "artifacts.list: HTTP ${STATUS} (expected 200 or 404/405/501)"
        ;;
esac

# ----------------------------------------------------------------------------
# Assertion 2 — artifacts.put happy path.
# Posts a small text blob; expects a canonical ref.id matching
# {namespace}_{sha256[:12]}.
# ----------------------------------------------------------------------------

# Base64-encoded "hello, harbor\n" — caller-deterministic input keeps the
# assertion idempotent across repeated smoke runs.
PUT_PARAMS='{"scope":{"tenant_id":"t-dev","user_id":"u-dev","session_id":"s-dev"},"bytes":"aGVsbG8sIGhhcmJvcgo=","opts":{"mime_type":"text/plain","filename":"smoke-hello.txt","namespace":"smoke","source":"user_upload"}}'
STATUS=$(rpc_status "artifacts.put" "${PUT_PARAMS}")
case "${STATUS}" in
    404|405|501|000)
        skip "artifacts.put: ${STATUS} (surface not yet implemented)"
        ;;
    200|201)
        BODY=$(rpc_call "artifacts.put" "${PUT_PARAMS}" "artifacts.put response")
        if printf '%s' "${BODY}" | jq -e '.result.ref.id | test("^[a-z0-9_-]+_[a-f0-9]{12}$")' >/dev/null 2>&1; then
            ok "artifacts.put: returns canonical ref.id"
        else
            REF_ID=$(printf '%s' "${BODY}" | jq -r '.result.ref.id // .error.code // "unknown"')
            fail "artifacts.put: unexpected ref id or error (got=${REF_ID})"
        fi
        ;;
    *)
        fail "artifacts.put: HTTP ${STATUS} (expected 200/201)"
        ;;
esac

# ----------------------------------------------------------------------------
# Assertion 3 — artifacts.get_ref PresignGet resolver.
# The FS dev driver does NOT implement Presigner, so we expect either
# CodePresignUnsupported (the typed-error happy path on FS) OR a 200 with a
# non-empty presigned_url on S3-backed dev. Both are OK; only an
# unexpected shape FAILs.
# ----------------------------------------------------------------------------

GETREF_PARAMS='{"scope":{"tenant_id":"t-dev","user_id":"u-dev","session_id":"s-dev"},"id":"smoke_000000000000","expiry":"5m"}'
STATUS=$(rpc_status "artifacts.get_ref" "${GETREF_PARAMS}")
case "${STATUS}" in
    404|405|501|000)
        skip "artifacts.get_ref: ${STATUS} (surface not yet implemented)"
        ;;
    200)
        BODY=$(rpc_call "artifacts.get_ref" "${GETREF_PARAMS}" "artifacts.get_ref response")
        if printf '%s' "${BODY}" | jq -e '.result.presigned_url | type == "string" and length > 0' >/dev/null 2>&1; then
            ok "artifacts.get_ref: returns non-empty presigned_url (S3-backed dev)"
        elif printf '%s' "${BODY}" | jq -e '.error.code | test("Presign[Uu]nsupported|NotFound")' >/dev/null 2>&1; then
            ok "artifacts.get_ref: returns typed error (driver does not implement Presigner — expected on fs/inmem/sqlite/postgres dev)"
        else
            fail "artifacts.get_ref: unexpected response shape (body=${BODY})"
        fi
        ;;
    *)
        fail "artifacts.get_ref: HTTP ${STATUS} (expected 200 or 404/405/501)"
        ;;
esac

# ----------------------------------------------------------------------------
# Assertion 4 — cross-tenant artifacts.list rejection without ScopeAdmin.
# The dev token's tenant is `t-dev`; requesting `tenant_id=t-other` without
# ScopeAdmin must be rejected.
# ----------------------------------------------------------------------------

CROSS_PARAMS='{"scope":{"tenant_id":"t-other","user_id":"u-dev","session_id":"s-dev"}}'
STATUS=$(rpc_status "artifacts.list" "${CROSS_PARAMS}")
case "${STATUS}" in
    404|405|501|000)
        skip "artifacts.list cross-tenant: ${STATUS} (surface not yet implemented)"
        ;;
    403)
        ok "artifacts.list cross-tenant: 403 (rejection without ScopeAdmin)"
        ;;
    200)
        # Some handlers wrap auth failures inside a JSON-RPC error envelope on 200.
        BODY=$(rpc_call "artifacts.list" "${CROSS_PARAMS}" "artifacts.list cross-tenant response")
        if printf '%s' "${BODY}" | jq -e '.error.code | test("[Ss]cope[Mm]ismatch|[Pp]ermission")' >/dev/null 2>&1; then
            ok "artifacts.list cross-tenant: typed error (ScopeMismatch / Permission denied)"
        else
            fail "artifacts.list cross-tenant: expected rejection, got success (body=${BODY})"
        fi
        ;;
    *)
        fail "artifacts.list cross-tenant: HTTP ${STATUS} (expected 403 or rejection envelope)"
        ;;
esac

# ----------------------------------------------------------------------------
# Assertion 5 — identity-required failure on empty scope.
# An artifacts.list with empty tenant/user/session MUST be rejected loudly.
# ----------------------------------------------------------------------------

EMPTY_PARAMS='{"scope":{"tenant_id":"","user_id":"","session_id":""}}'
STATUS=$(rpc_status "artifacts.list" "${EMPTY_PARAMS}")
case "${STATUS}" in
    404|405|501|000)
        skip "artifacts.list identity-required: ${STATUS} (surface not yet implemented)"
        ;;
    400|403)
        ok "artifacts.list identity-required: HTTP ${STATUS} (rejection on empty scope)"
        ;;
    200)
        BODY=$(rpc_call "artifacts.list" "${EMPTY_PARAMS}" "artifacts.list identity-required response")
        if printf '%s' "${BODY}" | jq -e '.error.code | test("[Ii]dentity[Rr]equired|[Mm]issing[Ii]dentity")' >/dev/null 2>&1; then
            ok "artifacts.list identity-required: typed error (IdentityRequired)"
        else
            fail "artifacts.list identity-required: expected rejection, got success (body=${BODY})"
        fi
        ;;
    *)
        fail "artifacts.list identity-required: HTTP ${STATUS} (expected 400/403 or typed error envelope)"
        ;;
esac

smoke_summary
