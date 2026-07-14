#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 173 — events.aggregate per-tenant attribution for admin-widened reads
# (HA-17, D-307). Opt-in `by_tenant` on EventAggregateRequest adds
# `counts_by_tenant` (tenant → event_type → count) to the response FOR admin-
# widened reads only, for tenants the caller is already entitled to. Additive
# wire fields (D-223/D-209 lockstep).
#
# Live-server assertions (404/405/501 → SKIP per CLAUDE.md §4.2):
#   1. The route POST /v1/events/aggregate is mounted (no-bearer POST → 401).
#   2. An own-scope aggregate WITH `by_tenant: true` → 200 with NO
#      `counts_by_tenant` (a non-widened read ignores the flag — fail-closed;
#      robust with any valid token regardless of scope).
#   3. An aggregate POST WITHOUT `by_tenant` → 200 with NO `counts_by_tenant`
#      key (backward compatible — byte-identical to pre-173).
#   4. A cross-tenant `by_tenant: true` read: if the dev token carries
#      admin/console:fleet → 200 with `counts_by_tenant` present and the
#      per-tenant counts summing to the bucket totals; if it does NOT → 403 at
#      the widening gate (attribution never bypasses the gate). Either outcome
#      proves the invariant.
# Static guards (always run, never skip): the additive-field invariants
# (ByTenant on the request, CountsByTenant on EventBucket) + the hand-mirror
# into the Console per-page wire module + the generated manifest.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

AGG_URL="$(api_url /v1/events/aggregate)"

TOKEN="dev-token-placeholder"
if [ -n "${HARBOR_DEV_TOKEN:-}" ]; then
  TOKEN="${HARBOR_DEV_TOKEN}"
