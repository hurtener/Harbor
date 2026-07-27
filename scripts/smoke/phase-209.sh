#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 209 smoke — `artifacts.get`, the operator fetch ceiling, and
# byte-offset windowing (D-353).
#
# The point of this script is the DEFAULT DRIVER. The only artifact read
# path that existed before this phase, `artifacts.get_ref`, type-asserts
# the optional `artifacts.Presigner` capability — which exactly one of
# five drivers implements, and NOT the `inmem` default the preflight dev
# server boots on. So every assertion below runs against `inmem`
# deliberately: if this surface only worked on a presigning store, this
# script would report SKIP where it now reports OK, and the gap the phase
# exists to close would still be open.
#
# What is asserted, all against the booted dev server:
#   1. put -> get round-trips the stored bytes on `inmem`.
#   2. A bounded read is TRUTHFUL: truncated=true with
#      total_size_bytes > returned_bytes, and offset echoed.
#   3. An offset window returns the requested byte range, and the last
#      window of an artifact reports truncated=false.
#   4. A request above the deployment ceiling is SERVED AT THE CEILING —
#      not refused — and reports the clamp through the same fields.
#   5. A cross-tenant id answers not-found rather than revealing whether
#      the artifact exists; a foreign-tenant SCOPE is refused flat on
#      tenant before the store is consulted.
#   6. A negative offset is refused loudly rather than reinterpreted.
#
# The 404/405/501 -> SKIP convention holds for the put and the first get,
# which is where an unwired surface is still an honest explanation. After
# that the surface is established, so a 404 there is a FAIL — treating it
# as a SKIP would let the regression each assertion guards pass as a
# green run (AGENTS.md §4.2 item 5).
#
# Portability (both traps cost this project a release):
#   - No `\t` / `\d` inside any grep -E pattern. BSD grep matches them,
#     GNU grep does not, so such a guard is silently inert on Linux CI.
#     `[[:space:]]` / `[[:digit:]]` only.
#   - No `CGO_ENABLED=0 go test -race`: the race detector needs cgo on
#     Linux and that combination fails to build.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

skip_all_if_server_down 'phase 209 artifacts.get'

DEV_TOKEN="${HARBOR_DEV_TOKEN:-dev-token}"

# control_post <method> <body-json>
# POSTs to the REST control surface; sets the STATUS / BODY globals.
# Bodies are passed as a single pre-built variable so bash brace
# expansion never touches the JSON literal.
control_post() {
    local method="$1" body="$2" raw
    STATUS="000"
    BODY="{}"
    if ! command -v curl >/dev/null 2>&1; then
        return 0
    fi
    raw=$(curl -s -w $'\n%{http_code}' \
        -H "Authorization: Bearer ${DEV_TOKEN}" \
        -H "Content-Type: application/json" \
        --max-time 10 \
        -X POST "$(api_url "/v1/control/${method}")" \
        --data "${body}" 2>/dev/null) || raw=$'{}\n000'
    STATUS="${raw##*$'\n'}"
    BODY="${raw%$'\n'*}"
    [ -z "${STATUS}" ] && STATUS="000"
    [ "${STATUS}" = "000" ] && BODY="{}"
    return 0
}

if ! command -v jq >/dev/null 2>&1; then
    skip 'phase 209: jq not available — the response-shape assertions need it'
    smoke_summary
    exit 0
fi

# The `harbor dev` JWT carries identity (tenant=dev, user=dev,
# session=dev). The body scope MUST match it — the shared body-identity
# gate refuses a body whose user/session disagree with the verified
# identity.
DEV_SCOPE='{"tenant":"dev","user":"dev","session":"dev"}'

# ---------------------------------------------------------------------------
# Seed. 128 bytes of deterministic ASCII, so every window assertion below
# has a known total and a known byte at every offset. Deterministic input
# keeps the run idempotent: the id is content-addressed, so re-running the
# smoke re-puts the same artifact.
#
# The payload is four DISTINGUISHABLE 32-byte blocks (32 'A', 32 'B',
# 32 'C', 32 'D'), so an offset window's CONTENT proves it started
# where it was asked to. A uniform payload would let a window that
# silently ignored `offset` pass on length alone.
SEED_B64='QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJCQkJDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDQ0NDREREREREREREREREREREREREREREREREREREREREREQ='
SEED_HEAD32_B64='QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE='
SEED_TAIL32_B64='REREREREREREREREREREREREREREREREREREREREREQ='
SEED_TOTAL=128

