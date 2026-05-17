#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 68 smoke — `harbor validate` (RFC §8; master-plan Phase 68
# detail block; D-088).
#
# Phase 68 replaces the Phase 63 stub of `harbor validate` with the
# real subcommand. The smoke exercises:
#
#   1. Exit-0 on a valid config (examples/harbor.yaml).
#   2. Exit-1 on a known-bad fixture; stable golden-pinned message.
#   3. Exit-2 on file-not-found (the io.not_found category).
#   4. --json wire shape: a parseable single-line JSON body.
#   5. Cross-phase Phase 67 integration (§17.6): scaffold a project,
#      then `harbor validate` the rendered config — SKIP when the
#      `scaffold` subcommand is still a stub (Phase 67 not merged).
#
# The smoke runs against the built ./bin/harbor binary (preflight
# builds it before running this script). If the binary is absent, all
# assertions SKIP cleanly per the 404/405/501 → SKIP convention.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

BIN="${ROOT}/bin/harbor"

# 1. Run the cmd/harbor package tests under -race. Covers: every
# validate behaviour (human + --json), each error category golden,
# the field-path AST walker, the goccy line extractor, the --quiet
# flag's interaction with success vs error output.
if go test -race -count=1 -timeout 60s ./cmd/harbor/... >/dev/null 2>&1; then
    ok 'phase 68: cmd/harbor tests pass under -race (validate body + goldens + helpers)'
else
    fail 'phase 68: cmd/harbor tests failed (run: go test -race ./cmd/harbor/...)'
fi

# 2. Built-binary checks. Preflight builds bin/harbor; if it is absent
# (build skipped), SKIP cleanly.
if [[ ! -x "${BIN}" ]]; then
    skip 'phase 68: bin/harbor not built (preflight build step skipped)'
    smoke_summary
    exit 0
fi

# 3. Valid config exits 0.
if "${BIN}" validate examples/harbor.yaml >/dev/null 2>&1; then
    ok 'phase 68: harbor validate examples/harbor.yaml exits 0 (acceptance criterion — happy path)'
else
    fail 'phase 68: harbor validate examples/harbor.yaml expected exit 0; got non-zero'
fi

# 4. Invalid fixture exits 1 with the stable error body.
fixture="cmd/harbor/testdata/validate/missing-llm-provider.yaml"
if [[ ! -f "${fixture}" ]]; then
    fail "phase 68: fixture missing: ${fixture}"
else
    set +e
    body=$("${BIN}" validate "${fixture}" 2>&1)
    rc=$?
    set -e
    if [[ "${rc}" -ne 1 ]]; then
        fail "phase 68: harbor validate ${fixture} expected exit 1; got ${rc}"
    elif ! printf '%s' "${body}" | grep -q 'config.semantic'; then
        fail "phase 68: invalid fixture error body did not mention 'config.semantic': ${body}"
    elif ! printf '%s' "${body}" | grep -q 'llm.provider'; then
        fail "phase 68: invalid fixture error body did not name the offending field 'llm.provider': ${body}"
    else
        ok 'phase 68: harbor validate <invalid> exits 1 with config.semantic + llm.provider in body (acceptance criterion — stable message)'
    fi
fi

# 5. --json wire shape: validate emits a single-line JSON body that
# parses cleanly and carries .code + .errors[]. Requires jq; SKIPs if
# absent.
if command -v jq >/dev/null 2>&1; then
    set +e
    json_body=$("${BIN}" validate --json "${fixture}" 2>&1)
    set -e
    code=$(printf '%s' "${json_body}" | jq -r '.code // empty' 2>/dev/null || echo '')
    category=$(printf '%s' "${json_body}" | jq -r '.errors[0].category // empty' 2>/dev/null || echo '')
    line=$(printf '%s' "${json_body}" | jq -r '.errors[0].line // empty' 2>/dev/null || echo '')
    if [[ "${code}" == "validation_failed" && "${category}" == "config.semantic" && -n "${line}" && "${line}" -gt 0 ]]; then
        ok "phase 68: harbor validate --json emits {code=validation_failed, errors[0].category=config.semantic, errors[0].line=${line}} (acceptance criterion — --json wire shape)"
    else
        fail "phase 68: --json body malformed (code='${code}' category='${category}' line='${line}'): ${json_body}"
    fi
else
    skip 'phase 68: jq not available — --json shape check skipped'
fi

# 6. file-not-found exits 2 (io.not_found category).
set +e
"${BIN}" validate /tmp/harbor-phase68-does-not-exist.yaml >/dev/null 2>&1
rc=$?
set -e
if [[ "${rc}" -eq 2 ]]; then
    ok 'phase 68: harbor validate <nonexistent> exits 2 (io.not_found / internal-error class — acceptance criterion exit-code matrix)'
else
    fail "phase 68: harbor validate <nonexistent> expected exit 2; got ${rc}"
fi

# 7. Cross-phase Phase 67 integration (§17.6). Scaffold a project,
# then validate the rendered config. SKIPs when scaffold is still a
# stub (Phase 67 not merged) — the `not_implemented` code is the
# signal. We probe with the simplest possible invocation
# (`harbor scaffold --json my-agent`) so a stub that doesn't yet
# recognise the post-merge flags (e.g. `--output`) returns its
# `not_implemented` body cleanly. When the real scaffold lands and
# accepts `--output`, this step will need a follow-up tweak to pass
# `--output <tmpdir>` — file that as a Phase 67 implementor's
# responsibility per §17.6 ("fix what the gate finds").
scaffold_dir="/tmp/harbor-phase68-scaffold-probe"
rm -rf "${scaffold_dir}" 2>/dev/null || true
set +e
scaffold_probe=$("${BIN}" scaffold --json my-agent 2>&1)
scaffold_rc=$?
set -e
if [[ "${scaffold_rc}" -ne 0 ]] && printf '%s' "${scaffold_probe}" | grep -q '"code":"not_implemented"'; then
    skip 'phase 68: harbor scaffold is still a stub (Phase 67 not merged); cross-phase Phase 67 ↔ Phase 68 integration deferred until merge — see §17.6'
else
    # Scaffold landed. Look for the rendered config — first check the
    # cwd, then any directory the scaffold may have created.
    rendered=$(find . -maxdepth 4 -name '*.yaml' -path '*my-agent*' -type f 2>/dev/null | head -n1 || true)
    if [[ -z "${rendered}" && -d "${scaffold_dir}" ]]; then
        rendered=$(find "${scaffold_dir}" -name '*.yaml' -type f 2>/dev/null | head -n1 || true)
    fi
    if [[ -n "${rendered}" ]]; then
        if "${BIN}" validate "${rendered}" >/dev/null 2>&1; then
            ok "phase 68: cross-phase Phase 67 → 68 integration — scaffolded config passes harbor validate (rendered: ${rendered})"
        else
            fail "phase 68: scaffolded config ${rendered} did not pass harbor validate; this is a Phase 67 ↔ Phase 68 integration regression — fix in the Phase 67 PR per §17.6"
        fi
    else
        skip 'phase 68: harbor scaffold accepted the invocation but no rendered config was found — cross-phase check inconclusive (Phase 67 implementor: extend this probe in your PR per §17.6)'
    fi
    # Cleanup — be conservative; only delete the probe dir we created.
    rm -rf "${scaffold_dir}" 2>/dev/null || true
    # If the scaffold dropped files into cwd, leave them — the smoke
    # is run inside the repo; the user shouldn't lose work to our
    # cleanup. A future scaffold landing will define the output path.
fi

smoke_summary
