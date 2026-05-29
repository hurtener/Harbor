#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 33 smoke — bifrost integration.
#
# Phase 33 ships the bifrost-backed LLM driver: a thin adapter that
# translates Harbor's CompleteRequest ↔ bifrost's BifrostChatRequest,
# multimodal ContentPart → bifrost ChatContentBlock (D-021), cost
# passthrough → llm.cost.recorded emit (Phase 36a subscribes), and
# ctx-cancellation hygiene on streams (brief 08 §"Cancellation
# caveat"). Self-registers under "bifrost"; blank-imported in
# cmd/harbor/main.go.
#
# The live six-provider conformance test in the package is gated
# behind HARBOR_LIVE_LLM=1 (and skips when OPENROUTER_API_KEY is
# missing). CI default skips it; the wave-end E2E exercises ONE
# provider against the real key.
#
# There is no HTTP / Protocol surface yet (lands in Phase 60+).
# Smoke skips the surface stub at the end.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Run the bifrost-driver tests under -race. The live-conformance
# test self-skips without HARBOR_LIVE_LLM=1, so CI default is fast.
if go test -race -count=1 -timeout 120s ./internal/llm/drivers/bifrost/... >/dev/null 2>&1; then
    ok 'phase 33: internal/llm/drivers/bifrost tests pass under -race (translate + driver + account + D-025 concurrent + cost emit)'
else
    fail 'phase 33: bifrost driver tests failed (run `go test -race ./internal/llm/drivers/bifrost/...` for detail)'
fi

# Static guard: extend Phase 32's no-tools-symbol grep to the bifrost
# driver path. Phase 107c / D-167 reversed the brief-07 stance for the
# React planner; bifrost now translates Harbor's `Tools[]` +
# `ToolChoice` surface into the per-provider wire shape. The legacy
# provider-private discriminators (`FunctionCall`, `ToolUse`,
# `ToolCallSpec`) remain forbidden — bifrost reaches them via its own
# `ChatTool*` types instead.
if grep -rIn --include='*.go' --exclude='*_test.go' -E '\b(FunctionCall|ToolUse|ToolCallSpec)\b' internal/llm/drivers/bifrost/ 2>/dev/null | grep -q .; then
    fail 'phase 33: legacy provider-native tool-calling symbol leak detected in internal/llm/drivers/bifrost/ (use bifrost ChatTool types instead)'
else
    ok 'phase 33: no legacy provider-native tool-calling symbol leak in internal/llm/drivers/bifrost/ (107c surface uses bifrost ChatTool* types)'
fi

# Document the live-conformance gate so operators know how to
# exercise the six-provider matrix locally.
if [[ "${HARBOR_LIVE_LLM:-}" == "1" ]]; then
    ok 'phase 33: HARBOR_LIVE_LLM=1 detected — live six-provider conformance test executes (see internal/llm/drivers/bifrost/conformance_test.go)'
else
    skip 'phase 33: live six-provider conformance test gated behind HARBOR_LIVE_LLM=1 (set in env + OPENROUTER_API_KEY to run locally; wave-end E2E exercises one provider)'
fi

skip "phase 33: bifrost driver has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
