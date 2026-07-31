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

# 4b. Phase plans must be TEXT. A single NUL makes grep treat the file as binary,
# and the two cross-reference checks below then degrade silently: the RFC check
# emits nothing at all (no OK, no FAIL) while the brief check emits a FALSE OK for
# a reference that does not resolve. Verified by probe — a plan carrying a NUL and
# a bogus `RFC §999.999` + `brief 99` passed both. `grep -a` alone does not fix it,
# so the file is rejected outright: a control byte in a phase plan is a defect in
# its own right, and this guard cannot itself be voided by the thing it detects.
for plan in docs/plans/phase-*.md; do
    if LC_ALL=C tr -d '\0' < "$plan" | cmp -s - "$plan"; then
        :
    else
        fail "${plan}: contains a NUL byte — phase plans must be text (a control byte silently voids the RFC and brief reference checks)"
    fi
done

# 5. Cross-reference resolution: every `RFC §N.M` in phase plans must resolve to a real heading.
for plan in docs/plans/phase-*.md; do
    refs=$(grep -a -oE 'RFC §[0-9]+(\.[0-9]+){0,2}' "$plan" | sort -u || true)
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
    # -a: see the note on the RFC check above — same silent-skip hazard.
    refs=$(grep -a -oE '\bbrief [0-9]{2}\b' "$plan" | sort -u || true)
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
#
# The output path is mktemp'd, NOT a fixed `/tmp/harbor-markdownlint.out`.
# `make preflight` runs this audit internally and sibling worktrees run
# preflight concurrently, so a fixed path means two audits clobber each
# other's diagnostic and the operator reads someone else's violations. The
# verdict was never wrong (it is the exit code), but the file the failure
# message points at was.
if command -v npx >/dev/null 2>&1; then
    MDLINT_OUT="$(mktemp -t harbor-markdownlint-XXXXXX)"
    if make -s markdownlint >"${MDLINT_OUT}" 2>&1; then
        ok 'markdownlint (pinned cli2, CI-parity globs) clean'
        rm -f "${MDLINT_OUT}"
    else
        fail "markdownlint found violations — see ${MDLINT_OUT} (run \`make markdownlint\`)"
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
# The pin is not only what a scaffolded `go.mod` requires: `cmd/harbor/root.go`
# reports `FallbackModuleVersion + "-dev"` as `harbor --version` on the same
# un-stamped builds, so a stale pin also misreports the running binary.
#
# Nothing prompts the bump on its own: godoc prose is not a gate and the golden
# fixtures only fire AFTER someone edits the constant. This check is the prompt.
#
# WHERE THE RELEASE LIST COMES FROM, and why it is not this branch's CHANGELOG.
# This check previously compared the pin against `./CHANGELOG.md` — the SAME
# branch's file — and declined to consult tags on the theory that CI's shallow
# checkout has none. That made the guard self-referential: a release-heal merge
# (`-s ours`) that discarded `main`'s release bookkeeping left a branch whose
# newest section and whose pin were BOTH five releases stale, so the pin sat at
# index 0 and the guard printed OK. Self-consistently satisfied, globally wrong.
#
# The release ledger is therefore the PUBLISHED TAGS, read in this order:
#   1. `git ls-remote --tags origin` — works in a shallow checkout precisely
#      because it asks the remote rather than the local object store. This is
#      the CI path.
#   2. Local tags — an ordinary contributor clone has them; no network.
#   3. `origin/main`'s CHANGELOG — a last resort when both are unavailable.
# Set HARBOR_DRIFT_AUDIT_OFFLINE=1 to skip step 1 (air-gapped, or to keep a
# tight local preflight loop off the network).
#
# If NO source resolves, the check WARNs that it could not run rather than
# printing an OK it has not earned — an unverifiable guard must not read as a
# passing one.
#
# The rule, once the published list is in hand:
#   - FAIL when the pin names no published release at all (a phantom version —
#     the catastrophic case: generated projects do not build).
#   - The pin may TRAIL the newest published tag by exactly one release. That
#     window is deliberate: the post-tag bump lands as its own commit, so
#     between cutting vX and merging that bump the pin legitimately names vX-1.
#   - FAIL when it trails by TWO OR MORE releases — the bump was forgotten.
#   - FAIL when the newest published release has no `## [X.Y.Z]` section in this
#     branch's CHANGELOG. That is the release-heal divergence itself, caught
#     directly instead of via its effect on the pin.
# -----------------------------------------------------------------------------
pin_line=$(grep -E '^const FallbackModuleVersion = ' cmd/harbor/scaffold/version.go 2>/dev/null || true)
if [ -z "${pin_line}" ]; then
    fail 'scaffold module pin: cmd/harbor/scaffold/version.go declares no FallbackModuleVersion const'
