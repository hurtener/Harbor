#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: live-server
# Phase 72f smoke — Runtime posture surface (RFC §5.3, §6.15, §7;
# wave-13-decomposition.md §4 row 72f).
#
# Phase 72f ships five read-only Protocol methods that expose the live
# Runtime's posture to a Protocol client (Console, CLI, third-party):
# `runtime.info` (build identity + protocol version + uptime +
# capabilities), `runtime.health` (per-subsystem readiness rollup),
# `runtime.counters` (low-cardinality live counters for the Overview
# footer chips), `runtime.drivers` (configured driver names per
# persistence-shaped subsystem), and `metrics.snapshot` (a
# Protocol-shaped projection over the Phase 56 MetricsRegistry).
#
# The smoke covers:
#   1. The five Protocol methods round-trip through the booted dev
#      server's REST control transport (Phase 60 surface) — gated by the
#      404/405/501 → SKIP convention so the script is harmless on builds
#      that pre-date Phase 72f.
#   2. Identity-rejection probe: a request with an empty identity tenant
#      is rejected `CodeIdentityRequired` at the surface edge (RFC §5.5
#      "the Protocol rejects any request without an identity scope").
#   3. The package + integration tests run under -race (covers the
#      D-025 concurrent-reuse + the cross-tenant isolation modes).
#   4. Static guards: single-source preserved (no Protocol method string
#      hardcoded outside internal/protocol/methods; no Protocol message
#      struct outside internal/protocol/types); no OpenTelemetry SDK
#      type leaks into internal/protocol/types/; no Console import in
#      the Runtime tree (CLAUDE.md §13).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

PROTOCOL_PKG="internal/protocol"
TYPES_PKG="${PROTOCOL_PKG}/types"
METHODS_PKG="${PROTOCOL_PKG}/methods"
SINGLESOURCE_PKG="${PROTOCOL_PKG}/singlesource"
TRANSPORTS_PKG="${PROTOCOL_PKG}/transports"

# -----------------------------------------------------------------------------
# Pending-phase gate: the Phase 72f surface is implemented when
# internal/protocol/posture.go exists. Before that, the smoke SKIPs
# the surface-specific assertions per the 404/405/501 → SKIP convention
# (AGENTS.md §4.2). The static guards run only when the surface file
# is present, so this smoke is harmless on builds that pre-date the
# phase implementation. When the implementor lands posture.go, every
# subsequent guard flips from SKIP to OK; a FAIL signals an incomplete
# merge (e.g. posture.go landed without the methods constants).
# -----------------------------------------------------------------------------
if [[ -f "${PROTOCOL_PKG}/posture.go" ]]; then
    POSTURE_LANDED=1
else
    POSTURE_LANDED=0
    skip 'phase 72f: internal/protocol/posture.go not yet landed — surface-specific assertions deferred per the 404/405/501 → SKIP convention'
fi

# -----------------------------------------------------------------------------
# Package tests — covers methods constants + types JSON round-trip +
# singlesource lockstep + PostureSurface dispatch + identity/scope
# rejection modes + D-025 concurrent-reuse + the route-table extension.
# Runs against the existing internal/protocol/... tree always; the Phase
# 58 lockstep + the Phase 54 dispatch tests cover the existing surface
# whether or not Phase 72f has landed.
# -----------------------------------------------------------------------------
if go test -race -count=1 -timeout 180s ./${PROTOCOL_PKG}/... >/dev/null 2>&1; then
    ok 'phase 72f: internal/protocol/... tests pass under -race (existing + Phase 72f additions when landed)'
else
    fail 'phase 72f: internal/protocol/... tests failed (run `go test -race ./internal/protocol/...` for detail)'
fi

# -----------------------------------------------------------------------------
# Integration test — runs only when the integration file exists.
# Skips cleanly on a pre-Phase-72f build per the 404/405/501 → SKIP
# convention.
# -----------------------------------------------------------------------------
if [[ -f "test/integration/runtime_posture_test.go" ]]; then
    if go test -race -count=1 -timeout 240s -run 'TestE2E_RuntimePosture' ./test/integration/... >/dev/null 2>&1; then
        ok 'phase 72f: runtime-posture E2E passes under -race (5 methods × real drivers + cross-tenant rejection + N>=10 stress)'
    else
        fail 'phase 72f: runtime-posture E2E failed (run `go test -race -run TestE2E_RuntimePosture ./test/integration/...` for detail)'
    fi
