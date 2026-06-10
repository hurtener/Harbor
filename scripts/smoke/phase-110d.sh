#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 110d smoke — assembly promotion (D-197).
# (RFC §6.4 / §6.13 / §9 / §10; docs/plans/phase-110d-assembly-promotion.md)
#
# Static + unit-test assertions:
#   1. The promoted assembly entry point + the acceptance-gated recipe
#      exist; the assemble package imports no `testing`.
#   2. The two duplicated fan-out bodies are GONE: devstack no longer
#      defines `tryAssemble` / `attachDevStackMCPServer`; cmd_dev.go no
#      longer defines `attachDevMCPServer` / `applyToolCatalogWiring` /
#      `projectMCPToolPolicies`. Both callers wrap assemble.Assemble.
#   3. The promoted helpers exist at their owning packages: the MCP
#      attach (incl. the ToolPolicy projection), auth.BuildProviders,
#      events.OpenWith + RegisterWithDeps (+ the durable driver's
#      deps-aware registration).
#   4. The recipe references the real exported symbols (compile-by-
#      construction; its code path runs in test/integration).
#   5. The focused unit slices pass under -race.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# --- 1. The promoted entry point + recipe exist ----------------------------

assert_file internal/runtime/assemble/assemble.go     'phase 110d: assemble.Assemble entry point file'
assert_file docs/recipes/embed-harbor-headless.md     'phase 110d: the acceptance-gated headless recipe'
assert_file test/integration/phase110d_assemble_test.go 'phase 110d: the recipe-path E2E + concurrency stress'

assert_grep_present 'func Assemble\(ctx context\.Context, cfg \*config\.Config, opts Options\) \(\*Stack, error\)' \
    internal/runtime/assemble/assemble.go 'phase 110d: error-returning Assemble signature'
assert_grep_absent '"testing"' internal/runtime/assemble/assemble.go \
    'phase 110d: the assembly is testing-free (not a test fixture wearing assembly clothes)'

# --- 2. The duplicated fan-out bodies are deleted ---------------------------

assert_grep_absent 'func tryAssemble'             harbortest/devstack/devstack.go 'phase 110d: devstack tryAssemble deleted'
assert_grep_absent 'func attachDevStackMCPServer' harbortest/devstack/devstack.go 'phase 110d: devstack MCP-attach mirror deleted'
assert_grep_absent 'func attachDevMCPServer'      cmd/harbor/cmd_dev.go 'phase 110d: cmd attachDevMCPServer deleted'
assert_grep_absent 'func applyToolCatalogWiring'  cmd/harbor/cmd_dev.go 'phase 110d: cmd applyToolCatalogWiring deleted'
assert_grep_absent 'func projectMCPToolPolicies'  cmd/harbor/cmd_dev.go 'phase 110d: cmd policy-projection helper promoted to mcpdrv'
assert_grep_present 'assemble\.Assemble\(' cmd/harbor/cmd_dev.go 'phase 110d: bootDevStack wraps assemble.Assemble'
assert_grep_present 'assemble\.Assemble\(' harbortest/devstack/devstack.go 'phase 110d: devstack.Assemble wraps assemble.Assemble'

# --- 3. The promoted helpers exist at their owning packages ----------------

assert_file internal/tools/drivers/mcp/attach.go 'phase 110d: MCP attach helper next to the driver'
assert_grep_present 'func Attach\(ctx context\.Context, ms config\.MCPServerConfig, deps AttachDeps\) error' \
    internal/tools/drivers/mcp/attach.go 'phase 110d: mcpdrv.Attach exported'
assert_grep_present 'func ProjectToolPolicies\(ms config\.MCPServerConfig\)' \
    internal/tools/drivers/mcp/attach.go 'phase 110d: the config→ToolPolicy projection rides with the driver'

assert_file internal/tools/auth/build_providers.go 'phase 110d: auth.BuildProviders file'
assert_grep_present 'func BuildProviders\(ctx context\.Context, cfg config\.ToolsConfig, deps BuildDeps\)' \
    internal/tools/auth/build_providers.go 'phase 110d: auth.BuildProviders exported'

assert_file internal/events/openwith.go 'phase 110d: events.OpenWith file'
assert_grep_present 'func OpenWith\(_ context\.Context, cfg config\.EventsConfig, r audit\.Redactor, deps Deps\)' \
    internal/events/openwith.go 'phase 110d: events.OpenWith exported'
assert_grep_present 'func RegisterWithDeps\(' internal/events/openwith.go 'phase 110d: deps-aware registration seam'
assert_grep_present 'events\.RegisterWithDeps\("durable"' internal/events/drivers/durable/durable.go \
    'phase 110d: the durable driver registers the deps-aware factory (StateStore sharing)'

# --- 4. The recipe is honest: every snippet symbol is real -----------------

assert_grep_present 'config\.Defaults\(\)'             docs/recipes/embed-harbor-headless.md 'phase 110d: recipe uses config.Defaults'
assert_grep_present 'ValidateCore\(\)'                 docs/recipes/embed-harbor-headless.md 'phase 110d: recipe uses ValidateCore'
# Phase 112b (D-206) flipped the recipe to the facade twin
# sdk/drivers/prod (identical registration set by construction).
assert_grep_present 'sdk/drivers/prod'                 docs/recipes/embed-harbor-headless.md 'phase 110d: recipe imports the production driver aggregator (sdk facade twin since 112b)'
assert_grep_present 'assemble\.Assemble\(ctx, cfg'     docs/recipes/embed-harbor-headless.md 'phase 110d: recipe calls assemble.Assemble'
assert_grep_present 'stack\.RunLoop\.Run\(ctx, steering\.RunSpec' docs/recipes/embed-harbor-headless.md 'phase 110d: recipe drives the run loop'
assert_grep_present 'planner\.AnswerEnvelope'          docs/recipes/embed-harbor-headless.md 'phase 110d: recipe reads the AnswerEnvelope'
assert_grep_present 'embed-harbor-headless'            docs/recipes/README.md 'phase 110d: recipe indexed'

# --- 5. The focused unit slices pass under -race ----------------------------

if go test ./internal/runtime/assemble/ ./internal/tools/auth/ ./internal/events/ ./internal/events/drivers/durable/ \
    -run 'Assemble|BuildProviders|OpenWith|RegisterWithDeps' -race -count=1 -timeout 300s >/dev/null 2>&1; then
    ok 'phase 110d: assemble/BuildProviders/OpenWith unit slices pass under -race'
else
    fail 'phase 110d: promoted-surface unit slices failed (run the go test line in scripts/smoke/phase-110d.sh for detail)'
fi

smoke_summary
