# Phase 75 — Console e2e Playwright harness baseline

## Summary

Phase 75 lands the **Playwright harness baseline** for the Harbor Console — the shared
test infrastructure (config, base page object, helpers, fixtures, CI hook) that every
Stage-2 Console page phase will hang its `<page>-page.spec.ts` off of. The harness runs
against `harbor console` (the static-build subcommand of the Harbor binary, per D-091),
not `harbor dev` — closing a binding correction to the original master-plan goal. The
phase itself ships zero page-specific assertions; its first consumer is **Phase 73a
Overview's `overview-page.spec.ts`**, which lands in the same wave (Stage 2.3) and
exercises the harness end-to-end (§13 primitive-with-consumer compliance). The wave-end
aggregator suite that asserts every page has a matching spec is **Phase 75a**, bundled
into the Stage-3 PR per `docs/plans/wave-13-decomposition.md` §7.

## RFC anchor

- RFC §7

## Briefs informing this phase

- brief 11
- brief 12

## Brief findings incorporated

- **brief 11 §"Findings summary"** — "Every operator-facing flow shipped in a phase
  has a matching `.spec.ts`." The harness baseline encodes this rule mechanically: the
  Stage-3 aggregator (Phase 75a) enumerates the 14-page IA and asserts a matching
  `<slug>-page.spec.ts` exists in `web/console/tests/`. The harness is the substrate;
  the per-page specs are this rule's concrete consumers.
- **brief 11 §"Source artifacts referenced"** — "Phases 72–75 currently spec [the
  Protocol-side and rendering substrate]." Phase 75's role in the original master-plan
  row is exactly that — the test substrate. Wave 13's narrowing splits the original
  scope into baseline (this phase) + aggregator (75a) so each Stage-2 page phase ships
  its own spec rather than the test infrastructure waiting on every page to land first.
- **brief 12 §"Why `harbor console`, not `harbor dev`, serves the Console"** — three
  reasons (decoupling, multi-runtime, audience separation). The original master-plan
  row for Phase 75 said the suite runs against `harbor dev`; D-091 settled in PR #138
  that the Console is served by `harbor console`, and the harness MUST target that
  subcommand. The phase plan corrects the original goal explicitly (§"Findings I'm
  departing from"). The harness boots a fresh `harbor console` per spec run on an
  ephemeral port (preflight pattern, D-104) and points it at a `httptest.Server`-style
  Runtime fixture so specs do not depend on a live production Runtime to pass.
- **brief 12 §"`harbor console` subcommand — what the future phase delivers"** — names
  the future phase's smoke acceptance: "(c) `protocol.ts`'s typed methods round-trip
  against a Phase 60 `httptest.Server`." The Playwright harness reuses this fixture
  shape: the per-spec Runtime is a `httptest.Server` wrapping the canonical Protocol
  mux (`internal/protocol/transports/...`), seeded with deterministic state via
  `harbortest.RunOnce` and the `harbortest/devstack.Assemble` helper (D-094). This keeps
  Playwright specs independent of any operator's local environment.
- **brief 12 §"Re-discussion checklist"** — "A `harbor console` subcommand phase is in
  the wave's stage list, depending on (Phase 60 wire transport, Phase 61 auth, the
  first Console SvelteKit phase)." The harness baseline depends on the same chain: it
  builds `bin/harbor console` to boot the static build, which means the Stage-1 first
  Console SvelteKit phase (the same phase that creates `web/console/` with
  `svelte.config.js` runes mode + `package.json` Svelte 5 pin + `tokens.css` + a
  generated `protocol.ts`) must land before this phase commits its first real test
  run. The harness in this phase is purely the test infrastructure; the first
  SvelteKit page consumer is 73a Overview, which depends on 75 (this phase).

## Findings I'm departing from (if any)