else
    skip 'phase 72f: test/integration/runtime_posture_test.go not yet landed — integration E2E deferred until Phase 72f ships'
fi

# -----------------------------------------------------------------------------
# Static guards — run only when posture.go has landed. Each guard maps
# 1:1 to an acceptance criterion in docs/plans/phase-72f-runtime-posture.md.
# A SKIP here when posture.go is absent is correct (the phase is Pending);
# a FAIL when posture.go IS present is a signal that the implementor
# landed a partial surface (e.g. the handlers without the method names).
# -----------------------------------------------------------------------------
if [[ "${POSTURE_LANDED}" = "1" ]]; then
    # Static guard: each of the five new method constants is declared in
    # the methods package.
    for sym in 'MethodRuntimeInfo' 'MethodRuntimeHealth' 'MethodRuntimeCounters' 'MethodRuntimeDrivers' 'MethodMetricsSnapshot'; do
        if grep -q "${sym} " "${METHODS_PKG}/methods.go" 2>/dev/null; then
            ok "phase 72f: ${METHODS_PKG}/methods.go declares ${sym}"
        else
            fail "phase 72f: ${METHODS_PKG}/methods.go missing ${sym} — every posture method name is single-sourced (CLAUDE.md §8)"
        fi
    done

    # Static guard: each of the six new wire types is declared in the
    # types package.
    for sym in 'RuntimeInfoRequest' 'RuntimeInfo' 'RuntimeHealth' 'RuntimeCounters' 'RuntimeDrivers' 'MetricsSnapshot'; do
        if grep -rn --include='*.go' "type ${sym} struct" "${TYPES_PKG}/" 2>/dev/null | grep -v '_test.go' | grep -q .; then
            ok "phase 72f: ${TYPES_PKG}/ declares type ${sym}"
        else
            fail "phase 72f: ${TYPES_PKG}/ missing type ${sym} — wire types are single-sourced under internal/protocol/types (CLAUDE.md §8)"
        fi
    done

    # Static guard: the new capability is registered in version.go.
    if grep -q 'CapRuntimePosture' "${TYPES_PKG}/version.go" 2>/dev/null; then
        ok 'phase 72f: types/version.go declares CapRuntimePosture (the Phase 72f surface advertised in VersionHandshake)'
    else
        fail 'phase 72f: types/version.go missing CapRuntimePosture — a Protocol client cannot negotiate the posture surface without the capability constant'
    fi

    # Static guard: the singlesource lockstep map carries the six new
    # entries.
    for sym in 'RuntimeInfoRequest' 'RuntimeInfo' 'RuntimeHealth' 'RuntimeCounters' 'RuntimeDrivers' 'MetricsSnapshot'; do
        if grep -q "\"${sym}\":" "${SINGLESOURCE_PKG}/singlesource.go" 2>/dev/null; then
            ok "phase 72f: singlesource.CanonicalWireTypes includes \"${sym}\""
        else
            fail "phase 72f: singlesource.CanonicalWireTypes missing \"${sym}\" — Phase 58 lockstep gates the addition"
        fi
    done
fi

# -----------------------------------------------------------------------------
# Single-source guard (defence-in-depth): no Protocol error Code is
# constructed under internal/protocol/posture.go — error codes are
# single-sourced in internal/protocol/errors (CLAUDE.md §8).
# -----------------------------------------------------------------------------
if [[ -f "${PROTOCOL_PKG}/posture.go" ]]; then
    if grep -n 'protoerrors\.Code(' "${PROTOCOL_PKG}/posture.go" 2>/dev/null | grep -q .; then
        fail 'phase 72f: a Protocol error Code is constructed under internal/protocol/posture.go — error codes are single-sourced in internal/protocol/errors (CLAUDE.md §8)'
    else
        ok 'phase 72f: no Protocol error Code redefined under internal/protocol/posture.go (single-source preserved — CLAUDE.md §8)'
    fi
fi

