#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: unit-tests
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"
# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

assert_grep_present 'Empty/invalid fields, duplicate tenants, and an' \
    'docs/plans/phase-233a-durable-session-overlay-personal-skills.md' \
    'phase 233a pins fail-loud static cutover validation'
assert_grep_present '__session_personal_cutover__' \
    'RFC-001-Harbor.md' \
    'phase 233a pins the reserved cutover control scope'
assert_grep_present '^### skills\.session_personal_cutover\.tenants' \
    'docs/CONFIG.md' \
    'phase 233a documents the static cutover contract'
assert_grep_present 'session_skill_cutover_pending' \
    'CHANGELOG.md' \
    'phase 233a records the intentional dual-read mutation refusal'
assert_grep_present 'session_personal_cutover:' \
    'examples/dev.yaml' \
    'phase 233a exposes the operator declaration example'

P233A_TMP="$(mktemp -d "${TMPDIR:-/tmp}/harbor-phase-233a.XXXXXX")"
trap 'rm -rf "${P233A_TMP}"' EXIT
P233A_GOLOG="${P233A_TMP}/go-test.log"

assert_go_tests_pass "${P233A_GOLOG}" './internal/config ./internal/agentcfg/sessionoverlay ./internal/sessions ./internal/runtime/serve' \
    'phase 233a: static cutover, durable resolver, erasure, and boot authority guards execute' \
    TestValidateSessionPersonalCutover_RefusesMalformedDeclarations \
    TestValidateSessionPersonalCutover_RefusesOverBoundAndNonASCIIDeclarations \
    TestValidateSessionPersonalCutover_RequiresSkillStore \
    TestCutoverController_UnlistedAndUndrainedRemainDualRead \
    TestSessionPersonalController_MutationsRequireStateOnlyBeforeAnyWrite \
    TestSessionSkillResolver_DualReadComposesOnlyExactLegacySessionTier \
    TestCascadeEraser_LegacySessionSkills_ExactSweepBeforeStateClear \
    TestNewSessionPersonalSkillAuthority_ResumesDeclaredCutoverAcrossRestart
smoke_summary
