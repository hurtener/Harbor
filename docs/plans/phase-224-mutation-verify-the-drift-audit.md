# Phase 224 — mutation-verify the drift-audit's own guards

## Summary

`scripts/drift-audit.sh` is the mechanical instrument that enforces Harbor's drift rules — the mirror invariant, the plan↔smoke pairing, the cross-reference resolution, the forbidden-name scan, the godoc-jargon scan, the scaffold pin, and the two macOS/Linux portability guards. Until this phase **nothing verified the instrument**. Its guards were mutation-verified by hand and the results recorded in code comments; no automated check re-ran those mutations, so a regression that re-broke one would have gone completely unnoticed. A guard that cannot fire is indistinguishable from a corpus with no violations.

This phase ships `scripts/smoke/phase-224.sh`: a regression harness that **executes** the mutations rather than trusting the recorded comments. For each of the audit's eighteen guard units it builds a throwaway fixture corpus, applies a mutation of the shape that guard exists to catch, runs the real `drift-audit.sh` against the mutated corpus, and asserts the audit printed **that guard's own** FAIL (or WARN) line.

**It found two live defects on its first run, and both are fixed here** (AGENTS.md §17.6). Neither was hypothetical and neither was visible to any existing gate:

1. **The `brief NN` cross-reference guard could not fail.** Check 3 turns on `nullglob` and does not turn it off until check 9, so by check 6 the unmatched glob in `ls "docs/research/${num}-"*.md` expanded to *nothing* — leaving a bare `ls` of the current directory, which always exits 0. **Every `brief NN` reference in every phase plan resolved, including ones naming briefs that do not exist.** The guard the §16 workflow leans on to keep brief citations honest has been inert.
2. **A smoke script with no `PREFLIGHT_REQUIRES` header killed the whole audit instead of reporting the smoke.** The audit runs under `set -euo pipefail`; with no header, `grep` exits 1, `pipefail` propagates it, and the failing command substitution made `set -e` abort at that point — exit 1, no diagnostic naming the file, and **six later guards never ran at all** (godoc hygiene, scaffold pin, release ledger, both portability guards, markdownlint). The one defect this guard exists to report was masking six others.

Defect 2 is also the phase's own design validation: an exit-code-only harness would have read the abort as "mutation caught". Asserting on each guard's **own message** is what separated them.

## RFC anchor

- RFC §3.4 — the fail-loudly principle. A guard whose only reachable outcome is a pass is silent degradation wearing a green badge, and that is precisely what both defects above were.
- RFC §4.3 — conformance gates. This harness is a conformance suite for the drift-audit: one fixture corpus, one mutation per guard, every guard must pass the same shape of check.
- RFC §8 — CLI layer. `make drift-audit` and `make preflight` are the gates this phase makes trustworthy.

## Briefs informing this phase

- brief 06
- brief 05

## Brief findings incorporated

- **brief 06 §6: "Metrics-cardinality lint test: a static check that fails CI if any metric registers a label deriving from `TraceID`/`RunID`/free-form input."** The brief's habit is to answer a class of drift with a *mechanical* check rather than reviewer diligence. This phase applies the habit one level up: the mechanical check that answers "is the mechanical check still working?"
- **brief 06 §4: "the moment the bus drops an event for a subscriber it emits a `bus.dropped` event on that subscriber's stream … This converts silent loss into a visible, replayable signal."** An inert guard is silent loss of verification, with the additional cruelty that it *looks* like a signal. The `[FAIL] … MUTATION NOT CAUGHT … THIS GUARD IS INERT` line is the visible signal for that loss.
- **brief 05 §4: "A single `statestoretest.RunSuite(t, factory)` helper drives **every** scenario against any factory function … no per-driver hand-waving."** `expect_caught` is that helper's shape: one function drives every guard through the identical four assertions, so a new guard is one table row rather than a bespoke check whose rigour is a matter of who wrote it.
- **brief 05 §5: "Artifacts are opt-in. A `NoOpArtifactStore` silent fallback that warns once and truncates is unsafe. **Harbor removes the no-op; in-memory is the floor and routing is mandatory above the threshold.**"** The same shape, applied to tooling: a guard that cannot fail is a no-op verifier reporting success. The repair is not "make it louder", it is "prove it can fire".

