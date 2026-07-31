# Phase 223 — drain the inert-smoke baseline

## Summary

The wave-v1.24 checkpoint (`ec6345b1`) added an inert-smoke gate to `scripts/preflight.sh`: a smoke script belonging to a **shipped** phase that reports `OK: 0` and `FAIL: 0` asserted nothing, and that is a bug (AGENTS.md §4.2 item 5). Twenty-four scripts already violated it on day one, so they were parked in `scripts/smoke/inert-baseline.txt` as declared debt rather than being allowed to disable the gate. This phase drains that list to **zero** and closes the residual holes the gate still carries.

The triage was measured, not estimated, and it changes the shape of the work: **thirteen of the twenty-four are not shipped phases at all.** They are in the baseline because `phase_is_shipped` (`scripts/preflight.sh:202`) cannot see their master-plan row, and because its status vocabulary leaves six of the master plan's eight not-shipped status words unnamed. Fixing the classifier — not the scripts — drains thirteen entries. The remaining eleven are genuine debt on genuinely shipped phases, and every one of them is repairable; **none is environment-dependent.**

## RFC anchor

- RFC §3.4 — the fail-loudly principle. A guard whose only reachable outcome is SKIP is silent degradation wearing a green badge.
- RFC §4.3 — conformance gates. Several of the repairs run a shipped subsystem's conformance/invariant test as the smoke's assertion.
- RFC §8 — CLI layer. `make preflight` is the pre-commit and CI gate this phase repairs.

## Briefs informing this phase

- brief 06
- brief 05

## Brief findings incorporated

- **brief 06 §4: "the moment the bus drops an event for a subscriber it emits a `bus.dropped` event on that subscriber's stream … This converts silent loss into a visible, replayable signal."** The design principle generalises exactly: an all-SKIP smoke is silent loss of verification. The gate already converts it into a visible signal; this phase finishes the job by removing the reason the signal is muted for twenty-four scripts.
- **brief 06 §6: "Metrics-cardinality lint test: a static check that fails CI if any metric registers a label deriving from `TraceID`/`RunID`/free-form input."** The brief's habit is to answer a class of drift with a *mechanical* check rather than reviewer diligence. `scripts/smoke/phase-223.sh` is that check for this class: it asserts the baseline file carries zero data lines and that no line in it names a script that no longer exists.
- **brief 06 §7: "Each phase ships with smoke checks … and updates the contributor docs."** The eight pure-Go phases in the baseline shipped *without* one — they shipped a placeholder that satisfied the drift-audit's plan↔smoke pairing rule (`scripts/drift-audit.sh:41-51`) while asserting nothing.
- **brief 05 §4: "A single `statestoretest.RunSuite(t, factory)` helper drives **every** scenario against any factory function … no per-driver hand-waving."** This is why the eight pure-Go repairs run a *named* test rather than the whole package: the named conformance/invariant test is the load-bearing thing, and naming it in the smoke makes its deletion or rename a preflight FAIL.
- **brief 05 §5: "Artifacts are opt-in. A `NoOpArtifactStore` silent fallback that warns once and truncates is unsafe. **Harbor removes the no-op; in-memory is the floor and routing is mandatory above the threshold.**"** Same shape, applied to tooling: a smoke that unconditionally `skip`s is a no-op verifier that reports success. The repair is not "make it louder," it is "give it a real thing to check, or prove it belongs to an unshipped phase."

## Findings I'm departing from (if any)

None.

One clarification rather than a departure: `scripts/smoke/inert-baseline.txt`'s own header argues that a stale entry is a WARNING rather than a FAIL because "whether some of these assert anything is environment-dependent (a live MCP server, a benchmark budget, a platform-gated tool)". Measurement contradicts the premise for this particular set — see "Triage (measured)": **zero** of the twenty-four are environment-dependent. All twenty-four report `OK: 0 / FAIL: 0` on a plain checkout with no server, no provider keys, and no MCP binary. The stale-entry check stays a WARNING (still the right posture for a file that is *allowed* to be non-empty in an emergency), but the phase's end state makes the question moot by emptying the file.

**As built:** the header was **rewritten** rather than left standing on the refuted premise — leaving a justification the phase's own measurement disproves would be exactly the stale-doc drift §17.6 binds to this PR. The WARN posture survives on a narrower and honest justification stated in the new header: a stale entry is provoked by a line an operator deliberately added under an emergency, and making it a hard FAIL would block the very commit that pays the debt down. A MISSING entry stays a FAIL, which is the direction that matters. The environment-dependence claim is replaced by the measurement that refuted it.

## Goals

