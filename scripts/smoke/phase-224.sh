#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 224 — mutation-verify the drift-audit's own guards.
#
# `scripts/drift-audit.sh` is the mechanical instrument that enforces Harbor's
# drift rules, and until this phase NOTHING verified the instrument. Its guards
# were mutation-verified BY HAND and the results recorded in its code comments;
# a regression that re-broke one of them would have gone completely unnoticed,
# because a guard that cannot fire is indistinguishable from a corpus with no
# violations. That is the failure mode the v1.24 and v1.25 waves found roughly a
# dozen times across the smoke corpus — here it is one level up, in the tool
# whose job is to detect it.
#
# WHAT THIS SCRIPT DOES. For each guard drift-audit implements it builds a
# throwaway fixture corpus, applies a mutation of the shape that guard exists to
# catch, runs the REAL drift-audit against the mutated corpus, and asserts the
# audit printed that guard's own FAIL (or WARN) line. The clean half is asserted
# from a pristine baseline run of the same corpus: the guard's OK line present,
# its FAIL line absent. A guard that stays silent on a corpus carrying the exact
# defect it names is INERT, and this script turns that into a red preflight.
#
# ---------------------------------------------------------------------------
# WHAT "INDEPENDENT" MEANS HERE, AND WHY IT IS NOT "DO NOT RUN THE AUDIT"
# ---------------------------------------------------------------------------
# A check that verifies drift-audit by RE-IMPLEMENTING its logic would share
# nothing useful and prove nothing: it would test the copy, not the program, and
# the copy is exactly where phase 223's tautological assertion came from
# (`rows_seen="${rows_total}"` — a comparison true by construction). So this
# script DOES run the real `scripts/drift-audit.sh`. Independence lives in three
# other places:
#
#   1. THE ORACLE IS EXTERNAL. Every expected verdict is a literal string in
#      THIS file, written by hand from the guard's own message. Nothing is
#      derived from the audit's output. There is no `expected="$(drift-audit …)"`
#      anywhere, which is the shape that makes a self-check vacuous.
#   2. THE CORPUS IS CONSTRUCTED, NOT OBSERVED. Mutations are applied to a
#      fixture tree built by this script under $TMPDIR, whose clean verdict is
#      known a priori (it is built to pass) and whose mutated verdict is known a
#      priori (the mutation is the violation). The live repository is never
#      touched — a mutation of a tracked file under a concurrent `make
#      preflight` would be a disaster, and drift-audit itself already carries
#      the scar of a fixed `/tmp` path that two concurrent audits clobbered.
#   3. THIS IS NOT A GUARD INSIDE THE SUBJECT. Had it been added as
#      "drift-audit check 18: verify my own guards", any global breakage of
#      drift-audit (an early `set -e` exit, an inverted summary) would take the
#      self-check down with it and still print a green exit code. It runs the
#      audit as a SEPARATE PROCESS and reads only its stdout and exit status.
#
# THE ONE PLACE THIS SCRIPT READS THE SUBJECT'S SOURCE is the forbidden-name
# word list, which cannot be written literally here (CLAUDE.md §13 forbids the
# predecessor's name anywhere in committed text). It is extracted from
# drift-audit.sh at run time. That is safe precisely because the oracle stays
# external: if the list were emptied, the extraction would yield nothing, the
# mutation would inject nothing, the audit would report OK — and this script
# would report FAIL, because its expectation ("this mutation must be caught")
# is written down here, not read from there. The extraction is additionally
# asserted non-empty.
#
# ---------------------------------------------------------------------------
# WHAT THIS SCRIPT DOES **NOT** PROVE — the contract, stated so it is not
# mistaken for more than it is
# ---------------------------------------------------------------------------
# Each case proves a guard is NOT INERT against one mutation of the shape it is
# written for. It does NOT prove the guard catches every violating shape. The
# escape guard's continuation-line blindness and its five-of-eight helper
# alternation were SHAPE gaps in a guard that was otherwise live, and no harness
# of this kind can enumerate shapes. Where a hardest-known shape exists it is
# the one used (see the two payloads in scripts/smoke/testdata/phase-224/).
#
# ---------------------------------------------------------------------------
# POPULATION, NOT JUST SHAPE — the second axis, added after this harness was
# found blind along it
# ---------------------------------------------------------------------------
# A guard has a SHAPE (what counts as a violation) and a POPULATION (where it
# looks). The first version of this harness verified shapes only: every
# mutation planted its defect in `internal/fixture/fixture.go`, even for the
# guards that scan `internal/ cmd/ sdk/`. So two REAL regressions passed it
# unnoticed — narrowing the godoc scan's roots to `internal/` alone (dropping
# `sdk/`, D-282's explicit adopter-facing extension) and deleting the
# forbidden-name scan's whole `cmd/` block both produced `OK 25 / SKIP 0 /
# FAIL 0`. The fixture files for those directories already existed; nothing
# planted in them.
#
# The rule this leaves behind is the same one D-374 recorded one level down: a
# sweep is only as wide as the population it enumerated. So every guard whose
# population is a DIRECTORY LIST now gets one case PER directory, and the
# mutation is planted in that directory's own fixture file:
#
#   godoc jargon scan  -> internal/ + cmd/ + sdk/          (3 cases)
#   forbidden-name     -> root docs + plans + briefs +
#                         internal/ + cmd/                  (5 cases)
#   escape portability -> scripts/smoke/*.sh + scripts/*.sh (2 cases)
#   mktemp portability -> scripts/smoke/** + scripts/**     (2 cases)
#
# The complementary half lives in drift-audit itself (check 2b, the population
# census): a corpus that is EMPTY fails there, because no case planted in a
# directory can fire if the directory is gone. Neither mechanism substitutes
# for the other — the census sees a vanished corpus, these cases see a guard
# that stopped reading a corpus that is still there.
#
# The residual, stated: a population a guard GAINS later (a fourth godoc root,
# say) has no case until someone writes one. The `ok`-line census below cannot
# see that, because adding a directory to an existing guard adds no `ok` line.
#
# The per-case residuals are enumerated in
# docs/plans/phase-224-mutation-verify-the-drift-audit.md § "Coverage census".
# The COVERAGE CENSUS at the bottom of this script is the mechanical half of
# that promise: it cross-checks every `ok` call drift-audit makes against the
# list of guards verified here, so a NEW guard added without a mutation case
# fails this smoke instead of quietly enjoying zero coverage. Silent partial
# coverage is the defect this phase exists to remove; it must not be
# reintroduced by the fix.
#
# See docs/plans/phase-224-mutation-verify-the-drift-audit.md
#   § "Smoke script additions" for the binding assertion list, and D-376.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

