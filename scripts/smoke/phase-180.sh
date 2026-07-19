#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 180 — TUI projection and reconciliation core (D-316).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file "internal/tui/projection/projection.go" "pure projection package exists"
assert_file "internal/tui/testdata/projection/corpus.json" "language-neutral fixture corpus exists"
assert_file "web/console/src/lib/sessions/tests/projection-corpus.spec.ts" "Console production reducers consume the shared corpus"
assert_file "test/integration/tui_projection_test.go" "authenticated real-driver projection integration exists"

if go list -deps ./internal/tui/projection | grep -Eq 'bubbletea|lipgloss'; then
    fail "projection imports a terminal framework"
else
    ok "projection has no Bubble Tea or Lip Gloss dependency"
fi

if go test -race -count=1 ./internal/tui/projection; then
    ok "projection reconciliation, shared fixtures, and concurrent reuse pass under race"
else
    fail "projection race tests failed"
fi

if go test -race -count=1 -run '^TestE2E_TUIProjection_' ./test/integration; then
    ok "authenticated in-memory driver integration passes under race"
else
    fail "authenticated projection integration failed"
fi

# Guard on installed node deps: the preflight job runs `make preflight` with
# no `npm ci`, so vitest is absent there. SKIP per the tool-absent convention
# (CLAUDE.md §4.2) — the frontend CI job runs this same vitest corpus for real.
if [ ! -d 'web/console/node_modules/vitest' ]; then
    skip "Console shared projection fixtures: vitest not installed (frontend CI job covers it)"
elif (cd web/console && npm test -- --run src/lib/sessions/tests/projection-corpus.spec.ts); then
    ok "Console consumes the shared fixture corpus with equivalent output"
else
    fail "Console shared projection fixtures failed"
fi

assert_grep_present 'default:' "internal/tui/projection/projection.go" \
    "every unrecognized canonical event reaches the safe generic block path"
assert_grep_present 'ClassifyEventType' "internal/tui/projection/projection_test.go" \
    "canonical event registry classification is mechanically tested"
assert_grep_present 'future.event' "internal/tui/testdata/projection/corpus.json" \
    "fixture corpus exercises the generic event path"

smoke_summary