else
    pin_version=$(printf '%s' "${pin_line}" | sed -E 's/.*"v?([^"]+)".*/\1/')

    # bash 3.2 (macOS default) has no `mapfile`; read every list portably.
    # `--sort=-v:refname` gives newest-first without relying on `sort -V`,
    # which BSD sort does not provide.
    released_versions=()
    release_source=''

    if [ "${HARBOR_DRIFT_AUDIT_OFFLINE:-0}" != "1" ]; then
        while IFS= read -r v; do
            [ -n "${v}" ] && released_versions+=("${v}")
        done < <(GIT_TERMINAL_PROMPT=0 git ls-remote --tags --refs --sort=-v:refname origin 2>/dev/null \
            | sed -nE 's#.*refs/tags/v([0-9]+\.[0-9]+\.[0-9]+)$#\1#p')
        [ "${#released_versions[@]}" -gt 0 ] && release_source='published tags (origin)'
    fi

    if [ "${#released_versions[@]}" -eq 0 ]; then
        while IFS= read -r v; do
            [ -n "${v}" ] && released_versions+=("${v}")
        done < <(git tag --list --sort=-v:refname 'v[0-9]*' 2>/dev/null \
            | sed -nE 's#^v([0-9]+\.[0-9]+\.[0-9]+)$#\1#p')
        [ "${#released_versions[@]}" -gt 0 ] && release_source='local tags'
    fi

    if [ "${#released_versions[@]}" -eq 0 ]; then
        while IFS= read -r v; do
            [ -n "${v}" ] && released_versions+=("${v}")
        done < <(git show origin/main:CHANGELOG.md 2>/dev/null \
            | grep -oE '^## \[[0-9]+\.[0-9]+\.[0-9]+\]' | sed -E 's/^## \[(.*)\]/\1/')
        [ "${#released_versions[@]}" -gt 0 ] && release_source="origin/main's CHANGELOG"
    fi

    if [ "${#released_versions[@]}" -eq 0 ]; then
        warn 'scaffold module pin: NOT CHECKED — no published-release source reachable (no remote tags, no local tags, no origin/main). This guard is inert here; it runs in CI.'
    else
        pin_index=-1
        for i in "${!released_versions[@]}"; do
            if [ "${released_versions[$i]}" = "${pin_version}" ]; then
                pin_index=$i
                break
            fi
        done
        newest="${released_versions[0]}"
        if [ "${pin_index}" -lt 0 ]; then
            fail "scaffold module pin: FallbackModuleVersion=v${pin_version} names no published release (checked against ${release_source}; newest is v${newest}) — a scaffolded go.mod would require a version the module proxy cannot resolve, and 'harbor --version' would report it"
        elif [ "${pin_index}" -ge 2 ]; then
            fail "scaffold module pin: FallbackModuleVersion=v${pin_version} trails the newest published release (v${newest}, per ${release_source}) by ${pin_index} releases — bump it in cmd/harbor/scaffold/version.go and regenerate the goldens (go test ./cmd/harbor -run TestScaffold_Golden -update)"
        else
            ok "scaffold module pin: FallbackModuleVersion=v${pin_version} is a published release (newest: v${newest}, per ${release_source}; a one-release trail is the tag->bump window)"
        fi

        # The pin can only be right if this branch's CHANGELOG knows the release
        # exists. A `-s ours` release-heal merge silently drops main's sections;
        # this is that divergence, named directly.
        if grep -qE "^## \[${newest}\]" CHANGELOG.md; then
            ok "release ledger: CHANGELOG.md carries a section for the newest published release (v${newest})"
        else
            fail "release ledger: v${newest} is published (per ${release_source}) but CHANGELOG.md on this branch has no '## [${newest}]' section — this branch's release bookkeeping has diverged from main (a '-s ours' heal merge does this); reconcile the released sections against origin/main before cutting a release"
        fi
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
