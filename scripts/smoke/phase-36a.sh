#!/usr/bin/env bash
# Phase 36a smoke — governance Subsystem + cost accumulator (RFC §6.15,
# master plan Phase 36a detail block, D-044).
#
# Phase 36a establishes the governance Subsystem interface (PreCall /
# PostCall), Wrap() composing OUTSIDE the rest of the chain, and the
# CostAccumulator with operator-configured per-tier ceilings. Ships
# LATENT — empty identity_tiers → permits every call.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Run the governance package tests under -race. Covers Subsystem
# interface contract + Wrap composition + cost accumulator math + ceiling
# enforcement + identity isolation + StateStore restart survival +
# concurrency + D-025.
if go test -race -count=1 -timeout 180s ./internal/governance/... >/dev/null 2>&1; then
    ok 'phase 36a: internal/governance tests pass under -race (Subsystem + Wrap + CostAccumulator + concurrency + D-025)'
else
    fail 'phase 36a: governance tests failed (run `go test -race ./internal/governance/...` for detail)'
fi

# Static guard — provider-native tool-calling symbols must not leak into
# governance (governance composes OUTSIDE the LLM chain; same boundary
# as RFC §6.4 elsewhere).
if grep -rIn --include='*.go' --exclude='*_test.go' -E '\b(ToolChoice|FunctionCall|ToolUse|ToolCallSpec)\b' internal/governance/ 2>/dev/null | grep -q .; then
    fail 'phase 36a: provider-native tool-calling symbol leak detected in internal/governance/ (RFC §6.4 boundary violation)'
else
    ok 'phase 36a: no provider-native tool-calling symbol leak in internal/governance/ (RFC §6.4 boundary preserved)'
fi

# Event-registry assertion — the budget event type must be registered.
if grep -q 'EventTypeBudgetExceeded' internal/governance/events.go 2>/dev/null; then
    ok 'phase 36a: governance.budget_exceeded event type registered'
else
    fail 'phase 36a: governance.budget_exceeded event type missing from internal/governance/events.go'
fi

# Wrapper-hook assertion — the governance package must seat the
# RegisterGovernanceWrapper hook on internal/llm at import time. Grep
# is sufficient — the production binary blank-imports the package, and
# the hook seats during init(); a typo in the hook name surfaces here.
if grep -q 'RegisterGovernanceWrapper' internal/llm/registry.go && \
   grep -q 'RegisterGovernanceWrapper' internal/governance/registry.go; then
    ok 'phase 36a: governance wrapper hook registered (llm.RegisterGovernanceWrapper → internal/governance.init)'
else
    fail 'phase 36a: governance wrapper hook missing (expected llm.RegisterGovernanceWrapper installation in internal/governance)'
fi

skip "phase 36a: governance subsystem has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
