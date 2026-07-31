#!/usr/bin/env bash
# PREFLIGHT_REQUIRES: static-only
#
# Phase 223 — drain the inert-smoke baseline.
#
# This is the meta-smoke: it guards the smoke harness itself rather than a
# runtime surface. Every assertion below reads a repository file that exists
# unconditionally, so there is no "surface not yet implemented" arm — a SKIP
# here would be the exact defect this phase closes (AGENTS.md §4.2 item 5).
#
# The negative arms are FAIL, not SKIP. While phase 223 was in flight they
# were SKIPs that printed the measured gap (24 entries / 233 of 339 rows / six
# unnamed status words / no no-summary guard). Now that the properties hold,
# a SKIP would be indistinguishable from a pass and would let every one of
# them regress silently — which is the shape this phase exists to remove. Each
# arm below is mutation-verified: break the property, watch OK -> FAIL.
#
# See docs/plans/phase-223-drain-the-inert-smoke-baseline.md
#   § "Smoke script additions" for the binding assertion list.

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "${ROOT}"

# shellcheck source=scripts/smoke/common.sh
source "scripts/smoke/common.sh"

BASELINE='scripts/smoke/inert-baseline.txt'
PREFLIGHT='scripts/preflight.sh'
MASTER='docs/plans/README.md'

# baseline_entries — echo the data lines (non-comment, non-blank, trimmed).
# ALWAYS returns 0: these scripts run under `set -e` and callers read this
# through a command substitution, so a grep "no match" status would abort
# the whole smoke on the (goal-state) empty file.
baseline_entries() {
    [ -f "${BASELINE}" ] || return 0
    grep -vE '^[[:space:]]*(#|$)' "${BASELINE}" 2>/dev/null \
        | sed -E 's/^[[:space:]]+//; s/[[:space:]]+$//' || true
    return 0
}

# ----------------------------------------------------------------------------
# 1. The baseline file itself is a required artifact of the gate. Deleting it
#    would silently disable the stale-entry sweep in scripts/preflight.sh,
#    which is a no-op when the file is absent.
# ----------------------------------------------------------------------------
assert_file "${BASELINE}" 'phase 223: the inert-smoke baseline file is present'

ENTRIES="$(baseline_entries)"
if [ -z "${ENTRIES}" ]; then
    ENTRY_COUNT=0
else
    ENTRY_COUNT="$(printf '%s\n' "${ENTRIES}" | wc -l | tr -d ' ')"
fi

# ----------------------------------------------------------------------------
# 2. Every listed entry names a script that EXISTS. The preflight stale-entry
#    sweep skips any entry whose file is missing (`[ -f "${entry}" ] ||
#    continue`), so a baseline line naming a deleted script rots forever and is
#    never reported stale. This guard is that missing check.
# ----------------------------------------------------------------------------
missing_entries=''
if [ "${ENTRY_COUNT}" -gt 0 ]; then
    while IFS= read -r entry; do
        [ -n "${entry}" ] || continue
        [ -f "${entry}" ] || missing_entries="${missing_entries} ${entry}"
    done <<EOF
${ENTRIES}
EOF
fi
if [ -n "${missing_entries}" ]; then
    fail "phase 223: baseline names script(s) that do not exist — the preflight stale sweep skips them by construction:${missing_entries}"
elif [ "${ENTRY_COUNT}" -eq 0 ]; then
    ok 'phase 223: no baseline entry can name a deleted script (the file carries no entries)'
else
    ok "phase 223: every baseline entry names an existing script (${ENTRY_COUNT} checked)"
fi

# ----------------------------------------------------------------------------
# 3. Every listed entry names a script whose phase is SHIPPED. A not-shipped
#    phase's all-SKIP skeleton is the §4.2 item 4 convention working correctly;
#    preflight classifies it INERT_PENDING and never asks the baseline about
#    it. Thirteen of the original 24 entries were exactly this — parked as
#    shipped-phase debt because the classifier could not read their row. This
#    is the guard that would have caught them at the moment they were added.
#
#    The row regex here is deliberately the SAME shape preflight uses
#    (`^\| *TOKEN *\|`, no `+`); assertion 4 below pins that the two agree.
# ----------------------------------------------------------------------------
unshipped_entries=''
if [ "${ENTRY_COUNT}" -gt 0 ]; then
    while IFS= read -r entry; do
        [ -n "${entry}" ] || continue
        tok="$(basename "${entry}" | sed 's/^phase-//; s/\.sh$//')"
        row="$(grep -m1 -E "^\| *${tok} *\|" "${MASTER}" 2>/dev/null || true)"
        st="$(printf '%s' "${row}" | sed -E 's/[[:space:]]*\|[[:space:]]*$//; s/.*\|[[:space:]]*//')"
        case "${st}" in
            Shipped*|shipped*) : ;;
            *) unshipped_entries="${unshipped_entries} ${entry}(status='${st:-<no row>}')" ;;
        esac
    done <<EOF