PUT_BODY='{"scope":'"${DEV_SCOPE}"',"bytes":"'"${SEED_B64}"'","opts":{"mime_type":"text/plain","filename":"smoke-209.txt","namespace":"smoke209"}}'
control_post 'artifacts.put' "${PUT_BODY}"
REF_ID=""
case "${STATUS}" in
    404|405|501|000)
        skip "phase 209: artifacts.put ${STATUS} (artifacts surface not wired on this build)"
        smoke_summary
        exit 0
        ;;
    200|201)
        REF_ID=$(printf '%s' "${BODY}" | jq -r '.ref.id // empty')
        if [ -z "${REF_ID}" ]; then
            fail "phase 209: artifacts.put returned no ref.id (body=${BODY})"
            smoke_summary
            exit 1
        fi
        ;;
    *)
        fail "phase 209: artifacts.put HTTP ${STATUS} (body=${BODY})"
        smoke_summary
        exit 1
        ;;
esac

# ---------------------------------------------------------------------------
# Assertion 1 — the round trip on the DEFAULT driver.
#
# `inmem` implements no Presigner, so `artifacts.get_ref` answers 501 on
# this same server. That this returns the stored bytes is the whole
# point: the byte read resolves through the MANDATORY `ArtifactStore.Get`
# rather than an optional capability.
# ---------------------------------------------------------------------------
GET_BODY='{"scope":'"${DEV_SCOPE}"',"id":"'"${REF_ID}"'"}'
control_post 'artifacts.get' "${GET_BODY}"
case "${STATUS}" in
    404|405|501|000)
        skip "phase 209: artifacts.get ${STATUS} (method not yet wired)"
        smoke_summary
        exit 0
        ;;
    200)
        if printf '%s' "${BODY}" | jq -e \
            --arg want "${SEED_B64}" --argjson total "${SEED_TOTAL}" \
            '.content == $want and .total_size_bytes == $total and .returned_bytes == $total and .truncated == false and .offset == 0 and (.ref.id | length) > 0' \
            >/dev/null 2>&1; then
            ok 'phase 209: artifacts.get round-trips the stored bytes on the default inmem driver (no Presigner needed)'
        else
            fail "phase 209: artifacts.get round trip mismatched (body=${BODY})"
        fi
        ;;
    *)
        fail "phase 209: artifacts.get HTTP ${STATUS} (body=${BODY})"
        ;;
esac

# ---------------------------------------------------------------------------
# Assertion 2 — a bounded read is TRUTHFUL about its bound.
#
# 404 is deliberately NOT a SKIP from here on: assertion 1 established the
# surface is wired on this build, so an unwired method no longer explains
# a 404 and treating it as one would hide a real regression.
# ---------------------------------------------------------------------------
BOUNDED_BODY='{"scope":'"${DEV_SCOPE}"',"id":"'"${REF_ID}"'","max_bytes":32}'
control_post 'artifacts.get' "${BOUNDED_BODY}"
case "${STATUS}" in
    405|501|000)
        skip "phase 209: bounded artifacts.get ${STATUS} (surface not reachable)"
        ;;
    200)
        if printf '%s' "${BODY}" | jq -e \
            --argjson total "${SEED_TOTAL}" \
            --arg head "${SEED_HEAD32_B64}" \
            '.truncated == true and .returned_bytes == 32 and .total_size_bytes == $total and .total_size_bytes > .returned_bytes and .offset == 0 and .content == $head' \
            >/dev/null 2>&1; then
            ok 'phase 209: a bounded artifacts.get reports truncated=true with total_size_bytes > returned_bytes'
        else
            fail "phase 209: bounded artifacts.get did not report its bound truthfully (body=${BODY})"
        fi
        ;;
    *)
        fail "phase 209: bounded artifacts.get HTTP ${STATUS} (body=${BODY})"
        ;;
esac

# ---------------------------------------------------------------------------
# Assertion 3 — byte-offset windowing, including the LAST window.
#
# The last window is the one that proves `truncated` is computed from
# `offset + returned_bytes` rather than from `returned_bytes < total`: a
# naive implementation reports truncated=true here and this assertion
# catches it.
# ---------------------------------------------------------------------------
WINDOW_BODY='{"scope":'"${DEV_SCOPE}"',"id":"'"${REF_ID}"'","offset":96,"max_bytes":32}'
control_post 'artifacts.get' "${WINDOW_BODY}"
case "${STATUS}" in
    405|501|000)
        skip "phase 209: offset artifacts.get ${STATUS} (surface not reachable)"
        ;;
    200)
        if printf '%s' "${BODY}" | jq -e \
            --argjson total "${SEED_TOTAL}" \
            --arg tail "${SEED_TAIL32_B64}" \
            '.offset == 96 and .returned_bytes == 32 and .total_size_bytes == $total and .truncated == false and .content == $tail' \
            >/dev/null 2>&1; then
            ok 'phase 209: an offset window returns the requested byte range and the LAST window reports truncated=false'
        else
            fail "phase 209: offset window wrong (body=${BODY})"
        fi
        ;;
    *)
        fail "phase 209: offset artifacts.get HTTP ${STATUS} (body=${BODY})"
        ;;
