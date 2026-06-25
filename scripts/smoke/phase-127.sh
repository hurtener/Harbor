#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
#
# Phase 127 smoke — Protocol wire-manifest consumability (runtime.info digest).
#
# Asserts the canonical wire-surface digest surface (RFC §5, §5.2):
#   - internal/protocol/wiresurface.Digest() exists.
#   - types.RuntimeInfo carries the additive WireSurfaceDigest field.
#   - the committed wire manifest carries a top-level wire_surface_digest
#     matching ^sha256:[0-9a-f]{64}$.
#   - the hand-maintained TS RuntimeInfo interface mirrors the field, and the
#     pure compareWireDigest comparator is exported.
#   - the manifest lockstep gate AND the generated-docs gate pass.
#   - the wiresurface unit tests + the wire-digest E2E pass under -race.
#   - live: when the dev server exposes runtime.info, the reported
#     wire_surface_digest equals the manifest's (skips per 404/405/501).
#
# Conventions (AGENTS.md §4.2): scripts/smoke/common.sh helpers; FAIL = 0.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# -----------------------------------------------------------------------------
# Static: the wiresurface package + Digest exist.
# -----------------------------------------------------------------------------
assert_file "internal/protocol/wiresurface/wiresurface.go" \
    "phase 127: wiresurface package present"
assert_grep_present "func Digest\(\)" "internal/protocol/wiresurface/wiresurface.go" \
    "phase 127: wiresurface.Digest declared"

# Static: RuntimeInfo (Go) carries the new field + json tag.
assert_grep_present 'wire_surface_digest' "internal/protocol/types/posture.go" \
    "phase 127: RuntimeInfo Go type declares wire_surface_digest"

# Static: the committed manifest carries a top-level sha256 digest (jq; skips
# if jq absent).
MANIFEST="web/console/src/lib/protocol/wire-manifest.gen.json"
DIGEST=""
if command -v jq >/dev/null 2>&1; then
    DIGEST=$(jq -r '.wire_surface_digest // empty' "$MANIFEST" 2>/dev/null || echo "")
    if printf '%s' "$DIGEST" | grep -Eq '^sha256:[0-9a-f]{64}$'; then
        ok "phase 127: manifest wire_surface_digest is a well-formed sha256 digest"
    else
        fail "phase 127: manifest wire_surface_digest missing or malformed (got '${DIGEST}')"
    fi
else
    skip "phase 127: jq not available — manifest digest shape check skipped"
fi

# Static: the hand-maintained TS RuntimeInfo mirrors the field, and the pure
# comparator is exported.
assert_grep_present 'wire_surface_digest' "web/console/src/lib/protocol/settings.ts" \
    "phase 127: TS RuntimeInfo interface declares wire_surface_digest"
assert_grep_present 'compareWireDigest' "web/console/src/lib/connection.ts" \
    "phase 127: connection.ts exports the pure wire-digest comparator"

# Single-source defence: no Protocol error Code redefined under wiresurface.
assert_grep_absent 'protoerrors\.Code(' "internal/protocol/wiresurface/wiresurface.go" \
    "phase 127: no Protocol error Code redefined under wiresurface (single-source preserved)"

# -----------------------------------------------------------------------------
# Build/test gates: both the manifest lockstep AND the generated-docs gate.
# -----------------------------------------------------------------------------
if make protocol-ts-gen-check >/dev/null 2>&1; then
    ok "phase 127: make protocol-ts-gen-check passes (manifest + RuntimeInfo field in lockstep)"
else
    fail "phase 127: make protocol-ts-gen-check failed (regenerate manifest / mirror the TS field)"
fi
if make protocol-docs-gen-check >/dev/null 2>&1; then
    ok "phase 127: make protocol-docs-gen-check passes (types.md regenerated for the new field)"
else
    fail "phase 127: make protocol-docs-gen-check failed (run make protocol-docs-gen and commit types.md)"
fi
if go test -race ./internal/protocol/wiresurface/... >/dev/null 2>&1; then
    ok "phase 127: wiresurface unit + concurrency tests pass under -race"
else
    fail "phase 127: wiresurface tests failed (go test -race ./internal/protocol/wiresurface/...)"
fi
if go test -race -run TestE2E_Phase127 ./test/integration/... >/dev/null 2>&1; then
    ok "phase 127: wire-digest E2E passes under -race (runtime.info digest == wiresurface.Digest() == manifest digest; missing-identity 401)"
else
    fail "phase 127: wire-digest E2E failed (go test -race -run TestE2E_Phase127 ./test/integration/...)"
fi

# -----------------------------------------------------------------------------
# Live (skips per 404/405/501): runtime.info over the wire returns a digest
# matching the manifest's. The dev server runs WITH the auth validator, so an
# unauthenticated call is rejected 401 — discover the dev token from the
# preflight server log (same posture as phase-72f.sh), else SKIP.
# -----------------------------------------------------------------------------
if [[ -z "${HARBOR_DEV_TOKEN:-}" ]] && [[ -n "${HARBOR_DATA_DIR:-}" ]] && [[ -f "${HARBOR_DATA_DIR}/server.log" ]]; then
    HARBOR_DEV_TOKEN="$(grep -m1 '^HARBOR_DEV_TOKEN=' "${HARBOR_DATA_DIR}/server.log" 2>/dev/null | sed 's/^HARBOR_DEV_TOKEN=//' || true)"
fi

live_digest_probe() {
    local desc="phase 127: live runtime.info wire_surface_digest matches the manifest"
    local url
    url="$(api_url '/v1/control/runtime.info')"
    if ! command -v curl >/dev/null 2>&1; then
        skip "${desc}: curl not available"
        return
    fi
    if [[ -z "${DIGEST}" ]]; then
        skip "${desc}: manifest digest not extracted (jq absent) — cannot compare"
        return
    fi
    # An empty body — the posture handler backfills identity from the JWT.
    local resp code payload
    resp=$(curl -s --max-time 5 \
        -X POST -H 'Content-Type: application/json' \
        ${HARBOR_DEV_TOKEN:+-H "Authorization: Bearer ${HARBOR_DEV_TOKEN}"} \
        --data '{}' -w $'\n%{http_code}' "${url}" 2>/dev/null) || resp=$'\n000'
    code="${resp##*$'\n'}"
    payload="${resp%$'\n'*}"
    case "${code}" in
        404|405|501|000|"")
            skip "${desc}: ${code:-000} (route not wired into this build)"
            return
            ;;
        401)
            skip "${desc}: 401 — HARBOR_DEV_TOKEN not discoverable; authenticated path covered by the E2E test"
            return
            ;;
    esac
    if [[ "${code}" != "200" ]]; then
        fail "${desc}: HTTP ${code} (${url})"
        return
    fi
    if ! command -v jq >/dev/null 2>&1; then
        skip "${desc}: jq not available — cannot extract the live digest"
        return
    fi
    local live
    live=$(printf '%s' "${payload}" | jq -r '.wire_surface_digest // empty' 2>/dev/null || echo "")
    if [[ "${live}" == "${DIGEST}" ]]; then
        ok "${desc}: ${live}"
    else
        fail "${desc}: live='${live}' manifest='${DIGEST}'"
    fi
}

live_digest_probe

smoke_summary