## Findings I'm departing from (if any)

None.

One clarification rather than a departure. Brief 06's mechanical-check habit is normally satisfied by a check that runs in CI. This one runs in `make preflight`'s parallel static batch and standalone, but **deliberately not inside `drift-audit.sh` itself** — see "The independence conclusion" below. That is an application of the habit, not a departure from it.

## The independence conclusion

The constraint was: *a check that runs `drift-audit.sh` to verify `drift-audit.sh` shares its blind spots.* The conclusion this phase reached, and the design it produced:

**Independence cannot mean "do not run the subject."** A harness that re-implements the audit's logic tests the copy, not the program — and the copy is exactly where phase 223's tautological assertion came from (`rows_seen="${rows_total}"`, a comparison true by construction). So `phase-224.sh` runs the **real, byte-identical** `scripts/drift-audit.sh`, and asserts the copy is byte-identical before trusting a single case.

Independence lives in four other places:

1. **The oracle is external.** Every expected verdict is a literal string written by hand in `phase-224.sh` from the guard's own message. Nothing is derived from the audit's output; there is no `expected="$(drift-audit …)"` anywhere. A guard that stops emitting its message is a FAIL, not a silently-updated expectation.
2. **The corpus is constructed, not observed.** The fixture tree is built by the harness under `$TMPDIR`. Its clean verdict is known a priori (it is built to pass — and the harness *asserts* the pristine run is clean before running any case) and its mutated verdict is known a priori (the mutation *is* the violation).
3. **The harness is not a guard inside the subject.** Had it landed as "drift-audit check 18: verify my own guards", any global breakage of the audit — an early `set -e` exit (defect 2 above was exactly that), an inverted summary — would take the self-check down with it and still exit green. It runs the audit as a **separate process** and reads only its stdout and exit status.
4. **The verdict is per-guard, never the exit code.** Each case asserts the audit printed *that guard's own* `[FAIL]`/`[WARN]` line. A mutation that happened to trip a different guard would make an exit-code check report "caught!" while the guard under test slept. This is not a theoretical refinement: it is the only reason defect 2 was found rather than papered over.

**The residual, named rather than hidden.** The harness's own correctness is verified by the meta-mutations recorded below — deliberately breaking it and the subject and watching it go red. That is the terminating base case of the turtles; it is a one-time manual act by construction, and the alternative (a harness verifying the harness) regresses infinitely. The census check below is the mechanical part that does *not* need re-running by hand.

## Goals

- Every guard `drift-audit.sh` implements has an automated case that applies its defect and asserts it fires.
- Mutations never touch the working tree — the live repository is read-only to this harness.
- Coverage is stated numerically in the smoke's own output, never implied.
- A guard added to `drift-audit.sh` **without** a mutation case fails this smoke, so coverage cannot silently decay.
- The two defects the harness found are fixed in this PR, not filed (AGENTS.md §17.6).

## Non-goals

- Proving a guard is **complete**. Each case proves a guard is *not inert* against one mutation of the shape it is written for. The escape guard's continuation-line blindness and its five-of-eight helper alternation were *shape* gaps in a guard that was otherwise live, and no harness of this kind can enumerate shapes. Where a hardest-known shape exists it is the one used (see the two payloads).
- Verifying `scripts/preflight.sh`. That instrument is phase 223's subject; this phase's subject is `drift-audit.sh` only.
- Re-designing any guard. Except for the two defect fixes, `drift-audit.sh`'s logic is unchanged.
- Adding a Go test. See "Shell smoke, not a Go test" below.

## Shell smoke, not a Go test

Decided deliberately against the alternative:

