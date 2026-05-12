#!/usr/bin/env bash
# Phase 36 smoke — retry with feedback (RFC §6.5; master plan Phase
# 36 detail block; D-043).
#
# Phase 36 ships the `CompleteRequest.Validator` seam + the retry
# wrapper. The wrapper composes ABOVE downgrade:
#
#   Open() → retry(downgrade(corrections(safety(driver))))
#
# Validator failures trigger a corrective re-ask bounded by
# `ModelProfile.MaxRetries`; each retry emits
# `llm.retry_with_feedback`. Exhaustion surfaces `ErrRetryExhausted`
# with the validator-failure chain.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Run the retry package tests under -race. Covers happy path,
# single-retry-then-pass, bounded retry exhaustion, ctx-cancel,
# inner-error-no-retry, and D-025 stress.
if go test -race -count=1 -timeout 120s ./internal/llm/retry/... >/dev/null 2>&1; then
    ok 'phase 36: internal/llm/retry tests pass under -race (happy + retry-then-pass + bounded + D-025)'
else
    fail 'phase 36: retry tests failed (run `go test -race ./internal/llm/retry/...` for detail)'
fi

# Static guard — provider-native tool-calling symbols must not leak.
if grep -rIn --include='*.go' --exclude='*_test.go' -E '\b(ToolChoice|FunctionCall|ToolUse|ToolCallSpec)\b' internal/llm/retry/ 2>/dev/null | grep -q .; then
    fail 'phase 36: provider-native tool-calling symbol leak detected in internal/llm/retry/ (RFC §6.4 boundary violation)'
else
    ok 'phase 36: no provider-native tool-calling symbol leak in internal/llm/retry/ (RFC §6.4 boundary preserved)'
fi

# Event-registry assertion.
if grep -q 'EventTypeRetryWithFeedback' internal/llm/events.go 2>/dev/null; then
    ok 'phase 36: llm.retry_with_feedback event type registered'
else
    fail 'phase 36: llm.retry_with_feedback event type missing from internal/llm/events.go'
fi

skip "phase 36: retry wrapper has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
