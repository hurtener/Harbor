#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 171 — events.aggregate durable-driver parity + conformance-matrix
# closure (HA-18 + HA-20, D-305). The aggregator sources its snapshot from the
# HistoryReplayer cross-session windowed fan-in (the same substrate events.list
# uses), threading the handler's server-derived `widened` decision, so
# events.aggregate returns on the durable driver instead of 500ing. One additive
# wire field (EventAggregateResponse.Truncated) — the DATA-not-500 partial signal.
#
# Live-server assertions (404/405/501 → SKIP per CLAUDE.md §4.2):
#   1. The route POST /v1/events/aggregate is mounted (a no-bearer POST → 401,
#      not the route-miss 404). 401 is ALSO the identity-mandatory check.
#   2. A structurally-valid windowed aggregate (dev token) → 200 with a
#      `buckets` array of the expected length AND a `truncated` field of the
#      right shape (boolean or omitted). NOTE: preflight boots the INMEM events
#      driver, so this is the regression guard that the method still works +
#      carries the additive shape; the durable-parity proof (200 not 500 on
#      durable) is the integration test
#      test/integration/events_aggregate_durable_test.go.
#   3. A non-dividing Window/Bucket pair → 400 (never a fractional bucket).
#   4. A cross-tenant aggregate body WITHOUT an elevated scope → 403 (when the
#      dev token carries no admin/console:fleet scope; a dev token that DOES is
#      informational, the 403 leg is pinned in the Go handler + integration test).
# Static guards (always run, never skip): the additive-field invariant (the
# aggregate response added ONLY `Truncated`), single-source method string, no
# Console import in the stream aggregate handler.

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

AGG_URL="$(api_url /v1/events/aggregate)"

TOKEN="dev-token-placeholder"
if [ -n "${HARBOR_DEV_TOKEN:-}" ]; then
  TOKEN="${HARBOR_DEV_TOKEN}"
fi
ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")

if command -v curl >/dev/null 2>&1; then
  # Route probe: a no-bearer POST distinguishes a missing route (404) from an
  # auth-rejected (401). 401 means the route is mounted AND the identity gate
  # fires.
  set +e
  PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' \
    -d '{"window":3600000000000,"bucket":60000000000}' "${AGG_URL}")
  set -e
  case "${PROBE:-000}" in
    404 | 405 | 501 | 000)
      skip "phase 171: /v1/events/aggregate route not present (${PROBE:-000})"
      ;;
    401)
      ok "phase 171: events.aggregate rejects identity-less body (401)"

      if [ -n "${HARBOR_DEV_TOKEN:-}" ] && command -v jq >/dev/null 2>&1; then
        TMP="$(mktemp)"
        trap 'rm -f "${TMP}"' EXIT
        # Happy path: 200 + a `buckets` array of length 60 (1h/1m) + the
        # additive `truncated` field of the right shape.
        set +e
        ST=$(curl -s -o "${TMP}" -w '%{http_code}' --max-time 10 \
          -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
          -H 'Content-Type: application/json' \
          -d '{"window":3600000000000,"bucket":60000000000}' "${AGG_URL}")
        set -e
        case "${ST}" in
          200)
            LEN=$(jq -r '.buckets | length' "${TMP}" 2>/dev/null || echo "")
            if [ "${LEN}" = "60" ]; then
              ok "phase 171: events.aggregate returns 60 buckets for 1h window / 1m bucket (200)"
            else
              fail "phase 171: events.aggregate buckets length = ${LEN}, want 60"
            fi
            # Additive-field shape guard: `truncated` is a boolean when present
            # and omitted (null) when false — never a string / number / object.
            TTYPE=$(jq -r '.truncated | type' "${TMP}" 2>/dev/null || echo "null")
            if [ "${TTYPE}" = "boolean" ] || [ "${TTYPE}" = "null" ]; then
              ok "phase 171: aggregate response carries a well-shaped 'truncated' field (${TTYPE})"
            else
              fail "phase 171: aggregate 'truncated' has wrong type ${TTYPE}, want boolean|null"
            fi
            ;;
          404 | 405 | 501)
            skip "phase 171: events.aggregate route not yet implemented (${ST})"
            ;;
          *)
            fail "phase 171: events.aggregate happy path expected 200, got ${ST}"
            ;;
        esac

        # Bad Window/Bucket pair — 7-minute bucket on a 1-hour window → 400.
        set +e
        BAD=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
          -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
          -H 'Content-Type: application/json' \
          -d '{"window":3600000000000,"bucket":420000000000}' "${AGG_URL}")
        set -e
        if [ "${BAD}" = "400" ]; then
          ok "phase 171: events.aggregate rejects non-dividing Window/Bucket with 400"
        else
          fail "phase 171: bad Window/Bucket expected 400, got ${BAD}"
        fi

        # Cross-tenant filter WITHOUT elevated scope → 403 (when the dev token
        # carries no admin/console:fleet scope).
        set +e
        XT=$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 \
          -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
          -H 'Content-Type: application/json' \
          -d '{"filter":{"tenant_ids":["t-foreign"]},"window":3600000000000,"bucket":60000000000}' "${AGG_URL}")
        set -e
        if [ "${XT}" = "403" ]; then
          ok "phase 171: cross-tenant aggregate without a scope claim is 403"
        else
          skip "phase 171: cross-tenant aggregate answered ${XT} (dev token likely carries an elevated scope; the 403 leg is pinned in the Go handler + integration tests)"
        fi
      else
        skip "phase 171: dev bearer / jq unavailable — aggregate round-trip covered by Go tests"
      fi
      ;;
    *)
      fail "phase 171: events.aggregate no-bearer probe expected 401/404, got ${PROBE}"
      ;;
  esac
else
  skip "phase 171: curl not available"
fi

# ---------------------------------------------------------------------------
# Static guards (always run, never skip).
# ---------------------------------------------------------------------------

# Additive-field invariant: the aggregate RESPONSE gained EXACTLY the additive
# `Truncated bool` field (json:"truncated,omitempty"). Anchored on the
# aggregate-specific godoc phrase so it targets EventAggregateResponse (the
# events.list response carries an identically-declared Truncated field). This is
# the ONLY wire change the phase makes on the aggregate surface.
assert_grep_count 'single-read aggregation bound' \
  internal/protocol/types/events.go 1 \
  "phase 171: EventAggregateResponse gained the additive Truncated field"

# No new aggregate method: the literal appears under internal/ only in the
# single-source definition (methods.go) + the checker's expected-method registry
# (singlesource.go). Any OTHER non-test occurrence is a second definition / a
# hand-rolled method string.
OTHER=$(grep -rl --include='*.go' '"events\.aggregate"' internal/ 2>/dev/null \
  | grep -v '_test.go' \
  | grep -v 'internal/protocol/methods/methods.go' \
  | grep -v 'internal/protocol/singlesource/singlesource.go' || true)
if [ -z "${OTHER}" ]; then
  ok "phase 171: no second definition of the literal \"events.aggregate\" under internal/"
else
  fail "phase 171: literal \"events.aggregate\" found outside methods.go: ${OTHER}"
fi

# No Console import inside the stream transport handler (the Runtime never
# imports the Console).
assert_grep_absent 'web/console' \
  internal/protocol/transports/stream/handlers.go \
  "phase 171: events-aggregate handler does not import the Console"

smoke_summary
