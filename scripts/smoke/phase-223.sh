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
# Shipping-progress aware, per §4.2 item 4: the guards whose property only
# becomes true once the phase lands report SKIP until then, and every such
# SKIP prints the MEASURED number that is still wrong. A SKIP that cannot
# name its own gap is indistinguishable from a pass, which is the failure
# mode this phase exists to remove.
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
#    would silently disable the stale-entry sweep at scripts/preflight.sh:548,
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
# 2. Every listed entry names a script that EXISTS. scripts/preflight.sh:553
#    skips any entry whose file is missing, so a baseline line naming a
#    deleted script rots forever and is never reported stale. This guard is
#    that missing check.
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
# 3. THE DRAIN. The baseline's steady state is EMPTY. A non-empty file is
#    declared debt: every line in it is a shipped phase whose smoke asserts
#    nothing.
# ----------------------------------------------------------------------------
if [ "${ENTRY_COUNT}" -eq 0 ]; then
    ok 'phase 223: inert-smoke baseline is fully drained (0 entries)'
else
    skip "phase 223: baseline still carries ${ENTRY_COUNT} entries — the drain has not landed yet"
fi

# ----------------------------------------------------------------------------
# 4. The classifier can SEE every master-plan phase row. scripts/preflight.sh:204
#    greps `^\| *NN +\|`, which requires a space before the closing pipe. The
#    master plan writes both `| 104 |` and `| 85a|`; the second form is
#    invisible and falls through to the "no row -> treat as Shipped" arm at
#    :205. That is how thirteen unshipped phases were parked in the baseline
#    as shipped-phase debt.
# ----------------------------------------------------------------------------
if [ -f "${MASTER}" ]; then
    rows_total="$(grep -cE '^\|[[:space:]]*[0-9][0-9a-z-]*[[:space:]]*\|' "${MASTER}" || true)"
    rows_seen="$(grep -cE '^\|[[:space:]]*[0-9][0-9a-z-]*[[:space:]]+\|' "${MASTER}" || true)"
    rows_total="${rows_total:-0}"
    rows_seen="${rows_seen:-0}"
    if [ "${rows_total}" -gt 0 ] && [ "${rows_seen}" -eq "${rows_total}" ]; then
        ok "phase 223: every master-plan phase row is resolvable by the preflight row regex (${rows_seen}/${rows_total})"
    else
        skip "phase 223: ${rows_seen}/${rows_total} master-plan phase rows resolvable — $((rows_total - rows_seen)) row(s) have no space before the closing pipe and read as Shipped by default (preflight.sh:204)"
    fi
else
    fail "phase 223: ${MASTER} is missing — the shipped/unshipped classifier has nothing to read"
fi

# ----------------------------------------------------------------------------
# 5. The classifier's NOT-SHIPPED vocabulary covers every non-Shipped status
#    word the master plan actually uses. scripts/preflight.sh:216 knows
#    Pending / Post-V1 / Deferred and defaults everything else to Shipped, so
#    `Ready`, `Cut`, `Revisit`, `Superseded`, `Reverted` and `Deprecated` all
#    classify as shipped today.
#
#    Computed FROM the master plan, not hard-coded, so a new status word
#    introduced without teaching the classifier trips this guard.
# ----------------------------------------------------------------------------
if [ -f "${MASTER}" ] && [ -f "${PREFLIGHT}" ]; then
    unknown_status=''
    while IFS= read -r st; do
        [ -n "${st}" ] || continue
        case "${st}" in
            Shipped*|shipped*) continue ;;
        esac
        # The classifier's case arms match on the status's leading word.
        word="$(printf '%s' "${st}" | awk '{print $1}')"
        [ -n "${word}" ] || continue
        if ! grep -qF -- "${word}" "${PREFLIGHT}"; then
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
        skip "phase 223: master-plan status word(s) the preflight classifier does not name —${unknown_status} — each currently defaults to Shipped (preflight.sh:216)"
    else
        ok 'phase 223: every non-Shipped master-plan status word is named by the preflight classifier'
    fi
fi

# ----------------------------------------------------------------------------
# 6. A smoke that exits 0 without printing a summary must FAIL, not vanish.
#    assess_smoke_output (scripts/preflight.sh:222) returns early at :230 when
#    there is no OK:/FAIL: block. Probed: such a script records nothing in any
#    inert bucket AND leaves TOTAL_FAIL untouched — silent green.
#
#    The marker below is the operator-facing text the repair must emit. It is
#    grepped rather than merely looking for `smoke_summary`, because that
#    token ALREADY appears at preflight.sh:228 inside the comment describing
#    the UNFIXED behaviour — a guard keyed on it reports OK before the repair
#    exists. (Caught while authoring this script; it is the same "pass value
#    is also the can't-tell value" shape the phase exists to remove.)
# ----------------------------------------------------------------------------
if grep -qF -- 'did not call smoke_summary' "${PREFLIGHT}"; then
    ok 'phase 223: preflight FAILs a smoke that exits 0 without printing a summary block'
else
    skip 'phase 223: preflight has no no-summary guard — a smoke that exits 0 without calling smoke_summary is invisible to both the inert gate and TOTAL_FAIL (preflight.sh:230)'
fi

smoke_summary
