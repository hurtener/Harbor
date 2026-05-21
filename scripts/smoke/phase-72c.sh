#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 72c (D-108) — search.* cluster (5 methods, one phase).
#
# This smoke runs the per-package conformance + integration tests under
# the race detector. The live HTTP server that mounts the search
# transport (auth middleware + control transport with WithSearchSurface)
# is part of `harbor dev`'s wiring, which lands in a future stage of
# Wave 13; until then the smoke pins the surface via `go test -race`
# against the real drivers, exactly as Phase 61 / Phase 62 do for the
# auth + conformance surfaces.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

SEARCH_PKG="internal/search"
PROTO_PKG="internal/protocol"
TRANSPORT_PKG="internal/protocol/transports/control"
TYPES_PKG="internal/protocol/types"
METHODS_PKG="internal/protocol/methods"
INTEGRATION_PKG="test/integration"

# ----------------------------------------------------------------------------
# 1. Per-package conformance + concurrent-reuse + identity-isolation tests.
# ----------------------------------------------------------------------------

if go test -race -count=1 -timeout 240s ./${SEARCH_PKG}/... >/dev/null 2>&1; then
    ok 'phase 72c: internal/search/... tests pass under -race (aggregate + 4 per-index Searchers + D-025 + identity isolation)'
else
    fail 'phase 72c: internal/search tests failed (run `go test -race ./internal/search/...` for detail)'
fi

# ----------------------------------------------------------------------------
# 2. Protocol-side dispatcher + wire-shape round-trip + handler error map.
# ----------------------------------------------------------------------------

if go test -race -count=1 -timeout 180s -run 'TestSearch' ./${PROTO_PKG}/ >/dev/null 2>&1; then
    ok 'phase 72c: internal/protocol search-surface tests pass under -race (5 methods + cross-tenant CodeScopeMismatch)'
else
    fail 'phase 72c: protocol search-surface tests failed (run `go test -race -run TestSearch ./internal/protocol/...` for detail)'
fi

if go test -race -count=1 -timeout 180s -run 'TestSearchHandler' ./${TRANSPORT_PKG}/ >/dev/null 2>&1; then
    ok 'phase 72c: control transport search_handler tests pass under -race (HTTP 200 / 400 / 401 / 403 / 404 path map)'
else
    fail 'phase 72c: control transport search-handler tests failed (run `go test -race -run TestSearchHandler ./internal/protocol/transports/control/...` for detail)'
fi

# ----------------------------------------------------------------------------
# 3. Wire-type single-source guards (CLAUDE.md §8 + the §13 single-source
#    rule). The protocol/methods exhaustiveness test and the wire-type
#    round-trip test must keep passing.
# ----------------------------------------------------------------------------

if go test -race -count=1 -timeout 60s ./${TYPES_PKG}/ ./${METHODS_PKG}/ >/dev/null 2>&1; then
    ok 'phase 72c: types + methods exhaustiveness tests pass under -race (the five search.* method constants are in lockstep)'
else
    fail 'phase 72c: types/methods exhaustiveness failed — search.* constants drifted'
fi

# ----------------------------------------------------------------------------
# 4. §17.1 integration test — real sessions + tasks + events +
#    artifacts + Protocol transport, cross-tenant isolation, identity-
#    mandatory rejection, heavy-payload ArtifactRef bypass.
# ----------------------------------------------------------------------------

if go test -race -count=1 -timeout 240s -run 'TestE2E_SearchCluster' ./${INTEGRATION_PKG}/ >/dev/null 2>&1; then
    ok 'phase 72c: search_cluster integration test passes under -race (5 methods round-trip + identity-mandatory + cross-tenant 403 + heavy-payload Ref bypass + N=16 concurrency stress)'
else
    fail 'phase 72c: search_cluster integration test failed (run `go test -race -run TestE2E_SearchCluster ./test/integration/...` for detail)'
fi

# ----------------------------------------------------------------------------
# 5. Static guards — surface existence + single-source preservation.
# ----------------------------------------------------------------------------

for sym in 'MethodSearchQuery' 'MethodSearchSessions' 'MethodSearchTasks' 'MethodSearchEvents' 'MethodSearchArtifacts'; do
    if grep -q "${sym}" "${METHODS_PKG}/methods.go" 2>/dev/null; then
        ok "phase 72c: ${METHODS_PKG} declares ${sym} (single-source preserved)"
    else
        fail "phase 72c: ${METHODS_PKG} missing ${sym}"
    fi
done

for sym in 'SearchRequest' 'SearchResponse' 'SearchResultRow' 'SearchFilter' 'SearchFacet' 'SearchArtifactRef'; do
    if grep -q "type ${sym}" "${TYPES_PKG}/search.go" 2>/dev/null; then
        ok "phase 72c: ${TYPES_PKG}/search.go declares ${sym}"
    else
        fail "phase 72c: ${TYPES_PKG}/search.go missing ${sym}"
    fi
done

if grep -q 'type SearchSurface' "${PROTO_PKG}/search.go" 2>/dev/null; then
    ok "phase 72c: ${PROTO_PKG}/search.go declares SearchSurface (Protocol-side dispatcher)"
else
    fail "phase 72c: ${PROTO_PKG}/search.go missing SearchSurface"
fi

if grep -q 'WithSearchSurface' "${TRANSPORT_PKG}/control.go" 2>/dev/null; then
    ok "phase 72c: ${TRANSPORT_PKG}/control.go declares WithSearchSurface (handler integration seam)"
else
    fail "phase 72c: ${TRANSPORT_PKG}/control.go missing WithSearchSurface"
fi

# Single-source preservation — no Protocol error Code constant constructed
# under the search subsystem (CLAUDE.md §8).
if grep -rIn --include='*.go' 'protoerrors\.Code(' "${SEARCH_PKG}/" 2>/dev/null | grep -v '_test.go' | grep -q .; then
    fail 'phase 72c: a Protocol error Code is constructed under internal/search — codes are single-sourced in internal/protocol/errors'
else
    ok 'phase 72c: no Protocol error Code redefined under internal/search (single-source preserved)'
fi

# ----------------------------------------------------------------------------
# 6. Live-server probes — `harbor dev` mounts the five `search.*` methods
#    via transports.WithSearch (D-132 §17.5 F1 fix). The probe POSTs an
#    empty SearchRequest body; the merged auth middleware backfills the
#    identity triple from the verified JWT. A 200 confirms the surface is
#    live; a 404/405/501 means a build without the surface (SKIP); a 401
#    with no discoverable token SKIPs (the authenticated happy path is
#    covered by the integration test).
# ----------------------------------------------------------------------------

SEARCH_BODY='{}'

probe_search_method() {
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
        --data "${SEARCH_BODY}" \
        "${url}" 2>/dev/null) || actual="000"

    case "${actual}" in
        404|405|501|000|000000|"")
            skip "${desc}: ${actual:-000} (Phase 72c search surface not yet wired into this build)"
            return
            ;;
        401)
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

probe_search_method 'search.query'     'phase 72c: search.query responds 200'
probe_search_method 'search.sessions'  'phase 72c: search.sessions responds 200'
probe_search_method 'search.tasks'     'phase 72c: search.tasks responds 200'
probe_search_method 'search.events'    'phase 72c: search.events responds 200'
probe_search_method 'search.artifacts' 'phase 72c: search.artifacts responds 200'

smoke_summary
