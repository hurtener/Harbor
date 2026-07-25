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

# -----------------------------------------------------------------------------
# Markdownlint parity — run the SAME markdownlint-cli2 version CI pins
# (markdownlint-cli2-action@v23 → markdownlint-cli2 0.22.1, see Makefile
# MARKDOWNLINT_CLI2_VERSION) with CI-identical globs, so local and CI never
# drift on a rule like MD029 (a v0.33-vs-v0.40 ordered-list gap bit the v1.2.0
# PR). A clone without npx (node) skips it — CI still enforces the gate.
# -----------------------------------------------------------------------------
if command -v npx >/dev/null 2>&1; then
    if make -s markdownlint >/tmp/harbor-markdownlint.out 2>&1; then
        ok 'markdownlint (pinned cli2, CI-parity globs) clean'
    else
        fail "markdownlint found violations — see /tmp/harbor-markdownlint.out (run \`make markdownlint\`)"
    fi
else
    warn 'npx not installed; skipped markdownlint parity (CI still enforces it)'
fi

# -----------------------------------------------------------------------------
# Godoc hygiene (phase 102) — no internal phase jargon in godoc-visible
# comments. pkg.go.dev renders every non-test comment under internal/, cmd/,
# and sdk/ (the public alias-based facade — the most adopter-visible surface,
# added to the scan per D-282); "Phase NN" / "phase-NN" / inline "D-NNN" /
# "brief NN" / wave-band references are contributor concepts that must not
# reach the public docs surface. Test files (_test.go) are contributor-
# internal and exempt. The sibling public test kit harbortest/ is a DELIBERATE
# carve-out (D-282): its ~150 D-094-mirror annotations are load-bearing
# twin-lockstep maintenance markers, so harbortest/ stays out of the scan set.
# The patterns are intentionally narrow (numbering forms only) so legitimate
# runtime vocabulary — e.g. "three phases: pending, running, completed" —
# never trips the scan.
# -----------------------------------------------------------------------------
godoc_jargon_count=0
godoc_jargon_patterns=(
    # Case-insensitive on the leading P and tolerant of the separator so the
    # hyphenated "Phase-39" and spaced "Phase 39" forms are caught alongside
    # "Phase39" / "phase-39" (the pre-tightening `(Phase|phase-)[0-9]+`
    # missed capital-P-hyphen — see the v1.13 checkpoint W5 fix).
    '[Pp]hase[ -]?[0-9]+'
    '\bD-[0-9]+'
    '\b[Bb]rief [0-9]+'
    '\b(Wave|Round|Stage)[ -][0-9A-Z]+'
    '\bwave-[0-9]+'
)
for pat in "${godoc_jargon_patterns[@]}"; do
    hits=$(grep -rE "$pat" --include='*.go' internal/ cmd/ sdk/ 2>/dev/null | grep -v '_test\.go' || true)
    if [ -n "$hits" ]; then
        fail "godoc hygiene: pattern '${pat}' found in non-test Go source (phase 102):"
        printf '%s\n' "$hits" | head -10 | sed 's/^/       /'
        godoc_jargon_count=$((godoc_jargon_count + 1))
    fi
done
if [ "${godoc_jargon_count}" -eq 0 ]; then
    ok 'godoc hygiene: no internal phase jargon in non-test Go source (phase 102)'
fi

# -----------------------------------------------------------------------------
# Scaffold module pin — the `require github.com/hurtener/Harbor <version>` line
# the scaffold engine emits when the binary cannot name its own release (an
# un-stamped source build: `go build ./cmd/harbor`, `go run`, `go test`) comes
# from `scaffold.FallbackModuleVersion`. It MUST name a version that is
# actually published, or every source-built scaffold emits a go.mod no proxy
# can resolve.
#
# Nothing prompts the bump on its own: godoc prose is not a gate and the golden
# fixtures only fire AFTER someone edits the constant. This check is the prompt.
#
# The rule (CHANGELOG is the release ledger; git tags are unavailable in CI's
# shallow checkout, so we do not consult them):
#   - FAIL when the pin names no released CHANGELOG section at all (a phantom
#     version — the catastrophic case: generated projects do not build).
#   - The pin may TRAIL the newest section by exactly one release. That window
#     is deliberate and correct: a release's CHANGELOG section lands on `main`
#     BEFORE its tag is cut, and pinning the untagged version would break every
#     source-built scaffold in the merge -> tag window. The pin therefore tracks
#     the last PUBLISHED release, and is bumped once that tag exists.
#   - FAIL when it trails by TWO OR MORE releases — by then the intervening
#     version is long tagged and the bump was simply forgotten.
# -----------------------------------------------------------------------------
pin_line=$(grep -E '^const FallbackModuleVersion = ' cmd/harbor/scaffold/version.go 2>/dev/null || true)
if [ -z "${pin_line}" ]; then
    fail 'scaffold module pin: cmd/harbor/scaffold/version.go declares no FallbackModuleVersion const'