- **Master-plan §75 goal text says "runs against `harbor dev`."** This phase
  deliberately departs from that wording per D-091 + brief 12 §"Why `harbor console`,
  not `harbor dev`, serves the Console". The Console is served by `harbor console`,
  not `harbor dev`. The harness configuration (`web/console/playwright.config.ts`)
  targets a `harbor console` instance booted per test run on an ephemeral port. The
  master-plan row is updated in the same PR to reflect this correction (§"Files added
  or changed" lists the row flip).

## Goals

- Land a Playwright harness at `web/console/tests/` with:
  - `playwright.config.ts` — single source for browser matrix, base URL, timeouts,
    retries, reporter shape, web-server boot hook (boots `harbor console` per spec
    run against a `httptest.Server`-style Runtime fixture).
  - `web/console/tests/fixtures/harbor-runtime.ts` — typed Playwright fixture that
    boots a `harbortest/devstack`-assembled Runtime + a `harbor console` binary on
    an ephemeral port, seeds deterministic identity, returns a teardown hook.
  - `web/console/tests/fixtures/page.ts` — shared `test`/`expect` re-export that
    extends Playwright's defaults with: (a) the `harbor-runtime` fixture, (b)
    auth-storage helpers that pre-populate WebCrypto-encrypted JWTs per D-091,
    (c) navigation helpers (`gotoPage(slug)`), (d) common selectors.
  - `web/console/tests/pages/base-page.ts` — page-object base class with a typed
    `Page` reference, shared a11y-ID-based selectors, and helpers for waiting on
    SvelteKit hydration via Playwright's `waitForLoadState("networkidle")` +
    explicit hydration markers.
  - `web/console/tests/helpers/protocol.ts` — typed helper that imports the
    generated `web/console/src/lib/protocol.ts` (D-093) so specs use the same
    typed methods + wire types Console code does.
  - `web/console/tests/helpers/identity.ts` — helpers for constructing a test
    identity triple and seeding it into the Runtime fixture.
  - `web/console/tests/README.md` — short authoring guide for per-page specs
    (file naming convention, fixture import, a11y-ID convention, screenshot
    posture, deterministic-clock seeding).
- Land the npm scripts in `web/console/package.json`:
  - `test:e2e` — `playwright test`.
  - `test:e2e:install` — `playwright install --with-deps chromium`.
  - `test:e2e:ui` — `playwright test --ui` (developer convenience).
- Wire a `frontend-e2e` CI job in `.github/workflows/ci.yml` that:
  - Sets up Go 1.26 + Node (LTS).
  - Runs `make build` so `bin/harbor` exists.
  - Installs npm deps + Playwright browsers (`npm ci && npm run test:e2e:install`).
  - Runs `npm run test:e2e` from `web/console/`.
  - Fails the build on any non-zero exit.
  - Skips gracefully when `web/console/` is absent (so phase-N+1 builds keep this
    phase's CI green per the CLAUDE.md §4.2 "404/405/501 → SKIP" pattern, adapted
    to "directory-missing → SKIP" for the frontend job).
- First consumer in the same wave: **Phase 73a's `overview-page.spec.ts`** lands in
  Stage 2.3 and exercises the harness end-to-end against the Overview page (counter
  cards, intervention queue, alert ribbon). The harness baseline ships with one
  meta-test (`web/console/tests/harness.spec.ts`) asserting that `harbor console`
  boots, serves `/`, and the Playwright runner reaches the index — proving the
  harness is alive before any page lands.
- Ship a Phase 75 smoke (`scripts/smoke/phase-75.sh`) classified `static-only` that
  asserts: the harness files exist, `playwright.config.ts` targets `harbor console`
  (not `harbor dev`), `web/console/package.json` declares the three e2e scripts,
  the CI workflow declares the `frontend-e2e` job, and no spec hand-rolls a
  `fetch` call (forbidden per CLAUDE.md §4.5 #11 — go through the typed Protocol
  client).
- Update glossary with two new terms ("Playwright harness", "frontend-e2e CI job").
- Append D-115 to `docs/decisions.md` capturing the harness-targets-`harbor console`
  correction + the per-page-spec convention + the harness-baseline-vs-aggregator
  split (75 / 75a).

## Non-goals

- **Per-page Playwright specs.** Each of the 14 Console page phases (73a–73n) ships
  its own `<slug>-page.spec.ts` in the same PR as the page. The harness baseline
  ships ONLY the infrastructure + the meta-test.
- **The Stage-3 wave-end aggregator suite (Phase 75a).** That phase asserts every
  one of the 14 page slugs has a matching `<slug>-page.spec.ts` and runs the full
  navigation across all pages. It lands in the final Stage-2 PR per §17.5.
- **A new Protocol method.** The harness is build-time infrastructure; it consumes
  the generated `protocol.ts` produced by D-093's generator. No Go-side wire-type
  additions.
- **A separate Console binary.** The harness boots `bin/harbor console` (one binary,
  one boot, per D-091). It does NOT spin a stand-alone `node` dev server in CI.
- **Visual regression / screenshot golden-comparison testing.** Out of V1 scope
  (Brief 11 §"Findings summary"). The harness exposes the screenshot-capture API
  Playwright ships, but no golden compares are wired.
- **Multi-runtime fleet-view e2e coverage.** The harness boots one Runtime per spec.
  Multi-runtime navigation is covered post-Wave-13 when the Settings page's
  Connected-Runtimes card matures.
- **Browser-matrix beyond Chromium.** Phase 75 ships Chromium-only. Firefox / WebKit
  matrices are post-V1; the `playwright.config.ts` projects array is structured so
  adding browsers is a one-line change.

## Acceptance criteria

- [ ] `web/console/playwright.config.ts` exists with a single `projects` entry
      (Chromium), the `webServer` block boots `bin/harbor console`, and the base
      URL is the ephemeral-port URL the fixture allocates.
- [ ] `web/console/tests/fixtures/harbor-runtime.ts` exposes a typed Playwright
      fixture that (a) builds a `harbortest/devstack.Assemble`d Runtime, (b)
      starts `bin/harbor console --runtime <name>=<url>` against it on an
      ephemeral port, (c) yields the booted URL + auth token, (d) tears down on
      test completion.
- [ ] `web/console/tests/fixtures/page.ts` extends Playwright's `test` /
      `expect` with the runtime fixture + auth-storage helper + navigation
      helper. All specs import from this file, never from `@playwright/test`
      directly.
- [ ] `web/console/tests/pages/base-page.ts` provides a typed page-object base
      class with `goto()`, `waitForHydration()`, and a typed `selectors` map.
- [ ] `web/console/tests/helpers/protocol.ts` re-exports typed wire types +
      method calls from the generated `web/console/src/lib/protocol.ts`. Specs
      that need to seed Runtime state call `protocol.<method>` rather than
      `fetch(...)`.
- [ ] `web/console/tests/helpers/identity.ts` provides
      `seedTestIdentity(runtime, {tenant, user, session})` that injects a
      deterministic identity quadruple into the Runtime fixture via
      `harbortest/devstack`.
- [ ] `web/console/tests/harness.spec.ts` — the meta-test — boots the fixture,
      navigates to `/`, asserts a 200 response and a known SvelteKit hydration
      marker. No page-specific assertions; this proves the harness is alive.
- [ ] `web/console/package.json` declares `test:e2e`, `test:e2e:install`,
      `test:e2e:ui` scripts. `@playwright/test` is pinned in `devDependencies`.
- [ ] `.github/workflows/ci.yml` declares a `frontend-e2e` job that runs after
      `go` (so `bin/harbor` exists), installs Node + npm deps + Playwright
      browsers, runs `npm run test:e2e` in `web/console/`, and skips
      gracefully when `web/console/` is absent.
- [ ] `web/console/tests/README.md` documents the spec-authoring convention:
      file naming (`<page-slug>-page.spec.ts`), fixture import path,
      a11y-ID-first selector convention, deterministic-clock seeding pattern.
- [ ] `scripts/smoke/phase-75.sh` (classified `static-only`) asserts the
      harness files exist, the config targets `harbor console`, the npm
      scripts are declared, and no spec hand-rolls `fetch`.
- [ ] Glossary entries added for "Playwright harness" and "frontend-e2e CI job".
- [ ] D-115 appended to `docs/decisions.md` capturing the harness posture +
      the correction to the master-plan row + the harness-baseline-vs-aggregator
      split.
- [ ] `docs/plans/README.md` row for Phase 75: `Status` flips to `Shipped`;
      the goal text is updated from "runs against `harbor dev`" to "runs
      against `harbor console`" with a footnote pointing at D-091 + D-115;
      `Deps` flipped to `60, 72` per the wave-13 decomposition §4.
- [ ] README Status row Phase 75 → Shipped; a one-line pointer to
      `web/console/tests/README.md` added to the testing section.
- [ ] **First consumer in same wave (§13 primitive-with-consumer):** Phase 73a
      Overview's `web/console/tests/overview-page.spec.ts` (NOT shipped in this
      phase; shipped in 73a's PR in Stage 2.3 of the same wave). This phase's
      plan names the consumer + the wave; Phase 73a's plan names this harness
      as its test-substrate dependency.

## Files added or changed

```text
web/console/
  playwright.config.ts                         # NEW — harness config
  package.json                                 # add test:e2e scripts + @playwright/test devDep
  tests/
    README.md                                  # NEW — authoring guide
    harness.spec.ts                            # NEW — meta-test (boots + 200)
    fixtures/
      harbor-runtime.ts                        # NEW — Runtime + harbor-console fixture
      page.ts                                  # NEW — extended test/expect
    pages/
      base-page.ts                             # NEW — page-object base class
    helpers/
      protocol.ts                              # NEW — re-export of generated protocol.ts
      identity.ts                              # NEW — seedTestIdentity()
.github/workflows/ci.yml                       # add `frontend-e2e` job
scripts/smoke/phase-75.sh                      # NEW — static-only smoke
docs/plans/phase-75-playwright-harness-baseline.md
docs/plans/README.md                           # row flip + goal/Deps amendment + Phase 75a row
docs/decisions.md                              # D-115
docs/glossary.md                               # two new terms (alphabetical insertion)
README.md                                      # Status row + testing pointer
```

The `web/console/tests/` directory is the canonical home for Playwright specs per
CLAUDE.md §4.5 #1's stack pin. No new top-level directory under `/`; the only top-level
addition is the harness sub-tree under the existing (Stage-1 first-Console-phase-created)
`web/console/`. CLAUDE.md §3 already permits `web/console/` (see §3's note "The Console
[…] If it later monorepos into `web/console/`, the binding rules in §4.5 still apply."),
so no §3 amendment is needed.

## Public API surface

The harness is build-time + test-time infrastructure; it exposes no Go-side public API.
The TypeScript surface that downstream per-page specs depend on:

```typescript
// web/console/tests/fixtures/page.ts
//
// The single import every per-page spec uses. Re-exports an extended `test` and
// `expect` with the harness fixtures pre-wired.
export { test, expect } from "./page";

// Each per-page spec writes:
//   import { test, expect } from "../fixtures/page";
//   test("Overview renders counter cards", async ({ page, runtime }) => { ... });

// web/console/tests/fixtures/harbor-runtime.ts
//
// The Runtime fixture is a Playwright `test.extend` worker-scoped fixture that
// boots one Runtime + one `harbor console` per worker.
export type RuntimeFixture = {
  /** The base URL `harbor console` is serving on (ephemeral port). */
  baseURL: string;
  /** A JWT scoped to the seeded test identity, ready for auth-storage seeding. */
  token: string;
  /** Direct typed Protocol client (generated from CanonicalWireTypes, D-093). */
  protocol: ProtocolClient;
  /** Seeds an identity into the Runtime fixture. */
  seedIdentity(triple: IdentityTriple): Promise<void>;
};

// web/console/tests/pages/base-page.ts
//
// Page-object base class. Per-page specs subclass this to keep selectors typed.
export class BasePage {
  constructor(protected readonly page: Page, protected readonly baseURL: string) {}
  async goto(slug: string): Promise<void>;
  async waitForHydration(): Promise<void>;
}
```

The `harbortest/devstack` Go-side helper (D-094) is the seam the TypeScript fixture
shells out to — the fixture invokes a small `bin/harbor` subcommand that wraps
`devstack.Assemble` for test-fixture use (the Go-side surface lands as a thin shim
behind a build tag so the production binary does not carry the fixture path; the
shim is `cmd/harbor/cmd_test_fixture.go` with `//go:build harborfixture`).

## Test plan

- **Unit:** N/A — the harness is configuration + fixtures. The meta-test
  (`harness.spec.ts`) is the harness's self-test.
- **Integration:** The harness's first integration test IS the meta-test:
  `web/console/tests/harness.spec.ts` boots the Runtime + `harbor console`
  fixture, navigates to `/`, asserts 200 + SvelteKit hydration. This is the
  in-package wiring test (CLAUDE.md §17.2 — "in-package: when the package itself
  IS the wiring boundary"). The harness boots two production binaries on the
  seam (`bin/harbor` for the Runtime + `bin/harbor console` for the Console
  serving the static build); the meta-test asserts they wire together.
- **Conformance:** N/A — the harness is single-implementation infrastructure;
  no driver seam.
- **Concurrency / leak:** N/A — the harness is not a Go-side reusable artifact.
  Per-worker fixture isolation is Playwright's own concern; we configure
  `workers: 1` in CI for deterministic ordering (post-V1 we may parallelise
  once flakes are characterised).

The §17.1 "Integration test required" rule applies: this phase has Deps `60, 72`
(both shipped subsystems) — it consumes the Phase 60 Protocol wire surface + the
Phase 72 events.subscribe scope. The meta-test (`harness.spec.ts`) is the
integration test: it wires the real `bin/harbor` binary, the real `harbor console`
subcommand (assuming that phase lands before this one or in the same wave; if not,
the harness gracefully skips via the directory-missing pattern), the real
`harbortest/devstack` stack, and the typed `protocol.ts` client. Identity
propagation is asserted via `seedTestIdentity` + the generated `IdentityScope`
wire type round-tripping through `protocol.sessions.create`. Failure mode: the
meta-test asserts that hitting `/` without a token (auth-storage cleared) returns
the SvelteKit auth-redirect page, not a 500.

## Smoke script additions

`scripts/smoke/phase-75.sh` is classified `static-only` per CLAUDE.md §4.2's smoke
classification (D-104):

- Existence of `web/console/playwright.config.ts`.
- Existence of `web/console/tests/fixtures/page.ts`,
  `web/console/tests/fixtures/harbor-runtime.ts`, `web/console/tests/pages/base-page.ts`,
  `web/console/tests/helpers/protocol.ts`, `web/console/tests/helpers/identity.ts`,
  `web/console/tests/harness.spec.ts`, `web/console/tests/README.md`.
- `playwright.config.ts` targets `harbor console`, NOT `harbor dev` (grep absent
  for `harbor dev` in the config + grep present for `harbor console`).
- `web/console/package.json` declares the three e2e scripts (`test:e2e`,
  `test:e2e:install`, `test:e2e:ui`).
- `.github/workflows/ci.yml` declares the `frontend-e2e` job.
- No `.spec.ts` file hand-rolls a `fetch(` call (CLAUDE.md §4.5 #11 + §13 — go
  through the typed Protocol client).

The smoke degrades gracefully (SKIP, not FAIL) when `web/console/` is absent —
the Stage-1 first-Console-SvelteKit phase that creates `web/console/` may land
in a parallel agent in the same stage as this phase; either way, the smoke does
not block. Once `web/console/` exists and the harness is in, every assertion
flips OK.

## Coverage target

- **TypeScript harness code (`web/console/tests/`):** No explicit coverage
  target — the harness IS the test substrate; coverage measurement of test code
  is not meaningful. The substrate's correctness is gated by `harness.spec.ts`
  passing.
- **Go-side shim (`cmd/harbor/cmd_test_fixture.go` under `//go:build harborfixture`):**
  N/A — guarded by a build tag, never in the production binary, not measured by
  the Go coverage suite.

## Dependencies

- **60** — Protocol wire transport (`harbor console` serves the static build
  that talks to the Runtime via the generated `protocol.ts` — D-093 — which
  in turn calls the Phase 60 surface).
- **72** — Console subscription Protocol scope (`events.subscribe` scope claim,
  D-079). The harness's meta-test does NOT exercise the subscription scope
  directly, but per-page specs (73a onward) do; declaring 72 as a dependency
  makes the consumer chain explicit.
- **64** — `harbor dev` headless (the Stage-1 first-Console-SvelteKit phase
  builds against `bin/harbor`; without `harbor dev`'s headless mode the build
  context is incomplete). The original master-plan row listed 64 + 73 as deps;
  Wave 13 narrows to 60 + 72 because (a) 73's per-page Protocol additions are
  pulled into each Stage-2 page phase + (b) 64 is transitively assumed via 60.
  The narrowing is documented in `docs/plans/wave-13-decomposition.md` §4.
- **72h** (Stage 1 Batch B — Console DB schema + **SvelteKit scaffold infrastructure**).
  Phase 72h ships `web/console/package.json` + `svelte.config.js` (runes mode
  per D-092) + `vite.config.ts` + `tokens.css` + `.stylelintrc.cjs` + the
  initial generated `protocol.ts` stub + `src/routes/+layout.svelte`. Phase
  75's Playwright harness requires this scaffold to exist; per the wave-13
  decomposition both phases land in Stage 1 Batch B. The smoke's
  directory-missing SKIP path keeps this phase's PR-time CI green if 72h
  lands first OR 75 lands first (no serial ordering).
- **73m** (Stage 2.3 — `harbor console` subcommand). Phase 75's meta-test boots
  `harbor console` against a test Runtime; the subcommand lands in 73m. The
  harness baseline (this phase) does NOT require `harbor console` to exist
  at scaffold-PR time (the meta-test SKIPs if `bin/harbor console --help`
  exits non-zero). 73m is listed here as a same-wave dep so the meta-test
  flips from SKIP to OK once 73m merges; it is NOT a Stage-1 → Stage-1 hard
  dep.

**Not a Dep:** Phase 73 (state inspection). Wave 13 narrows: the per-page
Protocol additions formerly lumped into Phase 73 are pulled into each Stage-2
page phase (73a Overview, 73b Live Runtime, ...). The harness baseline does
not need any of those surfaces; the meta-test only navigates to `/` and asserts
hydration. The wave-end aggregator (Phase 75a) consumes the full Stage-2 surface.

## Risks / open questions

- **Risk: parallel-agent dispatch in Stage 1 may land `web/console/` (the SvelteKit
  scaffold phase) and `web/console/tests/` (this phase) in the same merge window,
  causing scaffold-vs-harness import drift.** Mitigation: the harness imports ONLY
  from (a) `@playwright/test` (external) and (b) `web/console/src/lib/protocol.ts`
  (the generated artifact). The generated artifact's shape is pinned by D-093 +
  the `make protocol-ts-gen-check` CI gate, so even if the scaffold and the harness
  land in parallel PRs that touch the same files, the generator regenerates
  protocol.ts deterministically. Order of merges does not matter; the second-merging
  PR rebases cleanly.
- **Risk: Playwright's browser binary (`chromium`) is ~300MB; downloading per CI
  run is expensive.** Mitigation: `npm run test:e2e:install` is a separate step
  with its own cache layer (`actions/cache@v4` keyed on the Playwright version
  from `package-lock.json`). First CI run downloads; subsequent runs hit cache.
- **Risk: `bin/harbor console` lifecycle drift — the fixture's teardown may leak
  the process on test crashes.** Mitigation: the fixture wraps the `harbor console`
  child process with a `tree-kill`-equivalent (Node `child_process.spawn` with
  `detached: false`); test runner `afterEach` sends SIGTERM, then SIGKILL after a
  500ms grace. The Go-side `cmd_console.go` is required to honour SIGTERM cleanly
  (the `harbor console` phase plan owns this); if it doesn't, the fixture surfaces
  the leak loudly via a goroutine-count-style assertion (in TS, via
  `lsof -p <pid>` polling in `afterAll`).
- **Open question: should the harness adopt Playwright's "trace viewer" recording
  in CI?** Recommendation: yes, on failed-only — the trace artifact is uploaded
  via `actions/upload-artifact@v4` so failures are debuggable post-mortem. The
  Stage-3 aggregator (Phase 75a) wires the upload step; this phase's CI hook is
  trace-disabled to keep PR-CI fast.
- **Open question: should `playwright.config.ts` pin the browser binary version
  for reproducibility?** Resolved: yes — the `npm run test:e2e:install` step
  installs `chromium@<version-from-lockfile>` so PRs in different windows install
  the same binary. The `package.json` `devDependencies` pin on `@playwright/test`
  is the source of truth.
- **Open question: where does the `cmd/harbor/cmd_test_fixture.go` (build-tagged)
  shim live, and does it need a smoke?** Resolved: lives at
  `cmd/harbor/cmd_test_fixture.go` under `//go:build harborfixture`; its build is
  exercised by the harness's `harness.spec.ts` (which invokes
  `go build -tags harborfixture ./cmd/harbor` in the fixture setup). The phase-75
  smoke asserts the file exists and the build tag is present.
- **Risk: §13 amendment "Test stubs as production defaults" — the
  `harborfixture` build tag shim must NOT become the production default.**
  Mitigation: the shim is gated by a build tag the production `make build`
  target NEVER passes; `make build` produces the non-fixture binary, and
  the fixture is only built via `go build -tags harborfixture ...` from the
  TS fixture's setup. The phase-75 smoke asserts `bin/harbor` (production)
  does NOT contain the fixture symbols.

## Glossary additions

- **Playwright harness** — the `web/console/tests/` infrastructure (config,
  fixtures, page-object base class, helpers) that every Console page phase's
  `<slug>-page.spec.ts` consumes. Lands in Phase 75. Targets `harbor console`
  (D-091), not `harbor dev`. The harness boots a per-test `httptest.Server`-style
  Runtime via `harbortest/devstack` + a `bin/harbor console` instance on an
  ephemeral port (D-104 pattern). First consumer: Phase 73a's
  `overview-page.spec.ts` (§13 primitive-with-consumer). Wave-end aggregator:
  Phase 75a. D-115.
- **`frontend-e2e` CI job** — the GitHub Actions job declared in
  `.github/workflows/ci.yml` (lands in Phase 75) that builds `bin/harbor`,
  installs npm dependencies + Playwright browsers, and runs the Console
  Playwright suite from `web/console/`. Skips gracefully when `web/console/`
  is absent (directory-missing → SKIP, mirroring the CLAUDE.md §4.2 "404/405/501
  → SKIP" pattern for smokes). Runs after the `go` job in the workflow DAG
  so `bin/harbor` exists. D-115.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes (Phase 75 smoke is `static-only`; runs in the
      pre-server batch)
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (N/A — harness is test
      infrastructure; coverage measurement of test code is not meaningful; the
      `harness.spec.ts` meta-test is the gate)
- [ ] If multi-isolation paths changed: cross-session isolation test passes
      (N/A — the harness does not touch Go-side isolation paths; the per-page
      specs do, and each page phase's plan owns its own isolation test)
- [ ] **If this phase builds a reusable artifact: concurrent-reuse test passes
      — N≥100 concurrent invocations** (N/A — the harness is build-time +
      test-time infrastructure, not a Go-side reusable artifact; CLAUDE.md §5's
      concurrent-reuse contract applies to compiled artifacts like
      `flow.Engine` / `Tool` / `Planner`, not to TypeScript test fixtures)
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a
      cross-subsystem seam: an integration test exists** —
      `web/console/tests/harness.spec.ts` boots real `bin/harbor` + real
      `harbor console` + real `harbortest/devstack` Runtime, asserts hydration
      + the auth-redirect failure mode, and runs in the `frontend-e2e` CI job.
      Identity propagation is asserted via the `seedTestIdentity` fixture +
      a `protocol.sessions.create` round-trip.
- [ ] If new vocabulary: glossary updated (two new terms — "Playwright
      harness", "frontend-e2e CI job")
- [ ] If a brief finding was departed from: justified above (master-plan §75
      goal text "runs against `harbor dev`" is corrected to "runs against
      `harbor console`" per D-091 + brief 12; D-115 captures the lineage)
