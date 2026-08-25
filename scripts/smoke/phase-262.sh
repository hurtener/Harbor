#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 262 smoke — stock coordinator-bound external-grant credentials.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file "docs/plans/phase-262-stock-external-grant-credential-resolution.md" "phase 262 plan exists"
assert_grep_present '^|262 | Stock coordinator-bound credential resolution' "docs/plans/README.md" "phase 262 canonical index row exists"
assert_grep_present '## D-442' "docs/decisions.md" "D-442 decision exists"
assert_grep_present '## HA-73' "docs/notes/downstream-asks.md" "HA-73 register entry exists"
assert_grep_present 'credential_url' "docs/CONFIG.md" "stock credential config is documented"
assert_grep_present 'MaxRequestBytes = 32 << 10' "internal/llm/credentials/contract.go" "credential request bound is explicit"
assert_grep_present 'MaxResponseBytes = 64 << 10' "internal/llm/credentials/contract.go" "credential response bound is explicit"
assert_grep_present 'UnmarshalCanonicalRequest = internal.UnmarshalCanonicalRequest' "sdk/llm/credentials/credentials.go" "public strict request parser exists"
assert_grep_present 'ParseCanonicalResponse = internal.ParseCanonicalResponse' "sdk/llm/credentials/credentials.go" "public bound response parser exists"
assert_grep_present 'VerifiedGrantContextFrom' "internal/llm/credentials/httptransport/client.go" "stock resolver requires verified context"
assert_grep_present 'maxCacheEntries = 256' "internal/llm/credentials/httptransport/client.go" "material cache is bounded"
assert_grep_present 'defaultCacheTTL = 30' "internal/llm/credentials/httptransport/client.go" "material cache TTL is bounded"
assert_grep_present 'CredentialURL' "internal/runtime/serve/serve.go" "stock serve wires credential transport"
assert_grep_present 'ExternalGrant.*llm.ExternalGrantConfig' "sdk/server/server.go" "public server injection exists"
assert_grep_present 'TestClient_ConcurrentTwoOrganizationsSameRuntimeNoBleedAndSingleflight' "internal/llm/credentials/httptransport/client_test.go" "cross-organization concurrency is covered"
assert_grep_present 'TestClient_ClosePreventsInflightCacheRepopulation' "internal/llm/credentials/httptransport/client_test.go" "close epoch is covered"
assert_grep_present 'TestOpen_RuntimeInfoCoordinatorBoundReadyOnlyWithCredentialResolver' "sdk/server/server_test.go" "real runtime-info readiness is covered"
assert_grep_present 'TestOpen_RequiredCoordinatorBoundFailsWithoutCredentialResolver' "sdk/server/server_test.go" "required stock serve fails without resolver"
assert_grep_present 'TestOpen_RequiredCoordinatorBoundAcceptsInjectedCredentialResolver' "sdk/server/server_test.go" "required public injection succeeds"

smoke_summary
