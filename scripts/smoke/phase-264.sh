#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 264 smoke — optional grant-free provider route resolver.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file "docs/plans/phase-264-optional-provider-route-resolver.md" "phase 264 plan exists"
assert_grep_present '## D-444' "docs/decisions.md" "D-444 decision exists"
assert_grep_present 'type ProviderRouteResolver interface' "internal/llm/provider_route.go" "provider route seam is independent"
assert_grep_present 'ErrProviderRouteResolverUnavailable' "internal/llm/provider_route.go" "explicit missing resolver fails loud"
assert_grep_present 'ProviderRoute \*LLMProviderRouteSelector' "internal/protocol/types/control.go" "task start carries opaque route"
assert_grep_present 'WithTrustedProviderRoute' "internal/runtime/serve/runloop.go" "route context follows run admission"
assert_grep_present 'json:"-"' "internal/llm/provider_route.go" "credential is not serializable"
assert_grep_present 'func curatedRouteProvider' "internal/llm/drivers/bifrost/route_account.go" "provider selection is closed"
assert_grep_present 'TestConfigureStockProviderRoute_EmptyDoesNoEnvironmentWork' "internal/runtime/serve/provider_route_transport_test.go" "empty config is zero work"
assert_grep_present 'TestClient_ConcurrentTenantsDoNotBleedCredentials' "internal/llm/providerroute/httptransport/client_test.go" "tenant credential isolation is pinned"

smoke_summary
