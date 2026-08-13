#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 244 smoke — Draft-only personal-skill proposer tool (HA-62).
#
# PENDING STATIC SKELETON. Phase 244 is Planned (master-plan row 244 carries
# Status `Pending`), so this smoke records the plan + decision and pins the
# load-bearing plan contracts. It does NOT claim the surface is implemented:
# there is no live-server leg and no "surface works" assertion. When the
# phase ships, the implementor extends this script with the live assertions
# from the plan's "Smoke script additions" section (disabled by default,
# enable through normal agent policy, create a draft, assert the ref is
# caller-scoped with no user skill/membership, then validate through Phase
# 243; authority-shaped input, cross-user access, refused/malformed output,
# and a direct persist attempt fail without mutation).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source scripts/smoke/common.sh
assert_file docs/plans/phase-244-personal-skill-draft-tool.md "phase 244 plan exists"
assert_grep_present "D-423" docs/decisions.md "D-423 is recorded (HA-62)"
assert_grep_present "Pending" docs/plans/README.md "phase 244 is Planned/Pending in the master plan"
assert_grep_present "skill_create_draft" docs/plans/phase-244-personal-skill-draft-tool.md "draft tool is planned"
assert_grep_present "disabled by default" docs/plans/phase-244-personal-skill-draft-tool.md "disabled-by-default posture is planned"
assert_grep_present "zero" docs/plans/phase-244-personal-skill-draft-tool.md "zero mutation authority is planned"
assert_grep_present "Phase 243" docs/plans/phase-244-personal-skill-draft-tool.md "install path is exclusively Phase 243 validate/commit"
smoke_summary
