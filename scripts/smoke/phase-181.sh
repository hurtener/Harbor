#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# TUI terminal foundation and visual system.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_file "internal/tui/ui/ui.go" "semantic terminal design system exists"
assert_file "internal/tui/app/model.go" "Bubble Tea root model exists"
assert_file "internal/tui/testdata/golden/capture-manifest.json" "reference measurements and candidate capture manifest exists"
assert_file "test/integration/tui_terminal_pty_test.go" "real PTY lifecycle integration exists"

assert_grep_present 'ActionBreakpoint = 80' "internal/tui/ui/ui.go" "79/80 action transition is pinned"
assert_grep_present 'SidebarBreakpoint = 120' "internal/tui/ui/ui.go" "120/121 sidebar transition is pinned"
assert_grep_present 'SidebarWidth = 42' "internal/tui/ui/ui.go" "fixed sidebar width is pinned"
assert_grep_present 'LayerBase, LayerSidebar, LayerAutocomplete, LayerToast, LayerModal, LayerStartup' "internal/tui/app/model.go" "root layer order is pinned"

assert_grep_present '"phase_181": true' \
    "internal/tui/testdata/golden/capture-manifest.json" \
    "phase 181 capture review remains recorded while later candidates await review"
assert_grep_present 'TestCaptureMatrix_AllApplicableFoundationStates' \
    "internal/tui/app/golden_test.go" \
    "complete fixture-state capture matrix is mechanically generated"

if go test -race -count=1 ./internal/tui/ui ./internal/tui/app ./cmd/harbor; then
    ok "tokens, geometry, commands, focus, CLI honesty, and goldens pass under race"
else
    fail "terminal foundation race tests failed"
fi

if go test -race -count=1 -run '^TestE2E_TUITerminal' ./test/integration; then
    ok "real PTY resize, suspend/resume, signals, panic, and restoration pass"
else
    fail "real PTY lifecycle gate failed"
fi

binary=$(mktemp "${TMPDIR:-/tmp}/harbor-tui-cgofree.XXXXXX")
if CGO_ENABLED=0 go build -o "${binary}" ./cmd/harbor; then
    ok "CGo-free harbor binary builds with the terminal stack"
else
    fail "CGo-free harbor binary build failed"
fi
rm -f "${binary}"

for dependency in \
    'charm.land/bubbletea/v2 v2.0.8' \
    'charm.land/lipgloss/v2 v2.0.5' \
    'charm.land/bubbles/v2 v2.1.1' \
    'github.com/charmbracelet/x/ansi v0.11.7'; do
    if grep -q "${dependency}" go.mod; then
        ok "exact dependency pin: ${dependency}"
    else
        fail "missing exact dependency pin: ${dependency}"
    fi
done

smoke_summary
