#!/usr/bin/env bash
# Harbor drift-audit — verifies design coherence across RFC, phase plans, briefs, and rule files.
#
# Runs as part of `make preflight` and is also invokable standalone via `make drift-audit`.
# A FAIL means a phase plan, RFC section, or rule file has drifted out of sync.
# Designed to be cheap (file-level checks; no compilation).

set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "${ROOT}"

OK=0
FAIL=0
WARN=0

ok()   { OK=$((OK + 1));     printf '[OK]   %s\n' "$1"; }
fail() { FAIL=$((FAIL + 1)); printf '[FAIL] %s\n' "$1"; }
warn() { WARN=$((WARN + 1)); printf '[WARN] %s\n' "$1"; }

# 1. AGENTS.md ↔ CLAUDE.md verbatim mirror
if diff -q AGENTS.md CLAUDE.md >/dev/null 2>&1; then
    ok 'AGENTS.md == CLAUDE.md (mirror invariant)'
else
    fail 'AGENTS.md and CLAUDE.md have drifted; run `cp AGENTS.md CLAUDE.md`'
fi

# 2. Required design files exist
for f in RFC-001-Harbor.md AGENTS.md CLAUDE.md README.md LICENSE \
         docs/plans/README.md docs/plans/_template.md \
         docs/rfc/README.md docs/research/INDEX.md \
         docs/glossary.md docs/decisions.md \
         scripts/smoke/_template.sh scripts/smoke/common.sh; do
    if [ -f "$f" ]; then
        ok "required: ${f}"
    else
        fail "missing required file: ${f}"
    fi
done

# 3. Every phase plan has a matching smoke script
shopt -s nullglob
for plan in docs/plans/phase-*.md; do
    n=$(basename "$plan" | sed 's/^phase-//; s/-.*$//')
    smoke="scripts/smoke/phase-${n}.sh"
    if [ -f "$smoke" ]; then
        ok "phase ${n}: plan ↔ smoke pair OK"
    else
        fail "phase ${n}: plan exists but ${smoke} is missing"
    fi
done

# 4. Every phase plan contains the required headings (per docs/plans/_template.md)
required_sections=(
    "## Summary"
    "## RFC anchor"
    "## Briefs informing this phase"
    "## Acceptance criteria"
    "## Files added or changed"
    "## Test plan"
    "## Smoke script additions"
    "## Coverage target"
    "## Dependencies"
)
for plan in docs/plans/phase-*.md; do
    n=$(basename "$plan" .md)
    # phase-00-skeleton.md predates the template; allow legacy headings.
    if [ "$n" = "phase-00-skeleton" ]; then
        continue
    fi
    missing=0
    for h in "${required_sections[@]}"; do
        if ! grep -qF -- "$h" "$plan"; then
            fail "${plan}: missing required heading: ${h}"
            missing=$((missing + 1))
        fi
    done
    if [ "$missing" -eq 0 ]; then
        ok "${plan}: all required headings present"
    fi
done

# 5. Cross-reference resolution: every `RFC §N.M` in phase plans must resolve to a real heading.
for plan in docs/plans/phase-*.md; do
    refs=$(grep -oE 'RFC §[0-9]+(\.[0-9]+){0,2}' "$plan" | sort -u || true)
    if [ -z "$refs" ]; then
        continue
    fi
    bad=0
    while IFS= read -r ref; do
        section=$(printf '%s' "$ref" | sed 's/^RFC §//')
        # Match headings like ## 5., ## 5.1, ### 6.4, #### 6.4.1
        if ! grep -qE "^#{2,5} ${section}( |\.|$)" RFC-001-Harbor.md; then
            fail "${plan}: stale reference '${ref}' (no matching heading in RFC-001-Harbor.md)"
            bad=$((bad + 1))
        fi
    done <<< "$refs"
    if [ "$bad" -eq 0 ] && [ -n "$refs" ]; then
        ok "${plan}: $(printf '%s\n' "$refs" | wc -l | tr -d ' ') RFC reference(s) resolve"
    fi
done

# 6. Cross-reference resolution: every `brief NN` in phase plans must resolve to a real file.
for plan in docs/plans/phase-*.md; do
    refs=$(grep -oE '\bbrief [0-9]{2}\b' "$plan" | sort -u || true)
    if [ -z "$refs" ]; then
        continue
    fi
    bad=0
    while IFS= read -r ref; do
        num=$(printf '%s' "$ref" | sed 's/^brief //')
        if ! ls "docs/research/${num}-"*.md >/dev/null 2>&1; then
            fail "${plan}: stale reference '${ref}' (no matching docs/research/${num}-*.md)"
            bad=$((bad + 1))
        fi
    done <<< "$refs"
    if [ "$bad" -eq 0 ] && [ -n "$refs" ]; then
        ok "${plan}: $(printf '%s\n' "$refs" | wc -l | tr -d ' ') brief reference(s) resolve"
    fi
done

# 7. Forbidden-name scan in repo-root design docs and master plan.
forbidden=("Penguiflow" "penguiflow")
files_to_scan=(
    AGENTS.md
    CLAUDE.md
    RFC-001-Harbor.md
    README.md
    docs/plans/README.md
    docs/glossary.md
    docs/decisions.md
    docs/research/INDEX.md
    docs/plans/_template.md
    docs/rfc/README.md
)
for plan in docs/plans/phase-*.md; do
    files_to_scan+=("$plan")
done
scan_failed=0
for f in "${files_to_scan[@]}"; do
    [ -f "$f" ] || continue
    for word in "${forbidden[@]}"; do
        if grep -q -- "${word}" "$f" 2>/dev/null; then
            fail "predecessor name '${word}' present in ${f}"
            scan_failed=$((scan_failed + 1))
        fi
    done
done
if [ "$scan_failed" -eq 0 ]; then
    ok 'forbidden-name scan clean (rule files + phase plans + indices)'
fi

# 8. Ensure `make` knows about drift-audit.
if grep -qE '^drift-audit:' Makefile; then
    ok 'Makefile has drift-audit target'
else
    warn 'Makefile is missing a drift-audit target — recommended: make drift-audit'
fi

# Summary
printf '\n=== drift-audit summary ===\n'
printf 'OK:   %d\n' "${OK}"
printf 'WARN: %d\n' "${WARN}"
printf 'FAIL: %d\n' "${FAIL}"
if [ "${FAIL}" -gt 0 ]; then
    exit 1
fi
exit 0
