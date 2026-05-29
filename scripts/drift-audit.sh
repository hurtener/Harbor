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
    docs/plans/_template.md
    docs/rfc/README.md
)
for plan in docs/plans/phase-*.md; do
    files_to_scan+=("$plan")
done
# Extend the scan to every research brief. Briefs are distilled from the
# predecessor's source, so they are the most likely place the name leaks in;
# INDEX.md alone is not enough.
for brief in docs/research/*.md; do
    [ -f "$brief" ] || continue
    files_to_scan+=("$brief")
done
# Extend the scan to shipped Go source so a stray comment can't sneak
# the predecessor's name into a release binary. find used over a glob
# so we pick up new packages automatically.
if [ -d internal ]; then
    while IFS= read -r f; do
        files_to_scan+=("$f")
    done < <(find internal -type f -name '*.go' 2>/dev/null)
fi
if [ -d cmd ]; then
    while IFS= read -r f; do
        files_to_scan+=("$f")
    done < <(find cmd -type f -name '*.go' 2>/dev/null)
fi
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
    ok 'forbidden-name scan clean (rule files + phase plans + research briefs + indices + Go source)'
fi

# 8. Ensure `make` knows about drift-audit.
if grep -qE '^drift-audit:' Makefile; then
    ok 'Makefile has drift-audit target'
else
    warn 'Makefile is missing a drift-audit target — recommended: make drift-audit'
fi

# 9. PREFLIGHT_REQUIRES header is present + recognised on every
# scripts/smoke/phase-*.sh (D-104). The preflight orchestrator
# parallelises smokes by this header; a missing or unrecognised value
# would silently misroute a smoke into the wrong batch (a server-
# touching smoke into the parallel batch is the worst case — it
# produces nondeterministic flakes). Failing here gives the same loud
# signal at `make drift-audit` time as preflight does, so a missing
# header surfaces before the gate runs.
classify_drift_count=0
shopt -s nullglob
for smoke in scripts/smoke/phase-*.sh; do
    header=$(grep -E '^[[:space:]]*#[[:space:]]*PREFLIGHT_REQUIRES:' "$smoke" \
        | head -1 \
        | sed -E 's/^[[:space:]]*#[[:space:]]*PREFLIGHT_REQUIRES:[[:space:]]*//' \
        | tr -d '[:space:]')
    case "$header" in
        live-server|static-only|unit-tests)
            : # ok
            ;;
        '')
            fail "${smoke}: missing '# PREFLIGHT_REQUIRES: live-server|static-only|unit-tests' header (D-104)"
            classify_drift_count=$((classify_drift_count + 1))
            ;;
        *)
            fail "${smoke}: unrecognised PREFLIGHT_REQUIRES value '${header}' (want live-server|static-only|unit-tests) — D-104"
            classify_drift_count=$((classify_drift_count + 1))
            ;;
    esac
done
shopt -u nullglob
if [ "${classify_drift_count}" -eq 0 ]; then
    ok 'PREFLIGHT_REQUIRES header present + recognised on every phase smoke (D-104)'
fi

# -----------------------------------------------------------------------------
# Operator-skill frontmatter audit (phase 85k / V1.1.5 — see §18 of CLAUDE.md).
# Every docs/skills/<slug>/SKILL.md MUST carry a well-formed Dockyard-style
# frontmatter (`name` / `description` containing "Use when" / `license:
# Apache-2.0` / `metadata.framework: harbor` / `metadata.surface` in the
# canonical set / `metadata.verbs`). A skill with malformed frontmatter fails
# the gate. The helper is extracted to its own script so phase-85k.sh's smoke
# can re-run the same check on the live build.
# -----------------------------------------------------------------------------
if [ -x scripts/skills/check-frontmatter.sh ]; then
    if ! scripts/skills/check-frontmatter.sh; then
        fail 'one or more docs/skills/<slug>/SKILL.md files have malformed frontmatter — see §18 of CLAUDE.md'
    fi
fi

# -----------------------------------------------------------------------------
# Phase 106 regression guard — the Playground placeholder bubble must not
# come back. The literal text was load-bearing for the V1.1 bug where
# operators saw no model output.
# -----------------------------------------------------------------------------
if grep -rq "Message accepted by the Runtime" web/console/src/routes/\(console\)/playground/ 2>/dev/null; then
    fail "Phase 106 regression guard: playground placeholder text 'Message accepted by the Runtime.' is forbidden — see phase 106"
else
    ok 'Phase 106 regression guard: no playground placeholder text'
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
