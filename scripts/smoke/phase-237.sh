#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
# Phase 237 — governed agent-pack authoring and revision convergence.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source scripts/smoke/common.sh
assert_file docs/plans/phase-237-agent-owned-skills-governed-authoring.md "phase 237 plan exists"
assert_grep_present "D-411" docs/decisions.md "D-411 is recorded"
assert_grep_present "agent_id remains" docs/plans/phase-237-agent-owned-skills-governed-authoring.md "agent is not an identity principal"
assert_grep_present "Postgres" docs/plans/phase-237-agent-owned-skills-governed-authoring.md "durable Postgres evidence is planned"
assert_grep_present "next run" docs/plans/phase-237-agent-owned-skills-governed-authoring.md "next-run snapshot semantics are planned"
assert_grep_present "Protocol" docs/plans/phase-237-agent-owned-skills-governed-authoring.md "Protocol contract is planned"
assert_grep_present 'agent_config\.agent_packs\.propose' internal/protocol/methods/methods.go 'phase 237: propose method is canonical'
assert_grep_present 'agent_config\.agent_packs\.commit' internal/protocol/methods/methods.go 'phase 237: commit method is canonical'
assert_grep_present 'AgentPacks' internal/runtime/agentcfg/protocol/service.go 'phase 237: generic revisions carry packs'
assert_grep_present 'DryRun' internal/runtime/agentcfg/protocol/agentpacks.go 'phase 237: dry-run remains non-persistent'
smoke_summary