${ENTRIES}
EOF
fi
if [ -n "${unshipped_entries}" ]; then
    fail "phase 223: baseline entr(y|ies) for a phase that is NOT Shipped — an unshipped phase's skeleton belongs in INERT_PENDING, never in the baseline:${unshipped_entries}"
elif [ "${ENTRY_COUNT}" -eq 0 ]; then
    ok 'phase 223: no baseline entry can name an unshipped phase (the file carries no entries)'
else
    ok "phase 223: every baseline entry names a Shipped phase (${ENTRY_COUNT} checked)"
fi

# ----------------------------------------------------------------------------
# 4. THE DRAIN. The baseline's steady state is EMPTY. A non-empty file is
#    declared debt: every line in it is a shipped phase whose smoke asserts
#    nothing. FAIL, not SKIP — re-parking a script must cost a red preflight
#    and an explicit override, not a silent line.
# ----------------------------------------------------------------------------
if [ "${ENTRY_COUNT}" -eq 0 ]; then
    ok 'phase 223: inert-smoke baseline is fully drained (0 entries)'
else
    fail "phase 223: inert-smoke baseline carries ${ENTRY_COUNT} entr(y|ies) — its drained state is the steady state. Give the listed smoke(s) a real assertion and delete the line; do not park new debt here."
fi

# ----------------------------------------------------------------------------
# 5. The classifier can SEE every master-plan phase row. preflight's row regex
#    used to be `^\| *NN +\|`, which requires a space before the closing pipe.
#    The master plan writes both `| 104 |` and `| 85a|`; the second form was
#    invisible and fell through to the "no row -> treat as Shipped" arm. That
#    is how thirteen unshipped phases were parked in the baseline as
#    shipped-phase debt.
#
#    THE COUNT IS COMPUTED, NEVER ASSIGNED. The first cut of this guard
#    grepped preflight.sh for the fixed regex STRING and, on a match, set
#    `rows_seen="${rows_total}"` — so the comparison below was true BY
#    CONSTRUCTION and the guard could only ever detect a revert of that one
#    string. It could not detect the master-plan reformat its own comment
#    claimed it guarded, which is the failure this phase exists to remove:
#    a guard whose pass value is also its can't-tell value. (Found by the
#    v1.25 checkpoint's claims review; the same trap that had phase 221
#    writing a broken `go test` helper beside the fix for it.)
#
#    So: EXTRACT preflight's row regex from its source, substitute each phase
#    token the master plan actually carries, and RUN it against the master
#    plan. Both directions now fail — reverting preflight's regex, AND
#    reformatting a master-plan row into a shape the regex cannot see.
# ----------------------------------------------------------------------------
if [ -f "${MASTER}" ] && [ -f "${PREFLIGHT}" ]; then
    # Every phase row, by a DELIBERATELY PERMISSIVE scan — this is the
    # denominator, so it must see rows preflight's regex might not.
    rows_total=0
    rows_seen=0
    # `^\| *${n} *\|` today. The `${n}` survives as LITERAL TEXT — the sed
    # program is single-quoted so the shell expands nothing, and a command
    # substitution's OUTPUT is never re-expanded. It is substituted per phase
    # token in the loop below.
    ROW_RE_TEMPLATE="$(sed -nE 's#.*grep -m1 -E "([^"]*)" docs/plans/README\.md.*#\1#p' "${PREFLIGHT}" | head -1)"
    ROW_RE_NEEDLE='${n}'
    if [ -z "${ROW_RE_TEMPLATE}" ] || [ "${ROW_RE_TEMPLATE}" = "${ROW_RE_TEMPLATE#*${ROW_RE_NEEDLE}}" ]; then
        fail "phase 223: could not extract preflight's master-plan row regex from ${PREFLIGHT} (got '${ROW_RE_TEMPLATE:-none}') — this guard cannot run, and an unverifiable guard must not read as a passing one"
    else
        while IFS= read -r tok; do
            [ -n "${tok}" ] || continue
            rows_total=$((rows_total + 1))
            row_re="${ROW_RE_TEMPLATE//${ROW_RE_NEEDLE}/${tok}}"
            if grep -qE -- "${row_re}" "${MASTER}"; then
                rows_seen=$((rows_seen + 1))
            fi
        done <<EOF
$(grep -oE '^\|[[:space:]]*[0-9][0-9a-z-]*' "${MASTER}" | sed -E 's/^\|[[:space:]]*//' || true)
EOF
    fi
    if [ "${rows_total}" -gt 0 ] && [ "${rows_seen}" -eq "${rows_total}" ]; then
        ok "phase 223: every master-plan phase row is resolvable by the preflight row regex (${rows_seen}/${rows_total})"
    else
        fail "phase 223: only ${rows_seen}/${rows_total} master-plan phase rows are resolvable by the preflight row regex ('${ROW_RE_TEMPLATE:-none}') — an unresolvable row falls into the no-row arm and defaults to Shipped, which mis-parks unshipped phases as shipped-phase debt. Either the regex regressed (the historical case: '+\\|' instead of '*\\|', 233/339) or a row was reformatted into a shape it cannot see."
    fi
else
    fail "phase 223: ${MASTER} or ${PREFLIGHT} is missing — the shipped/unshipped classifier has nothing to read"
fi

# ----------------------------------------------------------------------------
# 6. The classifier's NOT-SHIPPED vocabulary covers every non-Shipped status
#    word the master plan actually uses. preflight once knew only Pending /
#    Post-V1 / Deferred and defaulted everything else to Shipped, so `Ready`,
#    `Cut`, `Revisit`, `Superseded`, `Reverted` and `Deprecated` — 13 rows —
#    all classified as shipped.
#
#    Computed FROM the master plan, not hard-coded, so a new status word
#    introduced without teaching the classifier trips this guard.
#
#    The needle is searched inside the `phase_status_arm` FUNCTION BODY, not
#    the whole of preflight.sh: a status word can perfectly well appear in a
#    comment (this file's own history is the proof — the first cut of this
#    guard matched `smoke_summary` inside the comment describing the UNFIXED
#    behaviour and reported OK before the repair existed). A guard whose pass
#    value is also its can't-tell value is the thing this phase removes.
# ----------------------------------------------------------------------------
if [ -f "${MASTER}" ] && [ -f "${PREFLIGHT}" ]; then
    ARM_BODY="$(awk '/^phase_status_arm\(\) \{/,/^\}/' "${PREFLIGHT}" || true)"
    if [ -z "${ARM_BODY}" ]; then
        fail "phase 223: ${PREFLIGHT} has no phase_status_arm() function — the not-shipped status vocabulary has no single home to check"
    else
        unknown_status=''
        while IFS= read -r st; do
            [ -n "${st}" ] || continue
            case "${st}" in
                Shipped*|shipped*) continue ;;
            esac
            # The classifier's case arms match on the status's leading word.
            word="$(printf '%s' "${st}" | awk '{print $1}')"
            [ -n "${word}" ] || continue
            if ! printf '%s\n' "${ARM_BODY}" | grep -qF -- "${word}"; then
                case " ${unknown_status} " in
                    *" '${word}' "*) : ;;
                    *) unknown_status="${unknown_status} '${word}'" ;;
                esac
            fi
        done <<EOF