- **The subject is a shell program invoked as a shell program.** `make drift-audit` runs `bash scripts/drift-audit.sh`; `scripts/preflight.sh:330` runs the same. A Go test would shell out to exactly this, adding a process layer and a package that owns it while testing the identical thing.
- **There is no Go package that owns `scripts/`.** A Go test would need a new home purely to host it, and §3 is the binding layout.
- **The house precedent is a smoke.** `scripts/smoke/phase-102.sh` and `scripts/smoke/phase-223.sh` are meta-guards over shell and docs, and they are smokes. §4.2 item 3 binds new assertions to the `common.sh` vocabulary, which this uses (`ok` / `fail` / `skip` / `assert_file` / `smoke_summary`).
- **It inherits preflight's inert gate for free.** Phase 224 is `Shipped`, so `scripts/preflight.sh` fails if this script ever reports `OK: 0` — the harness that guards guards is itself guarded against going dark.

Classified `PREFLIGHT_REQUIRES: static-only`: it touches no dev server, no `HARBOR_BIND`, no `HARBOR_DEV_TOKEN`, no `${HARBOR_DATA_DIR}/server.log`.

**Standalone-runnable is a requirement, not a side effect.** `bash scripts/smoke/phase-224.sh` from the repository root is the whole invocation — no server, no build, no environment, ~12s. That is deliberate: a verification instrument reachable only through the expensive full-preflight path is one nobody runs, which is how it becomes the next instrument nobody has watched fail. Every case in this phase was developed and mutation-verified through the standalone path; `make preflight` was never run locally.

## The fixture corpus

Mutations operate on a **constructed corpus**, never the repository. `phase-224.sh` builds a minimal tree under `$(mktemp -d "${TMPDIR:-/tmp}/harbor-phase224-XXXXXX")` containing exactly what the audit needs to pass clean: the thirteen required design files, one phase plan carrying all nine required headings plus a resolvable `RFC §3.4` and `brief 06`, one research brief, one smoke with a valid header, one operator skill, three clean Go files across `internal/` `cmd/` `sdk/`, a `cmd/harbor/scaffold/version.go`, a `CHANGELOG.md`, a `Makefile` with `drift-audit:` and a sentinel-switch `markdownlint:` target, a playground page, and a git repository carrying three local release tags (`v2.0.0`, `v1.9.0`, `v1.8.0`).

Three properties of the corpus are load-bearing:

- **`mktemp -d` per run, not a fixed path.** `drift-audit.sh` itself carried a fixed-`/tmp` markdownlint output path that two concurrent audits clobbered (`make preflight` runs the audit internally, and sibling worktrees run preflight concurrently). Re-introducing that shape in the harness that exists to verify the audit would be a poor joke. The template is rebuilt per invocation and `trap … EXIT`-removed; each case gets its own `cp -R` copy, so cases cannot contaminate one another.
- **The audit is copied and the copy is `cmp`-verified byte-identical** before any case runs. A silent `cp` failure would leave every case verifying nothing.
- **`HARBOR_DRIFT_AUDIT_OFFLINE=1`** removes the `git ls-remote` rung of the release-ledger lookup: the fixture has no remote, and ~20 network round trips per preflight is not a cost worth paying for a rung this corpus cannot populate. Named in the census below.

## Coverage census

**Eighteen guard units, eighteen covered, zero declared uncovered, twenty-two mutations.** Sixteen guards report an `ok` line; the NUL-byte guard is FAIL-only (silent on success) and the operator-skill frontmatter guard's OK is printed by the delegated child script — which is why the census total (16) and the guard total (18) differ, and why the smoke prints both numbers rather than leaving the reader to reconstruct them.