esac

# ---------------------------------------------------------------------------
# Assertion 4 — the clamp is SERVED, not refused, and it is not silent.
#
# A max_bytes far above any plausible ceiling must come back 200 with the
# artifact served (it is smaller than the ceiling), never a 4xx. A refusal
# here would mean the ceiling became a wall the caller cannot discover.
# ---------------------------------------------------------------------------
CLAMP_BODY='{"scope":'"${DEV_SCOPE}"',"id":"'"${REF_ID}"'","max_bytes":1073741824}'
control_post 'artifacts.get' "${CLAMP_BODY}"
case "${STATUS}" in
    405|501|000)
        skip "phase 209: over-ceiling artifacts.get ${STATUS} (surface not reachable)"
        ;;
    200)
        if printf '%s' "${BODY}" | jq -e \
            --argjson total "${SEED_TOTAL}" \
            '.returned_bytes == $total and .total_size_bytes == $total and .truncated == false' \
            >/dev/null 2>&1; then
            ok 'phase 209: a max_bytes above the ceiling is SERVED at the ceiling (200, not a refusal) and reports through the same fields'
        else
            fail "phase 209: over-ceiling artifacts.get returned an unexpected shape (body=${BODY})"
        fi
        ;;
    *)
        fail "phase 209: over-ceiling artifacts.get HTTP ${STATUS} — a clamp must be served, not refused (body=${BODY})"
        ;;
esac

# ---------------------------------------------------------------------------
# Assertion 5a — an id the caller's own scope does not hold answers
# not-found. It must not distinguish "never existed" from "exists in
# another identity", because a distinguishable refusal confirms the
# existence of another identity's artifact to a caller that cannot read
# it.
# ---------------------------------------------------------------------------
FOREIGN_ID_BODY='{"scope":'"${DEV_SCOPE}"',"id":"smoke209_ffffffffffff"}'
control_post 'artifacts.get' "${FOREIGN_ID_BODY}"
case "${STATUS}" in
    405|501|000)
        skip "phase 209: unknown-id artifacts.get ${STATUS} (surface not reachable)"
        ;;
    404)
        if printf '%s' "${BODY}" | jq -e '.code == "not_found"' >/dev/null 2>&1; then
            ok 'phase 209: an id outside the caller scope answers not_found rather than revealing existence'
        else
            fail "phase 209: unknown-id artifacts.get 404 but code is not not_found (body=${BODY})"
        fi
        ;;
    200)
        fail "phase 209: unknown-id artifacts.get returned 200 — an unstored id resolved (body=${BODY})"
        ;;
    *)
        fail "phase 209: unknown-id artifacts.get HTTP ${STATUS} (expected 404 not_found; body=${BODY})"
        ;;
esac

# ---------------------------------------------------------------------------
# Assertion 5b — a foreign-tenant SCOPE is refused flat on tenant, BEFORE
# the store is consulted. The dev token carries admin + console:fleet, so
# this is meaningful with the token the harness has: unlike
# artifacts.list, this method offers no elevation branch, because it
# hands over CONTENT rather than metadata.
#
# 404 is a FAIL here, not a pass: it would mean the request reached the
# store under another tenant's scope instead of being refused at the edge.
# ---------------------------------------------------------------------------
CROSS_TENANT_BODY='{"scope":{"tenant":"t-other","user":"dev","session":"dev"},"id":"'"${REF_ID}"'"}'
control_post 'artifacts.get' "${CROSS_TENANT_BODY}"
case "${STATUS}" in
    405|501|000)
        skip "phase 209: cross-tenant artifacts.get ${STATUS} (surface not reachable)"
        ;;
    403)
        if printf '%s' "${BODY}" | jq -e '.code == "scope_mismatch"' >/dev/null 2>&1; then
            ok 'phase 209: a foreign-tenant scope is refused flat with scope_mismatch — no admin-tier claim widens the byte read'
        else
            fail "phase 209: cross-tenant artifacts.get 403 but code is not scope_mismatch (body=${BODY})"
        fi
        ;;
    404)
        fail "phase 209: cross-tenant artifacts.get 404 — the request reached the store instead of being refused on tenant (body=${BODY})"
        ;;
    200)
        fail "phase 209: cross-tenant artifacts.get 200 — a differing tenant resolved bytes (body=${BODY})"
        ;;
    *)
        fail "phase 209: cross-tenant artifacts.get HTTP ${STATUS} (expected 403 scope_mismatch; body=${BODY})"
        ;;
esac

