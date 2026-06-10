#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 111b smoke — tool-OAuth completion leg (D-199; RFC §6.4 + §3.3;
# docs/plans/phase-111b-tool-oauth-completion.md).
#
# The phase ships `auth.CallbackHandler`, mounted by `harbor dev` at
# `GET /v1/tools/oauth/callback` (the documented default RedirectURI
# shape). The smoke covers:
#   1. The route is mounted: a no-param GET answers 400 (fails loud on
#      garbage), with the typed JSON error shape. 404/405/501 → SKIP
#      keeps pre-phase builds green (the sacred convention — 404 on
#      the no-param probe means "not mounted yet", distinguishable
#      from the handler's own flow_not_found because the probe
#      expects 400).
#   2. The flow-not-found mapping is live: once the route is known to
#      be mounted, `?state=bogus&code=x` answers 404 with
#      `.error == "flow_not_found"` (the handler's typed error shape,
#      asserted via jq).
#   3. Static guards: the handler + the dev-server and devstack mounts
#      exist; the choreography E2E + the recipe ship in-tree.
#   4. The package unit-test leg (`-run 'Callback|DenyFlow'`) under
#      -race.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

CALLBACK_URL="$(api_url '/v1/tools/oauth/callback')"

# --- 1 + 2. Live-route assertions -------------------------------------------

if ! command -v curl >/dev/null 2>&1; then
    skip "phase 111b: curl not available"
else
    probe=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "${CALLBACK_URL}" || echo "000")
    case "$probe" in
        404|405|501|000*)
            skip "phase 111b: callback route not mounted yet / server unreachable (${probe})"
            ;;
        400)
            ok "phase 111b: GET ${CALLBACK_URL} with no params → 400 (mounted, fails loud on garbage)"
            # The route is mounted — the 404 below is the handler's own
            # flow_not_found mapping, not an unmounted route.
            status=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 "${CALLBACK_URL}?state=bogus-smoke-state&code=x" || echo "000")
            if [ "$status" = "404" ]; then
                ok "phase 111b: bogus state → 404 (flow-not-found mapping live)"
            else
                fail "phase 111b: bogus state expected 404, got ${status}"
            fi
            if command -v jq >/dev/null 2>&1; then
                body=$(curl -s --max-time 5 "${CALLBACK_URL}?state=bogus-smoke-state&code=x" || echo '{}')
                errcode=$(printf '%s' "$body" | jq -r '.error' 2>/dev/null || echo "")
                if [ "$errcode" = "flow_not_found" ]; then
                    ok "phase 111b: typed error shape (.error = flow_not_found)"
                else
                    fail "phase 111b: error shape: .error expected flow_not_found, got '${errcode}'"
                fi
                noparam=$(curl -s --max-time 5 "${CALLBACK_URL}" || echo '{}')
                npcode=$(printf '%s' "$noparam" | jq -r '.error' 2>/dev/null || echo "")
                if [ "$npcode" = "invalid_request" ]; then
                    ok "phase 111b: no-param error shape (.error = invalid_request)"
                else
                    fail "phase 111b: no-param error shape: expected invalid_request, got '${npcode}'"
                fi
            else
                skip "phase 111b: jq not available for error-shape assertions"
            fi
            ;;
        *)
            fail "phase 111b: GET ${CALLBACK_URL} with no params expected 400, got ${probe}"
            ;;
    esac
fi

# --- 3. Static guards --------------------------------------------------------

assert_file internal/tools/auth/callback.go 'phase 111b: auth.CallbackHandler source'
assert_file test/integration/phase111b_oauth_completion_test.go 'phase 111b: the full-choreography E2E'
assert_file docs/recipes/steer-and-resume-a-run.md 'phase 111b: the steer-and-resume recipe (OAuth section)'

assert_grep_present 'func CallbackHandler\(providers map\[string\]OAuthProvider, opts \.\.\.CallbackOption\) http\.Handler' \
    internal/tools/auth/callback.go 'phase 111b: exported CallbackHandler signature'
assert_grep_present 'CallbackRoutePattern' cmd/harbor/cmd_dev.go \
    'phase 111b: harbor dev mounts the callback route'
assert_grep_present 'CallbackRoutePattern' harbortest/devstack/devstack.go \
    'phase 111b: devstack mirrors the callback mount'
assert_grep_present 'CompleteFlow' internal/tools/auth/callback.go \
    'phase 111b: the handler is CompleteFlow production caller (§13 pair closed)'

# --- 4. Unit-test leg --------------------------------------------------------

if go test -race -count=1 -run 'Callback|DenyFlow' ./internal/tools/auth/ >/dev/null 2>&1; then
    ok "phase 111b: callback handler unit tests pass under -race"
else
    fail "phase 111b: callback handler unit tests FAILED (go test -run 'Callback|DenyFlow' ./internal/tools/auth/)"
fi

smoke_summary
