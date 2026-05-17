#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 32 smoke — LLM client core + StreamSink contract + context safety net.
#
# Phase 32 ships the LLMClient interface, the multimodal sum-type
# Content/ContentPart/ImagePart/AudioPart/FilePart per D-021, the
# auto-materialize DataURL→ArtifactRef boundary per D-022, and the
# context-window safety net per D-026 (ErrContextLeak +
# ErrContextWindowExceeded). The §4.4 driver registry is in place; the
# mock driver self-registers under "mock". The bifrost driver lands in
# Phase 33, governance lands at Phase 36a — both are blank-import seams
# off of this package today.
#
# There is no HTTP / Protocol surface yet (lands in Phase 60+); the
# smoke skips the surface stub at the end. Test coverage runs the
# package suite (unit + safety pass + materialize + mock driver +
# D-025 concurrent-reuse) under -race.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

if go test -race -count=1 -timeout 180s ./internal/llm/... >/dev/null 2>&1; then
    ok 'phase 32: internal/llm tests pass under -race (interface + safety net + materialize + mock driver + D-025)'
else
    fail 'phase 32: internal/llm tests failed (run `go test -race ./internal/llm/...` for detail)'
fi

# Static guard: assert no provider-native tool-calling symbols leak into the
# LLM package. RFC §6.4 / brief 07: the runtime owns tool dispatch — the
# LLM client never sees Tools=, ToolChoice=, FunctionCall=, etc. Token
# `ToolName` is allowed (it identifies a Harbor tool by name in payloads
# routed to runtime/tool subsystems, not LLM-side dispatch fields).
if grep -rIn --include='*.go' --exclude='*_test.go' -E '\b(ToolChoice|FunctionCall|ToolUse|ToolCallSpec)\b' internal/llm/ 2>/dev/null | grep -q .; then
    fail 'phase 32: provider-native tool-calling symbol leak detected in internal/llm/ (RFC §6.4 boundary violation)'
else
    ok 'phase 32: no provider-native tool-calling symbol leak in internal/llm/ (RFC §6.4 boundary preserved)'
fi

skip "phase 32: LLM client has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
