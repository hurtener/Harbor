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
P233A_TMP="$(mktemp -d "${TMPDIR:-/tmp}/harbor-p233a.XXXXXX")"
trap 'rm -rf "${P233A_TMP}"' EXIT
P233A_MIGRATION_PATTERN='(session[_ .-]?personal|session[_ .-]?overlay|session[_ .-]?skills?|session[_ .-]?skill[_ .-]?cutover|agentcfg[.]session|agent[_ .-]?config[_ .-]?session|phase[ _.-]?233a|d[ _.-]?400)'

# p233a_migration_names_feature <file>
# Returns success when either the path or contents name Phase 233a's
# StateStore-record feature. Kept as one predicate so the real census and the
# mutation probes exercise exactly the same guard.
p233a_migration_names_feature() {
    local migration_file="$1"
    printf '%s\n' "${migration_file}" | grep -Eqi "${P233A_MIGRATION_PATTERN}" || \
        grep -aEqi "${P233A_MIGRATION_PATTERN}" "${migration_file}"
}

# Mutation-test the negative guard without writing into a real migrations
# directory. Representative filename and SQL spellings must trip; a benign
# migration proves the predicate is selective rather than always-successful.
P233A_PROBE_FILENAME="${P233A_TMP}/0002_session_skills.sql"
P233A_PROBE_CONTENT="${P233A_TMP}/0002_records.sql"
P233A_PROBE_AGENTCFG="${P233A_TMP}/0003_records.sql"
P233A_PROBE_DOTTED="${P233A_TMP}/0004_records.sql"
P233A_PROBE_AGENTCFG_SHORT="${P233A_TMP}/0005_records.sql"
P233A_PROBE_ALLOWED="${P233A_TMP}/0006_unrelated.sql"
printf '%s\n' 'SELECT 1;' >"${P233A_PROBE_FILENAME}"
printf '%s\n' 'CREATE TABLE session_skill_cutover (id TEXT);' >"${P233A_PROBE_CONTENT}"
printf '%s\n' 'CREATE TABLE agent_config_session_skills (id TEXT);' >"${P233A_PROBE_AGENTCFG}"
printf '%s\n' 'COMMENT ON TABLE state_records IS ''agent_config.session skills'';' >"${P233A_PROBE_DOTTED}"
printf '%s\n' 'CREATE TABLE agentcfg.session_personal (id TEXT);' >"${P233A_PROBE_AGENTCFG_SHORT}"
printf '%s\n' 'CREATE INDEX state_records_kind_idx ON state_records(kind);' >"${P233A_PROBE_ALLOWED}"

P233A_PROBES_OK=true
for probe in "${P233A_PROBE_FILENAME}" "${P233A_PROBE_CONTENT}" "${P233A_PROBE_AGENTCFG}" "${P233A_PROBE_DOTTED}" "${P233A_PROBE_AGENTCFG_SHORT}"; do
    if ! p233a_migration_names_feature "${probe}"; then
        fail "phase 233a: migration guard mutation probe escaped (${probe##*/})"
        P233A_PROBES_OK=false
    fi
done
if p233a_migration_names_feature "${P233A_PROBE_ALLOWED}"; then
    fail 'phase 233a: migration guard rejects an unrelated migration control'
    P233A_PROBES_OK=false
fi
if [ "${P233A_PROBES_OK}" = true ]; then
    ok 'phase 233a: migration guard rejects filename, cutover, and agent_config.session mutation probes'
fi

P233A_MIGRATION_CENSUS=0
P233A_MIGRATION_VIOLATION=''
while IFS= read -r migration_file; do
    P233A_MIGRATION_CENSUS=$((P233A_MIGRATION_CENSUS + 1))
    if p233a_migration_names_feature "${migration_file}"; then
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
