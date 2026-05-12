#!/usr/bin/env bash
# Phase 35 smoke — structured-output strategies + downgrade chain
# (RFC §6.5; master plan Phase 35 detail block; D-043).
#
# Phase 35 ships the `OutputMode = Native | Tools | Prompted` enum +
# the downgrade chain `Native → Prompted → Text` on
# `IsInvalidJSONSchemaError` failures. The wrapper composes OUTSIDE
# corrections + safety:
#
#   Open() → retry(downgrade(corrections(safety(driver))))
#
# Each downgrade emits `llm.mode_downgraded` with identity quadruple
# + From/To/Reason; exhaustion surfaces `ErrDowngradeExhausted`.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Run the output package tests under -race. Covers the per-mode happy
# paths, the downgrade chain (Native → Prompted → text), exhaustion,
# non-schema-error termination, the D-025 stress, and the
# integration test wiring the full chain.
if go test -race -count=1 -timeout 120s ./internal/llm/output/... >/dev/null 2>&1; then
    ok 'phase 35: internal/llm/output tests pass under -race (per-mode shaping + downgrade chain + D-025 + integration)'
else
    fail 'phase 35: output tests failed (run `go test -race ./internal/llm/output/...` for detail)'
fi

# Static guard — extend the no-provider-native-tool-call symbol scan
# to the output path. `OutputMode.Tools` is a HARBOR-SIDE prompted-
# output strategy, NOT a passthrough to provider tool-calling APIs.
if grep -rIn --include='*.go' --exclude='*_test.go' -E '\b(ToolChoice|FunctionCall|ToolUse|ToolCallSpec)\b' internal/llm/output/ 2>/dev/null | grep -q .; then
    fail 'phase 35: provider-native tool-calling symbol leak detected in internal/llm/output/ (RFC §6.4 boundary violation)'
else
    ok 'phase 35: no provider-native tool-calling symbol leak in internal/llm/output/ (RFC §6.4 boundary preserved)'
fi

# Event-registry assertion — `llm.mode_downgraded` is registered in
# `internal/llm/events.go::init`. A simple grep is sufficient; the
# event-bus tests prove it survives registration.
if grep -q 'EventTypeModeDowngraded' internal/llm/events.go 2>/dev/null; then
    ok 'phase 35: llm.mode_downgraded event type registered'
else
    fail 'phase 35: llm.mode_downgraded event type missing from internal/llm/events.go'
fi

# There is no HTTP / Protocol surface yet.
skip "phase 35: output downgrade chain has no HTTP/Protocol surface yet (lands in Phase 60+)"
skip "phase 35: live-LLM downgrade smoke runs in the wave-end E2E (HARBOR_LIVE_LLM=1)"

smoke_summary
