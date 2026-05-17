#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 34 smoke — provider correction layer + SchemaSanitizer
# (RFC §6.5; master plan Phase 34 detail block).
#
# Phase 34 ships the per-provider correction layer that sits between
# the runtime and the Phase 32 `safetyClient(driver)`. Compose order
# (settled by D-NNN this PR): `corrections(safetyClient(driver))` —
# the safety pass sees the POST-correction request so leak-detection
# and the token-budget guard apply to the final outgoing payload.
#
# Five quirks (brief 03 §4, master plan):
#   1. Message reordering (NIM)
#   2. Schema sanitization (additionalProperties / strict)
#   3. Reasoning-effort routing (thinking-class models)
#   4. Response-format envelope translation (JSON-only / Anthropic)
#   5. Usage backfill (proxies reporting 0/0)
#
# There is no HTTP / Protocol surface yet (lands in Phase 60+).
# Smoke skips the surface stub at the end.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# Run the corrections package tests under -race. Covers the five
# quirks, the D-025 concurrent-reuse stress, and the registry-path
# composition test.
if go test -race -count=1 -timeout 120s ./internal/llm/corrections/... >/dev/null 2>&1; then
    ok 'phase 34: internal/llm/corrections tests pass under -race (5 quirks + D-025 + registry composition)'
else
    fail 'phase 34: corrections tests failed (run `go test -race ./internal/llm/corrections/...` for detail)'
fi

# Static guard — extend Phase 32 / 33's no-tools-symbol grep to the
# corrections path. RFC §6.4 / brief 07: Harbor's runtime owns tool
# dispatch — provider-native `Tools` / `ToolChoice` / `FunctionCall` /
# `ToolUse` types MUST NOT appear in `internal/llm/corrections/`.
# The corrections layer is structured-output + message-shape ONLY.
if grep -rIn --include='*.go' --exclude='*_test.go' -E '\b(ToolChoice|FunctionCall|ToolUse|ToolCallSpec)\b' internal/llm/corrections/ 2>/dev/null | grep -q .; then
    fail 'phase 34: provider-native tool-calling symbol leak detected in internal/llm/corrections/ (RFC §6.4 boundary violation)'
else
    ok 'phase 34: no provider-native tool-calling symbol leak in internal/llm/corrections/ (RFC §6.4 boundary preserved)'
fi

# Composition invariant — Phase 35's downgrade chain has not yet
# shipped. The corrections layer translates `response_format` shape
# but does NOT downgrade `FormatJSONSchema` → `FormatJSONObject` →
# text on provider failure. That's Phase 35's surface.
skip "phase 34: structured-output downgrade chain ships in Phase 35 (corrections layer translates shape only)"
skip "phase 34: corrections layer has no HTTP/Protocol surface yet (lands in Phase 60+)"

smoke_summary
