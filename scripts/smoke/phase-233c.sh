#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file "docs/plans/phase-233c-bifrost-reasoning-fidelity.md" "phase 233c plan exists"
assert_grep_present 'HA-51' "docs/plans/phase-233c-bifrost-reasoning-fidelity.md" "HA-51 is planned"
assert_grep_present 'D-402' "docs/decisions.md" "D-402 is recorded"
assert_grep_present 'byte-exact concatenation' "docs/plans/phase-233c-bifrost-reasoning-fidelity.md" "raw source bytes are authoritative"
assert_grep_present 'decoded JSON/SSE' "docs/plans/phase-233c-bifrost-reasoning-fidelity.md" "decoder regression is binding"
assert_grep_present 'exact bytes `0x0a,0x0a`' "docs/plans/phase-233c-bifrost-reasoning-fidelity.md" "decoded newlines are real bytes"
assert_grep_present 'choice index 0 only' "docs/plans/phase-233c-bifrost-reasoning-fidelity.md" "stream and unary use one choice policy"
assert_grep_present 'callback sequence is.*false.*true' "docs/plans/phase-233c-bifrost-reasoning-fidelity.md" "empty raw delta still receives terminal callback"
assert_grep_present 'durable `state.history`' "docs/plans/phase-233c-bifrost-reasoning-fidelity.md" "restart oracle is durable"
assert_grep_present 'No Protocol method, wire type' "docs/plans/phase-233c-bifrost-reasoning-fidelity.md" "scope stays zero-wire"
assert_file "internal/llm/drivers/bifrost/custom_provider_wire_test.go" "phase 233c decoded-wire regression exists"
assert_file "internal/llm/drivers/bifrost/reasoning_test.go" "phase 233c shared-driver regression exists"
assert_file "test/integration/phase233c_reasoning_fidelity_test.go" "phase 233c downstream integration regression exists"
assert_grep_present 'preserves newline-bearing reasoning bytes between live projection and durable reopen' \
    "web/console/src/lib/sessions/tests/history.spec.ts" \
    "Console live and durable history preserve exact newline bytes"

P233C_TMP="$(mktemp -d "${TMPDIR:-/tmp}/harbor-phase-233c.XXXXXX")"
trap 'rm -rf "${P233C_TMP}"' EXIT

assert_go_tests_pass "${P233C_TMP}/go-test.log" \
    '-race -count=1 ./internal/llm/drivers/bifrost ./test/integration' \
    'phase 233c: decoded wire, shared-driver isolation, and durable restart parity execute under race' \
    TestE2E_CustomProvider_StreamReasoningByteFidelity \
    TestReasoningCapture_ConcurrentReuseNoBleedOrLeak \
    TestE2E_Phase233c_ReasoningFidelity_DurableRestart
smoke_summary
