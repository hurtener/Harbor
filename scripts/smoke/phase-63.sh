#!/usr/bin/env bash
# Phase 63 smoke — Harbor CLI skeleton (harbor + cobra) (RFC §8;
# master-plan Phase 63 detail block; D-084).
#
# Phase 63 turns cmd/harbor into a cobra-rooted CLI binary that registers
# the seven settled subcommands. Only "harbor version" is fully
# implemented; the other six exit non-zero with a structured CLIError
# pointing to their implementing phase (the §13 "test stubs as production
# defaults" amendment).
#
# The smoke runs against the built ./bin/harbor binary (preflight builds
# it before running this script). If the binary is absent, the assertions
# SKIP cleanly per the 404/405/501 -> SKIP convention.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

BIN="${ROOT}/bin/harbor"
GOLDEN_HELP="${ROOT}/cmd/harbor/testdata/golden/help.txt"
EXPECTED_PROTOCOL="0.1.0"

# 1. Run the cmd/harbor package tests under -race. Covers: the cobra
# command tree (NewRootCmd construction), the version subcommand
# (human + --json), each stub subcommand's structured-error shape
# (human + --json), the CLIError JSON round-trip, the --help golden.
if go test -race -count=1 -timeout 60s ./cmd/harbor/... >/dev/null 2>&1; then
    ok 'phase 63: cmd/harbor tests pass under -race (cobra root + version + stub subcommands + CLIError + golden)'
else
    fail 'phase 63: cmd/harbor tests failed (run: go test -race ./cmd/harbor/...)'
fi

# 2. Built-binary checks. Preflight builds bin/harbor; if it is absent
# (build skipped), SKIP cleanly.
if [[ ! -x "${BIN}" ]]; then
    skip 'phase 63: bin/harbor not built (preflight build step skipped)'
    smoke_summary
    exit 0
fi

# 3. "harbor --help" matches the golden file.
if [[ ! -f "${GOLDEN_HELP}" ]]; then
    fail "phase 63: golden file missing (${GOLDEN_HELP})"
else
    actual_help=$("${BIN}" --help 2>&1 || true)
    expected_help=$(cat "${GOLDEN_HELP}")
    if [[ "${actual_help}" == "${expected_help}" ]]; then
        ok 'phase 63: harbor --help matches cmd/harbor/testdata/golden/help.txt (acceptance criterion 1 — golden test)'
    else
        fail 'phase 63: harbor --help does not match the golden (regenerate with: go test -update ./cmd/harbor/)'
    fi
fi

# 4. "harbor version" human-mode shape — contains the three labels.
version_human=$("${BIN}" version 2>&1 || true)
if printf '%s' "${version_human}" | grep -q '^harbor v' && \
   printf '%s' "${version_human}" | grep -q '^protocol ' && \
   printf '%s' "${version_human}" | grep -q '^build '; then
    ok 'phase 63: harbor version prints harbor / protocol / build labels (acceptance criterion 2 — version surface)'
else
    fail 'phase 63: harbor version missing one of harbor / protocol / build labels'
fi

# 5. "harbor version --json" shape — valid JSON with the three required
# fields non-empty. Needs jq; SKIP if absent.
if command -v jq >/dev/null 2>&1; then
    version_json=$("${BIN}" version --json 2>&1 || true)
    harbor_field=$(printf '%s' "${version_json}" | jq -r '.harbor // empty' 2>/dev/null || echo '')
    protocol_field=$(printf '%s' "${version_json}" | jq -r '.protocol // empty' 2>/dev/null || echo '')
    build_field=$(printf '%s' "${version_json}" | jq -r '.build_hash // empty' 2>/dev/null || echo '')
    if [[ -n "${harbor_field}" && -n "${protocol_field}" && -n "${build_field}" ]]; then
        ok 'phase 63: harbor version --json emits non-empty harbor / protocol / build_hash fields'
    else
        fail "phase 63: harbor version --json missing field(s): harbor='${harbor_field}' protocol='${protocol_field}' build_hash='${build_field}'"
    fi
    # 5b. Protocol version matches types.ProtocolVersion (pinned 0.1.0).
    if [[ "${protocol_field}" == "${EXPECTED_PROTOCOL}" ]]; then
        ok "phase 63: harbor version --json .protocol == ${EXPECTED_PROTOCOL} (matches types.ProtocolVersion; D-077)"
    else
        fail "phase 63: harbor version --json .protocol expected ${EXPECTED_PROTOCOL}, got '${protocol_field}'"
    fi