| # | Guard (`drift-audit.sh`) | Mutation applied | Expected |
|---|---|---|---|
| 1 | mirror invariant | append a line to fixture `CLAUDE.md` | FAIL |
| 2 | required design files | delete fixture `docs/glossary.md` | FAIL |
| 3 | plan ↔ smoke pairing | delete fixture `scripts/smoke/phase-01.sh` | FAIL |
| 4 | required plan headings | delete `## Summary` from the fixture plan | FAIL |
| 5 | NUL byte in a phase plan | write a NUL into the fixture plan (via `awk` `%c`; `printf '\0'` is not portable) | FAIL |
| 6 | RFC cross-reference | plant an anchor naming a section number the RFC has no heading for | FAIL |
| 7 | brief cross-reference | plant a two-digit brief citation naming a brief file that does not exist | FAIL |
| 8 | forbidden-name scan (docs limb) | plant the word in the fixture plan | FAIL |
| 9 | forbidden-name scan (Go limb) | plant the word in fixture `internal/…/fixture.go` | FAIL |
| 10 | Makefile `drift-audit:` target | rename the target | **WARN** (and exit 0) |
| 11 | `PREFLIGHT_REQUIRES` absent | strip the header from the fixture smoke | FAIL |
| 12 | `PREFLIGHT_REQUIRES` unrecognised | set the value to `sometimes` | FAIL |
| 13 | operator-skill frontmatter | set `license: MIT` | FAIL |
| 14 | playground placeholder | write the forbidden bubble text into the fixture page | FAIL |
| 15 | markdownlint parity wiring | trip the fixture's sentinel so `make markdownlint` exits 1 | FAIL |
| 16 | godoc jargon | plant `// Phase 42 …` | FAIL |
| 17 | godoc jargon, test-path anchoring | plant `// Rationale: D-999. See fixture_regression_test.go …` in a **production** file | FAIL |
| 18 | scaffold pin — phantom release | pin `v0.0.1` (unpublished) | FAIL |
| 19 | scaffold pin — trails by two | pin `v1.8.0` (index 2) | FAIL |
| 20 | release ledger | delete `## [2.0.0]` from the fixture CHANGELOG | FAIL |
| 21 | smoke regex portability | drop in `bad-escape.sh.txt` | FAIL |
| 22 | mktemp template portability | drop in `bad-mktemp.sh.txt` | FAIL |

Cases 17, 21 and 22 deliberately use the **hardest known shape** rather than the easy one, because a payload that the historically-broken guard would also have caught proves nothing:

- **17** puts the jargon in a production file whose comment *names a `_test.go` file*. The v1.25 checkpoint's F5 finding was that an unanchored `grep -v '_test\.go'` discards such a line by its body, hiding 26 real hits. Verified: reverting the anchoring turns this case red (meta-mutation 1).
- **21** puts `[^\n]` in the second argument of a **file-first** helper on a **backslash-continued** line — the shape that defeated the escape guard's first cut twice over. Verified: narrowing the helper alternation back to `assert_grep[a-z_]*` turns this case red (meta-mutation 2).
- **22** puts `mktemp` behind a one-shot env assignment inside a command substitution — one of the five non-leading shapes the guard's `^mktemp` first cut walked past.

### What the census does NOT cover, stated rather than implied

| Guard | Residual |
|---|---|
| markdownlint parity | Only the **wiring** is verified (the `make markdownlint` exit code reaching a FAIL). The pinned cli2 version and the CI-parity globs are not exercised — the fixture supplies its own `markdownlint` target. The npx-absent WARN arm is not exercised either; on a host with no `npx` the audit WARN-skips the guard and the case **SKIPs with that reason named**. |
| scaffold module pin | Only the **local-tags** rung of the release-source lookup. The `git ls-remote` rung (suppressed by `HARBOR_DRIFT_AUDIT_OFFLINE=1`), the `origin/main` CHANGELOG rung, and the "no source reachable → WARN" arm are not mutated. |
| forbidden-name scan | The `docs/` and `internal/` limbs. The `cmd/` limb and the research-brief limb share the same loop and the same file list; they are not separately mutated. |
| required design files | One file of thirteen (they share one loop). |
| all guards | **Non-inertness, not completeness.** See "Non-goals". |

