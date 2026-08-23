#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 255 smoke — HA-71 provider-neutral descriptors and runtime-origin
# model discovery. This gate is static-only and never contacts a provider.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file "docs/plans/phase-255-provider-descriptors.md" "phase 255 plan exists"
assert_grep_present '## D-435' "docs/decisions.md" "D-435 decision exists"
assert_grep_present '## HA-71' "docs/notes/downstream-asks.md" "HA-71 is registered"
assert_file "internal/llm/provider/catalog.go" "provider-neutral catalog contract exists"
assert_file "internal/llm/provider/catalog_test.go" "catalog contract tests exist"
assert_file "internal/llm/drivers/bifrost/provider_catalog.go" "Bifrost provider adapter exists"
assert_grep_present 'ListModelsRequest' "internal/llm/drivers/bifrost/provider_catalog.go" "adapter uses Bifrost model discovery"
assert_grep_present 'ProviderExtra' "internal/llm/drivers/bifrost/provider_catalog_test.go" "opaque provider metadata redaction is tested"
assert_grep_present 'RuntimeOrigin' "internal/llm/provider/catalog.go" "runtime-origin outcome is explicit"
assert_grep_present 'SupportManual' "internal/llm/provider/catalog.go" "manual capability state is explicit"
assert_grep_present 'SupportPartial' "internal/llm/provider/catalog.go" "partial capability state is explicit"
assert_grep_present 'SupportStale' "internal/llm/provider/catalog.go" "stale capability state is explicit"
assert_grep_present 'SupportUnpriced' "internal/llm/provider/catalog.go" "unpriced capability state is explicit"
assert_grep_present 'provider_reply_malformed' "internal/llm/provider/catalog.go" "malformed provider reply fails closed"
assert_grep_present 'NewProviderCatalog' "internal/llm/drivers/bifrost/provider_catalog.go" "real Bifrost setup consumer exists"
assert_grep_present 'NewProviderCatalogWithDeps' "internal/llm/drivers/bifrost/provider_catalog.go" "runtime catalog shares the live credential seam"
assert_grep_present 'handleProviderOperation' "internal/protocol/posture.go" "protected runtime-origin Protocol projection exists"
assert_grep_present 'ProviderOperation' "internal/protocol/types/posture.go" "provider operation rides the canonical posture envelope"
assert_grep_present 'runtime_origin=false' "cmd/harbor/cmd_llm_provider.go" "offline CLI cannot claim runtime origin"
assert_grep_present 'newLLMCmd' "cmd/harbor/root.go" "provider CLI is registered"
assert_file "cmd/harbor/cmd_llm_provider.go" "provider CLI exists"
assert_grep_present 'llm providers' "README.md" "provider CLI is documented"
assert_grep_present 'HA-71' "CHANGELOG.md" "unreleased changelog names HA-71"
assert_grep_present 'Provider descriptor' "docs/glossary.md" "provider descriptor glossary term exists"

smoke_summary
