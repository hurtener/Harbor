#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
#
# Phase 111a smoke — governance enforcement assembly (D-198).
# (RFC §6.15 / §6.5 / §6.11; docs/plans/phase-111a-governance-enforcement-assembly.md)
#
# Static + unit-test assertions:
#   1. `governance.SetFactory` has a NON-TEST production caller (the
#      SDK friction audit's exact regression check) — the assembly —
#      and the exported `NewSubsystemFromConfig` helper exists.
#   2. The Wave A posture-only boot warning is GONE from the assembly,
#      and the operator-facing surfaces no longer carry "not yet
#      wired" phrasing (config godoc, example yaml, CONFIG.md,
#      skills).
#   3. The headless escape (`governance.Wrap` + multi-runtime godoc on
#      SetFactory) and the recipe section exist.
#   4. The three-enforcer E2E + the focused unit slices pass under
#      -race.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

# --- 1. SetFactory's first production caller + the assembly helper ---------

assert_file internal/governance/assembly.go 'phase 111a: governance assembly helper file'
assert_grep_present 'func NewSubsystemFromConfig\(cfg Config, store state\.StateStore, bus events\.EventBus\) \(Subsystem, error\)' \
    internal/governance/assembly.go 'phase 111a: NewSubsystemFromConfig exported with the pinned signature'
assert_grep_present 'governance\.SetFactory\(' internal/runtime/assemble/assemble.go \
    'phase 111a: the production assembly calls governance.SetFactory (first non-test caller)'
assert_grep_present 'governance\.NewSubsystemFromConfig\(' internal/runtime/assemble/assemble.go \
    'phase 111a: the assembly builds the Subsystem eagerly at boot (fail-loud)'
assert_grep_present 'governance\.ConfigFromOperator\(' internal/runtime/assemble/assemble.go \
    'phase 111a: the factory consumes the exported 110c config projection'

# --- 2. The posture-only warning + phrasing are gone ------------------------

assert_grep_absent 'enforcement is NOT yet wired' internal/runtime/assemble/assemble.go \
    'phase 111a: the Wave A posture-only boot warning removed from the assembly'
assert_grep_absent 'Enforcement is NOT yet wired' internal/config/config.go \
    'phase 111a: GovernanceConfig godoc no longer carries the not-enforced caveat'
assert_grep_absent 'ENFORCEMENT NOT YET WIRED' examples/harbor.yaml \
    'phase 111a: example config flipped to enforcement phrasing'
assert_grep_absent 'Enforcement is not yet' docs/CONFIG.md \
    'phase 111a: CONFIG.md flipped to enforcement phrasing'
assert_grep_absent 'Enforcement is not yet wired' docs/skills/define-the-agent-yaml/SKILL.md \
    'phase 111a: the agent-yaml skill flipped to enforcement phrasing (§18 same-PR rule)'
assert_grep_absent 'enforcement is not yet wired' docs/skills/validate-and-package/SKILL.md \
    'phase 111a: the validate-and-package skill flipped to enforcement phrasing (§18 same-PR rule)'

# --- 3. The documented headless / multi-runtime escape ----------------------

assert_grep_present 'Multi-runtime limitation \(D-198\)' internal/governance/registry.go \
    'phase 111a: SetFactory godoc documents the process-global limitation'
assert_grep_present 'governance\.Wrap' docs/recipes/embed-harbor-headless.md \
    'phase 111a: the headless recipe carries the Wrap composition'
assert_grep_present 'NewSubsystemFromConfig' docs/recipes/embed-harbor-headless.md \
    'phase 111a: the headless recipe carries the assembly helper'

# --- 4. The E2E + unit slices pass under -race -------------------------------

assert_file test/integration/phase111a_governance_test.go 'phase 111a: the three-enforcer E2E file'

if go test ./internal/governance/ -run 'NewSubsystemFromConfig|NewCompound|LLMOpen_Governance|LLMOpen_Factory' \
    -race -count=1 -timeout 300s >/dev/null 2>&1; then
    ok 'phase 111a: governance assembly unit slices pass under -race'
else
    fail 'phase 111a: governance assembly unit slices failed (run the go test line in scripts/smoke/phase-111a.sh for detail)'
fi

if go test ./test/integration/ -run 'Phase111a' -race -count=1 -timeout 300s >/dev/null 2>&1; then
    ok 'phase 111a: three-enforcer E2E + isolation + concurrent-reuse pass under -race'
else
    fail 'phase 111a: integration E2E failed (go test ./test/integration/ -run Phase111a -race)'
fi

smoke_summary
