#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 101 smoke — GitHub Actions Node 24 modernisation.
#
# Static-only: asserts no workflow step pins a Node-20-bound action version.
# GitHub forces Node 24 on JavaScript actions 2026-06-16 and removes the
# Node 20 runtime 2026-09-16; the deprecated pins are actions/checkout@v4/v5
# and actions/setup-go@v4/v5 (plus the two repo-specific stragglers asserted
# below). The workflows' live behaviour is gated by CI itself — every PR run
# exercises the bumped actions — so the smoke only guards against version
# regressions reappearing in committed YAML.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# 1. No Node-20-bound actions/checkout or actions/setup-go pins remain.
if grep -rE 'actions/(checkout|setup-go)@v[45]' .github/workflows/ >/dev/null 2>&1; then
    fail "phase 101: Node-20-bound actions/checkout or actions/setup-go pin found in .github/workflows/"
else
    ok "phase 101: no actions/checkout@v4/v5 or actions/setup-go@v4/v5 pins remain"
fi

# 2. The two repo-specific Node 20 stragglers stay bumped.
if grep -rE 'DavidAnson/markdownlint-cli2-action@v(1[0-9]|[0-9])$' .github/workflows/ >/dev/null 2>&1; then
    fail "phase 101: markdownlint-cli2-action pinned below v20 (Node 20 runtime)"
else
    ok "phase 101: markdownlint-cli2-action on a Node-24 major (v20+)"
fi

if grep -rE 'actions/deploy-pages@v[1-4]' .github/workflows/ >/dev/null 2>&1; then
    fail "phase 101: actions/deploy-pages pinned below v5 (Node 20 runtime)"
else
    ok "phase 101: actions/deploy-pages on a Node-24 major (v5+)"
fi

# 3. Every workflow file still parses as YAML (basic sanity after the bump).
#    Degrades to SKIP where python3 + PyYAML are unavailable — the parse
#    check is belt-and-braces; GitHub itself rejects unparsable workflows.
if python3 -c "import yaml" >/dev/null 2>&1; then
    yaml_ok=1
    for wf in .github/workflows/*.yml; do
        if ! python3 -c "import sys, yaml; yaml.safe_load(open(sys.argv[1]))" "${wf}" >/dev/null 2>&1; then
            fail "phase 101: ${wf} does not parse as YAML"
            yaml_ok=0
        fi
    done
    if [ "${yaml_ok}" -eq 1 ]; then
        ok "phase 101: every .github/workflows/*.yml parses as YAML"
    fi
else
    skip "phase 101: python3 + PyYAML unavailable — YAML parse sanity not run"
fi

smoke_summary