AUDIT='scripts/drift-audit.sh'
FRONTMATTER='scripts/skills/check-frontmatter.sh'
BODY_IDENTITY='scripts/check-smoke-body-identity.sh'
PAYLOADS='scripts/smoke/testdata/phase-224'

WORK=''
cleanup() {
    if [ -n "${WORK}" ]; then
        rm -rf "${WORK}"
    fi
}
trap cleanup EXIT

# ----------------------------------------------------------------------------
# Hard preconditions. Each is a FAIL, never a SKIP: all three are unconditional
# repository fixtures, and "the harness could not run" must not read like "the
# harness found nothing wrong" (AGENTS.md §4.2 item 5).
# ----------------------------------------------------------------------------
PRECONDITIONS_OK=1
for required in "${AUDIT}" "${FRONTMATTER}" "${BODY_IDENTITY}" \
                "${PAYLOADS}/bad-escape.sh.txt" "${PAYLOADS}/bad-mktemp.sh.txt"; do
    if [ ! -f "${required}" ]; then
        fail "phase 224: ${required} is missing — the mutation harness has nothing to verify"
        PRECONDITIONS_OK=0
    fi
done
if ! command -v git >/dev/null 2>&1; then
    fail 'phase 224: git is not available — the fixture corpus cannot mint the local release tags the scaffold-pin guard reads, so two guards would go unverified. This is a repository tool; git is not optional.'
    PRECONDITIONS_OK=0
fi
if [ "${PRECONDITIONS_OK}" -ne 1 ]; then
    smoke_summary || true
    exit 1
fi

# The forbidden-name word list, read from the subject (see the header note on
# why this single read does not compromise the external oracle).
FORBIDDEN_WORD="$(sed -nE 's/^forbidden=\((.*)\)$/\1/p' "${AUDIT}" \
    | head -1 | tr -d '"' | awk '{print $1}')"
if [ -z "${FORBIDDEN_WORD}" ]; then
    fail "phase 224: could not read the forbidden-name word list from ${AUDIT} — either the list was emptied (which would silently disable the naming rule) or its declaration moved. The mutation for that guard cannot be built, and an unverifiable guard must not read as a passing one."
    smoke_summary || true
    exit 1
fi

WORK="$(mktemp -d "${TMPDIR:-/tmp}/harbor-phase224-XXXXXX")"
TEMPLATE="${WORK}/template"

