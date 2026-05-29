#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 105 — Console first-attach UX.
#
# Smoke assertions for the /v1/dev/bootstrap.json endpoint and the
# related Console changes. The endpoint is dev-only (mounted by
# harbor dev and harbor console, never by harbor serve).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# Live-server assertions (require a booted harbor dev instance)
# ----------------------------------------------------------------------------

BOOTSTRAP_URL="$(api_url /v1/dev/bootstrap.json)"

# assert_body_path_eq + assert_body_path_truthy — local helpers. common.sh
# ships assert_status / assert_post_status / assert_json_path, but they
# either GET the URL themselves or compare a path-value pair against a
# fresh fetch. The bootstrap endpoint mints a FRESH token on every call,
# so we issue ONE POST + reuse the captured body across all the field
# assertions; otherwise the freshness assertion below races itself.
assert_body_path_eq() {
    local path="$1" expected="$2" body="$3" desc="$4"
    local actual
    actual=$(printf '%s' "${body}" | jq -r "${path}" 2>/dev/null || echo "")
    if [ "${actual}" = "${expected}" ]; then
        ok "${desc}: ${path} = ${expected}"
    else
        fail "${desc}: ${path} expected ${expected}, got ${actual}"
    fi
}
assert_body_path_truthy() {
    local path="$1" body="$2" desc="$3"
    local value
    value=$(printf '%s' "${body}" | jq -r "${path} // empty" 2>/dev/null || echo "")
    if [ -n "${value}" ]; then
        ok "${desc}"
    else
        fail "${desc} -- path ${path} resolved to empty/null"
    fi
}

# 1. Endpoint reachable on harbor dev (POST returns 200 from a loopback peer).
assert_post_status 200 "${BOOTSTRAP_URL}" '{}' \
  "bootstrap: POST returns 200 on dev build"

# Capture one bootstrap response and inspect every field from it.
BOOTSTRAP_RESULT="$(curl -sS -X POST -d '{}' "${BOOTSTRAP_URL}")"

# 2. Token field non-empty
assert_body_path_truthy '.token' "${BOOTSTRAP_RESULT}" "bootstrap: token field non-empty"

# 3. Identity correctly populated
assert_body_path_eq '.identity.tenant'  'dev' "${BOOTSTRAP_RESULT}" "bootstrap: identity.tenant is dev"
assert_body_path_eq '.identity.user'    'dev' "${BOOTSTRAP_RESULT}" "bootstrap: identity.user is dev"
assert_body_path_eq '.identity.session' 'dev' "${BOOTSTRAP_RESULT}" "bootstrap: identity.session is dev"

# 4. Admin scope first
assert_body_path_eq '.scopes[0]' 'admin' "${BOOTSTRAP_RESULT}" "bootstrap: admin scope first"

# 5. Token freshness — two calls return different tokens
TOKEN1="$(echo "${BOOTSTRAP_RESULT}" | jq -r '.token')"
BOOTSTRAP_RESULT2="$(curl -sS -X POST -d '{}' "${BOOTSTRAP_URL}")"
TOKEN2="$(echo "${BOOTSTRAP_RESULT2}" | jq -r '.token')"
if [ "${TOKEN1}" = "${TOKEN2}" ]; then
  fail "bootstrap: token freshness — two calls returned the same token"
else
  ok "bootstrap: token freshness — two calls returned different tokens"
fi

# 6. base_url and protocol_version present
assert_body_path_truthy '.base_url' "${BOOTSTRAP_RESULT}" "bootstrap: base_url non-empty"
assert_body_path_truthy '.protocol_version' "${BOOTSTRAP_RESULT}" "bootstrap: protocol_version non-empty"

# 7. Non-loopback rejection — requires a non-loopback peer. The preflight
#    harness runs the dev server locally, so we can't simulate a non-loopback
#    peer directly. SKIP with rationale.
skip "non-loopback rejection: requires a non-loopback peer simulation (test gap, manual verification only)"

# ----------------------------------------------------------------------------
# Static assertions (run against the source tree)
# ----------------------------------------------------------------------------

# 8. AttachToLocalCard imported by settings page
if grep -q 'AttachToLocalCard' web/console/src/routes/"(console)"/settings/+page.svelte 2>/dev/null; then
  ok "static: settings page imports AttachToLocalCard"
else
  fail "static: settings page does not import AttachToLocalCard"
fi

# 9. Layout has the goto('/settings') redirect
if grep -q "goto('/settings'" web/console/src/routes/"(console)"/+layout.svelte 2>/dev/null; then
  ok "static: layout has goto('/settings') redirect"
else
  fail "static: layout does not have goto('/settings') redirect"
fi

smoke_summary
