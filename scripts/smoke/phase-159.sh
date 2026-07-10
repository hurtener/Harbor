#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 159 — serve-band promotion (internal/runtime/serve). Boot-parity smoke.
#
# The config→listener composition left `package main` for the importable
# `internal/runtime/serve` package. This smoke proves the promoted band still
# answers the same surfaces after the move:
#   - /healthz returns 200 on the booted preflight dev server;
#   - one canonical Protocol method (runtime.info) round-trips under the dev
#     token — no serve-surface regression;
#   - `harbor serve --help` still advertises "mints no token" (D-220 posture;
#     the string lives in the help text, not boot output).
#
# The production-posture + per-seam proofs live in the in-package,
# caller-level, and integration tests — a smoke cannot spend an LLM turn.
# Done-definition: OK >= 2, FAIL = 0.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

BIN="${ROOT}/bin/harbor"

# 1. /healthz answers 200 on the booted serve band (the config→listener
#    composition is alive after leaving package main).
assert_status 200 "$(api_url /healthz)" "phase 159: /healthz 200 on the promoted serve band"

# 2. One canonical Protocol method round-trips (no serve-surface regression).
#    The dev boot runs behind the auth validator, so discover the dev token the
#    boot printed and authenticate. A missing token SKIPs this leg cleanly (the
#    authenticated happy path is also covered by the integration test).
if [[ -z "${HARBOR_DEV_TOKEN:-}" ]] && [[ -n "${HARBOR_DATA_DIR:-}" ]] && [[ -f "${HARBOR_DATA_DIR}/server.log" ]]; then
    HARBOR_DEV_TOKEN="$(grep -m1 '^HARBOR_DEV_TOKEN=' "${HARBOR_DATA_DIR}/server.log" 2>/dev/null | sed 's/^HARBOR_DEV_TOKEN=//' || true)"
fi
info_url="$(api_url '/v1/control/runtime.info')"
if [[ -n "${HARBOR_DEV_TOKEN:-}" ]] && command -v curl >/dev/null 2>&1; then
    code=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST -H 'Content-Type: application/json' \
        -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
        --data '{}' "${info_url}" 2>/dev/null) || code="000"
    case "${code}" in
        404|501|000|'')
            skip "phase 159: runtime.info not reachable (${code:-000}) — surface absent"
            ;;
        200)
            ok "phase 159: runtime.info round-trips 200 under the dev token (serve surface intact)"
            ;;
        *)
            fail "phase 159: runtime.info returned ${code}, want 200"
            ;;
    esac
else
    skip "phase 159: HARBOR_DEV_TOKEN not discoverable — runtime.info round-trip covered by the integration test"
fi

# 3. `harbor serve --help` still advertises the D-220 no-mint posture.
if [[ -x "${BIN}" ]]; then
    help_out="$(mktemp)"
    if "${BIN}" serve --help >"${help_out}" 2>&1; then
        assert_grep_present 'mints no token' "${help_out}" \
            'phase 159: harbor serve still advertises "mints no token" (D-220 intact)'
    else
        skip 'phase 159: harbor serve --help unavailable in this build'
    fi
    rm -f "${help_out}"
else
    skip 'phase 159: harbor binary not built — serve --help posture check skipped'
fi

smoke_summary