- Drain `scripts/smoke/inert-baseline.txt` from 24 data lines to **0**.
- Repair the classifier that produced 13 of those 24 false entries, so a not-yet-shipped phase's skeleton is never mis-parked as shipped-phase debt.
- Give each of the 11 genuinely-inert shipped-phase smokes at least one assertion that a mutation makes FAIL — never SKIP.
- Close the residual holes in the gate itself (below), so a script cannot go dark by a route the gate does not watch.
- Ship a meta-smoke (`scripts/smoke/phase-223.sh`) that keeps the drained state drained.

## Non-goals

- Rewriting the smoke harness or the classification scheme (D-104). The `# PREFLIGHT_REQUIRES:` classes and the `common.sh` helper vocabulary are unchanged.
- Auditing the smokes that already assert. Only the inert set is in scope. (The full-preflight sweep run for this plan found no un-baselined inert *shipped*-phase script — the baseline is complete as of this branch.)
- Changing any master-plan row. Status corrections, if any are found, are the coordinator's; this phase reads `docs/plans/README.md`, it does not edit it.
- Shipping the surfaces the unshipped 85-band / 86a / 107b skeletons are waiting for.
- Deleting any smoke script. Triage found no script in the delete category — see below.

## Triage (measured)

Method: read all 24 scripts; run each standalone on a clean checkout (`bash scripts/smoke/phase-NN.sh`); cross-reference each phase's master-plan row and its real status; verify every delegation claim by grep against the named target. All 24 reported `OK: 0  FAIL: 0`.

### Category counts

| Category | Count | Entries |
|---|---:|---|
| **(e) Misclassified — not a shipped phase at all** (a fifth category the triage frame did not anticipate) | **13** | 85a, 85b, 85c, 85d, 85e, 85f, 85g, 85h, 85i, 85j, 85m, 86a, 107b |
| **(a) Surface exists, script simply unwritten** | **9** | 01, 02, 03, 04, 07, 23, 24, 42, 79 |
| **(c) Assertions exist, but in another script; this one is a bare pointer** | **2** | 132, 132-stream |
| **(b) Surface never built / dissolved → delete the script** | **0** | — |
| **(d) Genuinely environment-dependent → stays listed, justified** | **0** | — |

24 = 13 + 9 + 2 + 0 + 0.

### Category (e) — 13 entries, drained by fixing the classifier, not the scripts

`phase_is_shipped` (`scripts/preflight.sh:202-220`) is wrong twice over, and both faults push in the same direction: *treat a not-shipped phase as shipped.*

**Fault 1 — the row regex cannot see a third of the master plan.** `scripts/preflight.sh:204` greps `^\| *${n} +\|`, which requires at least one space between the phase token and the closing pipe. The master plan writes both `| 104 |` and `| 85a|`; the second form has none. Measured on `docs/plans/README.md`: **228 phase rows are visible to that regex and 106 are invisible.** An invisible row falls into the "no row at all" arm (`:205-209`) and is treated as Shipped.

