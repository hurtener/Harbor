#!/usr/bin/env bash
# check-smoke-body-identity.sh — the smoke-corpus half of the D-374
# identity-less-body guard.
#
# THE RULE. A Protocol request type that declares no `identity` field is not
# scoped by one: the artifacts types scope by `scope`, `SearchRequest` scopes by
# `filter`. The control transport decodes strictly, so a body carrying an
# `identity` member for one of those methods is refused `unknown field
# "identity"` (HTTP 400). Before the strict decode landed the member was
# accepted and silently discarded, which is why smoke scripts accumulated it
# while staying green.
#
# WHY THIS EXISTS SEPARATELY FROM THE CONSOLE GUARD. The TypeScript lockstep
# scan (`web/console/scripts/check-protocol-ts-lockstep.mjs`, check (e))
# enforces the same rule for the Console, where every call goes through one
# typed `request()` helper and the call sites are enumerable. The smoke corpus
# has no such choke point: bodies are hand-written shell strings. This corpus
# has now produced TWO separate instances of the bug (`common.sh`'s
# `assert_json_path_resolves`, then phases 213 and 218), so it does not get to
# stay unguarded.
#
# HOW IT ASSOCIATES A BODY WITH A METHOD. Every line that names a Protocol
# method — directly, or through a `VAR='/v1/control/<method>'` assignment — is
# an ANCHOR. Every identity-declaring body literal is attributed to its NEAREST
# anchor, ties going to the anchor above (a body normally follows its URL). The
# body is a violation iff that nearest anchor's method is identity-less. A blind
# ±N window was tried first and produced false positives wherever an
# identity-less call sits a couple of lines from a legitimate identity-ful one
# (phase-183's `artifacts.list` beside `runtime.health`); nearest-anchor
# attribution is what makes the check precise enough to be worth having.
#
# WHY IT IS KEYED ON THE METHOD NAME. The first sweep of this corpus keyed on
# the curl syntax — a literal `-d '{…}'` next to a literal route — and missed
# three separate construction shapes:
#   1. the body passed as a helper-function argument, with no `-d` at all
#      (`assert_post_status_auth <status> <url> <body> …`);
#   2. the body built by variable interpolation (`"{\"identity\":${ID},…}"`);
#   3. the route built from a loop variable (`/v1/control/${m}`).
# A method NAME survives all three, so that is the anchor here.
#
# WHAT IT DOES NOT CATCH, stated rather than implied: a body whose nearest
# anchor is not the method it is actually sent to (two calls interleaved on
# adjacent lines), or a body built by `jq -n` with no literal `identity` key.
# It is a shell corpus; a complete static decision is not available. The
# residual is bounded by preflight, which executes these scripts against a live
# Runtime — but only for the methods preflight happens to exercise, which is
# exactly the gap that let this class ship twice.
#
# Exit 0 clean, 1 with every violation printed.
set -uo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"

MANIFEST='web/console/src/lib/protocol/wire-manifest.gen.json'
METHODS_MD='docs/site/protocol/methods.md'
SMOKE_DIR='scripts/smoke'

FAILED=0

for f in "${MANIFEST}" "${METHODS_MD}"; do
    if [ ! -f "${f}" ]; then
        echo "[FAIL] smoke body-identity: ${f} is missing — run 'make protocol-ts-gen' / 'make protocol-docs-gen'." >&2
        echo "       This check cannot run without the route -> request-type join, and skipping it" >&2
        echo "       silently is how this bug class shipped twice." >&2
        exit 1
    fi
done
if ! command -v jq >/dev/null 2>&1; then
    echo "[FAIL] smoke body-identity: jq is required and not available." >&2
    exit 1
fi

# ---------------------------------------------------------------------------
# 1. Derive the method -> has-identity map from the two generated sources.
#    methods.md rows look like:
#      | `search.query` | `POST /v1/control/search.query` | search | [`SearchRequest`](…) | … |
# ---------------------------------------------------------------------------
ALL_METHODS_FILE="$(mktemp "${TMPDIR:-/tmp}/smokeident-all.XXXXXX")"
LESS_METHODS_FILE="$(mktemp "${TMPDIR:-/tmp}/smokeident-less.XXXXXX")"
trap 'rm -f "${ALL_METHODS_FILE}" "${LESS_METHODS_FILE}"' EXIT

ROWS=0
while IFS='|' read -r _ mcell _ _ reqcell _; do
    method="$(printf '%s' "${mcell}" | tr -d ' `')"
    [ -n "${method}" ] || continue
    reqtype="$(printf '%s' "${reqcell}" | sed -n 's/.*\[`\([A-Za-z0-9_]*\)`\].*/\1/p')"
    [ -n "${reqtype}" ] || continue
    ROWS=$((ROWS + 1))
    printf '%s\n' "${method}" >>"${ALL_METHODS_FILE}"
    has_id="$(jq -r --arg t "${reqtype}" \
        '((.types[$t].fields // []) | map(select(.key == "identity")) | length) > 0' \
        "${MANIFEST}" 2>/dev/null || printf 'true')"
    if [ "${has_id}" = "false" ]; then
        printf '%s\n' "${method}" >>"${LESS_METHODS_FILE}"
    fi
done < <(grep -E '^\| `[a-z_.]+` \| `(GET|POST) ' "${METHODS_MD}")