# ----------------------------------------------------------------------------
# build_template — materialise a minimal corpus that drift-audit passes CLEAN.
#
# Every file here exists to satisfy exactly one guard's happy path. The corpus
# is deliberately tiny (one phase plan, one brief, one skill, three Go files):
# a mutation must be attributable, and a large corpus makes the audit's output
# hard to attribute and every case slow.
# ----------------------------------------------------------------------------
build_template() {
    local d="${TEMPLATE}"
    mkdir -p "${d}/docs/plans" "${d}/docs/rfc" "${d}/docs/research" \
             "${d}/docs/skills/fixture-skill" \
             "${d}/scripts/smoke" "${d}/scripts/skills" \
             "${d}/internal/fixture" "${d}/cmd/fixture" \
             "${d}/cmd/harbor/scaffold" "${d}/sdk/fixture"
    mkdir -p "${d}/web/console/src/routes/(console)/playground"

    # The SUBJECT UNDER TEST — a copy of the real script, verified byte-identical
    # below. It is copied rather than symlinked so the fixture is self-contained
    # and `cd`-relative reads inside it cannot escape to the live tree.
    cp "${AUDIT}" "${d}/scripts/drift-audit.sh"
    cp "${FRONTMATTER}" "${d}/scripts/skills/check-frontmatter.sh"
    chmod +x "${d}/scripts/drift-audit.sh" "${d}/scripts/skills/check-frontmatter.sh"

    # The D-374 smoke body-identity helper is a STAND-IN here, not the real
    # script, and the distinction is load-bearing enough to state twice (see
    # the coverage census at the bottom). The real helper joins the generated
    # wire manifest against the generated methods table and then walks the
    # smoke corpus; reproducing all three in a fixture would be reproducing
    # its logic, which is exactly the "test the copy, not the program" trap
    # this harness exists to avoid. What IS under test here is drift-audit's
    # DELEGATION to it — that a missing helper, a lost executable bit, or a
    # non-zero exit each produce a loud FAIL rather than silence. That
    # delegation shipped with no `else` at all, so a lost executable bit made
    # the whole guard vanish; the stand-in is what lets both directions be
    # exercised. The helper's own detection logic carries its own guards and
    # its own mutation record under D-374.
    cat > "${d}/scripts/check-smoke-body-identity.sh" <<'FIXTURE_EOF'
#!/usr/bin/env bash
# STAND-IN for scripts/check-smoke-body-identity.sh. See build_template.
echo '[OK]   smoke body-identity: no identity body attributed to an identity-less method (fixture stand-in)'
exit 0
FIXTURE_EOF
    chmod +x "${d}/scripts/check-smoke-body-identity.sh"

    cat > "${d}/AGENTS.md" <<'FIXTURE_EOF'
# Fixture rules file

The mirror invariant requires this file and CLAUDE.md to be byte-identical.
FIXTURE_EOF
    cp "${d}/AGENTS.md" "${d}/CLAUDE.md"

    cat > "${d}/README.md" <<'FIXTURE_EOF'
# Fixture

A throwaway corpus for the drift-audit mutation harness.
FIXTURE_EOF
    printf 'Fixture licence placeholder.\n' > "${d}/LICENSE"

    cat > "${d}/RFC-001-Harbor.md" <<'FIXTURE_EOF'
# Fixture RFC

## 3. Principles

### 3.4 The fail-loudly principle

A guard whose only reachable outcome is a pass is silent degradation.
FIXTURE_EOF

    cat > "${d}/CHANGELOG.md" <<'FIXTURE_EOF'
# Changelog

## [1.28]

- An artifact-release tag (two-component) — a release identity, never a
  module tag; the ledger must not count it.

## [1.27]

- An artifact-release tag (two-component) — same, never a module tag.

## [1.26.12]

- The newest published fixture MODULE release — the three-component
  grammar the ledger accepts (v1.26.12, v1.26.11, ...).

## [1.26.11]

- An older fixture module release.

## [1.26.10]

- The oldest fixture module release.
FIXTURE_EOF

    # `drift-audit:` must be a real target: the audit greps for it, and the
    # markdownlint guard invokes `make -s markdownlint` in this root. The
    # markdownlint target is a sentinel switch rather than a real lint run —
    # see the coverage census for what that does and does not verify.
    cat > "${d}/Makefile" <<'FIXTURE_EOF'
drift-audit:
	@bash scripts/drift-audit.sh

markdownlint:
	@test ! -f .markdownlint-should-fail
FIXTURE_EOF

    cat > "${d}/docs/plans/README.md" <<'FIXTURE_EOF'
# Fixture master plan

| #  | Name    | Status  |
|----|---------|---------|
| 01 | fixture | Shipped |
FIXTURE_EOF
    cat > "${d}/docs/plans/_template.md" <<'FIXTURE_EOF'
# Phase NN — fixture template
FIXTURE_EOF
    cat > "${d}/docs/rfc/README.md" <<'FIXTURE_EOF'
# Fixture merged RFCs
FIXTURE_EOF
    cat > "${d}/docs/research/INDEX.md" <<'FIXTURE_EOF'
# Fixture brief index
FIXTURE_EOF
    cat > "${d}/docs/research/06-fixture.md" <<'FIXTURE_EOF'
# Brief 06 — fixture

A brief the fixture phase plan resolves against.
FIXTURE_EOF
    cat > "${d}/docs/glossary.md" <<'FIXTURE_EOF'
# Fixture glossary
FIXTURE_EOF
    cat > "${d}/docs/decisions.md" <<'FIXTURE_EOF'
# Fixture decisions
FIXTURE_EOF

    # The one phase plan: all nine required headings, one resolvable RFC
    # reference, one resolvable brief reference.
    cat > "${d}/docs/plans/phase-01-fixture.md" <<'FIXTURE_EOF'
# Phase 01 — fixture

## Summary

A phase plan that satisfies every structural check drift-audit makes.

## RFC anchor

- RFC §3.4

## Briefs informing this phase

- brief 06

## Acceptance criteria

- [ ] The audit passes clean on this corpus.

## Files added or changed

- none

## Test plan

- **Unit:** none

## Smoke script additions

- none

## Coverage target

- none

## Dependencies

- none
FIXTURE_EOF

    cat > "${d}/scripts/smoke/_template.sh" <<'FIXTURE_EOF'
#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
set -euo pipefail
FIXTURE_EOF
    cat > "${d}/scripts/smoke/common.sh" <<'FIXTURE_EOF'
#!/usr/bin/env bash
set -euo pipefail
FIXTURE_EOF
    cat > "${d}/scripts/smoke/phase-01.sh" <<'FIXTURE_EOF'
#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
set -euo pipefail
FIXTURE_EOF
    chmod +x "${d}/scripts/smoke/phase-01.sh"

    cat > "${d}/docs/skills/fixture-skill/SKILL.md" <<'FIXTURE_EOF'
---
name: fixture-skill
description: A fixture playbook. Use when verifying the frontmatter audit.
license: Apache-2.0
metadata:
  framework: harbor
  surface: cli
  verbs: ""
---

# Fixture skill
FIXTURE_EOF

    # Go sources for the forbidden-name scan and the godoc-jargon scan. Both
    # scans walk internal/, cmd/ and (godoc only) sdk/, so all three are
    # populated and all three are clean.
    cat > "${d}/internal/fixture/fixture.go" <<'FIXTURE_EOF'
// Package fixture is a clean corpus file for the drift-audit mutation harness.
package fixture

// Name returns the fixture name.
func Name() string { return "fixture" }
FIXTURE_EOF
    cat > "${d}/cmd/fixture/main.go" <<'FIXTURE_EOF'
// Command fixture is a clean corpus file for the drift-audit mutation harness.
package main

func main() {}
FIXTURE_EOF
    cat > "${d}/sdk/fixture/fixture.go" <<'FIXTURE_EOF'
// Package fixture is a clean corpus file for the drift-audit mutation harness.
package fixture
FIXTURE_EOF
    cat > "${d}/cmd/harbor/scaffold/version.go" <<'FIXTURE_EOF'
// Package scaffold is a clean corpus file for the drift-audit mutation harness.
package scaffold

// FallbackModuleVersion names the release a source build cannot name itself.
const FallbackModuleVersion = "v1.26.12"
FIXTURE_EOF

    cat > "${d}/web/console/src/routes/(console)/playground/+page.svelte" <<'FIXTURE_EOF'
<p>A fixture playground page with no placeholder bubble.</p>
FIXTURE_EOF

    # Local release tags are the ledger the scaffold-pin guard reads once
    # HARBOR_DRIFT_AUDIT_OFFLINE=1 removes the remote rung. Newest first:
    # v1.26.12 (the three-component MODULE pin — the grammar the ledger
    # accepts), v1.26.11 (one back), v1.26.10 (two back). The ledger ALSO
    # carries tags the guard must REFUSE: v1.27 (a two-component ARTIFACT
    # tag — a release identity, not a module version; a loose grammar
    # would make it `newest` and demand a two-component pin the proxy
    # cannot resolve), v9.9.9-rc1 (a prerelease) and v1.27.0.1 (a four-
    # component malformed dotted string). The pristine corpus only passes
    # if the grammar accepts the three-component form AND refuses all
    # three junk tags — the v1.28 artifact-vs-module split (the two-
    # component tag must not shift `newest`) is this baseline, mirrored.
    (
        cd "${d}"
        git init -q .
        git -c user.email='fixture@harbor.invalid' -c user.name='fixture' \
            commit -q --allow-empty -m 'fixture'
        git tag v1.26.10
        git tag v1.26.11
        git tag v1.26.12
        git tag v1.27
        git tag v9.9.9-rc1
        git tag v1.27.0.1
    ) >/dev/null 2>&1
}

