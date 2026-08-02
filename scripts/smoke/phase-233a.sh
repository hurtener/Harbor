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
assert_grep_present '^- No migration edits,' \
    'docs/plans/phase-233a-durable-session-overlay-personal-skills.md' \
    'phase 233a retains the no-migration-files design constraint'

# D-400 stores overlays, owned skills, and cutover progress in the existing
# StateStore record envelope. A SQL migration for any of those names would be
# a second persistence model. Check both migration filenames and contents, and
# fail if the census itself becomes empty so "nothing scanned" cannot pass.
P233A_MIGRATION_CENSUS=0
P233A_MIGRATION_VIOLATION=''
while IFS= read -r migration_file; do
    P233A_MIGRATION_CENSUS=$((P233A_MIGRATION_CENSUS + 1))
    if printf '%s\n' "${migration_file}" | grep -Eqi '(session[_ .-]?personal|session[_ .-]?overlay|agentcfg\.session|phase[ _.-]?233a|d[ _.-]?400)' || \
        grep -aEqi '(session[_ .-]?personal|session[_ .-]?overlay|agentcfg\.session|phase[ _.-]?233a|D-400)' "${migration_file}"; then
        P233A_MIGRATION_VIOLATION="${migration_file}"
        break
    fi
done < <(find internal -type f -path '*/migrations/*' -print | sort)

if [ -n "${P233A_MIGRATION_VIOLATION}" ]; then
    fail "phase 233a: forbidden migration artifact names the StateStore-record feature (${P233A_MIGRATION_VIOLATION})"
elif [ "${P233A_MIGRATION_CENSUS}" -eq 0 ]; then
    fail 'phase 233a: migration census is empty — no-migration-files guard did not inspect a real corpus'
else
    ok "phase 233a: no session-personal migration artifact exists (${P233A_MIGRATION_CENSUS} existing migrations inspected)"
fi

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