else
    skip 'phase 63: jq not available — harbor version --json shape check skipped'
fi

# 6. Stub subcommand exit codes and structured-error shape.
# Each of these must exit non-zero with code "not_implemented" and a hint
# mentioning a phase number.
stubs=(dev scaffold validate inspect-events inspect-runs inspect-topology)
for sub in "${stubs[@]}"; do
    if "${BIN}" "${sub}" --json >/dev/null 2>&1; then
        fail "phase 63: harbor ${sub} --json exited 0 — stub subcommands MUST exit non-zero (§13 amendment)"
        continue
    fi
    # Capture stderr (cobra emits the structured error there).
    stderr_body=$("${BIN}" "${sub}" --json 2>&1 1>/dev/null || true)
    if command -v jq >/dev/null 2>&1; then
        code=$(printf '%s' "${stderr_body}" | jq -r '.code // empty' 2>/dev/null || echo '')
        hint=$(printf '%s' "${stderr_body}" | jq -r '.hint // empty' 2>/dev/null || echo '')
        if [[ "${code}" == "not_implemented" ]] && printf '%s' "${hint}" | grep -qiE 'phase [0-9]+'; then
            ok "phase 63: harbor ${sub} --json emits {code=not_implemented, hint mentions phase}"
        else
            fail "phase 63: harbor ${sub} --json structured-error malformed (code='${code}' hint='${hint}')"
        fi
    else
        # No jq — substring fallback.
        if printf '%s' "${stderr_body}" | grep -q '"code":"not_implemented"' && \
           printf '%s' "${stderr_body}" | grep -qiE 'phase [0-9]+'; then
            ok "phase 63: harbor ${sub} --json emits not_implemented code + phase hint (substring check)"
        else
            fail "phase 63: harbor ${sub} --json malformed structured error: ${stderr_body}"
        fi
    fi
done

# 7. Static guard: cobra is a direct go.mod dependency (not just indirect).
if grep -qE '^[[:space:]]*github\.com/spf13/cobra' go.mod && \
   ! grep -qE '^[[:space:]]*github\.com/spf13/cobra .* // indirect' go.mod; then
    ok 'phase 63: github.com/spf13/cobra is a direct go.mod dependency (RFC §10 settled CLI library)'
else
    fail 'phase 63: github.com/spf13/cobra must be a direct go.mod dependency (RFC §10)'
fi

# 8. Static guard: cmd/harbor does NOT import internal/protocol/errors
# for its CLI structured-error type — the CLI surface is distinct from
# the Protocol wire error surface (acceptance criterion: CLIError is
# defined in cmd/harbor/errors.go, single-source preserved).
if grep -rIn --include='*.go' '"github.com/hurtener/Harbor/internal/protocol/errors"' cmd/harbor/ 2>/dev/null | grep -q .; then
    fail 'phase 63: cmd/harbor imports internal/protocol/errors — the CLI structured error is a separate surface (operator-facing exit codes, not Protocol wire codes); CLIError lives in cmd/harbor/errors.go'
else
    ok 'phase 63: cmd/harbor does not import internal/protocol/errors (CLI structured-error surface kept distinct from Protocol wire error codes)'
fi

# Phase 63 ships no live HTTP server — harbor dev is a stub pointing to
# Phase 64. Skip the live-wire assertions per the 404/405/501 -> SKIP
# convention; the substantive surface assertions above run against the
# binary directly, no listener needed.
skip 'phase 63: the cobra skeleton ships no HTTP server; harbor dev is a non-zero-exit stub pointing to phase 64; preflight tolerates the stub via its structured-error detection'

smoke_summary