else
    pin_version=$(printf '%s' "${pin_line}" | sed -E 's/.*"v?([^"]+)".*/\1/')
    # Released sections only, newest first; the [Unreleased] heading is skipped.
    # bash 3.2 (macOS default) has no `mapfile`; read the list portably.
    changelog_versions=()
    while IFS= read -r v; do
        [ -n "${v}" ] && changelog_versions+=("${v}")
    done < <(grep -oE '^## \[[0-9]+\.[0-9]+\.[0-9]+\]' CHANGELOG.md | sed -E 's/^## \[(.*)\]/\1/')
    pin_index=-1
    for i in "${!changelog_versions[@]}"; do
        if [ "${changelog_versions[$i]}" = "${pin_version}" ]; then
            pin_index=$i
            break
        fi
    done
    if [ "${#changelog_versions[@]}" -eq 0 ]; then
        fail 'scaffold module pin: CHANGELOG.md carries no released "## [X.Y.Z]" section to check the pin against'
    elif [ "${pin_index}" -lt 0 ]; then
        fail "scaffold module pin: FallbackModuleVersion=v${pin_version} names no released CHANGELOG section — a scaffolded go.mod would require a version the module proxy cannot resolve"
    elif [ "${pin_index}" -ge 2 ]; then
        fail "scaffold module pin: FallbackModuleVersion=v${pin_version} trails the newest release (v${changelog_versions[0]}) by ${pin_index} releases — bump it in cmd/harbor/scaffold/version.go to the newest PUBLISHED tag and regenerate the goldens (go test ./cmd/harbor -run TestScaffold_Golden -update)"
    else
        ok "scaffold module pin: FallbackModuleVersion=v${pin_version} is a published release (newest: v${changelog_versions[0]}; a one-release trail is the merge->tag window)"
    fi
fi

# -----------------------------------------------------------------------------
# Smoke regex portability — `\t` and `\d` inside a grep -E pattern are matched
# by BSD grep (macOS, where contributors run preflight) and NOT by GNU grep
# (Linux, where CI runs it). A guard written with either one passes locally and
# is dead in CI, which is the failure mode this whole gate exists to prevent:
# it does not error, it silently never matches.
#
# `\s`, `\w` and `\b` are supported by BOTH and are deliberately NOT flagged.
#
# Use a POSIX class instead: `[[:space:]]` for `\t`, `[[:digit:]]` for `\d`.
# Shell contexts (printf '%s\t', IFS=$'\t') are unaffected — this scans only
# quoted patterns handed to an assert_grep_* helper or a `grep -E` call.
# -----------------------------------------------------------------------------
bad_escapes=$(grep -rnE "(assert_grep[a-z_]*|grep [^|;]*-[a-zA-Z]*E[^|;]*)[[:space:]]+'[^']*\\\\[td]" \
    scripts/smoke/*.sh scripts/*.sh 2>/dev/null | grep -vE ":[0-9]+:[[:space:]]*#" || true)
if [ -n "${bad_escapes}" ]; then
    while IFS= read -r line; do
        [ -n "${line}" ] && fail "smoke regex portability: non-portable '\\t'/'\\d' in a grep -E pattern (GNU grep will never match it; use [[:space:]] / [[:digit:]]) — ${line%%:*}:$(printf '%s' "${line}" | cut -d: -f2)"
    done <<< "${bad_escapes}"
else
    ok 'smoke regex portability: no non-portable \t/\d escapes in grep -E patterns (BSD matches them, GNU does not)'
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
