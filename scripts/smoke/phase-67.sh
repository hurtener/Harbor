#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 67 smoke — `harbor scaffold` (RFC §8; master-plan Phase 67
# detail block; D-087).
#
# Phase 67 replaces the Phase 63 `harbor scaffold` stub with the real
# subcommand that materialises a new Harbor agent project skeleton from
# an embedded template (default `minimal-react`). The scaffolded
# harbor.yaml passes `internal/config.Load + Validate` with zero
# further edits — the in-PR stand-in for `harbor validate`
# (sibling-shipping in Phase 68; CLI integration lands in Phase 68's
# PR per CLAUDE.md §17.6).
#
# Assertions (in order):
#   1. cmd/harbor + cmd/harbor/scaffold tests pass under -race.
#   2. The scaffold engine's config-validate test (the in-PR stand-in
#      for `harbor validate`) is green.
#   3. ./bin/harbor scaffold --json round-trips the success shape.
#   4. Every expected file is present in the scaffolded tree.
#   5. A second scaffold against the same dir exits non-zero with
#      .code == "output_dir_exists".
#   6. The scaffolded project actually builds end-to-end:
#      `go mod tidy && go build ./...` succeeds after appending a real
#      `replace github.com/hurtener/Harbor => ${ROOT}` directive. Catches
#      regressions where the template's go.mod or imports drift away
#      from a buildable shape (Wave 11 §17.5 audit, finding F3).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

BIN="${ROOT}/bin/harbor"

# 1. Package tests under -race.
if go test -race -count=1 -timeout 120s ./cmd/harbor/... ./cmd/harbor/scaffold/... >/dev/null 2>&1; then
    ok 'phase 67: cmd/harbor + cmd/harbor/scaffold tests pass under -race (cobra-driver + engine-level + golden + negative)'
else
    fail 'phase 67: cmd/harbor or cmd/harbor/scaffold tests failed (run: go test -race ./cmd/harbor/... ./cmd/harbor/scaffold/...)'
fi

# 2. The in-PR stand-in for `harbor validate` — the engine's
# RenderedConfig_PassesConfigValidate test wires the scaffolded
# harbor.yaml against the real internal/config package.
if go test -race -count=1 -timeout 60s -run 'TestScaffold_RenderedConfig_PassesConfigValidate' ./cmd/harbor/scaffold/... >/dev/null 2>&1; then
    ok 'phase 67: scaffolded harbor.yaml validates via internal/config.Load + Validate (stand-in for `harbor validate`, Phase 68 sibling-shipping)'
else
    fail 'phase 67: scaffolded harbor.yaml does NOT validate (run: go test -run TestScaffold_RenderedConfig_PassesConfigValidate ./cmd/harbor/scaffold/...)'
fi

# 3-5. Built-binary checks. Preflight builds bin/harbor; if it is absent,
# SKIP cleanly per the 404/405/501 -> SKIP convention.
if [[ ! -x "${BIN}" ]]; then
    skip 'phase 67: bin/harbor not built (preflight build step skipped)'
    smoke_summary
    exit 0
fi

TMPDIR="$(mktemp -d -t harbor-phase67-XXXXXX)"
trap 'rm -rf "${TMPDIR}"' EXIT

OUT="${TMPDIR}/smoke-agent"

# 3. `harbor scaffold --json` happy path.
JSON_BODY="$("${BIN}" scaffold --name smoke-agent --output "${OUT}" --json 2>&1)" || {
    fail "phase 67: harbor scaffold --json exited non-zero: ${JSON_BODY}"
    smoke_summary
    exit 1
}
if command -v jq >/dev/null 2>&1; then
    name_field="$(printf '%s' "${JSON_BODY}" | jq -r '.name // empty' 2>/dev/null || echo '')"
    output_field="$(printf '%s' "${JSON_BODY}" | jq -r '.output_dir // empty' 2>/dev/null || echo '')"
    files_count="$(printf '%s' "${JSON_BODY}" | jq -r '.files | length' 2>/dev/null || echo '0')"
    if [[ "${name_field}" == "smoke-agent" && -n "${output_field}" && "${files_count}" -ge 5 ]]; then
        ok "phase 67: harbor scaffold --json emits {name, output_dir, files[>=5]} (got files=${files_count})"
    else
        fail "phase 67: harbor scaffold --json shape malformed (name='${name_field}' output_dir='${output_field}' files_count='${files_count}')"
    fi
