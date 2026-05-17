#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 36b smoke — token-bucket rate limiter + per-call MaxTokens
# (RFC §6.15, master plan Phase 36b detail block, D-044).
#
# Phase 36b builds on Phase 36a's Subsystem scaffolding. Per-(identity,
# model) token bucket; bucket state persists across runtime restart
# (three-driver conformance with in-mem / SQLite / Postgres state).
# Per-call MaxTokens fails loud with ErrMaxTokensExceeded.
# Latent default: TierConfig.RateLimit.Capacity == 0 and
# TierConfig.MaxTokens == 0 → permits every call.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Re-run the governance suite — Phase 36a and 36b share the package, so
# the same go test invocation covers both phases' acceptance tests.
if go test -race -count=1 -timeout 180s ./internal/governance/... >/dev/null 2>&1; then
    ok 'phase 36b: internal/governance tests pass under -race (RateLimiter + MaxTokensEnforcer + bucket persistence + D-025)'
else
    fail 'phase 36b: governance tests failed (run `go test -race ./internal/governance/...` for detail)'
fi

# Static guard — provider-native tool-calling symbols must not leak.
if grep -rIn --include='*.go' --exclude='*_test.go' -E '\b(ToolChoice|FunctionCall|ToolUse|ToolCallSpec)\b' internal/governance/ 2>/dev/null | grep -q .; then
    fail 'phase 36b: provider-native tool-calling symbol leak in internal/governance/'
else
    ok 'phase 36b: no provider-native tool-calling symbol leak in internal/governance/'
fi

# Phase 36b event-registry assertions.
if grep -q 'EventTypeRateLimited' internal/governance/events.go 2>/dev/null && \
   grep -q 'EventTypeMaxTokensExceeded' internal/governance/events.go 2>/dev/null; then
    ok 'phase 36b: governance.rate_limited + governance.maxtokens_exceeded event types registered'
else
    fail 'phase 36b: governance.rate_limited or governance.maxtokens_exceeded missing from internal/governance/events.go'
fi

skip "phase 36b: governance subsystem has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