build_template

# The subject must be the REAL script, not a stale or truncated copy. A silent
# `cp` failure would leave every case below verifying nothing.
if cmp -s "${AUDIT}" "${TEMPLATE}/scripts/drift-audit.sh"; then
    ok 'phase 224: the fixture runs the REAL scripts/drift-audit.sh (byte-identical copy, not a re-implementation)'
else
    fail "phase 224: the fixture's copy of ${AUDIT} differs from the original — every case below would be testing something other than the shipped audit"
    smoke_summary || true
    exit 1
fi

# ----------------------------------------------------------------------------
# run_audit <corpus-dir> <output-path> — run the audit inside the corpus and
# echo its exit code.
#
# HARBOR_DRIFT_AUDIT_OFFLINE=1 removes the `git ls-remote` rung of the
# release-ledger lookup: the fixture has no remote, the call would be a network
# round trip on every one of the ~20 runs below, and determinism matters more
# here than exercising a rung this corpus cannot populate (named in the census).
# ----------------------------------------------------------------------------
run_audit() {
    local dir="$1" out="$2" rc=0
    (
        cd "${dir}"
        HARBOR_DRIFT_AUDIT_OFFLINE=1 bash scripts/drift-audit.sh
    ) >"${out}" 2>&1 || rc=$?
    printf '%s' "${rc}"
}

BASE_OUT="${WORK}/baseline.out"
BASE_RC="$(run_audit "${TEMPLATE}" "${BASE_OUT}")"
if [ "${BASE_RC}" -ne 0 ]; then
    fail "phase 224: the PRISTINE fixture corpus does not pass drift-audit (exit ${BASE_RC}). Every mutation case below would be meaningless — a FAIL could no longer be attributed to the mutation. Audit output follows."
    head -80 "${BASE_OUT}" | sed 's/^/       /'
    smoke_summary || true
    exit 1
fi
ok 'phase 224: the pristine fixture corpus passes drift-audit clean — mutations below are attributable'

