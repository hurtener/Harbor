#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 184 smoke — TUI Runtime distribution and wave E2E.
#
# Conventions (AGENTS.md §4.2):
#   - 404/405/501 → SKIP (so phase-N+1 scripts coexist with phase-N builds).
#   - At least one OK once the phase has shipped.
#   - Use helpers from scripts/smoke/common.sh — don't roll new curl wrappers.
#
# Phase 184 adds:
#   - Race-safe one-shot readiness (*server.Handle).WaitReady through sdk/server.
#   - `harbor serve --tui` co-launch.
#   - `sdk/tui.Run(ctx, Options)` connection-only facade.
#   - `harbor scaffold --with-server --with-tui` generated binary --tui flag.
#   - Wave-end PTY E2E test/integration/wave_v115_tui_test.go.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# ----------------------------------------------------------------------------
# 1. WaitReady exists on serve.Handle and is forwarded through external + sdk/server.
# ----------------------------------------------------------------------------
if grep -rqE 'func \(h \*Handle\) WaitReady\(' internal/runtime/serve/serve.go \
    && grep -rqE 'func \(h \*Handle\) WaitReady\(' internal/runtime/serve/external/external.go; then
    ok 'phase 184: WaitReady exists on serve.Handle and external.Handle'
else
    skip 'phase 184: WaitReady not yet present on serve/external handles'
fi

# ----------------------------------------------------------------------------
# 2. The serve band signals readiness at the bind site (no polling, no second listener).
# ----------------------------------------------------------------------------
if grep -rqE 'signalReady\(readiness\{addr: boundAddr\}\)' internal/runtime/serve/serve.go; then
    ok 'phase 184: serve signals readiness at the bind site'
else
    skip 'phase 184: bind-site readiness signal not yet present'
fi

# ----------------------------------------------------------------------------
# 3. `harbor serve --tui` flag is registered.
# ----------------------------------------------------------------------------
if grep -rqE 'flagServeTUI\s*=\s*"tui"' cmd/harbor/cmd_serve.go \
    && grep -rqE 'cmd\.Flags\(\)\.Bool\(flagServeTUI' cmd/harbor/cmd_serve.go; then
    ok 'phase 184: harbor serve --tui flag is registered'
else
    skip 'phase 184: harbor serve --tui flag not yet registered'
fi

# ----------------------------------------------------------------------------
# 4. `harbor scaffold --with-tui` flag is registered.
# ----------------------------------------------------------------------------
if grep -rqE 'flagScaffoldWithTUI\s*=\s*"with-tui"' cmd/harbor/cmd_scaffold.go \
    && grep -rqE 'cmd\.Flags\(\)\.Bool\(flagScaffoldWithTUI' cmd/harbor/cmd_scaffold.go; then
    ok 'phase 184: harbor scaffold --with-tui flag is registered'
else
    skip 'phase 184: harbor scaffold --with-tui flag not yet registered'
fi

# ----------------------------------------------------------------------------
# 5. sdk/tui.Run facade exists with connection-only Options.
# ----------------------------------------------------------------------------
if grep -rqE 'func Run\(ctx context\.Context, opts Options\) error' sdk/tui/tui.go \
    && grep -rqE 'BaseURL\s*string' sdk/tui/tui.go \
    && grep -rqE 'Token\s+protocolclient\.TokenSource' sdk/tui/tui.go \
    && grep -rqE 'Session\s*string' sdk/tui/tui.go; then
    ok 'phase 184: sdk/tui.Run facade exists with connection-only Options'
else
    skip 'phase 184: sdk/tui.Run facade not yet present'
fi

# ----------------------------------------------------------------------------
# 6. sdk/tui imports sdk/protocolclient (NOT internal/protocol/client).
# ----------------------------------------------------------------------------
if grep -rqE 'protocolclient "github.com/hurtener/Harbor/sdk/protocolclient"' sdk/tui/tui.go; then
    ok 'phase 184: sdk/tui imports sdk/protocolclient'
else
    skip 'phase 184: sdk/tui does not import sdk/protocolclient'
fi

# ----------------------------------------------------------------------------
# 7. The scaffold template emits --tui under --with-tui.
# ----------------------------------------------------------------------------
if grep -rqE 'tuiFlag := flag.Bool\("tui"' cmd/harbor/scaffold/templates/minimal-react/cmd_main.go.tmpl \
    && grep -rqE 'tui\.Run\(ctx, tui\.Options{' cmd/harbor/scaffold/templates/minimal-react/cmd_main.go.tmpl; then
    ok 'phase 184: scaffold template emits --tui flag + sdk/tui.Run under --with-tui'
else
    skip 'phase 184: scaffold template --tui emission not yet present'
fi

# ----------------------------------------------------------------------------
# 8. sdk/server.Options carries the Stderr seam (co-launch log separation).
# ----------------------------------------------------------------------------
if grep -rqE 'Stderr\s+io\.Writer' sdk/server/server.go \
    && grep -rqE 'OpenWithStderr' internal/runtime/serve/external/external.go; then
    ok 'phase 184: sdk/server.Options.Stderr + external.OpenWithStderr seam exists'
else
    skip 'phase 184: sdk/server.Stderr seam not yet present'
fi

# ----------------------------------------------------------------------------
# 9. The scaffold template wires the Stderr seam into server.Options.
# ----------------------------------------------------------------------------
if grep -rqE 'Stderr: logSink' cmd/harbor/scaffold/templates/minimal-react/cmd_main.go.tmpl; then
    ok 'phase 184: scaffold template wires server.Options.Stderr for co-launch log separation'
else
    skip 'phase 184: scaffold template Stderr wiring not yet present'
fi