The census is also **mechanical**, not just prose: `phase-224.sh` cross-checks every `ok` call in `drift-audit.sh` against the list of verified guards. An `ok` no case claims fails this smoke ("a new guard shipped with zero coverage"); a claimed message the audit no longer emits fails it too ("a guard was renamed and the case is asserting against a message that does not exist"). Silent partial coverage is the defect this phase removes; it must not be reintroduced by the fix.

## Defects found and fixed in this PR (§17.6)

### D1 — `brief NN` resolution was inert under `nullglob`

`scripts/drift-audit.sh` check 3 runs `shopt -s nullglob` and does not run `shopt -u nullglob` until check 9. Check 6 tested brief existence with `ls "docs/research/${num}-"*.md >/dev/null 2>&1`. With `nullglob` on, an unmatched glob expands to **nothing**, so the command degrades to a bare `ls` of the current directory — exit 0, "resolves". Probe:

```text
without nullglob:  STALE detected (correct)
with    nullglob:  RESOLVED (wrong)
```

Confirmed end-to-end on the **live repository**, not only in the fixture: with a bogus two-digit brief citation planted in this very plan, the pre-fix audit (`git show HEAD:scripts/drift-audit.sh`) printed

```text
[OK]   docs/plans/phase-224-…: 3 brief reference(s) resolve
```

while the fixed audit prints

```text
[FAIL] docs/plans/phase-224-…: stale reference 'brief NN' (no matching docs/research/NN-*.md)
```

and exits 1. (The citation is written `NN` here rather than verbatim for the obvious reason: a literal one would make this plan fail the very check it is describing — which is itself a small proof the guard now works.)

**Fix.** A `brief_exists()` helper that loops the glob and tests `[ -f ]`. It is correct with `nullglob` **either way** — on, the body never runs; off, `$f` is the literal unmatched pattern and the test rejects it. That independence is deliberate: a guard must not be a function of a shell option some earlier check happened to set.

**Blast radius.** Every `brief NN` citation in every phase plan has been unverified since `nullglob` was introduced. A follow-up sweep of existing citations is *not* bundled here — after the fix, `make drift-audit` is the sweep, and it passes on the current tree (measured, see the gate output), so there is no debt to drain.

### D2 — a smoke with no `PREFLIGHT_REQUIRES` header aborted the audit

The header extraction was a bare command substitution over a pipeline. Under `set -euo pipefail`, no header ⇒ `grep` exits 1 ⇒ `pipefail` propagates ⇒ the assignment fails ⇒ `set -e` **kills the audit**. Observed in the mutated corpus: exit 1, output ending at check 8, no message naming the file, and six later guards never reached.

**Fix.** `|| true` on the pipeline inside the substitution, with a comment recording that it is load-bearing rather than defensive noise, so a future cleanup does not delete it.

## Meta-verification (the harness itself, broken on purpose)

A guard verifying guards that cannot itself fail would be the most embarrassing possible outcome. Four deliberate breakages, each producing the expected red:

| # | Breakage | Result |
|---|---|---|
| 1 | Revert `drift-audit.sh`'s godoc `_test.go` **path** anchoring to the unanchored body match | `[FAIL] phase 224 [godoc jargon scan (test-path anchoring)]: MUTATION NOT CAUGHT … THIS GUARD IS INERT` — `OK: 24  FAIL: 1` |
| 2 | Narrow the escape guard's helper alternation back to `assert_grep[a-z_]*` (blind to `assert_not_grep`) | `[FAIL] phase 224 [smoke regex portability]: MUTATION NOT CAUGHT …` — `OK: 24  FAIL: 1` |
| 3 | Neuter a **harness-side** mutation function (`mut_mirror` → no-op) | `[FAIL] phase 224 [mirror invariant]: MUTATION NOT CAUGHT …` — `OK: 24  FAIL: 1` |
| 4 | Add a new `ok` guard to `drift-audit.sh` with no mutation case | `[FAIL] phase 224: scripts/drift-audit.sh emits OK line(s) that NO mutation case above verifies …` — `OK: 24  FAIL: 1` |

