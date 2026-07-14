#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 172 — events.aggregate origin-anchored (epoch-aligned) bucket grid
# (HA-16, D-306). An optional `anchor` on EventAggregateRequest floors bucket
# boundaries onto a fixed grid (anchor + k*Bucket); absent ⇒ today's
# clock-anchored behaviour. Additive wire field (D-223/D-209 lockstep).
#
# Live-server assertions (404/405/501 → SKIP per CLAUDE.md §4.2):
#   1. The route POST /v1/events/aggregate is mounted (no-bearer POST → 401).
#   2. Two aggregate POSTs (dev token) with the SAME epoch `anchor` + window +
#      bucket a short interval apart → both 200, and at least one shared
#      `bucket_start` instant across the two responses (the addressability
#      proof — a bucket coordinate is re-requestable).
#   3. An aggregate POST with NO `anchor` → 200 with the same bucket count as
#      the anchored call (the backward-compatible clock-anchored path).
# Static guards (always run, never skip): the additive-field invariant (the
# Anchor field is a pointer on EventAggregateRequest); the field is mirrored in
# the Console per-page wire module + the generated manifest.

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

# 1 h window / 1 min bucket, both in nanoseconds. Epoch anchor for the
# globally-shared grid.
WINDOW_NS=3600000000000
BUCKET_NS=60000000000
ANCHORED_BODY='{"window":'"${WINDOW_NS}"',"bucket":'"${BUCKET_NS}"',"anchor":"1970-01-01T00:00:00Z"}'
PLAIN_BODY='{"window":'"${WINDOW_NS}"',"bucket":'"${BUCKET_NS}"'}'

if command -v curl >/dev/null 2>&1; then
  set +e
  PROBE=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
    -X POST -H 'Content-Type: application/json' \
    -d "${ANCHORED_BODY}" "${AGG_URL}")
  set -e
  case "${PROBE:-000}" in
    404 | 405 | 501 | 000)
      skip "phase 172: /v1/events/aggregate route not present (${PROBE:-000})"
      ;;
    401)
      ok "phase 172: events.aggregate rejects identity-less anchored body (401)"

      if [ -n "${HARBOR_DEV_TOKEN:-}" ] && command -v jq >/dev/null 2>&1; then
        A1="$(mktemp)"; A2="$(mktemp)"; PL="$(mktemp)"
        trap 'rm -f "${A1}" "${A2}" "${PL}"' EXIT

        # Two anchored calls a short interval apart → both 200 with a shared
        # bucket_start (addressability).
        set +e
        S1=$(curl -s -o "${A1}" -w '%{http_code}' --max-time 10 \
          -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
          -H 'Content-Type: application/json' -d "${ANCHORED_BODY}" "${AGG_URL}")
        S2=$(curl -s -o "${A2}" -w '%{http_code}' --max-time 10 \
          -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
          -H 'Content-Type: application/json' -d "${ANCHORED_BODY}" "${AGG_URL}")
        set -e
        if [ "${S1}" = "200" ] && [ "${S2}" = "200" ]; then
          ok "phase 172: two anchored aggregate calls both return 200"
          # Bucket boundaries land on the epoch minute grid (bucket = 1 min,
          # epoch anchor → every bucket_start is minute-aligned, seconds :00).
          # Count any bucket_start that is NOT minute-aligned — want zero.
          OFFGRID=$(jq -r '[.buckets[].bucket_start | select(test(":00Z$") | not)] | length' "${A1}" 2>/dev/null || echo "?")
          if [ "${OFFGRID}" = "0" ]; then
            ok "phase 172: all anchored bucket_start values land on the epoch minute grid"
          else
            fail "phase 172: ${OFFGRID} anchored bucket_start value(s) off the epoch grid"
          fi
          # Shared coordinate: the two responses' bucket_start sets intersect.
          SHARED=$(comm -12 \
            <(jq -r '.buckets[].bucket_start' "${A1}" | sort -u) \
            <(jq -r '.buckets[].bucket_start' "${A2}" | sort -u) | wc -l | tr -d ' ')
          if [ "${SHARED:-0}" -gt 0 ]; then
            ok "phase 172: anchored calls share ${SHARED} bucket_start coordinate(s) (addressable twice)"
          else
            fail "phase 172: anchored calls shared 0 bucket_start coordinates — not addressable"
          fi
        else
          fail "phase 172: anchored aggregate calls expected 200/200, got ${S1}/${S2}"
        fi

        # Backward-compatible path: no anchor → 200 with the SAME bucket count.
        set +e
        SP=$(curl -s -o "${PL}" -w '%{http_code}' --max-time 10 \
          -X POST -H "Authorization: Bearer ${TOKEN}" "${ID_HEADERS[@]}" \
          -H 'Content-Type: application/json' -d "${PLAIN_BODY}" "${AGG_URL}")
        set -e
        if [ "${SP}" = "200" ]; then
          LP=$(jq -r '.buckets | length' "${PL}" 2>/dev/null || echo "")
          LA=$(jq -r '.buckets | length' "${A1}" 2>/dev/null || echo "")
          if [ -n "${LP}" ] && [ "${LP}" = "${LA}" ] && [ "${LP}" = "60" ]; then
            ok "phase 172: no-anchor path returns 60 buckets, matching the anchored count (backward compatible)"
          else
            fail "phase 172: no-anchor buckets=${LP}, anchored=${LA}, want both 60"
          fi
        else
          fail "phase 172: no-anchor aggregate expected 200, got ${SP}"
        fi
      else
        skip "phase 172: dev bearer / jq unavailable — anchored grid covered by Go tests"
      fi
      ;;
    *)
      fail "phase 172: events.aggregate no-bearer probe expected 401/404, got ${PROBE}"
      ;;
  esac
else
  skip "phase 172: curl not available"
fi

# ---------------------------------------------------------------------------
# Static guards (always run, never skip).
# ---------------------------------------------------------------------------

# Additive-field invariant: EventAggregateRequest gained the optional pointer
# Anchor field (json:"anchor,omitempty"). A pointer is load-bearing — a value
# type would defeat omitempty and force a non-optional TS mirror (D-306 NIT).
assert_grep_count 'Anchor \*time.Time `json:"anchor,omitempty"`' \
  internal/protocol/types/events.go 1 \
  "phase 172: EventAggregateRequest.Anchor is an optional *time.Time"

# The additive field is mirrored into the Console per-page wire module and the
# generated manifest (the D-223 lockstep is gated separately by
# make protocol-ts-gen-check; these guards trip on a dropped hand-mirror).
assert_grep_present 'anchor' \
  web/console/src/lib/protocol/events.ts \
  "phase 172: Console events.ts mirrors the anchor wire field"
assert_grep_present 'anchor' \
  web/console/src/lib/protocol/wire-manifest.gen.json \
  "phase 172: wire manifest carries the anchor field"

smoke_summary