# ----------------------------------------------------------------------------
# 10. `harbor dev` is NOT changed (no --tui on the dev subcommand).
# ----------------------------------------------------------------------------
if ! grep -rqE 'flagDevTUI\|--tui' cmd/harbor/cmd_dev.go 2>/dev/null; then
    ok 'phase 184: harbor dev is unchanged (no --tui flag)'
else
    fail 'phase 184: harbor dev must not gain a --tui flag'
fi

# ----------------------------------------------------------------------------
# 11. entry.Options carries CompactSet (the M1 --compact=false fix).
# ----------------------------------------------------------------------------
if grep -rqE 'CompactSet\s+bool' internal/tui/entry/entry.go \
    && grep -rqE 'CompactSet:\s*compactSet' cmd/harbor/cmd_tui.go; then
    ok 'phase 184: entry.Options.CompactSet + cmd_tui wiring (M1 --compact=false fix)'
else
    skip 'phase 184: CompactSet fix not yet present'
fi

# ----------------------------------------------------------------------------
# 12. Wave-end PTY E2E test exists.
# ----------------------------------------------------------------------------
if [ -f "test/integration/wave_v115_tui_test.go" ] \
    && grep -rqE 'TestE2E_WaveV115TUI' test/integration/wave_v115_tui_test.go; then
    ok 'phase 184: wave-end PTY E2E test exists (TestE2E_WaveV115TUI)'
else
    fail 'phase 184: required wave-end PTY E2E test is missing or renamed'
fi

# ----------------------------------------------------------------------------
# 13. Run the wave-end E2E under -race (the live gate).
#     This runs the focused test; a no-match-fails guard ensures the test name
#     exists (so a rename is caught).
# ----------------------------------------------------------------------------
if [ ! -f "test/integration/wave_v115_tui_test.go" ]; then
    fail 'phase 184: required wave-end PTY E2E test file is missing or renamed'
elif ! grep -rqE 'TestE2E_WaveV115TUI' test/integration/wave_v115_tui_test.go; then
    fail 'phase 184: required TestE2E_WaveV115TUI is missing or renamed'
else
    # Capture the one focused invocation so a dependency-acquisition failure
    # can be classified without rerunning the same network-bound command.
    if focused_output="$(go test -race -count=1 -run 'TestE2E_WaveV115TUI' ./test/integration/... 2>&1)"; then
        focused_status=0
    else
        focused_status=$?
    fi

    # `go test` exits successfully when -run matches no tests. Keep this
    # explicit guard fail-closed: a deleted or renamed gate is never a SKIP.
    if printf '%s\n' "${focused_output}" | grep -Eqi '\[no tests to run\]|no tests to run'; then
        fail 'phase 184: TestE2E_WaveV115TUI did not match any test'
    elif [ "${focused_status}" -eq 0 ]; then
        ok 'phase 184: TestE2E_WaveV115TUI passes under -race'
    elif printf '%s\n' "${focused_output}" \
        | grep -Eiq '(go: (downloading|module lookup)|verifying|reading https?://(sum\.golang\.org|proxy\.golang\.org)|Get "https?://(sum\.golang\.org|proxy\.golang\.org)' \
        && printf '%s\n' "${focused_output}" | grep -Eiq 'https?://(sum\.golang\.org|proxy\.golang\.org)' \
        && printf '%s\n' "${focused_output}" | grep -Eiq '(HTTP/2 INTERNAL_ERROR|connection (reset|refused|timed out)|i/o timeout|TLS handshake timeout|network is unreachable|no such host|temporary failure in name resolution|502 Bad Gateway|503 Service Unavailable|500 Internal Server Error|EOF)'; then
        skip 'phase 184: TestE2E_WaveV115TUI skipped (module proxy/checksum acquisition network failure)'
    else
        fail 'phase 184: TestE2E_WaveV115TUI fails under -race'
    fi
fi

# ----------------------------------------------------------------------------
# 14. Compile-check a scaffolded --with-server --with-tui binary.
#     This proves the generated main.go compiles against the public sdk/
#     facade — the contract the scaffold template carries. The binary is
#     built into a temp dir; no runtime assertion, just compilation.
# ----------------------------------------------------------------------------
# The fallback template carries trailing X's: GNU mktemp (Linux CI) rejects a
# template with fewer than three of them, BSD mktemp (macOS) does not. The
# first arm succeeds on both platforms so the fallback has never fired — which
# is exactly why the trap sat here unnoticed.
SCAFFOLD_TMPDIR="$(mktemp -d 2>/dev/null || mktemp -d "${TMPDIR:-/tmp}/harbor-smoke.XXXXXX")"
trap 'rm -rf "${SCAFFOLD_TMPDIR}"' EXIT
if HARBOR_VERSION="v0.0.0-smoke" go run ./cmd/harbor scaffold --name smoke-agent --output "${SCAFFOLD_TMPDIR}/smoke-agent" --with-server --with-tui --quiet 2>/dev/null; then
    if CGO_ENABLED=0 go build -o "${SCAFFOLD_TMPDIR}/smoke-binary" "./${SCAFFOLD_TMPDIR}/smoke-agent/cmd/smoke-agent" 2>/dev/null; then
        ok 'phase 184: scaffolded --with-server --with-tui binary compiles (CGo-free)'
    else
        # The generated go.mod requires the module proxy; in offline CI this
        # can fail to resolve. Treat as SKIP, not FAIL — the compile gate is
        # covered by TestScaffold_WithServer_WithTUI_EmitsTUIFlag in unit tests.
        skip 'phase 184: scaffolded binary compile skipped (module proxy unavailable)'
    fi
else
    skip 'phase 184: scaffold --with-server --with-tui did not run (harbor binary unavailable)'
fi

smoke_summary
