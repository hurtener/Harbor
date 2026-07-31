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
#
# brief_exists is a LOOP, not `ls docs/research/${num}-*.md`, and the difference
# is the whole guard. Check 3 above turns on `nullglob` and does not turn it off
# until check 9, so by the time this check runs an UNMATCHED glob expands to
# NOTHING — which turned `ls "docs/research/99-"*.md` into a bare `ls` of the
# current directory. That always succeeds, so EVERY `brief NN` reference
# resolved and this guard could not fail. Found by phase 224's mutation harness
# (`scripts/smoke/phase-224.sh`), which planted `brief 99` in a fixture plan and
# watched the audit report OK; the shell-level probe confirms `shopt -s
# nullglob` flips the same test from "stale detected" to "resolves".
#
# The loop below is correct with nullglob EITHER WAY: on, the body never runs;
# off, `$f` is the literal unmatched pattern and `[ -f ]` rejects it. That
# independence is deliberate — a guard must not be a function of a shell option
# some earlier check happened to set.
brief_exists() {
    local num="$1" f
    for f in docs/research/"${num}"-*.md; do
        if [ -f "$f" ]; then
            return 0
        fi
    done
    return 1
}
for plan in docs/plans/phase-*.md; do
    # -a: see the note on the RFC check above — same silent-skip hazard.
    refs=$(grep -a -oE '\bbrief [0-9]{2}\b' "$plan" | sort -u || true)
    if [ -z "$refs" ]; then
        continue
    fi
    bad=0
    while IFS= read -r ref; do
        num=$(printf '%s' "$ref" | sed 's/^brief //')
        if ! brief_exists "${num}"; then
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
    # `|| true` is load-bearing, not defensive noise. This script runs under
    # `set -euo pipefail`: when a smoke carries NO header, grep exits 1, pipefail
    # propagates that through the pipeline, and the failing command substitution
    # made `set -e` KILL THE WHOLE AUDIT right here — exit 1 with no diagnostic
    # naming the file, and every check below this point (godoc hygiene, the
    # scaffold pin, the release ledger, both portability guards, markdownlint)
    # silently never ran. So the single defect this guard exists to report was
    # instead masking six other guards. Found by phase 224's mutation harness,
    # which asserts on each guard's OWN message rather than on the exit code —
    # an exit-code-only check would have read the abort as "caught!".
    header=$(grep -E '^[[:space:]]*#[[:space:]]*PREFLIGHT_REQUIRES:' "$smoke" \
        | head -1 \
        | sed -E 's/^[[:space:]]*#[[:space:]]*PREFLIGHT_REQUIRES:[[:space:]]*//' \
        | tr -d '[:space:]' || true)
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
    # The explicit `${TMPDIR:-/tmp}` path form, NOT `mktemp -t`: GNU coreutils
    # documents `-t` as deprecated, and phase-220.sh:304 already says so where
    # it makes the same call. The trailing X's are mandatory for GNU mktemp.
    MDLINT_OUT="$(mktemp "${TMPDIR:-/tmp}/harbor-markdownlint-XXXXXX")"
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
#
# The exemption is ANCHORED ON THE PATH FIELD (`^[^:]*_test\.go:`), never on the
# line body. A bare `grep -v '_test\.go'` discards any VIOLATING line that merely
# MENTIONS a _test.go filename — and a production comment citing an integration
# test by its `phaseNN_*_test.go` name is exactly that shape, so the unanchored
# form was blind to 26 real hits (v1.25 checkpoint F5).
#
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
    hits=$(grep -rE "$pat" --include='*.go' internal/ cmd/ sdk/ 2>/dev/null | grep -vE '^[^:]*_test\.go:' || true)
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
# Smoke regex portability — `\t`, `\d` and `\n` inside a grep -E pattern are
# matched by BSD grep (macOS, where contributors run preflight) and NOT by GNU
# grep (Linux, where CI runs it). A guard written with any of them passes
# locally and is dead in CI, which is the failure mode this whole gate exists
# to prevent: it does not error, it silently never matches.
#
# `\s`, `\w` and `\b` are supported by BOTH and are deliberately NOT flagged.
#
# `\n` joined the scan after the v1.25 checkpoint found `[^\n]` in
# scripts/smoke/phase-222.sh used as "any character". An ERE bracket expression
# has NO C escapes, so `[^\n]` is the class "neither a backslash nor the letter
# n" — a violating declaration containing an `n` between the anchors slips past
# on CI while the local run catches it, the same divergence class as `\t`/`\d`
# and with the same silent-never-matches signature. grep is line-oriented, so a
# pattern never needs to exclude a newline: `.` already cannot cross one.
#
# Use a POSIX class instead: `[[:space:]]` for `\t`, `[[:digit:]]` for `\d`,
# and plain `.` where `[^\n]` was meant. Shell contexts (printf '%s\t',
# IFS=$'\t') are unaffected — this scans only quoted patterns handed to an
# assert_grep_* helper or a `grep -E` call.
#
# BACKSLASH CONTINUATIONS ARE JOINED FIRST. The scan used to be a single
# `grep -rnE`, which requires the helper name and its quoted pattern on ONE
# line — and the house style for these helpers puts the pattern on the NEXT
# line:
#
#     assert_not_grep "${WIRE_GO}" \
#         'ExtraSystemBlocks[^\n]*map\[' \
#         'desc'
#
# so the guard could not see the very shape that motivated widening it to
# `\n`. Mutation-verified: with the one-line scan, re-introducing that exact
# `[^\n]` left this check reporting OK. The awk below rebuilds each LOGICAL
# line (joining trailing-backslash continuations, the way the shell does) and
# reports the line the logical line STARTED on, so multi-line calls are in
# scope. A guard that cannot see the corpus's dominant call shape is the
# §4.2-item-5 problem wearing a different hat.
# -----------------------------------------------------------------------------
bad_escapes=$(awk '
    BEGIN {
        q = sprintf("%c", 39)   # a single quote, unquotable inside this program
        # The ORIGINAL single-line pattern, with two changes: `\n` joins the
        # flagged set, and the backslash must be UNESCAPED. The parity guard
        # `([^q]*[^q\\])?` is what keeps `"\\n\\n"` — an ERE escape for a
        # LITERAL backslash followed by the letter n, which is exactly how
        # scripts/smoke/phase-220.sh greps the Go source text `"\n\n"` — from
        # being reported. Without it the widened set turns correct code red,
        # and a guard that cries wolf gets deleted.
        #
        # The helper alternation is `(assert|soft)_[a-z_]*grep[a-z_]*`, not
        # `assert_grep[a-z_]*`. The narrower form named only five of the eight
        # grep-shaped helpers the corpus defines — it could not see
        # `assert_not_grep`, `assert_not_grep_or_fail`, `assert_not_grep_or_skip`,
        # `assert_block_grep` or `soft_grep`, and `assert_not_grep` is exactly
        # where the `[^\n]` that motivated widening this guard lived.
        #
        # `[^q]*` — not `[[:space:]]+` — between the helper and the pattern.
        # The pattern is the FIRST single-quoted argument AFTER the helper
        # token, which is not always the adjacent one: `assert_grep_present`
        # takes the pattern first, but `assert_grep` / `assert_not_grep` /
        # `assert_block_grep` take the FILE first. Requiring adjacency made the
        # guard blind to every file-first helper. Anchoring on "first quoted
        # arg after the helper" (rather than "any quoted arg on the line") is
        # what keeps `printf '%s\n' "${out}" | grep -qE '...'` from being a
        # false positive: the printf'"'"'s escape sits BEFORE the grep token.
        re = "((assert|soft)_[a-z_]*grep[a-z_]*|grep [^|;]*-[a-zA-Z]*E[^|;]*)[^" q "]*" \
             q "([^" q "]*[^" q "\\\\])?\\\\[tdn]"
    }
    FNR == 1 { buf = ""; start = 0 }
    {
        line = $0
        if (buf == "") { start = FNR }
        # Strip the continuation backslash and keep accumulating.
        if (line ~ /\\$/) {
            sub(/\\$/, "", line)
            buf = buf line
            next
        }
        buf = buf line
        logical = buf
        buf = ""
        stripped = logical
        sub(/^[[:space:]]+/, "", stripped)
        if (substr(stripped, 1, 1) == "#") next   # whole-line comment
        if (logical ~ re) printf "%s:%s\n", FILENAME, start
    }
' scripts/smoke/*.sh scripts/*.sh 2>/dev/null || true)
if [ -n "${bad_escapes}" ]; then
    while IFS= read -r line; do
        [ -n "${line}" ] && fail "smoke regex portability: non-portable '\\t'/'\\d'/'\\n' in a grep -E pattern (GNU grep will never match it; use [[:space:]] / [[:digit:]] / plain .) — ${line}"
    done <<< "${bad_escapes}"
else
    ok 'smoke regex portability: no non-portable \t/\d/\n escapes in grep -E patterns (BSD matches them, GNU does not)'
fi

# -----------------------------------------------------------------------------
# mktemp template portability — the SAME macOS/Linux divergence class as the
# grep-escape guard above, and the second one to reach CI unnoticed.
#
# GNU mktemp (Linux, where CI runs preflight) REJECTS a template with fewer
# than three trailing `X`s outright: `mktemp: too few X's in template`, exit
# non-zero. BSD mktemp (macOS, where contributors run preflight) accepts it and
# invents a suffix. So a template without them passes every local run and kills
# the script in CI — after every assertion has already reported OK, which is
# what makes it expensive to read.
#
# A `mktemp` with NO template (`mktemp`, `mktemp -d`) is portable by
# construction and is deliberately not flagged; only an invocation that PASSES
# a template is checked, and the requirement is exactly the GNU one: the
# template ends in `XXX` or longer.
#
# Redirects (`mktemp -d 2>/dev/null`) are stripped before the scan — they are
# not template arguments, and treating one as a template is how a guard of this
# shape acquires false positives and then gets deleted.
#
# The scan splits each line into command segments on the shell operators and
# only considers a segment whose COMMAND WORD is `mktemp`. That is what keeps
# this guard from tripping on its own source: the word appears here in prose, in
# a grep pattern and inside awk regexes, none of which are command position.
# This file is deliberately still IN scope — it runs its own `mktemp` at the
# markdownlint output path, and a guard that exempts its own file cannot see it.
#
# "Command word" is NOT "first word". The first cut tested `^mktemp`, which is
# only true when the invocation LEADS its segment, and a measured probe found
# five shapes it silently walked past: after `if`, after `then`, behind a
# one-shot env assignment (`TMPDIR=/x mktemp …`), behind `!`, and behind
# `command`. So the leading shell keywords, one-shot env assignments, `!` and
# `command` are stripped FIRST, repeatedly, and the command-word test runs on
# what is left. A guard that only sees the easy shape is worth about as much as
# no guard, and reads as though it covers the corpus.
#
# The same probe found the opposite defect: `mktemp -p /tmp name.XXXXXX` was
# FLAGGED, because `-p`'s directory argument was mistaken for the template. A
# guard that cries wolf gets deleted, which is strictly worse than not having
# one, so option arguments are consumed: `-p`, `--tmpdir` and `--suffix`.
# (Residual, named rather than hidden: GNU's `--tmpdir` takes an OPTIONAL
# ATTACHED argument, so a bare `--tmpdir /dir tmpl.XX` really does treat `/dir`
# as the template and this guard would consume it and miss the bad template.
# The corpus has no such invocation, and erring toward a false negative is the
# right trade for a guard whose failure mode is deletion. `-t` is deliberately
# NOT in the consuming set: `mktemp -t TEMPLATE` takes a real template, which
# must carry the X's like any other.)
# -----------------------------------------------------------------------------
bad_mktemp=$(grep -rn 'mktemp' scripts --include='*.sh' 2>/dev/null \
    | grep -vE ':[0-9]+:[[:space:]]*#' \
    | awk -F: '
        {
            path = $1; lineno = $2
            body = $0
            sub(/^[^:]*:[^:]*:/, "", body)
            # Split on the shell operators ONLY. `$` and `{` are deliberately
            # NOT separators: `${TMPDIR:-/tmp}/name.XXXXXX` is one argument,
            # and splitting it truncates the template before its X'"'"'s.
            nseg = split(body, seg, /[`|;&(]+/)
            for (s = 1; s <= nseg; s++) {
                inv = seg[s]
                # Peel everything that can legally PRECEDE a command word:
                # shell keywords, `!`, `command`, and one-shot env
                # assignments. Repeatedly, because they compose
                # (`if ! TMPDIR=/x command mktemp …`).
                while (1) {
                    sub(/^[[:space:]]+/, "", inv)
                    if (sub(/^(!|if|then|elif|else|while|until|do|command|time|exec|nohup|builtin)[[:space:]]+/, "", inv)) continue
                    if (sub(/^[A-Za-z_][A-Za-z0-9_]*=[^[:space:]]*[[:space:]]+/, "", inv)) continue
                    break
                }
                if (inv !~ /^mktemp([[:space:]]|$)/) continue  # not command position
                sub(/^mktemp/, "", inv)
                # The invocation ends at a shell terminator. `.` is NOT one —
                # it is the conventional separator before the X'"'"'s
                # (`name.XXXXXX`), and treating it as a terminator hid every
                # dotted template'"'"'s suffix.
                sub(/[);|&].*$/, "", inv)
                gsub(/[0-9]*[<>]+[^[:space:]]+/, " ", inv)  # drop redirects
                ntok = split(inv, tok, /[[:space:]]+/)
                template = ""
                eat_next = 0
                for (i = 1; i <= ntok; i++) {
                    if (tok[i] == "") continue
                    if (eat_next) { eat_next = 0; continue }   # option ARGUMENT
                    if (substr(tok[i], 1, 1) == "-") {
                        # Flags whose argument is a DIRECTORY or a SUFFIX, not
                        # a template. `--flag=value` carries its own argument.
                        if (tok[i] ~ /^(-p|--tmpdir|--suffix)$/) eat_next = 1
                        continue
                    }
                    template = tok[i]
                    break
                }
                gsub(/["'"'"'`]/, "", template)             # strip shell quoting FIRST
                if (template == "") continue                # no template: portable
                if (template !~ /XXX$/)
                    printf "%s:%s: %s\n", path, lineno, template
            }
        }' || true)
if [ -n "${bad_mktemp}" ]; then
    while IFS= read -r line; do
        [ -n "${line}" ] && fail "mktemp template portability: template without trailing X's (GNU mktemp rejects it outright; BSD accepts it — use e.g. \"\${TMPDIR:-/tmp}/name.XXXXXX\") — ${line}"
    done <<< "${bad_mktemp}"
else
    ok 'mktemp template portability: every mktemp template carries trailing X'"'"'s (GNU rejects fewer than three; BSD does not)'
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