$(grep -E '^\|[[:space:]]*[0-9][0-9a-z-]*[[:space:]]*\|' "${MASTER}" \
    | sed -E 's/[[:space:]]*\|[[:space:]]*$//; s/.*\|[[:space:]]*//' \
    | sort -u || true)
EOF
        if [ -n "${unknown_status}" ]; then
            fail "phase 223: master-plan status word(s) phase_status_arm does not name —${unknown_status} — each defaults to Shipped, so an unshipped phase's skeleton reads as shipped-phase debt. Teach the classifier the word."
        else
            ok 'phase 223: every non-Shipped master-plan status word is named by phase_status_arm'
        fi
    fi
else
    # No `else` here meant a missing master plan or a missing preflight.sh
    # produced SILENCE — assertion 6 simply vanished from the counters, which
    # reads exactly like a script that never had it. Both files are repository
    # fixtures, not optional surfaces; their absence is a FAIL.
    fail "phase 223: cannot check the status vocabulary — missing $( [ -f "${MASTER}" ] || printf '%s ' "${MASTER}"; [ -f "${PREFLIGHT}" ] || printf '%s ' "${PREFLIGHT}" )"
fi

# ----------------------------------------------------------------------------
# 7. A smoke that exits 0 without printing a summary must FAIL, not vanish.
#    assess_smoke_output used to return early when there was no OK:/FAIL:
#    block. Probed: such a script recorded nothing in any inert bucket AND
#    left TOTAL_FAIL untouched — silent green, invisible to both gates.
#
#    The marker below is the operator-facing text the repair emits. It is
#    grepped rather than merely looking for `smoke_summary`, because that
#    token already appears in preflight.sh's own comments — a guard keyed on
#    it reports OK whether or not the repair is there. If the phrase is
#    reworded, this smoke must be updated in the same commit.
# ----------------------------------------------------------------------------
if grep -qF -- 'did not call smoke_summary' "${PREFLIGHT}"; then
    ok 'phase 223: preflight FAILs a smoke that exits 0 without printing a summary block'
else
    fail 'phase 223: preflight has no no-summary guard — a smoke that exits 0 without calling smoke_summary is invisible to BOTH the inert gate and TOTAL_FAIL. Restore the FAIL arm in assess_smoke_output.'
fi

smoke_summary