else
    # Fallback substring check.
    if printf '%s' "${JSON_BODY}" | grep -q '"name":"smoke-agent"' && \
       printf '%s' "${JSON_BODY}" | grep -q '"files":\['; then
        ok 'phase 67: harbor scaffold --json contains name + files (substring check; jq absent)'
    else
        fail "phase 67: harbor scaffold --json malformed: ${JSON_BODY}"
    fi
fi

# 4. Files exist.
expected_files=(README.md agent.go agent_test.go go.mod harbor.yaml)
missing=0
for f in "${expected_files[@]}"; do
    if [[ ! -f "${OUT}/${f}" ]]; then
        fail "phase 67: scaffolded tree missing ${f}"
        missing=$((missing + 1))
    fi
done
if [[ "${missing}" -eq 0 ]]; then
    ok "phase 67: scaffolded tree contains every expected file (${#expected_files[@]} files)"
fi

# 5. A second scaffold against the same dir exits non-zero with
# .code == "output_dir_exists".
SECOND_BODY="$("${BIN}" scaffold --name smoke-agent --output "${OUT}" --json 2>&1 || true)"
if "${BIN}" scaffold --name smoke-agent --output "${OUT}" --json >/dev/null 2>&1; then
    fail 'phase 67: second scaffold against pre-existing dir exited 0 (must fail loud — §13)'
elif command -v jq >/dev/null 2>&1; then
    code_field="$(printf '%s' "${SECOND_BODY}" | jq -r '.code // empty' 2>/dev/null || echo '')"
    if [[ "${code_field}" == "output_dir_exists" ]]; then
        ok 'phase 67: second scaffold against pre-existing dir emits .code == "output_dir_exists"'
    else
        fail "phase 67: second scaffold .code expected 'output_dir_exists', got '${code_field}' (body: ${SECOND_BODY})"
    fi
else
    if printf '%s' "${SECOND_BODY}" | grep -q '"code":"output_dir_exists"'; then
        ok 'phase 67: second scaffold against pre-existing dir emits output_dir_exists code (substring check)'
    else
        fail "phase 67: second scaffold malformed: ${SECOND_BODY}"
    fi
fi

# 6. Scaffolded project actually builds (catches go.mod / import / template
# drift that the in-tree tests miss — Wave 11 §17.5 audit, finding F3).
# Scaffolds a fresh project into a temp dir, appends a real `replace`
# directive pointing at the in-tree Harbor checkout, then runs
# `go mod tidy && go build ./...`.
BUILD_OUT="${TMPDIR}/build-test-agent"
if ! "${BIN}" scaffold --name build-test-agent --output "${BUILD_OUT}" >/dev/null 2>&1; then
    fail 'phase 67: scaffold for build-check failed'
else
    {
        printf '\n'
        printf 'replace github.com/hurtener/Harbor => %s\n' "${ROOT}"
    } >> "${BUILD_OUT}/go.mod"
    build_log=$(mktemp)
    if (cd "${BUILD_OUT}" && go mod tidy && go build ./...) >"${build_log}" 2>&1; then
        ok 'phase 67: scaffolded project builds end-to-end (go mod tidy + go build ./... against in-tree Harbor)'
        rm -f "${build_log}"
    else
        fail 'phase 67: scaffolded project does NOT build (template/import/go.mod regression — see tail below)'
        echo "    --- go mod tidy / go build output (tail 40 lines) ---"
        tail -40 "${build_log}" | sed 's/^/    /'
        echo "    --- end ---"
        rm -f "${build_log}"
    fi
fi

smoke_summary