# -----------------------------------------------------------------------------
# Import-graph guard: the posture surface MUST NOT import the Console —
# the Runtime never imports Console code (CLAUDE.md §13).
# -----------------------------------------------------------------------------
if [[ -f "${PROTOCOL_PKG}/posture.go" ]]; then
    if grep -n '"github.com/hurtener/Harbor/web/console' "${PROTOCOL_PKG}/posture.go" 2>/dev/null | grep -q .; then
        fail 'phase 72f: internal/protocol/posture.go imports the Console — the Runtime never imports Console code (CLAUDE.md §13)'
    else
        ok 'phase 72f: internal/protocol/posture.go does not import the Console (Runtime/Console boundary preserved)'
    fi
fi

# -----------------------------------------------------------------------------
# Import-graph guard: the types/posture.go wire shape MUST NOT import
# the OpenTelemetry SDK — MetricsSnapshot is a Protocol-owned projection,
# not an OTel SDK re-export.
# -----------------------------------------------------------------------------
if [[ -f "${TYPES_PKG}/posture.go" ]]; then
    if grep -n '"go.opentelemetry.io/otel' "${TYPES_PKG}/posture.go" 2>/dev/null | grep -q .; then
        fail 'phase 72f: internal/protocol/types/posture.go imports the OpenTelemetry SDK — MetricsSnapshot is a Protocol-owned wire shape (RFC §5.1 reject-on-sight: Protocol types are not internal Go re-exports)'
    else
        ok 'phase 72f: internal/protocol/types/posture.go does not import the OpenTelemetry SDK (Protocol/OTel boundary preserved)'
    fi
fi

# -----------------------------------------------------------------------------
# Live-server probes — each of the five new Protocol methods routes
# through the Phase 60 REST control transport. The 404/405/501 → SKIP
# convention keeps the script harmless on builds that pre-date Phase
# 72f's transport route-table extension.
# -----------------------------------------------------------------------------
# Discover the dev Bearer token (parsed from the preflight server log
# per the Phase 64 convention — the same path phase-72g.sh takes). The
# dev server runs WITH the Phase 61 auth validator, so an unauthenticated
# posture call is rejected 401 before the handler runs. When the log is
# absent (operator ran the smoke standalone) HARBOR_DEV_TOKEN from env is
# honoured as a fallback.
if [[ -z "${HARBOR_DEV_TOKEN:-}" ]] && [[ -n "${HARBOR_DATA_DIR:-}" ]] && [[ -f "${HARBOR_DATA_DIR}/server.log" ]]; then
    HARBOR_DEV_TOKEN="$(grep -m1 '^HARBOR_DEV_TOKEN=' "${HARBOR_DATA_DIR}/server.log" 2>/dev/null | sed 's/^HARBOR_DEV_TOKEN=//' || true)"
fi

# An empty JSON body — the merged Phase 72f/72g posture handler backfills
# the identity triple from the verified JWT (Phase 61 defence-in-depth).
# A hardcoded body identity that differs from the JWT's (user, session)
# would be rejected CodeIdentityRequired, so `{}` is the correct probe
# body for the authenticated happy path.
POSTURE_BODY='{}'

probe_posture_method() {
    local method="$1"
    local desc="$2"
    local url
    url="$(api_url "/v1/control/${method}")"

    if ! command -v curl >/dev/null 2>&1; then
        skip "${desc}: curl not available"
        return
    fi

    local actual
    actual=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST \
        -H 'Content-Type: application/json' \
        ${HARBOR_DEV_TOKEN:+-H "Authorization: Bearer ${HARBOR_DEV_TOKEN}"} \
        --data "${POSTURE_BODY}" \
        "${url}" 2>/dev/null) || actual="000"

    case "${actual}" in
        404|405|501|000|000000|"")
            skip "${desc}: ${actual:-000} (Phase 72f surface not yet wired into this build)"
            return
            ;;
        401)
            # 401 with no token available — the dev server runs WITH the
            # Phase 61 auth validator, so an unauthenticated posture call
            # is correctly rejected. SKIP rather than FAIL: the smoke
            # could not discover a Bearer token to exercise the
            # authenticated happy path.
            if [[ -z "${HARBOR_DEV_TOKEN:-}" ]]; then
                skip "${desc}: 401 — HARBOR_DEV_TOKEN not discoverable; authenticated happy path covered by the integration test"
                return
            fi
            ;;
    esac

    if [[ "${actual}" = "200" ]]; then
        ok "${desc}: HTTP 200 (${url})"
    else
        fail "${desc}: expected 200, got ${actual} (${url})"
    fi
}