# ---------------------------------------------------------------------------
# Assertion 6 — a negative offset is refused loudly rather than
# reinterpreted as "from the end" or silently floored to zero.
# ---------------------------------------------------------------------------
NEG_BODY='{"scope":'"${DEV_SCOPE}"',"id":"'"${REF_ID}"'","offset":-1}'
control_post 'artifacts.get' "${NEG_BODY}"
case "${STATUS}" in
    405|501|000)
        skip "phase 209: negative-offset artifacts.get ${STATUS} (surface not reachable)"
        ;;
    400)
        if printf '%s' "${BODY}" | jq -e '.code == "invalid_request"' >/dev/null 2>&1; then
            ok 'phase 209: a negative offset is refused with invalid_request, not silently floored'
        else
            fail "phase 209: negative-offset artifacts.get 400 but code is not invalid_request (body=${BODY})"
        fi
        ;;
    200)
        fail "phase 209: negative-offset artifacts.get returned 200 — the offset was silently reinterpreted (body=${BODY})"
        ;;
    *)
        fail "phase 209: negative-offset artifacts.get HTTP ${STATUS} (expected 400 invalid_request; body=${BODY})"
        ;;
esac

# ---------------------------------------------------------------------------
# Assertion 7 — the operator ceiling is a REAL configuration surface, not
# two constants renamed. A static guard, because the live server boots on
# the defaults and cannot demonstrate an operator override.
# ---------------------------------------------------------------------------
assert_grep_present 'fetch_default_max_bytes' internal/config/config.go \
    'phase 209: artifacts.fetch_default_max_bytes is a configuration field'
assert_grep_present 'fetch_hard_max_bytes' internal/config/config.go \
    'phase 209: artifacts.fetch_hard_max_bytes is a configuration field'
assert_grep_present 'artifacts.fetch_default_max_bytes' internal/config/validate.go \
    'phase 209: the read-back default is validated by name in the config validator'
assert_grep_present 'artifacts.fetch_hard_max_bytes' internal/config/validate.go \
    'phase 209: the read-back ceiling is validated by name in the config validator'
assert_grep_present 'fetch_default_max_bytes' examples/harbor.yaml \
    'phase 209: the reference config documents the read-back default'
assert_grep_present 'fetch_hard_max_bytes' examples/harbor.yaml \
    'phase 209: the reference config documents the read-back ceiling'

# The single-source rule for the two constants: the builtin must resolve
# them from internal/config, not carry its own literals. A guard against
# the tool and the wire surface drifting into two different ceilings.
assert_grep_present 'config.DefaultArtifactFetchMaxBytes' internal/tools/builtin/artifact_fetch.go \
    'phase 209: artifact_fetch single-sources its default on internal/config'
assert_grep_present 'config.DefaultArtifactFetchHardMaxBytes' internal/tools/builtin/artifact_fetch.go \
    'phase 209: artifact_fetch single-sources its ceiling on internal/config'

# ---------------------------------------------------------------------------
# Assertion 8 — the behavioural gate for the halves with no HTTP surface:
# the builtin's offset windowing and the surface's own unit + concurrent
# suites, under -race.
# ---------------------------------------------------------------------------
if go test -race -count=1 -timeout 300s -run 'TestArtifactFetch' ./internal/tools/builtin/ >/dev/null 2>&1; then
    ok 'phase 209: artifact_fetch offset + ceiling tests pass under -race'
else
    fail 'phase 209: artifact_fetch tests failed (run `go test -race -run TestArtifactFetch ./internal/tools/builtin/`)'
fi

if go test -race -count=1 -timeout 300s -run 'TestArtifactsGetHandler|TestArtifactsHandler_Concurrent' ./internal/protocol/ >/dev/null 2>&1; then
    ok 'phase 209: artifacts.get handler + concurrent-reuse suites pass under -race'
else
    fail 'phase 209: artifacts.get handler suites failed (run `go test -race -run TestArtifactsGetHandler ./internal/protocol/`)'
fi

# The transport's per-method body-scope row selection. Bundled here as a
# §17.6 fix: the mutation sweep found that dropping ANY arm of that switch
# broke zero tests — a content read silently fell through to the
# admin-elevatable default, and the surface's own tenant check covered for
# it, so a live smoke stayed green. The pin covers all five methods.
if go test -race -count=1 -timeout 300s -run 'TestArtifactsBodyScope' ./internal/protocol/transports/control/ >/dev/null 2>&1; then
    ok 'phase 209: the artifacts transport pins its per-method body-scope row (all five methods) under -race'
else
    fail 'phase 209: the artifacts body-scope row pin failed (run `go test -race -run TestArtifactsBodyScope ./internal/protocol/transports/control/`)'
fi

if go test -race -count=1 -timeout 300s -run 'TestE2E_ArtifactsGet' ./test/integration/ >/dev/null 2>&1; then
    ok 'phase 209: TestE2E_ArtifactsGet_* passes — the byte read works end to end through the real control transport'
else
    fail 'phase 209: TestE2E_ArtifactsGet_* failed (run `go test -race -run TestE2E_ArtifactsGet ./test/integration/`)'
fi

smoke_summary