**Fault 2 — the status vocabulary names 2 of the 8 not-shipped status words the master plan actually uses.** `:216-219` recognises `Pending*`, `Post-V1*` and `Deferred*` as not-shipped and defaults everything else to Shipped; `Deferred` appears zero times in the master plan, so only two arms are live. Full census of the status column (334 phase rows; the classifier matches on the status's leading word):

| Status prefix | Rows | Classified today |
|---|---:|---|
| `Shipped` / `Shipped*` | 293 | shipped ✓ |
| `Post-V1` | 14 | not shipped ✓ |
| `Pending` | 14 | not shipped ✓ |
| `Cut — …` | 4 | **shipped ✗** |
| `Ready now` | 3 | **shipped ✗** |
| `Revisit after …` | 3 | **shipped ✗** |
| `Superseded by … (not shipped)` | 1 | **shipped ✗** |
| `Reverted` | 1 | **shipped ✗** |
| `Deprecated → superseded by …` | 1 | **shipped ✗** |
| `Deferred` | 0 | (dead arm) |

Verified per entry by running a corrected `phase_is_shipped` over the 24: fixing only the regex drains exactly one entry (86a, `Post-V1`). Fixing the vocabulary as well drains all thirteen. The two fixes are one change.

The thirteen and their real statuses (all read from `docs/plans/README.md`):

| Entry | Master-plan status | Row visible to gate today? |
|---|---|---|
| 85a | `Ready now` | no |
| 85b | `Ready now (scope ↑)` | no |
| 85c | `Cut — RC deprecates sampling` | no |
| 85d | `Revisit after SDK-RC` | no |
| 85e | `Cut — RC deprecates roots` | no |
| 85f | `Ready now (slim)` | no |
| 85g | `Deprecated → superseded by 109a–c (D-172)` | no |
| 85h | `Cut — RC redesigns Tasks` | no |
| 85i | `Cut — RC redesigns Tasks` | no |
| 85j | `Revisit after RC-final` | no |
| 85m | `Revisit after SDK-RC` | no |
| 86a | `Post-V1` | no |
| 107b | `Superseded by 107c (not shipped)` | no |

A fourteenth baseline entry, `phase-132-stream.sh`, also has an invisible row (`|132-stream| … | Shipped (V1.8) |`) — but that phase genuinely *is* shipped, so correcting the classifier does not drain it. It is handled in category (c) below. The two effects are independent: fourteen of the twenty-four have unreadable rows; thirteen of those are additionally not shipped.

All thirteen scripts are bare skeletons (one unconditional `skip`) except 86a — four source-grep guards that correctly SKIP because the dispatcher does not exist — and 107b, whose seven guards are gated on `internal/planner/react/stream_filter.go`, **verified absent**: 107b was superseded by 107c and never shipped. Their all-SKIP behaviour is the §4.2 item 4 convention working *correctly*; classifying them as `INERT_PENDING` is the right answer, and the gate already prints that class as an expected, non-fatal report (`scripts/preflight.sh:529-534`).

**Why this fix cannot make preflight stricter.** Both faults default an unrecognised phase to *Shipped*, the strict arm. Correcting them can only move a script from `INERT_SHIPPED`/`INERT_BASELINED` toward `INERT_PENDING` — never the reverse. Of the 106 invisible rows, 91 are `Shipped`-prefixed (the default was accidentally right) and 15 are not; nothing currently passing can start failing. Verified by census, not assumed.

### Category (a) — 9 entries, real assertions to write

Eight are pure Go packages whose smoke is a single unconditional `skip` with a comment explaining that `go test` carries the load. The comment is true and the conclusion is wrong: `make preflight` (`Makefile:121-122`) does **not** run `go test`, so these eight phases have zero preflight coverage. The repair is not novel — their immediate siblings already solved it. `scripts/smoke/phase-05.sh:24-34` is the exemplar: class `unit-tests`, run one *named* invariant test, `ok` on pass, `fail` on failure, `skip` only when the package directory is absent. It reports `OK: 1` today.

| Entry | Package | Named test to run (verified present in the tree) |
|---|---|---|
| 01 | `internal/identity` | `TestWith_RefusesToWidenTheTenant` + `TestWithVerified_RejectsAnIncompleteTriple` (the §6 isolation invariant) |
| 02 | `internal/config` | `TestDefaults_BaselineGolden` |
| 03 | `internal/audit` | `TestReflective_RedactsStructFieldsByJSONTag` |
| 04 | `internal/telemetry` | `TestNewTracer_EmptyServiceName_FailsLoudly` |
| 07 | `internal/state` | `TestValidateIdentity_Cases` + the in-mem driver's conformance-suite entry point |
| 23 | `internal/memory` | the in-mem driver's conformance-suite entry point |
| 24 | `internal/memory/strategy` | `TestRollingSummary_Restore` + `TestNone_RejectsInvalidSnapshot` |
| 42 | `internal/planner` | `TestAnswerEnvelope_GoldenJSON_Phase106ByteCompat` |

Each smoke's `PREFLIGHT_REQUIRES` header flips `static-only` → `unit-tests` (the class exists for exactly this; 119 smokes already use it) and each writes its `go test` log to a path **unique to that phase** — the parallel batch runs up to `nproc` of these concurrently (`scripts/preflight.sh:262-263`) and a shared log path is a cross-talk bug waiting to happen.

The ninth, **phase 79**, is a benchmark-and-CI-gate phase with no Go test to run and no HTTP surface, and its existing comment argues at length that its SKIP is legitimate. It is not: the phase shipped three artefacts that can be asserted, and all three are load-bearing.

- `test/benchmarks/` exists and holds six `*_bench_test.go` files — assert the package **compiles and its benchmarks are discoverable** (`go test -run '^$' -bench=. -benchtime=1x ./test/benchmarks/...`; one iteration, bounded).
- `scripts/perf/check-regression.sh` exists — assert present **and executable**.
- The `perf-regression` CI job exists at `.github/workflows/ci.yml:265` and invokes that script at `:310` — assert both, because a benchmark gate whose CI job was deleted is precisely the "perf baseline CI never reads" failure the v1.24 audit named.

If the bench compile is kept (it should be — it is the assertion that actually guards the benchmarks), the script's class becomes `unit-tests`.

### Category (c) — 2 entries, a delegation that is real but unasserted

`scripts/smoke/phase-132.sh` and `scripts/smoke/phase-132-stream.sh` each contain one `skip` that points at `scripts/smoke/phase-112b.sh`. **The claim is true** — verified by grep: `scripts/smoke/phase-112b.sh:478-542` is leg 6 (`examples/embed-runonce/` compiles; `TestRunOnce_ConcurrentReuse_NoBleedNoLeak` exists; `go test -race -run "TestRunOnce|TestNewRunContext"` passes) and `:544-573` is leg 7 (`func WithStream` exists; `TestRunOnce_WithStream_ChunksArriveBeforeEnvelope` exists; the `-race` run passes). Both legs report real OKs.

So this is not dead coverage — it is **unasserted delegation**, which breaks silently the moment someone strips a leg from 112b. Note that `phase-132-stream.sh` is not even required to exist: `scripts/drift-audit.sh:44` derives the smoke name as `phase-$(basename plan | sed 's/^phase-//; s/-.*$//')`, so `phase-132-stream-withstream.md` pairs with `phase-132.sh`, not `phase-132-stream.sh`.

Deleting them is therefore available, and is rejected. The stronger repair is to make the delegation itself the assertion — each script `assert_grep_present`s the leg markers it depends on inside `scripts/smoke/phase-112b.sh`, so removing leg 6 or leg 7 turns *these* scripts red and names the missing coverage. That converts two pointers into two tripwires at a cost of a few lines each.

### Category (b) and (d) — empty, and why

**(b) delete: no candidate.** The nearest were the six `Cut` / `Deprecated` / `Superseded` phases (85c, 85e, 85g, 85h, 85i, 107b), and deleting their smokes is both blocked and wrong: blocked because `scripts/drift-audit.sh:41-51` FAILs when a `docs/plans/phase-NN-*.md` has no `scripts/smoke/phase-NN.sh`, and all six plan files still exist; wrong because the correct classification is "not shipped", which the category-(e) classifier fix already delivers. Deleting a plan file is a master-plan decision, not this phase's.

**(d) environment-dependent: no candidate.** Every one of the 24 was run on a plain checkout with no dev server, no `OPENROUTER_API_KEY` / `ANTHROPIC_API_KEY`, and no MCP binary, and every one reported `OK: 0  FAIL: 0`. Not one of them has a branch that *would* have asserted under a richer environment. The baseline header's environment-dependence rationale describes a hazard this particular set does not contain.

## The gate's own residual holes

`scripts/preflight.sh` fails on `TOTAL_FAIL > 0` (`:586-589`), and `TOTAL_FAIL` is bumped by (i) any smoke exiting non-zero (`:346-349`, `:518-521`), (ii) drift-audit failing (`:246-249`), and (iii) `${#INERT_SHIPPED[@]}` (`:582`). Three holes remain, all verified by probe rather than by reading:

**Hole 1 — a smoke that exits 0 without printing a summary is invisible to both gates.** `assess_smoke_output` (`:222-242`) returns early at `:230` when the captured output has no `OK:` / `FAIL:` lines, on the documented assumption that "the non-zero exit path already accounts for that." Probed: a script that prints output and `exit 0` without calling `smoke_summary` records nothing in any inert bucket **and** leaves `TOTAL_FAIL` untouched — silent green. Nothing in the harness requires a smoke to call `smoke_summary`; today every one does, which makes this latent rather than live. Fix: when the output has no summary block **and** the script exited 0, count it a FAIL naming the missing `smoke_summary`.

**Hole 2 — a baseline line naming a deleted script rots silently.** The stale-entry sweep (`:548-566`) skips any entry whose file does not exist (`:553`, `[ -f "${entry}" ] || continue`). A dropped script leaves an immortal line. This phase empties the file, so the hole has nothing to hide today — but `scripts/smoke/phase-223.sh` asserts the property directly (every remaining line names an existing file) so it survives a future re-baselining.

**Hole 3 — an unparseable master-plan row is indistinguishable from a missing one.** `:205-209` treats "no row" as Shipped, and the comment at `:170-175` calls this deliberate fail-loudness. It is — but the loudness is misdirected: the operator sees a FAIL on the *script*, not on the *row*. Combined with fault 1 above, that is how 13 entries entered the baseline without anyone noticing the master plan was the actual problem. Fix: after correcting the regex and the vocabulary, emit a named report when a phase token resolves to no row, or resolves to a status word in neither list. The strict default (unknown ⇒ Shipped) is preserved; only its silence is removed. `HARBOR_PREFLIGHT_ALLOW_INERT=1` (`:579-583`) stays as-is; it is the documented emergency valve.

**Not a hole, verified.** A script with `FAIL > 0` is never counted inert (`:231` requires both counters zero) and its non-zero exit is already counted. The `PIPESTATUS[0]` capture at `:517` is correct — the two clobbering shorthands are named in the comment at `:511-515` and neither is present in the file. An unclassified smoke exits the harness at `:137` before any summary, which is loud by construction.

## Scope recommendation

**One phase.** The list is 24 entries but only **four** units of work, and the largest one is a repetition of a shape that already exists in the tree:

1. Classifier fix in `scripts/preflight.sh` (row regex + status vocabulary + unknown-status report) — one function, on the order of fifteen lines. Drains **13**.
2. Eight `unit-tests`-class smokes modelled line-for-line on `scripts/smoke/phase-05.sh`. Drains **8**.
3. Phase 79's three-assertion smoke. Drains **1**.
4. Two delegation tripwires. Drains **2**.

Plus the hole fixes, `scripts/smoke/phase-223.sh`, and D-368. No Go production code changes; no new dependency; no Protocol, Console, or config surface touched.

A split was considered and is **not** recommended. The obvious seam is "classifier first, scripts second," but the classifier fix is exactly what makes the remaining eleven a *complete and final* set — split them and the second PR lands against a baseline file whose contents have already moved, which is the merge-order fragility this phase exists to remove.

**The one condition that justifies a split:** if repairing a smoke exposes a real defect in its subsystem, AGENTS.md §17.6 binds the fix to this PR — and if that defect is larger than a smoke repair, split as "the defect" versus "the remaining drain", never as a category split. Scope guard: if a repaired smoke's named test does not exist or does not pass, do **not** substitute a weaker assertion; report it as a finding and treat it as the §17.6 case.

## Acceptance criteria

- [x] `scripts/smoke/inert-baseline.txt` contains **zero** data lines. The header stays, rewritten to describe an empty file as the steady state and to state what re-adding a line costs.
- [x] `phase_is_shipped` in `scripts/preflight.sh` matches a master-plan row whose phase cell has no trailing space (`| 85a|` as well as `| 104 |`). Verified against `docs/plans/README.md`: the number of phase rows the classifier can resolve rises from **233 to 339**. (The plan measured 228/334 before the five v1.25 rows — 219/220/221/222/223 — were added to the master plan; the invisible count, 106, is unchanged and is the number the fault was measured by.)
- [x] `phase_is_shipped` classifies `Ready now`, `Cut`, `Revisit after`, `Superseded by`, `Reverted`, and `Deprecated` as NOT shipped; keeps `Pending` / `Post-V1` / `Deferred` as NOT shipped; keeps `Shipped` / `Shipped*` as shipped. As built the vocabulary lives in its own `phase_status_arm` function with three outcomes (`shipped` / `not-shipped` / `unknown`) so the unknown case can be reported without weakening the strict default.
- [x] A phase token that resolves to **no row**, or to a status word in neither list, produces a named report line in the preflight summary — still defaulting to Shipped, so the strict arm is preserved. Scoped to the inert gate's classification call site rather than to every smoke: 21 smoke scripts have no master-plan row at all (`108a`–`108p`, `123`, `73e`/`f`/`h`/`j`), and printing all 21 every run is noise an operator learns to skip — which is the failure this report exists to close. A row's unreadability has no consequence until the gate asks about it.
- [x] Each of 01, 02, 03, 04, 07, 23, 24, 42 reports `OK ≥ 1`, runs a **named** test, and writes its `go test` log to a path unique to that phase.
- [x] `scripts/smoke/phase-79.sh` reports `OK ≥ 3`: benchmarks compile/discover, `scripts/perf/check-regression.sh` is present and executable, and the `perf-regression` job in `.github/workflows/ci.yml` both exists and invokes that script.
- [x] `scripts/smoke/phase-132.sh` and `scripts/smoke/phase-132-stream.sh` each report `OK ≥ 1` by asserting their delegated legs are still present in `scripts/smoke/phase-112b.sh` (4 OKs each).
- [x] **Mutation verification, one per repaired script (11 total).** For each, break the guarded property, run the script, and record that the counter moves `OK → FAIL` — never `OK → SKIP`. The mutation and the observed output go in the PR body. A repair that cannot be shown failing is not a repair.
- [x] `assess_smoke_output` counts a FAIL when a smoke exits 0 with no summary block; verified with a throwaway probe script that prints output and `exit 0`.
- [x] `make preflight` reports zero baselined entries, zero inert shipped-phase scripts, zero STALE entries, and PASS.
- [x] `make preflight` run twice back to back is stable — no `OK` / `SKIP` drift between runs from the newly added `go test` invocations.
- [x] `scripts/smoke/phase-223.sh` passes and is itself mutation-verified (add a line to the baseline file → FAIL; point a line at a nonexistent script → FAIL). Its negative arms are FAIL rather than the authoring-time SKIP: with the properties now holding, a SKIP would be indistinguishable from a pass.
- [x] `make drift-audit` and `make check-mirror` pass.
- [x] D-368 filed in `docs/decisions.md`.

## Files added or changed

```text
docs/plans/phase-223-drain-the-inert-smoke-baseline.md   (new — this file)
scripts/smoke/phase-223.sh                               (new — the meta-smoke)
scripts/smoke/inert-baseline.txt                         (24 data lines -> 0; header rewritten)
scripts/preflight.sh                                     (phase_is_shipped row regex + status
                                                          vocabulary + unknown-status report;
                                                          assess_smoke_output no-summary FAIL)
scripts/smoke/phase-01.sh                                (static-only -> unit-tests; named test)
scripts/smoke/phase-02.sh                                (static-only -> unit-tests; named test)
scripts/smoke/phase-03.sh                                (static-only -> unit-tests; named test)
scripts/smoke/phase-04.sh                                (static-only -> unit-tests; named test)
scripts/smoke/phase-07.sh                                (static-only -> unit-tests; named test)
scripts/smoke/phase-23.sh                                (static-only -> unit-tests; named test)
scripts/smoke/phase-24.sh                                (static-only -> unit-tests; named test)
scripts/smoke/phase-42.sh                                (static-only -> unit-tests; named test)
scripts/smoke/phase-79.sh                                (three real assertions)
scripts/smoke/phase-132.sh                               (delegation tripwire)
scripts/smoke/phase-132-stream.sh                        (delegation tripwire)
scripts/smoke/common.sh                                  (one new helper — see below)
scripts/drift-audit.sh                                   (§17.6 bundled: mktemp the markdownlint
                                                          diagnostic path — see below)
docs/glossary.md                                         (inert smoke, inert baseline)
docs/decisions.md                                        (D-368)
```

**One §4.3 addition to that list, and the reason it is not optional.** `scripts/smoke/common.sh`
gains `assert_go_tests_pass`. `go test -run NoSuchTest ./pkg` prints "no tests to run" and exits
**zero**, so the eight repairs — each of which asserts by NAME — could not have detected their own
named test being renamed or deleted if they had checked the exit code alone. The helper runs
`go test -v -run '^(T1|T2|…)$'` and greps a `--- PASS:` line per name. This was mutation-verified on
all eight, and the captured `go test` output shows exit code 0 in every case: an exit-code-only guard
would have been a false OK on all eight, which is the same shape as the SKIPs this phase removes.
CLAUDE.md §4.2 item 3 is the sanctioned home for a new helper (`common.sh`, with a docstring); the
alternative — eight copies of the same grep — is the shape §4.2 item 3 exists to prevent.

**One §17.6 bundled fix in `scripts/drift-audit.sh`,** named here rather than smuggled in. It wrote
its markdownlint output to a FIXED `/tmp/harbor-markdownlint.out`, and `make preflight` runs the
audit internally — so two sibling worktrees running preflight concurrently (this wave's own dispatch
model) clobber each other's diagnostic and the operator is pointed at someone else's violations. The
verdict was never wrong, because that comes from the exit code; the file the failure message names
was. Fixed with `mktemp`, three lines. It is bundled because it is a defect in the gate this phase
exists to make trustworthy, and because a diagnostic that can silently belong to another process is
the same class of thing as a guard that can silently assert nothing.

No new top-level directory (AGENTS.md §3 unchanged). No Go production code. `docs/plans/README.md` is the coordinator's to update.

## Public API surface

N/A — this phase changes verification tooling only. No Go package, Protocol method, wire type, CLI verb, config key, or Console route is added, removed, or altered. No `docs/skills/` surface changes, so the §18 same-PR skill rule does not fire.

## Test plan

- **Unit:** N/A — no Go code is added or changed. The eight repaired smokes *invoke* existing Go tests; they do not author new ones.
- **Integration:** N/A per AGENTS.md §17.1 — this phase opens no cross-subsystem seam and consumes no shipped subsystem's surface. Its integration-shaped gate is `make preflight` itself, run end-to-end as an acceptance criterion (every smoke against a live `harbor dev`), twice, for stability.
- **Conformance:** the repaired smokes for phases 07 and 23 invoke the shipped in-mem driver conformance suites (RFC §4.3) as their assertion. That is the reason for naming a specific test rather than running the whole package.
- **Concurrency / leak:** N/A — no reusable artifact is built (AGENTS.md §5, D-025). One concurrency-shaped concern *is* addressed: the eight new `unit-tests`-class smokes join a parallel batch capped at CPU count (`scripts/preflight.sh:262-263`), so each must write its `go test` log to a distinct path. The back-to-back preflight stability run is the check.

**Mutation verification is the load-bearing test method here**, and it is specified per script rather than as a blanket statement:

| Script | Mutation | Required observation |
|---|---|---|
| 01, 02, 03, 04, 07, 23, 24, 42 | temporarily rename the named test in the Go source | `OK → FAIL` — never SKIP: the package directory still exists, so the `skip` arm must be unreachable |
| 79 | (a) break a benchmark's compile; (b) `chmod -x scripts/perf/check-regression.sh`; (c) rename the `perf-regression` job in `ci.yml` | each produces its own distinct `FAIL` |
| 132 / 132-stream | delete the leg-6 / leg-7 markers from `scripts/smoke/phase-112b.sh` | `OK → FAIL` naming the missing leg |
| 223 (meta) | add a line to `inert-baseline.txt`; then point a line at a nonexistent script | `FAIL` in both cases |
| classifier | set a master-plan row's status to a word in neither list | the named report line appears |
| `assess_smoke_output` | probe script that prints and `exit 0` with no summary | preflight FAILs naming the missing `smoke_summary` |

## Smoke script additions

`scripts/smoke/phase-223.sh` (class `static-only`) is the meta-case — it guards the drained state rather than a runtime surface:

1. `scripts/smoke/inert-baseline.txt` exists and contains **zero** data lines (non-comment, non-blank). This is the assertion that makes the drain permanent; re-parking a script silently is no longer possible.
2. Every data line that *does* exist — zero today, possibly non-zero after a future emergency — names a file that exists. This closes hole 2, which the preflight stale-sweep skips by construction (`scripts/preflight.sh:553`).
3. Every data line that exists names a script whose phase is `Shipped` in the master plan. A not-shipped phase belongs in `INERT_PENDING`, never in the baseline; this is the guard that would have caught the 13 category-(e) entries at the moment they were added.
4. `phase_is_shipped`'s row regex in `scripts/preflight.sh` resolves **every** phase-numbered row in `docs/plans/README.md`, both `| 85a|` and `| 104 |` forms — asserted by counting resolvable rows and requiring the count to equal the total, so a future master-plan reformat that reintroduces the blind spot FAILs here.
5. `phase_is_shipped`'s not-shipped vocabulary covers every non-`Shipped` status word present in the master plan — computed from the file, so a new status word introduced without teaching the classifier FAILs.
6. `assess_smoke_output` carries the no-summary FAIL arm, so hole 1 cannot be reverted silently. **The implementor must emit the exact operator-facing phrase `did not call smoke_summary` in that arm** — the smoke greps for that phrase, and the binding reason is recorded in the script's own comment: a guard keyed on the token `smoke_summary` alone reports OK *today*, because that token already appears at `scripts/preflight.sh:228` inside the comment describing the unfixed behaviour. That false OK was caught while authoring the script and is the same "pass value is also the can't-tell value" shape the phase exists to remove. If the phrase is reworded, the smoke must be updated in the same commit.

Assertions 4 and 5 are computed against the live `docs/plans/README.md` rather than hard-coded, so they track the master plan instead of pinning today's snapshot. Helpers come from `scripts/smoke/common.sh` only (AGENTS.md §4.2 item 3); no new curl wrapper and no new helper are introduced.

**Skeleton state, measured.** As authored (before the phase lands) the script reports `OK: 2  SKIP: 4  FAIL: 0` — it is not inert, so it cannot itself become baseline debt. Assertions 1 and 2 hold today; 3, 4, 5 and 6 SKIP, and each SKIP prints the measured gap (`24 entries`, `228/334 rows`, `'Cut' 'Deprecated' 'Ready' 'Reverted' 'Revisit' 'Superseded'`, `no no-summary guard`) rather than a bare "not implemented". Mutation-verified in both directions during authoring: appending a line naming a nonexistent script turns assertion 2 `OK → FAIL` (rc=1), and emptying the data lines turns assertion 3 `SKIP → OK` (`OK: 3`). Assertions 4, 5 and 6 flip to OK when their targets in `scripts/preflight.sh` land.

Note there is no "surface not yet implemented" SKIP anywhere in this script: every arm reads a repository file that exists unconditionally, and every SKIP is gated on a condition this phase itself opens.

**As built, three changes to that skeleton.** (1) **Every negative arm is now a `fail`, not a `skip`.** The SKIPs were the authoring-time posture, correct while the properties did not yet hold; with the phase landed, a SKIP would be indistinguishable from a pass and would let all six regress in silence — and the acceptance criteria require a baseline line to produce a FAIL. The script reports `OK: 7 / SKIP: 0 / FAIL: 0`. (2) **The plan's assertion 3 — every data line names a Shipped phase — is implemented as its own arm** (the authored skeleton had six checks, of which the drain and the file-existence check were two; the shipped-phase check was the one not yet present). It is the guard that would have caught the thirteen category-(e) entries at the moment they were added, and it is mutation-verified by adding `scripts/smoke/phase-85a.sh` to the baseline: `FAIL … status='Ready now'`. (3) **Assertion 5's vocabulary grep is scoped to the `phase_status_arm` function body**, not the whole of `scripts/preflight.sh`. A status word can legitimately appear in a comment, so a whole-file grep is the same "pass value is also the can't-tell value" shape the script's own header warns about — the identical fault the author caught on `smoke_summary`. Assertion 4 likewise reads the row regex out of `preflight.sh` by exact text rather than restating it, so it cannot pass against a stale copy.

## Coverage target

**N/A — no Go code is added or changed by this phase.** The deliverable is shell tooling (`scripts/preflight.sh`, eleven `scripts/smoke/*.sh`, one text file) plus a plan, a glossary edit, and a decision entry. Stating a percentage would be inventing a number for packages this phase does not touch.

The equivalent gate is stated instead, and it is binding: **`make preflight` reports zero baselined entries, zero inert shipped-phase scripts, zero stale entries, and PASS — and each of the eleven repaired scripts has a recorded mutation that turns its OK into a FAIL.**

Coverage on the Go packages whose tests the repaired smokes now invoke is unchanged and remains governed by those phases' own targets (01: 90%, 02: 85%, 03: 90%, 04: 85%, 07: 85%, 23: 85%, 24: 85%, 42: 90%, per `docs/plans/README.md`).

## Dependencies

- Phase 00 (skeleton) — the smoke harness and `scripts/smoke/common.sh`.
- The wave-v1.24 §17.5 checkpoint (`ec6345b1`), which introduced the inert gate, the baseline file, and the 24 measured violations this phase drains. Nothing in this phase makes sense without it.

No dependency on any unshipped phase. The eleven repaired scripts assert against surfaces that shipped between Phase 01 and Phase 132; the thirteen classifier-drained scripts need no surface at all.

## Risks / open questions

- **Risk: the eight `go test` invocations lengthen preflight.** They join a batch of 119 existing `unit-tests` smokes running at CPU-count parallelism, and each runs a single named test rather than a whole package. Mitigation: one named test per script; measure before/after wall time and record it in the PR. If any single invocation exceeds a few seconds, narrow it further rather than dropping it.
- **Risk: a repaired smoke turns red for a real reason.** That is the gate working, and AGENTS.md §17.6 binds the fix to this PR rather than a follow-up. It is also the one condition that justifies splitting the phase (see "Scope recommendation").
- **Risk: fixing `phase_is_shipped` newly resolves 106 previously-invisible rows and changes classification for scripts outside the baseline.** Analysed: both faults default to Shipped (the strict arm), so correction can only relax, never tighten; the census confirms 91 of the 106 are `Shipped`-prefixed anyway. The acceptance criteria require a full `make preflight` PASS, which is the empirical check.
- **Open question (coordinator, not this phase): the master plan's status column has ten distinct leading words across thirteen distinct full strings, and no schema.** This phase teaches the classifier the current vocabulary and makes an unknown word *reported* rather than silently strict, but the durable fix is a constrained status vocabulary in `docs/plans/README.md`.
- **Open question: should `docs/plans/README.md` phase cells be normalised to a single `| NNN |` form?** It would remove fault 1 at the source and simplify every future row-reading tool. It touches 106 rows of the master plan and belongs to the coordinator, not to a hygiene phase.
- **Open question: three master-plan rows read `Ready now` (85a, 85b, 85f).** After this phase they classify as not-shipped, which is correct today. If any of them ships, its smoke must gain real assertions in that phase's own PR — the gate will then FAIL it, which is the intended behaviour and worth stating so it is not mistaken for a regression introduced here.

## Glossary additions

- **Inert smoke** — a phase smoke script that completes with `OK: 0` and `FAIL: 0`: it ran, and it asserted nothing. On a shipped phase this is a defect (AGENTS.md §4.2 item 5); on an unshipped phase it is the expected skeleton state (§4.2 item 4).
- **Inert baseline** — `scripts/smoke/inert-baseline.txt`, the declared-debt list of inert shipped-phase smokes that predate the gate. It is not an exemption list: an unlisted inert script FAILs preflight, and a listed script that starts asserting is reported as a stale line to delete. Its steady state after this phase is empty.

Both land in `docs/glossary.md` in the same PR.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target — **N/A: no Go code is added or changed; see "Coverage target" for the binding preflight gate that stands in its place.**
- [ ] If multi-isolation code paths changed: cross-session isolation test passes — **N/A: no identity-touching code path changes. Phase 01's repaired smoke *asserts* the isolation invariant `TestWith_RefusesToWidenTheTenant`; it does not modify it.**
- [ ] **If this phase builds a reusable artifact … concurrent-reuse test passes** — **N/A: this phase builds no Go artifact. The parallel-batch concern it does raise — unique `go test` log paths for the eight new `unit-tests` smokes — is covered by the back-to-back preflight stability run.**
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists …** — **N/A: no cross-subsystem seam. The end-to-end gate is `make preflight` itself, run twice, as an acceptance criterion.**
- [ ] If new vocabulary: glossary updated — *inert smoke*, *inert baseline*
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — no departure; D-368 files the settled decision
- [ ] Eleven mutation records (mutation applied, observed `OK → FAIL`) pasted in the PR body
- [ ] Before/after preflight wall time recorded in the PR body