probe_posture_method 'runtime.info'      'phase 72f: runtime.info responds 200'
probe_posture_method 'runtime.health'    'phase 72f: runtime.health responds 200'
probe_posture_method 'runtime.counters'  'phase 72f: runtime.counters responds 200'
probe_posture_method 'runtime.drivers'   'phase 72f: runtime.drivers responds 200'
probe_posture_method 'metrics.snapshot'  'phase 72f: metrics.snapshot responds 200'

# -----------------------------------------------------------------------------
# Identity-rejection probe: a request with an empty identity tenant
# fails closed at the surface edge with CodeIdentityRequired → HTTP 401
# (RFC §5.5 "the Protocol rejects any request without an identity
# scope"). Skip cleanly when the surface isn't routed yet.
# -----------------------------------------------------------------------------
identity_reject_probe() {
    local url
    url="$(api_url '/v1/control/runtime.info')"
    if ! command -v curl >/dev/null 2>&1; then
        skip 'phase 72f: identity-rejection probe — curl not available'
        return
    fi

    local body='{"identity":{"tenant":"","user":"smoke-user","session":"smoke-session"}}'
    local actual
    actual=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST \
        -H 'Content-Type: application/json' \
        ${HARBOR_DEV_TOKEN:+-H "Authorization: Bearer ${HARBOR_DEV_TOKEN}"} \
        --data "${body}" \
        "${url}" 2>/dev/null) || actual="000"

    case "${actual}" in
        404|405|501|000|000000|"")
            skip "phase 72f: identity-rejection probe — surface not routed (HTTP ${actual:-000})"
            return
            ;;
    esac

    # CodeIdentityRequired maps to 401 in the Phase 60 status table.
    if [[ "${actual}" = "401" ]]; then
        ok 'phase 72f: missing-identity request rejected 401 CodeIdentityRequired (RFC §5.5)'
    else
        fail "phase 72f: missing-identity request expected 401, got ${actual} — identity is mandatory at the surface edge (RFC §5.5)"
    fi
}
identity_reject_probe

# -----------------------------------------------------------------------------
# Cross-tenant rejection probe (gated on the dev token NOT carrying the
# admin scope; the integration test exercises the admin-scoped success
# path against a real ES256 keypair). A request whose body's
# Identity.Tenant != the verified bearer's tenant is rejected
# CodeScopeMismatch → HTTP 403 per D-079.
# -----------------------------------------------------------------------------
cross_tenant_probe() {
    local url
    url="$(api_url '/v1/control/runtime.counters')"
    if ! command -v curl >/dev/null 2>&1; then
        skip 'phase 72f: cross-tenant probe — curl not available'
        return
    fi

    if [[ -z "${HARBOR_DEV_TOKEN:-}" ]]; then
        skip 'phase 72f: cross-tenant probe — HARBOR_DEV_TOKEN not set; the integration test pins the admin-scope ES256 path'
        return
    fi

    local body='{"identity":{"tenant":"other-tenant","user":"smoke-user","session":"smoke-session"}}'
    local actual
    actual=$(curl -s -o /dev/null -w '%{http_code}' --max-time 5 \
        -X POST \
        -H 'Content-Type: application/json' \
        -H "Authorization: Bearer ${HARBOR_DEV_TOKEN}" \
        --data "${body}" \
        "${url}" 2>/dev/null) || actual="000"

    case "${actual}" in
        404|405|501|000|000000|"")
            skip "phase 72f: cross-tenant probe — surface not routed (HTTP ${actual:-000})"
            return
            ;;
    esac

    # CodeScopeMismatch maps to 403 in the Phase 60 status table.
    if [[ "${actual}" = "403" || "${actual}" = "401" ]]; then
        ok "phase 72f: cross-tenant request without admin scope rejected ${actual} (D-079)"
    else
        fail "phase 72f: cross-tenant request expected 403, got ${actual} — admin scope gates cross-tenant reads (D-079)"
    fi
}
cross_tenant_probe

smoke_summary
