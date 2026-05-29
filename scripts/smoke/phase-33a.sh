#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 33a smoke — custom OpenAI-compatible providers + per-provider
# timeouts (extension of Phase 33's bifrost driver).
#
# Phase 33a extends the bifrost driver's Account so operators can wire
# NIM, vLLM, ollama, lm-studio, or any other OpenAI-compatible
# endpoint via `harbor.yaml` without per-provider Go code. Adds
# first-class per-provider Timeout / MaxRetries / RetryBackoff* /
# Concurrency / BufferSize knobs (NIM cold-start latency is the
# motivating case).
#
# There is no HTTP / Protocol surface here either (lands in Phase 60+).
# This smoke runs the bifrost driver's full test suite + extends the
# Phase 33 static guard to confirm the custom-provider files don't
# leak provider-native tool-calling symbols.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Run the bifrost-driver tests under -race. Phase 33a adds
# custom_provider_test.go (config-mapping) + custom_provider_wire_test.go
# (httptest-based wire-level: happy / timeout / 5xx) plus a D-025
# concurrent-reuse stress for the mixed-config path.
if go test -race -count=1 -timeout 120s ./internal/llm/drivers/bifrost/... >/dev/null 2>&1; then
    ok 'phase 33a: internal/llm/drivers/bifrost tests pass under -race (custom-provider config + wire happy/timeout/5xx + D-025 mixed-config)'
else
    fail 'phase 33a: bifrost driver tests failed (run `go test -race ./internal/llm/drivers/bifrost/...` for detail)'
fi

# Static guard: same Phase 33 boundary applies to the Phase 33a files.
# Phase 107c / D-167 reversed the brief-07 stance — `ToolChoice` +
# `Tools` are now Harbor's own surface. Only the legacy provider-private
# discriminators stay forbidden here.
if grep -rIn --include='*.go' --exclude='*_test.go' -E '\b(FunctionCall|ToolUse|ToolCallSpec)\b' internal/llm/drivers/bifrost/ 2>/dev/null | grep -q .; then
    fail 'phase 33a: legacy provider-native tool-calling symbol leak detected in internal/llm/drivers/bifrost/ (extension of Phase 33 guard)'
else
    ok 'phase 33a: no legacy provider-native tool-calling symbol leak in internal/llm/drivers/bifrost/ (107c-revised boundary preserved)'
fi

# Config-layer validation: custom-provider validator rejects malformed
# entries at boot. We exercise this via the config package test suite
# (which the full preflight already runs); this smoke confirms the
# package builds under -race.
if go test -race -count=1 -timeout 60s ./internal/config/... >/dev/null 2>&1; then
    ok 'phase 33a: internal/config tests pass under -race (custom-provider + network-defaults validation)'
else
    fail 'phase 33a: internal/config tests failed (run `go test -race ./internal/config/...` for detail)'
fi

skip "phase 33a: bifrost driver has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