Breakages 1 and 2 are the important pair: they are real historical regressions of the subject, not synthetic harness damage, and they prove the harness detects a genuine revert rather than only its own sabotage.

## Acceptance criteria

- [x] `scripts/smoke/phase-224.sh` exists, is executable, is `PREFLIGHT_REQUIRES: static-only`, and passes standalone with `FAIL: 0` and `OK > 0`.
- [x] Every guard unit in `scripts/drift-audit.sh` has at least one mutation case; the smoke prints the covered/total tally in its own output.
- [x] Each case asserts on the guard's **own** `[FAIL]`/`[WARN]` message, not on the audit's exit code alone.
- [x] Each case additionally asserts the pristine corpus printed that guard's OK line and did **not** print its bad line.
- [x] The `WARN`-class guard asserts the audit still exits **0** — a WARN must not fail the gate.
- [x] No mutation touches a file under the repository working tree; the corpus lives under `mktemp -d` with a `≥3`-X template and is removed by an `EXIT` trap.
- [x] A new `ok` guard added to `drift-audit.sh` without a case fails this smoke (mechanical census; meta-mutation 4).
- [x] Both defects the harness found (D1, D2) are fixed in this PR.
- [x] The harness is mutation-verified: four deliberate breakages each produce a FAIL (table above).

## Files added or changed

```text
scripts/smoke/phase-224.sh                          # added — the mutation harness
scripts/smoke/testdata/phase-224/bad-escape.sh.txt  # added — escape-guard payload
scripts/smoke/testdata/phase-224/bad-mktemp.sh.txt  # added — mktemp-guard payload
scripts/drift-audit.sh                              # changed — D1 + D2 fixes
docs/plans/phase-224-mutation-verify-the-drift-audit.md  # added — this plan
docs/plans/README.md                                # changed — row 224 + detail block
docs/decisions.md                                   # changed — D-376
```

The payloads carry a `.txt` suffix deliberately: the audit's escape scan globs `scripts/smoke/*.sh` and its mktemp scan greps `scripts --include='*.sh'`, so a `.sh` name would make the **real** audit FAIL on the repository itself. Both files carry a header saying so, because a deliberate defect in a file that looks like a smoke script is exactly the thing a future contributor will "fix".

`scripts/smoke/testdata/` is not a new top-level directory (§3 names `scripts/`), and mirrors the established `internal/<area>/testdata/` convention.

## Public API surface

N/A — no Go, no Protocol, no config. The only contract introduced is the smoke's own: `expect_caught <label> <severity> <ok-marker> <bad-marker> <mutate-fn>`, used within one file.

## Test plan

- **Unit:** N/A — no Go code.
- **Integration:** `scripts/smoke/phase-224.sh` *is* the integration test. It wires the real `scripts/drift-audit.sh` and the real `scripts/skills/check-frontmatter.sh` against a constructed corpus — real subjects on the seam, no mocks, per §17.3 item 1. Its failure modes are the twenty-two mutations, well past §17.3 item 3's "at least one".
- **Conformance:** the coverage census is the conformance sweep — every guard the audit emits must be represented, checked mechanically rather than by review.
- **Concurrency / leak:** N/A — no long-lived component and no reusable artifact (D-025 does not apply to a shell harness). Concurrency *safety* is addressed instead by the per-run `mktemp -d`, which is the property the audit's own fixed-`/tmp` incident showed matters when sibling worktrees run preflight at once.
- **Identity propagation:** N/A — this phase touches no identity-scoped path.

## Smoke script additions

`scripts/smoke/phase-224.sh` (new). Binding assertion list:

1. The fixture's copy of `scripts/drift-audit.sh` is byte-identical to the original (`cmp`).
2. The pristine fixture corpus passes drift-audit with exit 0 — otherwise every case below is unattributable and the script fails immediately.
3. Twenty-two mutation cases, one row per the coverage census table above. Each asserts: clean-corpus OK line present, clean-corpus bad line absent, mutated-corpus `[FAIL]`/`[WARN]` line matching the guard's own message, and an exit status matching the severity.
4. The coverage census: every `ok` call in `drift-audit.sh` is claimed by a verified guard, and every claimed message still exists in `drift-audit.sh`.

Hard preconditions, each a FAIL rather than a SKIP (§4.2 item 5 — "the harness could not run" must not read like "the harness found nothing wrong"): the audit, the frontmatter checker, both payloads, and `git` (the fixture mints local release tags for the scaffold-pin guard). The single legitimate SKIP is the markdownlint case on a host with no `npx`, where the audit itself WARN-skips the guard; the SKIP names the guard and the reason.

## Coverage target

N/A — no Go packages are touched, so there is no `go test -cover` figure to hold. The phase's equivalent gate is stated numerically and enforced mechanically instead: **18 of 18 guard units covered, 0 declared uncovered**, asserted by the census check rather than by this sentence.

## Dependencies

- 223 — the inert-smoke gate in `scripts/preflight.sh` is what keeps *this* smoke from going dark, and its meta-smoke is the exemplar this one follows.

## Risks / open questions

- **Message coupling.** Each case pins a literal fragment of a guard's message, so rewording a guard's FAIL text turns this smoke red. That is intended and matches `scripts/smoke/phase-223.sh` assertion 7's posture: the coupling is cheap to repair (one string) and it is the only thing that keeps a case attributable to a specific guard. The census's "orphaned signature" arm names the file and the message when it happens.
- **Runtime.** Twenty-three audit runs over a tiny corpus; measured at roughly 12s wall on a developer laptop. It runs in preflight's **parallel** static batch, so it overlaps with the rest of that batch.
- **Shape coverage remains a judgement call.** The harness proves non-inertness, and the three hardest-shape payloads narrow the gap, but a guard can still be blind to a shape nobody thought of. Every future shape discovery should land as an additional case here in the same PR that fixes the guard — that is the §17.6 habit applied to this file.
- **The turtles terminate at meta-verification.** The harness's own correctness rests on the four recorded breakages, re-run by hand when it is materially changed. A harness verifying the harness regresses infinitely; the census is the part that stays mechanical.

## Glossary additions

None. "Mutation verification" is already the wave's working vocabulary (`docs/plans/wave-v125-coordination.md`, `scripts/smoke/inert-baseline.txt`) and describes a practice, not a Harbor runtime concept.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [ ] `make preflight` passes — **NOT RUN LOCALLY, by explicit operator directive** (no local preflight, no hand-booted dev server; CI runs the identical gate on the PR). Marked unticked rather than ticked-with-an-asterisk: a checklist that records a gate as passed when it was not run is the same shape of dishonest signal this phase exists to remove. What *was* run locally, and is the evidence standing in for it: `make vet`, `make test` (`-race`), `make lint`, `make build`, `make check-mirror`, `make drift-audit` (`OK: 1401 / WARN: 0 / FAIL: 0`), `markdownlint-cli2` repo-wide (534 files, 0 errors), `bash scripts/smoke/phase-224.sh` standalone (`OK: 25 / SKIP: 0 / FAIL: 0`), and the four meta-mutations
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve — and, for the first time, the `brief NN` half of that claim is actually checked (D1)
- [x] Coverage on touched packages ≥ stated target — N/A, no Go packages touched
- [x] If multi-isolation paths changed: cross-session isolation test passes — N/A, none touched
- [x] Concurrent-reuse test — N/A, this phase builds no reusable artifact (a shell harness is not a compiled artifact under D-025)
- [x] Integration test — the smoke itself, wiring the real audit and the real frontmatter checker against a constructed corpus (§17.2 "in-package: when the package itself IS the wiring boundary")
- [x] If new vocabulary: glossary updated — N/A
- [x] If a brief finding was departed from: justified above + decisions.md entry filed — no departure; D-376 records the design
