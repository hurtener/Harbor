#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
# Phase 244 smoke — Draft-only personal-skill proposer tool (HA-62, D-423).
# Shipped (v1.28): pins the shipped contract — `skill_create_draft` is an
# ordinary disabled-by-default runtime tool with zero mutation authority,
# sharing the canonical semantic DTO/validator/serializer/PackageHash with
# Phase 243, and installation is exclusively the Phase 243 validate/commit
# workflow. The live assertions (disabled by default, enable through normal
# agent policy, create a draft, assert the ref is caller-scoped with no user
# skill/membership, then validate through Phase 243; authority-shaped input,
# cross-user access, refused/malformed output, and a direct persist attempt
# fail without mutation) are exercised by the phase's in-package suites
# (internal/skills/drafter/, internal/runtime/agentcfg/protocol/), not
# duplicated here.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
source scripts/smoke/common.sh
assert_file docs/plans/phase-244-personal-skill-draft-tool.md "phase 244 plan exists"
assert_grep_present "D-423" docs/decisions.md "D-423 is recorded (HA-62)"
assert_grep_present "Shipped (v1.28)" docs/plans/README.md "phase 244 is Shipped (v1.28) in the master plan"
assert_grep_present "skill_create_draft" docs/plans/phase-244-personal-skill-draft-tool.md "draft tool is documented"
assert_grep_present "disabled-by-default" docs/plans/phase-244-personal-skill-draft-tool.md "disabled-by-default posture is documented"
assert_grep_present "zero mutation authority" docs/plans/phase-244-personal-skill-draft-tool.md "zero mutation authority is documented"
assert_grep_present "fail without mutation" docs/plans/phase-244-personal-skill-draft-tool.md "authority-shaped input and persist attempts fail without mutation"
assert_grep_present "Phase 243 validate/commit" docs/plans/phase-244-personal-skill-draft-tool.md "install path is exclusively Phase 243 validate/commit"
assert_grep_present "installed: false" docs/glossary.md "the draft carries the explicit installed: false state"
assert_grep_present "zero mutation authority" docs/glossary.md "glossary records the tool's zero mutation authority"
smoke_summary
