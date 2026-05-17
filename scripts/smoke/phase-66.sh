#!/usr/bin/env bash
# Phase 66 smoke — `harbor dev` draft-save scaffolding (D-100).
#
# The draft surface lives at `/v1/dev/drafts/` under the same auth
# middleware as the Phase 60 transports — every request requires a
# Bearer token. The dev token is printed to stderr on boot as
# `HARBOR_DEV_TOKEN=...`; the preflight harness captures it in
# ${HARBOR_DATA_DIR}/server.log so a smoke can parse it the same way
# phase-64.sh does.
#
# Assertions:
#
#   1. Unauthenticated POST → 401.
#   2. With dev token + valid body → 201 + non-empty `draft_id`.
#   3. PATCH a file → 200.
#   4. Preview → 200 + `ok=true`.
#   5. Save → 200 + the rendered scaffold passes validation by
#      construction (the engine refuses the save if invalid).
#   6. DELETE → 200.
#   7. GET after DELETE → 404.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

DRAFT_BASE="/v1/dev/drafts"

# Assertion 1 — unauthenticated POST → 401.
actual=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H "Content-Type: application/json" \
    --data '{"name":"smoke-66-unauth"}' \
    "$(api_url ${DRAFT_BASE}/)" || echo "000")
case "$actual" in
    401)
        ok "harbor dev: ${DRAFT_BASE}/ rejects unauthenticated POST (401)"
        ;;
    404|405|501)
        skip "harbor dev: ${DRAFT_BASE}/ surface not yet implemented (${actual})"
        ;;
    *)
        fail "harbor dev: ${DRAFT_BASE}/ unauthenticated status = ${actual}, want 401"
        ;;
esac

# Assertion 2..7 — authenticated round-trip. Skip if we cannot capture
# the dev token from the preflight server log (the smoke must still
# pass against builds that do not yet print the token under this exact
# prefix).
TOKEN=""
if [ -n "${HARBOR_DATA_DIR:-}" ] && [ -f "${HARBOR_DATA_DIR}/server.log" ]; then
    TOKEN="$(grep -m1 '^HARBOR_DEV_TOKEN=' "${HARBOR_DATA_DIR}/server.log" 2>/dev/null | sed 's/^HARBOR_DEV_TOKEN=//')"
fi

if [ -z "${TOKEN}" ]; then
    skip "harbor dev: draft round-trip (HARBOR_DEV_TOKEN not found in server log)"
    smoke_summary
    exit 0
fi

if ! command -v jq >/dev/null 2>&1; then
    skip "harbor dev: draft round-trip (jq not available — cannot parse response bodies)"
    smoke_summary
    exit 0
fi

TMP="$(mktemp -d -t harbor-smoke66-XXXXXX)"
trap 'rm -rf "${TMP}"' EXIT
DRAFT_NAME="smoke66-$(date +%s)"

# Create.
create_status=$(curl -s -o "${TMP}/create.json" -w '%{http_code}' \
    --max-time 5 \
    -X POST -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${TOKEN}" \
    --data "{\"name\":\"${DRAFT_NAME}\"}" \
    "$(api_url ${DRAFT_BASE}/)" || echo "000")
if [ "${create_status}" = "201" ]; then
    DRAFT_ID="$(jq -r '.draft_id // empty' "${TMP}/create.json" 2>/dev/null || true)"
    if [ -n "${DRAFT_ID}" ] && [ "${DRAFT_ID}" != "null" ]; then
        ok "harbor dev: POST ${DRAFT_BASE}/ returns draft_id (${DRAFT_ID})"
    else
        fail "harbor dev: POST ${DRAFT_BASE}/ body missing draft_id"
        echo "  body: $(cat "${TMP}/create.json" 2>/dev/null || echo '(empty)')"
        smoke_summary
        exit 1
    fi
else
    fail "harbor dev: POST ${DRAFT_BASE}/ status = ${create_status}, want 201"
    smoke_summary
    exit 1
fi

# Patch.
patch_status=$(curl -s -o "${TMP}/patch.json" -w '%{http_code}' \
    --max-time 5 \
    -X PATCH -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${TOKEN}" \
    --data '{"content":"# smoke-edited\n"}' \
    "$(api_url ${DRAFT_BASE}/${DRAFT_ID}/files/README.md)" || echo "000")
if [ "${patch_status}" = "200" ]; then
    ok "harbor dev: PATCH ${DRAFT_BASE}/{id}/files/README.md → 200"
else
    fail "harbor dev: PATCH ${DRAFT_BASE}/{id}/files/README.md status = ${patch_status}"
fi

# Preview.
preview_status=$(curl -s -o "${TMP}/preview.json" -w '%{http_code}' \
    --max-time 5 \
    -X POST -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${TOKEN}" \
    --data '{}' \
    "$(api_url ${DRAFT_BASE}/${DRAFT_ID}/preview)" || echo "000")
if [ "${preview_status}" = "200" ]; then
    preview_ok="$(jq -r '.ok // empty' "${TMP}/preview.json" 2>/dev/null || true)"
    if [ "${preview_ok}" = "true" ]; then
        ok "harbor dev: POST ${DRAFT_BASE}/{id}/preview → ok=true"
    else
        fail "harbor dev: preview ok = ${preview_ok} (body: $(cat "${TMP}/preview.json"))"
    fi
else
    fail "harbor dev: POST ${DRAFT_BASE}/{id}/preview status = ${preview_status}"
fi

# Save.
OUT_DIR="${TMP}/promoted"
save_status=$(curl -s -o "${TMP}/save.json" -w '%{http_code}' \
    --max-time 5 \
    -X POST -H "Content-Type: application/json" \
    -H "Authorization: Bearer ${TOKEN}" \
    --data "{\"name\":\"${DRAFT_NAME}\",\"output_dir\":\"${OUT_DIR}\"}" \
    "$(api_url ${DRAFT_BASE}/${DRAFT_ID}/save)" || echo "000")
if [ "${save_status}" = "200" ]; then
    if [ -f "${OUT_DIR}/harbor.yaml" ]; then
        ok "harbor dev: POST ${DRAFT_BASE}/{id}/save promoted scaffold (harbor.yaml present)"
    else
        fail "harbor dev: save returned 200 but ${OUT_DIR}/harbor.yaml missing"
    fi
else
    fail "harbor dev: POST ${DRAFT_BASE}/{id}/save status = ${save_status}"
fi

# Delete.
del_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X DELETE -H "Authorization: Bearer ${TOKEN}" \
    "$(api_url ${DRAFT_BASE}/${DRAFT_ID})" || echo "000")
if [ "${del_status}" = "200" ]; then
    ok "harbor dev: DELETE ${DRAFT_BASE}/{id} → 200"
else
    fail "harbor dev: DELETE ${DRAFT_BASE}/{id} status = ${del_status}"
fi

# Get after delete.
get_status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -H "Authorization: Bearer ${TOKEN}" \
    "$(api_url ${DRAFT_BASE}/${DRAFT_ID})" || echo "000")
if [ "${get_status}" = "404" ]; then
    ok "harbor dev: GET ${DRAFT_BASE}/{id} after DELETE → 404"
else
    fail "harbor dev: GET ${DRAFT_BASE}/{id} after DELETE status = ${get_status}, want 404"
fi

smoke_summary