# A parse that matched nothing is a HARD failure, never a vacuous pass: a
# join-driven guard whose join yields nothing looks exactly like a clean run.
if [ "${ROWS}" -eq 0 ]; then
    echo "[FAIL] smoke body-identity: parsed ZERO method rows from ${METHODS_MD} — the generated" >&2
    echo "       table's shape changed and this check has gone inert. Fix the parser rather than" >&2
    echo "       leaving it matching nothing." >&2
    exit 1
fi
COUNT_LESS="$(grep -c . "${LESS_METHODS_FILE}" 2>/dev/null || printf '0')"
if [ "${COUNT_LESS}" -eq 0 ]; then
    echo "[FAIL] smoke body-identity: parsed ${ROWS} rows but found ZERO identity-less request" >&2
    echo "       types. That is not a plausible Protocol surface (the artifacts and search" >&2
    echo "       clusters are identity-less), so the manifest join is broken." >&2
    exit 1
fi

# ---------------------------------------------------------------------------
# 2. Per script: anchor every method reference, attribute every identity body
#    to its nearest anchor, flag the identity-less ones.
# ---------------------------------------------------------------------------
for script in "${SMOKE_DIR}"/*.sh; do
    [ -f "${script}" ] || continue

    # Expand `VAR=…/v1/…/<method>` assignments so a later `"${VAR}"` line is
    # still anchored on the method it holds. The expanded copy is what the
    # anchor scan reads; line numbers are preserved because only the tail of an
    # assignment line is appended to its own use sites' lines.
    EXPANDED="$(mktemp "${TMPDIR:-/tmp}/smokeident-exp.XXXXXX")"
    cp "${script}" "${EXPANDED}"
    while IFS= read -r assign; do
        var="${assign%%=*}"
        var="$(printf '%s' "${var}" | tr -d ' \t')"
        val="${assign#*=}"
        [ -n "${var}" ] || continue
        # Append the assigned value to every line that dereferences the var, so
        # the anchor scan sees the method name at the point of use. `index()`
        # on a literal, NOT a regex match: the dereference forms (`${VAR}` and
        # `$VAR`) are full of ERE metacharacters, and escaping them portably
        # across awk implementations is exactly the kind of fragility that
        # makes a guard quietly stop matching.
        awk -v braced="\${${var}}" -v bare="\$${var}" -v val="${val}" \
            '{ if (index($0, braced) > 0 || index($0, bare) > 0) print $0 "  #__anchor__ " val; else print $0 }' \
            "${EXPANDED}" >"${EXPANDED}.tmp" && mv "${EXPANDED}.tmp" "${EXPANDED}"
    done < <(grep -E "^[[:space:]]*[A-Za-z_][A-Za-z0-9_]*=.*(/v1/|api_url)" "${script}" || true)

    # Anchor lines: "<lineno> <method>". A line naming several methods anchors
    # on each; a comment line is not a request and never anchors.
    ANCHORS="$(mktemp "${TMPDIR:-/tmp}/smokeident-anch.XXXXXX")"
    while IFS= read -r method; do
        [ -n "${method}" ] || continue
        # `sed` rewrites "<lineno>:<content>" to "<lineno> <method>" — used
        # instead of a `printf '…\n'` loop so no backslash escape shares a
        # continued logical line with a `grep -E`, which drift-audit's
        # portability scan reads as a non-portable pattern escape.
        grep -nE "(^|[^A-Za-z0-9_.])${method}([^A-Za-z0-9_.]|$)" "${EXPANDED}" 2>/dev/null \
            | grep -vE '^[0-9]+:[[:space:]]*#' \
            | sed "s/:.*/ ${method}/" \
            >>"${ANCHORS}" || true
    done <"${ALL_METHODS_FILE}"

    if [ ! -s "${ANCHORS}" ]; then
        rm -f "${EXPANDED}" "${ANCHORS}"
        continue
    fi

    # Identity-declaring body literals, in either quoting style.
    while IFS=: read -r bln _; do
        [ -n "${bln}" ] || continue
        best_d=999999; best_m=''
        while read -r aln amethod; do
            [ -n "${aln}" ] || continue
            d=$((bln - aln)); [ "${d}" -lt 0 ] && d=$((-d))
            # Ties go to the anchor at or above the body (a body follows its URL).
            if [ "${d}" -lt "${best_d}" ] || { [ "${d}" -eq "${best_d}" ] && [ "${aln}" -le "${bln}" ]; }; then
                best_d="${d}"; best_m="${amethod}"
            fi
        done <"${ANCHORS}"
        [ -n "${best_m}" ] || continue
        if grep -qxF "${best_m}" "${LESS_METHODS_FILE}"; then
            echo "[FAIL] smoke body-identity: ${script}:${bln} sends a body declaring 'identity', and its"
            printf '  %s\n' "nearest Protocol method is '${best_m}' (${best_d} line(s) away), whose request type"
            printf '  %s\n' "declares NO 'identity' field. The control transport decodes strictly, so that body"
            printf '  %s\n' "is refused 'unknown field \"identity\"' (400) at runtime (D-374)."
            FAILED=1
        fi
    done < <(grep -nE '\{[[:space:]]*\\?"identity\\?"[[:space:]]*:' "${script}" | grep -vE '^[0-9]+:[[:space:]]*#' || true)

    rm -f "${EXPANDED}" "${ANCHORS}"
done

if [ "${FAILED}" -ne 0 ]; then
    echo "" >&2
    echo "smoke body-identity guard: FAIL" >&2
    exit 1
fi
echo "[OK]   smoke body-identity: no identity body attributed to an identity-less method (${COUNT_LESS} such methods, ${ROWS} rows joined)"
exit 0