fi
ID_HEADERS=(-H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev")

# 1 h window / 1 min bucket, both in nanoseconds.
WINDOW_NS=3600000000000
BUCKET_NS=60000000000
# Own-scope body with by_tenant (non-widened ⇒ flag ignored, fail-closed).
OWN_BYTENANT_BODY='{"window":'"${WINDOW_NS}"',"bucket":'"${BUCKET_NS}"',"by_tenant":true,"filter":{"tenant_ids":["dev"],"user_ids":["dev"],"session_ids":["dev"]}}'
# Plain body (no by_tenant) — backward-compatible.
PLAIN_BODY='{"window":'"${WINDOW_NS}"',"bucket":'"${BUCKET_NS}"'}'
# Cross-tenant widened body with by_tenant.
CROSS_BYTENANT_BODY='{"window":'"${WINDOW_NS}"',"bucket":'"${BUCKET_NS}"',"by_tenant":true,"filter":{"tenant_ids":["dev","fleet-tenant-b"]}}'

if command -v curl >/dev/null 2>&1; then
  set +e
  PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' \
    -d "${PLAIN_BODY}" "${AGG_URL}")
  set -e
  case "${PROBE:-000}" in
    404 | 405 | 501 | 000)
      skip "phase 173: /v1/events/aggregate route not present (${PROBE:-000})"
      ;;
    401)
      ok "phase 173: events.aggregate rejects identity-less body (401)"

      if [ -n "${HARBOR_DEV_TOKEN:-}" ] && command -v jq >/dev/null 2>&1; then
        OWN="$(mktemp)"; PL="$(mktemp)"; CR="$(mktemp)"
        trap 'rm -f "${OWN}" "${PL}" "${CR}"' EXIT

        # (2) Own-scope by_tenant → 200 with NO counts_by_tenant (fail-closed).
        set +e
        SO=$(curl -s -o "${OWN}" -w '%{http_code}' --max-time 10 \
          -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
          -H 'Content-Type: application/json' -d "${OWN_BYTENANT_BODY}" "${AGG_URL}")
        set -e
        if [ "${SO}" = "200" ]; then
          HAS=$(jq -r '[.buckets[] | select(.counts_by_tenant != null)] | length' "${OWN}" 2>/dev/null || echo "?")
          if [ "${HAS}" = "0" ]; then
            ok "phase 173: own-scope by_tenant read carries no counts_by_tenant (non-widened ignored, fail-closed)"
          else
            fail "phase 173: own-scope by_tenant leaked counts_by_tenant on ${HAS} bucket(s)"
          fi
        else
          fail "phase 173: own-scope by_tenant aggregate expected 200, got ${SO}"
        fi

        # (3) No by_tenant → 200 with NO counts_by_tenant (backward compatible).
        set +e
        SP=$(curl -s -o "${PL}" -w '%{http_code}' --max-time 10 \
          -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
          -H 'Content-Type: application/json' -d "${PLAIN_BODY}" "${AGG_URL}")
        set -e
        if [ "${SP}" = "200" ]; then
          HASP=$(jq -r '[.buckets[] | select(.counts_by_tenant != null)] | length' "${PL}" 2>/dev/null || echo "?")
          if [ "${HASP}" = "0" ]; then
            ok "phase 173: plain aggregate (no by_tenant) carries no counts_by_tenant (backward compatible)"
          else
            fail "phase 173: plain aggregate leaked counts_by_tenant on ${HASP} bucket(s)"
          fi
        else
          fail "phase 173: plain aggregate expected 200, got ${SP}"
        fi

        # (4) Cross-tenant by_tenant: admin → 200 with reconciling attribution;
        #     non-admin → 403 at the widening gate (attribution never bypasses).
        set +e
        SC=$(curl -s -o "${CR}" -w '%{http_code}' --max-time 10 \
          -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
          -H 'Content-Type: application/json' -d "${CROSS_BYTENANT_BODY}" "${AGG_URL}")
        set -e
        case "${SC}" in
          200)
            # Σ counts_by_tenant == counts on every bucket; 0 mismatches wanted.
            MISMATCH=$(jq -r '
              [ .buckets[]
                | (.counts_by_tenant // {}) as $cbt
                | (.counts // {}) as $c
                | ( [ $cbt[] | to_entries[] ] | group_by(.key)
                    | map({key: .[0].key, value: (map(.value) | add)}) | from_entries ) as $sum
                | ( ($c | keys) + ($sum | keys) | unique )
                | map( select( ($c[.] // 0) != ($sum[.] // 0) ) )
                | length
              ] | add // 0' "${CR}" 2>/dev/null || echo "?")
            if [ "${MISMATCH}" = "0" ]; then
              ok "phase 173: admin cross-tenant by_tenant → 200, counts_by_tenant reconciles with counts (Σ invariant)"
            else
              fail "phase 173: admin cross-tenant by_tenant Σ mismatch on ${MISMATCH} (tenant,type) pair(s)"
            fi
            ;;
          403)
            ok "phase 173: non-admin cross-tenant by_tenant → 403 (widening gate fires before attribution)"
            ;;
          *)
            fail "phase 173: cross-tenant by_tenant expected 200 (admin) or 403 (non-admin), got ${SC}"
            ;;
        esac
      else
        skip "phase 173: dev bearer / jq unavailable — attribution covered by Go + integration tests"
      fi
      ;;
    *)
      fail "phase 173: events.aggregate no-bearer probe expected 401/404, got ${PROBE}"
      ;;
  esac
else
  skip "phase 173: curl not available"
fi

# ---------------------------------------------------------------------------
# Static guards (always run, never skip).
# ---------------------------------------------------------------------------

# Additive-field invariants: the opt-in request flag and the per-bucket
# attribution map, both optional (omitempty) so a caller that does not opt in
# sees a byte-identical response.
assert_grep_count 'ByTenant bool `json:"by_tenant,omitempty"`' \
  internal/protocol/types/events.go 1 \
  "phase 173: EventAggregateRequest.ByTenant is an optional opt-in flag"
assert_grep_count 'CountsByTenant map\[string\]map\[string\]int64 `json:"counts_by_tenant,omitempty"`' \
  internal/protocol/types/events.go 1 \
  "phase 173: EventBucket.CountsByTenant is the optional per-tenant attribution"

# The additive fields are mirrored into the Console per-page wire module and the
# generated manifest (the D-223 lockstep is gated separately by
# make protocol-ts-gen-check; these guards trip on a dropped hand-mirror).
assert_grep_present 'by_tenant' \
  web/console/src/lib/protocol/events.ts \
  "phase 173: Console events.ts mirrors the by_tenant wire field"
assert_grep_present 'counts_by_tenant' \
  web/console/src/lib/protocol/events.ts \
  "phase 173: Console events.ts mirrors the counts_by_tenant wire field"
assert_grep_present 'by_tenant' \
  web/console/src/lib/protocol/wire-manifest.gen.json \
  "phase 173: wire manifest carries the by_tenant field"
assert_grep_present 'counts_by_tenant' \
  web/console/src/lib/protocol/wire-manifest.gen.json \
  "phase 173: wire manifest carries the counts_by_tenant field"

smoke_summary