# ----------------------------------------------------------------------------
# expect_caught <label> <severity> <ok-marker> <bad-marker> <mutate-fn>
#
# The four assertions per case, all of which must hold:
#   a. the CLEAN corpus printed this guard's OK line   (the guard runs at all)
#   b. the CLEAN corpus did NOT print its bad line     (the baseline is clean)
#   c. the MUTATED corpus printed `[<severity>] …<bad-marker>…`
#   d. the audit's exit status matches the severity (FAIL => non-zero,
#      WARN => zero — a WARN must not fail the gate)
#
# (c) is keyed on the SPECIFIC guard's message, never on the exit code alone.
# That distinction is load-bearing: a mutation that happens to trip some OTHER
# guard would make a bare exit-code check report "caught!" while the guard under
# test slept — the same class of false pass this phase exists to remove.
#
# <ok-marker> may be empty for a guard that prints nothing on success. Only the
# negative half is observable for those, and the OK message says so rather than
# implying a verification that did not happen.
# ----------------------------------------------------------------------------
CASES=0
SEEN_CASES=''
# One registry is the denominator for BOTH coverage dimensions below:
#
#   guard-id | drift-audit OK signature (or - for FAIL-only/delegated guards)
#            | ^-separated mutation cases
#
# Deriving EXPECTED_CASES and CENSUS_SIGNATURES from this single table avoids
# the old three-list failure mode where a whole expect_caught block, its case
# manifest row, or its OK signature could disappear independently and leave a
# self-consistent but incomplete count. Markdownlint remains registered even
# when npx is unavailable; its conditional branch records the named SKIP as a
# seen case, so deleting that entire branch is still red.
GUARD_REGISTRY='mirror|mirror invariant|mirror invariant
required-files|required:|required design files
plan-smoke|plan ↔ smoke pair OK|plan-to-smoke pairing
plan-headings|all required headings present|required plan headings
nul-byte|-|NUL byte in a phase plan
rfc-references|RFC reference(s) resolve|RFC cross-reference resolution
brief-references|brief reference(s) resolve|brief cross-reference resolution
population|population census: every scanned corpus is non-empty|population census (a scanned corpus went empty)
forbidden-name|forbidden-name scan clean|forbidden-name scan (root-docs limb)^forbidden-name scan (phase-plan limb)^forbidden-name scan (research-brief limb)^forbidden-name scan (internal/ limb)^forbidden-name scan (cmd/ limb)
make-target|Makefile has drift-audit target|Makefile drift-audit target
preflight-headers|PREFLIGHT_REQUIRES header present|PREFLIGHT_REQUIRES header absent^PREFLIGHT_REQUIRES value unrecognised
skills-frontmatter|-|operator-skill frontmatter^skills-frontmatter delegation (executable bit lost)
body-identity|-|body-identity delegation (executable bit lost)^body-identity delegation (helper missing)^body-identity delegation (helper reports a violation)
playground|no playground placeholder text|playground placeholder regression
markdownlint|markdownlint (pinned cli2|markdownlint parity wiring
godoc|godoc hygiene: no internal phase jargon|godoc jargon scan^godoc jargon scan (test-path anchoring)^godoc jargon scan (cmd/ root)^godoc jargon scan (sdk/ root, the D-282 extension)
scaffold-pin|is a published release|scaffold pin names a phantom release^scaffold pin trails by two releases
release-ledger|release ledger: CHANGELOG.md carries|release ledger vs CHANGELOG
smoke-regex|smoke regex portability: no non-portable|smoke regex portability (scripts/smoke/ root)^smoke regex portability (scripts/ root)'
MKTEMP_GUARD_ID='mk''temp'
GUARD_REGISTRY="${GUARD_REGISTRY}
${MKTEMP_GUARD_ID}|every ${MKTEMP_GUARD_ID} template carries trailing X|${MKTEMP_GUARD_ID} template portability (scripts/smoke/ root)^${MKTEMP_GUARD_ID} template portability (scripts/ root)"

EXPECTED_CASES=()
CENSUS_SIGNATURES=()
while IFS='|' read -r guard_id signature registered_cases; do
    [ -n "${guard_id}" ] || continue
    if [ "${signature}" != '-' ]; then
        CENSUS_SIGNATURES+=("${signature}")
    fi
    old_ifs="${IFS}"
    IFS='^'
    # shellcheck disable=SC2206 # intentional delimiter split; labels contain spaces.
    split_cases=(${registered_cases})
    IFS="${old_ifs}"
    for registered_case in "${split_cases[@]}"; do
        EXPECTED_CASES+=("${registered_case}")
    done
done <<EOF
${GUARD_REGISTRY}
EOF

note_seen_case() {
    SEEN_CASES="${SEEN_CASES}
$1"
}

expect_caught() {
    local label="$1" sev="$2" ok_marker="$3" bad_marker="$4" mutate="$5"
    CASES=$((CASES + 1))
    note_seen_case "${label}"

    local clean_ok=0 clean_bad=0
    if [ -z "${ok_marker}" ]; then
        clean_ok=1
    elif grep -qF -- "${ok_marker}" "${BASE_OUT}"; then
        clean_ok=1
    fi
    if grep -qF -- "${bad_marker}" "${BASE_OUT}"; then
        clean_bad=1
    fi

    local dir="${WORK}/case-${CASES}" out="${WORK}/case-${CASES}.out" rc caught=0
    cp -R "${TEMPLATE}" "${dir}"
    "${mutate}" "${dir}"
    rc="$(run_audit "${dir}" "${out}")"
    if grep -F -- "${bad_marker}" "${out}" | grep -qF -- "[${sev}]"; then
        caught=1
    fi

    if [ "${clean_ok}" -ne 1 ]; then
        fail "phase 224 [${label}]: the CLEAN fixture never printed this guard's OK line ('${ok_marker}') — the guard does not run at all against this corpus, so the mutation below proves nothing about it"
    elif [ "${clean_bad}" -ne 0 ]; then
        fail "phase 224 [${label}]: the CLEAN fixture already reports '${bad_marker}' — the baseline is not clean for this guard, so a mutated FAIL would not be attributable to the mutation"
    elif [ "${caught}" -ne 1 ]; then
        fail "phase 224 [${label}]: MUTATION NOT CAUGHT. The corpus carries the exact defect this guard names and drift-audit printed no [${sev}] line matching '${bad_marker}' (exit ${rc}). THIS GUARD IS INERT — repair it in ${AUDIT}; do not weaken this case."
        printf -- '--- mutated-corpus audit output (tail) ---\n'
        tail -25 "${out}" | sed 's/^/       /'
    elif [ "${sev}" = 'FAIL' ] && [ "${rc}" -eq 0 ]; then
        fail "phase 224 [${label}]: drift-audit printed the FAIL line but exited 0 — the summary's exit plumbing dropped it, so 'make preflight' would go green on a real violation"
    elif [ "${sev}" = 'WARN' ] && [ "${rc}" -ne 0 ]; then
        fail "phase 224 [${label}]: this guard is a WARN, but the mutated audit exited ${rc} — a WARN must not fail the gate"
    else
        ok "phase 224 [${label}]: guard is LIVE (clean corpus OK; mutated corpus ${sev})"
    fi
}

# ----------------------------------------------------------------------------
# Mutations. One function per case; each takes the corpus directory. None of
# them touches the working tree — every path is rooted at "$1".
#
# In-place edits go through a temp file plus `mv` rather than `sed -i`: BSD sed
# and GNU sed disagree on `-i`'s argument, which is the same macOS/Linux
# divergence class the audit's own portability guards exist to catch.
# ----------------------------------------------------------------------------
FIXTURE_PLAN='docs/plans/phase-01-fixture.md'
FIXTURE_GO='internal/fixture/fixture.go'
FIXTURE_CMD_GO='cmd/fixture/main.go'
FIXTURE_SDK_GO='sdk/fixture/fixture.go'
FIXTURE_BRIEF='docs/research/06-fixture.md'

mut_mirror()        { printf 'A line CLAUDE.md carries and AGENTS.md does not.\n' >> "$1/CLAUDE.md"; }
mut_required_file() { rm -f "$1/docs/glossary.md"; }
mut_plan_smoke()    { rm -f "$1/scripts/smoke/phase-01.sh"; }
mut_rfc_ref()       { printf '\nA stale anchor: RFC §99.99 does not exist.\n' >> "$1/${FIXTURE_PLAN}"; }
mut_brief_ref()     { printf '\nA stale citation: brief 99 does not exist.\n' >> "$1/${FIXTURE_PLAN}"; }
mut_markdownlint()  { : > "$1/.markdownlint-should-fail"; }
mut_playground()    { printf '<p>%s</p>\n' 'Message accepted by the Runtime.' \
                        >> "$1/web/console/src/routes/(console)/playground/+page.svelte"; }

# --- forbidden-name scan: ONE case per POPULATION LIMB -----------------------
# The scan assembles its file list from five separate blocks — a literal list
# of root design docs, the phase-plan glob, the research-brief glob, a `find`
# over internal/, and a `find` over cmd/. Deleting any ONE block leaves the
# other four reporting the same single OK line, so a case per limb is what
# makes a narrowing visible. The `cmd/` limb had no case at all: deleting its
# whole block produced OK 25 / SKIP 0 / FAIL 0.
mut_forbidden_rootdoc() { printf '\n%s\n' "${FORBIDDEN_WORD}" >> "$1/README.md"; }
mut_forbidden_doc()     { printf '\n%s\n' "${FORBIDDEN_WORD}" >> "$1/${FIXTURE_PLAN}"; }
mut_forbidden_brief()   { printf '\n%s\n' "${FORBIDDEN_WORD}" >> "$1/${FIXTURE_BRIEF}"; }
mut_forbidden_go()      { printf '\n// %s\n' "${FORBIDDEN_WORD}" >> "$1/${FIXTURE_GO}"; }
mut_forbidden_cmd()     { printf '\n// %s\n' "${FORBIDDEN_WORD}" >> "$1/${FIXTURE_CMD_GO}"; }

# --- portability guards: ONE case per POPULATION ROOT ------------------------
# The escape guard globs `scripts/smoke/*.sh scripts/*.sh`; the mktemp guard
# greps `scripts` RECURSIVELY. Planting only under scripts/smoke/ means a
# narrowing to smoke-only is invisible — and scripts/ root is where
# drift-audit.sh, preflight.sh and the D-374 helper actually live, so it is
# the limb whose loss would matter most.
mut_bad_escape()      { cp "${PAYLOADS}/bad-escape.sh.txt" "$1/scripts/smoke/phase-02.sh"; }
mut_bad_escape_root() { cp "${PAYLOADS}/bad-escape.sh.txt" "$1/scripts/portability-fixture.sh"; }
mut_bad_mktemp()      { cp "${PAYLOADS}/bad-mktemp.sh.txt" "$1/scripts/smoke/phase-03.sh"; }
mut_bad_mktemp_root() { cp "${PAYLOADS}/bad-mktemp.sh.txt" "$1/scripts/mktemp-fixture.sh"; }

# --- delegated guards: the VANISH shape --------------------------------------
# Both delegations were `if [ -x helper ]; then … fi` with no else. A lost
# executable bit removed the entire check with no failure, no skip and no
# output — the guard did not fail, it disappeared. The body-identity one is
# worse still: it emits no `ok` of its own, so the coverage census below could
# not see it either, and the harness went on claiming full coverage of a set
# that did not include it.
mut_frontmatter_notexec()   { chmod -x "$1/scripts/skills/check-frontmatter.sh"; }
mut_bodyidentity_notexec()  { chmod -x "$1/scripts/check-smoke-body-identity.sh"; }
mut_bodyidentity_missing()  { rm -f "$1/scripts/check-smoke-body-identity.sh"; }
mut_bodyidentity_reports()  {
    cat > "$1/scripts/check-smoke-body-identity.sh" <<'MUT_EOF'
#!/usr/bin/env bash
echo '[FAIL] smoke body-identity: fixture-induced violation'
exit 1
MUT_EOF
    chmod +x "$1/scripts/check-smoke-body-identity.sh"
}

# --- population census -------------------------------------------------------
# Removing a whole scanned corpus. Before the census existed this was SILENT:
# the godoc scan's grep over a missing sdk/ errors into /dev/null under
# `|| true`, the hit count stays zero, and the guard prints its OK having read
# nothing.
mut_census_empty_population() { rm -rf "$1/sdk"; }

# A production comment naming an integration test by its `phaseNN_*_test.go`
# filename. The v1.25 checkpoint's F5 finding: an UNANCHORED `grep -v
# '_test\.go'` discards this VIOLATING line because its body mentions a test
# filename, even though its PATH is a production file. Twenty-six real hits hid
# behind that. This case fails against the unanchored form and passes against
# the shipped `^[^:]*_test\.go:` form.
mut_godoc_jargon_plain() {
    printf '\n// Phase 42 introduced this helper.\n' >> "$1/${FIXTURE_GO}"
}
mut_godoc_jargon_testpath() {
    printf '\n// Rationale: D-999. See fixture_regression_test.go for the round trip.\n' \
        >> "$1/${FIXTURE_GO}"
}

# The godoc scan's roots are `internal/ cmd/ sdk/`. Both cases below plant in a
# root the harness previously never touched, so narrowing the argument list
# turns them red instead of passing green. `sdk/` is D-282's explicit
# adopter-facing extension and is the root a "tidy-up" is most likely to drop;
# dropping it produced OK 25 / SKIP 0 / FAIL 0.
#
# Each also uses a DIFFERENT pattern from the guard's list, so between them the
# four cases exercise four of the five patterns rather than the same one three
# times — and the bad markers stay per-case attributable, which the shared
# `found in non-test Go source` prefix alone would not be.
mut_godoc_jargon_cmd() {
    printf '\n// Delivered in Wave 8 of the rollout.\n' >> "$1/${FIXTURE_CMD_GO}"
}
mut_godoc_jargon_sdk() {
    printf '\n// The rationale is recorded in brief 07.\n' >> "$1/${FIXTURE_SDK_GO}"
}

mut_headings() {
    local p="$1/${FIXTURE_PLAN}"
    grep -v '^## Summary$' "${p}" > "${p}.tmp"
    mv "${p}.tmp" "${p}"
}

# A single NUL byte. `printf '\0'` is not portable across bash builds, so the
# byte is emitted through awk's `%c`, which is.
mut_nul_byte() {
    local p="$1/${FIXTURE_PLAN}"
    printf '\nA control byte follows: ' >> "${p}"
    awk 'BEGIN { printf "%c", 0 }' >> "${p}"
    printf '\n' >> "${p}"
}

mut_makefile_target() {
    local m="$1/Makefile"
    sed 's/^drift-audit:/drift-audit-renamed:/' "${m}" > "${m}.tmp"
    mv "${m}.tmp" "${m}"
}

mut_preflight_header_missing() {
    local s="$1/scripts/smoke/phase-01.sh"
    grep -v 'PREFLIGHT_REQUIRES' "${s}" > "${s}.tmp"
    mv "${s}.tmp" "${s}"
}

mut_preflight_header_bogus() {
    local s="$1/scripts/smoke/phase-01.sh"
    sed 's/PREFLIGHT_REQUIRES: static-only/PREFLIGHT_REQUIRES: sometimes/' "${s}" > "${s}.tmp"
    mv "${s}.tmp" "${s}"
}

mut_skill_frontmatter() {
    local f="$1/docs/skills/fixture-skill/SKILL.md"
    sed 's/^license: Apache-2.0$/license: MIT/' "${f}" > "${f}.tmp"
    mv "${f}.tmp" "${f}"
}

mut_pin_phantom() {
    local v="$1/cmd/harbor/scaffold/version.go"
    sed 's/"v1.26.12"/"v0.0.1"/' "${v}" > "${v}.tmp"
    mv "${v}.tmp" "${v}"
}

mut_pin_trails() {
    local v="$1/cmd/harbor/scaffold/version.go"
    # v1.26.10 sits TWO releases back from v1.26.12 (index 2 in the ledger),
    # so this is a genuine two-release trail — the numeric ordering across
    # the three-component grammar (v1.26.12 > v1.26.11 > v1.26.10) is what
    # makes it one.
    sed 's/"v1.26.12"/"v1.26.10"/' "${v}" > "${v}.tmp"
    mv "${v}.tmp" "${v}"
}

mut_release_ledger() {
    local c="$1/CHANGELOG.md"
    grep -v '^## .1.26.12.$' "${c}" > "${c}.tmp"
    mv "${c}.tmp" "${c}"
}

# ----------------------------------------------------------------------------
# The cases. Ordered as the guards appear in scripts/drift-audit.sh.
# ----------------------------------------------------------------------------
expect_caught 'mirror invariant' FAIL \
    'AGENTS.md == CLAUDE.md (mirror invariant)' \
    'AGENTS.md and CLAUDE.md have drifted' \
    mut_mirror

expect_caught 'required design files' FAIL \
    'required: docs/glossary.md' \
    'missing required file: docs/glossary.md' \
    mut_required_file

expect_caught 'plan-to-smoke pairing' FAIL \
    'plan ↔ smoke pair OK' \
    'plan exists but scripts/smoke/phase-01.sh is missing' \
    mut_plan_smoke

expect_caught 'required plan headings' FAIL \
    'all required headings present' \
    'missing required heading: ## Summary' \
    mut_headings

# The NUL guard prints nothing on success — it exists because a control byte
# silently voids the two cross-reference checks below it, and the empty
# ok-marker records that only its negative half is observable.
expect_caught 'NUL byte in a phase plan' FAIL \
    '' \
    'contains a NUL byte' \
    mut_nul_byte

expect_caught 'RFC cross-reference resolution' FAIL \
    'RFC reference(s) resolve' \
    "stale reference 'RFC §99.99'" \
    mut_rfc_ref

expect_caught 'brief cross-reference resolution' FAIL \
    'brief reference(s) resolve' \
    "stale reference 'brief 99'" \
    mut_brief_ref

expect_caught 'population census (a scanned corpus went empty)' FAIL \
    'population census: every scanned corpus is non-empty' \
    'the Go source under sdk/ (the D-282 scan extension) population is EMPTY' \
    mut_census_empty_population

expect_caught 'forbidden-name scan (root-docs limb)' FAIL \
    'forbidden-name scan clean' \
    'present in README.md' \
    mut_forbidden_rootdoc

expect_caught 'forbidden-name scan (phase-plan limb)' FAIL \
    'forbidden-name scan clean' \
    'present in docs/plans/phase-01-fixture.md' \
    mut_forbidden_doc

expect_caught 'forbidden-name scan (research-brief limb)' FAIL \
    'forbidden-name scan clean' \
    'present in docs/research/06-fixture.md' \
    mut_forbidden_brief

expect_caught 'forbidden-name scan (internal/ limb)' FAIL \
    'forbidden-name scan clean' \
    'present in internal/fixture/fixture.go' \
    mut_forbidden_go

expect_caught 'forbidden-name scan (cmd/ limb)' FAIL \
    'forbidden-name scan clean' \
    'present in cmd/fixture/main.go' \
    mut_forbidden_cmd

expect_caught 'Makefile drift-audit target' WARN \
    'Makefile has drift-audit target' \
    'Makefile is missing a drift-audit target' \
    mut_makefile_target

expect_caught 'PREFLIGHT_REQUIRES header absent' FAIL \
    'PREFLIGHT_REQUIRES header present + recognised' \
    "missing '# PREFLIGHT_REQUIRES" \
    mut_preflight_header_missing

expect_caught 'PREFLIGHT_REQUIRES value unrecognised' FAIL \
    'PREFLIGHT_REQUIRES header present + recognised' \
    "unrecognised PREFLIGHT_REQUIRES value 'sometimes'" \
    mut_preflight_header_bogus

expect_caught 'operator-skill frontmatter' FAIL \
    'skills frontmatter audit:' \
    'have malformed frontmatter' \
    mut_skill_frontmatter

# --- the two DELEGATED guards, verified at the delegation --------------------
# Both shipped as `if [ -x helper ]; then … fi` with NO else, so a guard that
# could not run was indistinguishable from one that passed. These four cases
# are the ones that would have caught it.
expect_caught 'skills-frontmatter delegation (executable bit lost)' FAIL \
    'skills frontmatter audit:' \
    'delegated guard unavailable: scripts/skills/check-frontmatter.sh' \
    mut_frontmatter_notexec

expect_caught 'body-identity delegation (executable bit lost)' FAIL \
    '' \
    'delegated guard unavailable: scripts/check-smoke-body-identity.sh' \
    mut_bodyidentity_notexec

expect_caught 'body-identity delegation (helper missing)' FAIL \
    '' \
    'delegated guard unavailable: scripts/check-smoke-body-identity.sh' \
    mut_bodyidentity_missing

expect_caught 'body-identity delegation (helper reports a violation)' FAIL \
    '' \
    'a smoke body sends `identity` to a Protocol method' \
    mut_bodyidentity_reports

expect_caught 'playground placeholder regression' FAIL \
    'no playground placeholder text' \
    'is forbidden — see phase 106' \
    mut_playground

# The markdownlint guard is gated on npx EXISTING on the host: without it the
# audit itself WARNs and prints no OK line, so there is nothing to mutate
# against. That is the audit's own documented degradation, not a gap in this
# harness — but a SKIP must say which guard went unverified and why.
if command -v npx >/dev/null 2>&1; then
    expect_caught 'markdownlint parity wiring' FAIL \
        'markdownlint (pinned cli2, CI-parity globs) clean' \
        'markdownlint found violations' \
        mut_markdownlint
else
    note_seen_case 'markdownlint parity wiring'
    skip 'phase 224 [markdownlint parity wiring]: npx is absent, so drift-audit WARN-skips this guard on this host and it cannot be mutation-verified here (CI has node and does verify it)'
fi

expect_caught 'godoc jargon scan' FAIL \
    'godoc hygiene: no internal phase jargon' \
    "pattern '[Pp]hase[ -]?[0-9]+'" \
    mut_godoc_jargon_plain

expect_caught 'godoc jargon scan (test-path anchoring)' FAIL \
    'godoc hygiene: no internal phase jargon' \
    "pattern '\bD-[0-9]+'" \
    mut_godoc_jargon_testpath

expect_caught 'godoc jargon scan (cmd/ root)' FAIL \
    'godoc hygiene: no internal phase jargon' \
    "pattern '\b(Wave|Round|Stage)[ -][0-9A-Z]+'" \
    mut_godoc_jargon_cmd

expect_caught 'godoc jargon scan (sdk/ root, the D-282 extension)' FAIL \
    'godoc hygiene: no internal phase jargon' \
    "pattern '\b[Bb]rief [0-9]+'" \
    mut_godoc_jargon_sdk

expect_caught 'scaffold pin names a phantom release' FAIL \
    'is a published release' \
    'names no published release' \
    mut_pin_phantom

expect_caught 'scaffold pin trails by two releases' FAIL \
    'is a published release' \
    'trails the newest published release' \
    mut_pin_trails

expect_caught 'release ledger vs CHANGELOG' FAIL \
    'release ledger: CHANGELOG.md carries a section' \
    'release ledger: v1.26.12 is published' \
    mut_release_ledger

expect_caught 'smoke regex portability (scripts/smoke/ root)' FAIL \
    'smoke regex portability: no non-portable' \
    'scripts/smoke/phase-02.sh:' \
    mut_bad_escape

expect_caught 'smoke regex portability (scripts/ root)' FAIL \
    'smoke regex portability: no non-portable' \
    'scripts/portability-fixture.sh:' \
    mut_bad_escape_root

expect_caught 'mktemp template portability (scripts/smoke/ root)' FAIL \
    'every mktemp template carries trailing X' \
    'scripts/smoke/phase-03.sh:' \
    mut_bad_mktemp

expect_caught 'mktemp template portability (scripts/ root)' FAIL \
    'every mktemp template carries trailing X' \
    'scripts/mktemp-fixture.sh:' \
    mut_bad_mktemp_root

# ----------------------------------------------------------------------------
# COVERAGE CENSUS — the mechanical half of "say what you do not cover".
#
# Silent partial coverage is the defect this phase removes; a harness that
# verified nine of fourteen guards while reading like a full sweep would be the
# same bug in a new place. So the guards verified above are cross-checked
# against every `ok` call drift-audit makes:
#
#   - an `ok` line no signature claims  => a guard was added with NO mutation
#     case, and this smoke FAILs naming it.
#   - a signature no `ok` line matches  => a guard was renamed or deleted and
#     the case above is now testing a message that no longer exists.
#
# THREE verified guards are deliberately absent from this list because they
# print no `ok` of their own: the NUL-byte check (FAIL-only) and the two
# DELEGATED checks, whose OK lines are printed by the child script rather than
# by drift-audit's own `ok`. All three have cases above. That is why the census
# total and the case count differ, and the numbers are printed rather than left
# for the reader to reconstruct.
#
# THE THIRD ONE IS WHY THIS PARAGRAPH IS NOT DECORATION. The smoke
# body-identity delegation was added to drift-audit seven commits after this
# harness was written. It emits no `ok`, so the mechanical cross-check below
# could not see it; the hand-written exception list said "two"; and this script
# went on printing "18 of 18 guards, 0 declared uncovered" while a nineteenth
# guard sat entirely unverified — and that guard was itself the one with the
# vanishing `if [ -x … ]`. An `ok`-keyed census cannot see a guard that emits
# no `ok`, so the hand-maintained half of this list is LOAD-BEARING and must be
# revisited whenever a delegated or FAIL-only guard is added. That limitation
# is now stated here rather than implied by a number.
# ----------------------------------------------------------------------------
# The execution manifest is a second, independent denominator. The signature
# census below cannot see a deleted expect_caught block when its guard still
# emits an OK line; this check makes that whole-case deletion fail loudly.
missing_case=''
for expected_case in "${EXPECTED_CASES[@]}"; do
    if ! printf '%s\n' "${SEEN_CASES}" | grep -qxF -- "${expected_case}"; then
        missing_case="${missing_case} '${expected_case}'"
    fi
done
if [ -n "${missing_case}" ]; then
    fail "phase 224: declared mutation case(s) did not execute —${missing_case}. A whole-case deletion must be red, not disappear from the coverage census."
fi

# The pattern deliberately carries no quote character: an ERE holding one would
# put a single quote on this line right after a `grep -…E` token, which is the
# exact shape the audit's own escape guard scans for. `ok` followed by a space
# is enough — the `ok()` DEFINITION is `ok()`, with a paren, and does not match.
AUDIT_OK_LINES="$(grep -nE '^[[:space:]]*ok[[:space:]]' "${AUDIT}" || true)"
if [ -z "${AUDIT_OK_LINES}" ]; then
    fail "phase 224: found no 'ok' calls in ${AUDIT} — the census cannot run, and an unverifiable check must not read as a passing one"
else
    unclaimed=''
    ok_line_count=0
    while IFS= read -r line; do
        [ -n "${line}" ] || continue
        ok_line_count=$((ok_line_count + 1))
        matched=0
        for sig in "${CENSUS_SIGNATURES[@]}"; do
            if printf '%s\n' "${line}" | grep -qF -- "${sig}"; then
                matched=1
                break
            fi
        done
        if [ "${matched}" -eq 0 ]; then
            unclaimed="${unclaimed}
       ${line}"
        fi
    done <<EOF
${AUDIT_OK_LINES}
EOF

    orphaned=''
    for sig in "${CENSUS_SIGNATURES[@]}"; do
        if ! printf '%s\n' "${AUDIT_OK_LINES}" | grep -qF -- "${sig}"; then
            orphaned="${orphaned} '${sig}'"
        fi
    done

    if [ -n "${unclaimed}" ]; then
        fail "phase 224: ${AUDIT} emits OK line(s) that NO mutation case above verifies — a new guard shipped with zero coverage, which is exactly the silent-partial-coverage defect this phase removes. Add a case to this script (or declare it uncovered here with a reason):${unclaimed}"
    elif [ -n "${orphaned}" ]; then
        fail "phase 224: this harness expects OK line(s) that ${AUDIT} no longer emits —${orphaned}. A guard was renamed or deleted and the matching case above is now asserting against a message that does not exist."
    else
        ok "phase 224: coverage census — all ${ok_line_count} of ${ok_line_count} guards that report an OK are mutation-verified above, plus the NUL-byte guard and the TWO delegated guards (skills-frontmatter, D-374 smoke body-identity; none of the three prints an OK of its own): ${CASES} mutations over $((ok_line_count + 3)) guard units, 0 uncovered. Two are covered at the DELEGATION only — a missing helper, a lost executable bit, or a non-zero exit — not in the helpers' own detection logic, which carries its own guards; see build_template's stand-in note."
    fi
fi

smoke_summary
