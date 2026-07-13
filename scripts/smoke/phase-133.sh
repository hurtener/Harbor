#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 133 smoke — the scaffold-with-tools EXECUTION gate (D-267;
# docs/plans/phase-133-scaffold-tools-execution.md).
#
# This phase has no runtime surface of its own: it hardens the
# scaffold template so a tools-declaring agent's generated test does
# not just COMPILE RegisterTools but actually REGISTERS and DISPATCHES
# a tool through the catalog. The heavy gate — scaffold an external
# module, `go test ./...`, and a self-test proving the gate bites — is
# owned by scripts/smoke/phase-112b.sh (which already pays the
# external-module build cost; §4.3 call, recorded in the phase plan).
#
# These STATIC assertions pin the template surface so a future refactor
# cannot silently drop the register-and-dispatch block (the §1 / §13
# false-green: a tools-declaring agent that compiles + passes while no
# tool is ever invoked).
#
# Conventions (AGENTS.md §4.2): scripts/smoke/common.sh helpers; FAIL = 0.

set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

TEST_TPL="cmd/harbor/scaffold/templates/minimal-react/agent_test.go.tmpl"
AGENT_TPL="cmd/harbor/scaffold/templates/minimal-react/agent.go.tmpl"

# 1. The register-and-dispatch block is gated on COMPILED tools being
#    declared. Built-ins do NOT travel through RegisterTools (v1.13.1):
#    the runtime registers them from `tools.built_in` at boot, so a
#    registrar that also registered them killed the boot with a duplicate
#    tool name. The gate covers what the registrar actually carries.
assert_grep_present '\{\{- if \.CustomTools\}\}' \
    "${TEST_TPL}" \
    'phase 133: agent_test.go.tmpl gates the dispatch test on declared custom tools'

# 2. The gated block calls RegisterTools (the register half).
assert_grep_present 'RegisterTools\(cat\)' \
    "${TEST_TPL}" \
    'phase 133: the dispatch test calls RegisterTools'

# 3. ...and drives a tool THROUGH the catalog/executor (the dispatch
#    half) — Resolve off the catalog + Invoke the descriptor. This is
#    the observable dispatch signal, not merely "RegisterTools is
#    defined".
assert_grep_present 'cat\.Resolve\(' \
    "${TEST_TPL}" \
    'phase 133: the dispatch test resolves the tool off the catalog'
assert_grep_present 'desc\.Invoke\(ctx' \
    "${TEST_TPL}" \
    'phase 133: the dispatch test invokes the tool through the executor'

# 3b. The registrar template must never register a built-in (v1.13.1).
#     Built-ins are config-driven and the RUNTIME owns them (it registers
#     `tools.built_in` with the real SkillStore / ArtifactStore / Bus /
#     Redactor). A generated `builtin.RegisterWith` handed the catalog the
#     same name twice, so any `--with-server` scaffold declaring a built-in
#     died at boot with `duplicate tool name`. This static pin keeps it out.
assert_grep_absent 'builtin\.RegisterWith' \
    "${AGENT_TPL}" \
    'phase 133: agent.go.tmpl does NOT register built-ins (the runtime does, from tools.built_in)'
assert_grep_absent 'sdk/tools/builtin' \
    "${AGENT_TPL}" \
    'phase 133: agent.go.tmpl does not import the built-in registry at all'

# 4. The execution gate proper lives in phase-112b.sh.
assert_grep_present 'EXTERNAL EXECUTION GATE' \
    scripts/smoke/phase-112b.sh \
    'phase 133: the go test execution gate is wired into phase-112b.sh'
assert_grep_present 'execution-gate self-test' \
    scripts/smoke/phase-112b.sh \
    'phase 133: phase-112b.sh proves the dispatch gate bites (self-test)'

smoke_summary
