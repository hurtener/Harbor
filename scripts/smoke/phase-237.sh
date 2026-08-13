#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source scripts/smoke/common.sh
assert_file docs/plans/phase-237-agent-owned-skills-governed-authoring.md "phase 237 plan exists"
assert_grep_present "D-411" docs/decisions.md "D-411 is recorded"
assert_grep_present "agent_id remains" docs/plans/phase-237-agent-owned-skills-governed-authoring.md "agent is not an identity principal"
assert_grep_present "Postgres" docs/plans/phase-237-agent-owned-skills-governed-authoring.md "durable Postgres evidence is planned"
assert_grep_present "next run" docs/plans/phase-237-agent-owned-skills-governed-authoring.md "next-run snapshot semantics are planned"
assert_grep_present "Protocol" docs/plans/phase-237-agent-owned-skills-governed-authoring.md "Protocol contract is planned"
smoke_summary
