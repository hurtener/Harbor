# Harbor Console e2e Playwright harness

This directory is the Playwright **harness baseline** (Phase 75 / D-115). It is
the shared test infrastructure every Console page phase hangs its per-page spec
off of. The harness targets the `harbor console` subcommand (D-091) — **not**
`harbor dev`. The Console static build is served exclusively by `harbor
console`; the Harbor Runtime ships headless.

## Layout

| Path | Role |
|---|---|
| `../playwright.config.ts` | Browser matrix, timeouts, reporter. Chromium-only at V1. |
| `fixtures/harbor-runtime.ts` | Boots a Runtime + `harbor console` per worker on an ephemeral port; yields the base URL + token. |
| `fixtures/page.ts` | The **single import** every spec uses — extended `test` / `expect` with `runtime`, `seedAuth`, `gotoPage`. |
| `pages/base-page.ts` | Page-object base class — typed selectors + `gotoSlug` + `waitForHydration`. |
| `helpers/protocol.ts` | Loads the generated typed Protocol client (`src/lib/protocol.ts`, D-093). |
| `helpers/identity.ts` | Builds the deterministic test isolation triple. |
| `harness.spec.ts` | The meta-test — proves the harness is alive (boot + hydrate). |

## Authoring a per-page spec

Each of the 14 Console page phases (73a–73n) ships its own spec **in the same
PR as the page**. The harness baseline ships only the infrastructure; it does
**not** ship per-page specs (that is a Phase 75 non-goal).

1. **File naming.** `<page-slug>-page.spec.ts` — e.g. `overview-page.spec.ts`,
   `sessions-page.spec.ts`. The Phase 75a wave-end aggregator enumerates the
   14 IA slugs and asserts a matching spec exists; a missing pair is a build
   break.
2. **Import from `fixtures/page.ts` — never `@playwright/test` directly.**

   ```ts
   import { test, expect } from "./fixtures/page";

   test("Overview renders the counter cards", async ({ page, runtime, helpers }) => {
     test.skip(!runtime.available, "harbor console subcommand absent");
     await helpers.seedAuth(runtime.token);
     await helpers.gotoPage("overview");
     await expect(page.locator("[data-testid='overview-counters']")).toBeVisible();
   });
   ```

3. **a11y-ID-first selectors.** Select by `data-testid` or ARIA role — never a
   brittle CSS path. The Console components stamp `data-testid` attributes for
   exactly this. Subclass `BasePage` to keep the page's selectors typed.
4. **Seed Runtime state through the typed Protocol client.** Need a session, a
   task, an event? Call the generated `protocol.ts` client via
   `helpers/protocol.ts`. **Never hand-roll a raw browser HTTP call** — that is
   a §13 forbidden practice (CLAUDE.md §4.5 #11): the wire shapes are
   generated, so a raw call silently rots when `CanonicalWireTypes` changes.
   The phase-75 smoke greps this directory for hand-rolled HTTP calls and fails
   on a hit, so keep all Runtime access on the typed client.
5. **Deterministic-clock seeding.** When a spec asserts on timestamps or
   time-windowed data, seed the Runtime fixture with a fixed clock rather than
   asserting against wall-clock time. Never `waitForTimeout` as a
   synchronisation primitive — wait on a selector or a network condition.
6. **Identity isolation.** Use `makeTestIdentity({ ... })` from
   `helpers/identity.ts` to build distinct triples within one spec when
   asserting cross-session / cross-tenant isolation.

## Running locally

```bash
cd web/console
npm ci
npm run test:e2e:install   # one-time: download the Chromium browser binary
make -C ../.. build        # produce bin/harbor (the harness boots it)
npm run test:e2e           # run the suite
npm run test:e2e:ui        # interactive runner (developer convenience)
```

When `bin/harbor` is missing or the `harbor console` subcommand is absent
(pre-Phase-73m), the harness yields an unavailable `runtime` fixture and every
spec SKIPs cleanly — the harness baseline never blocks CI.

## CI

The `frontend-e2e` job in `.github/workflows/ci.yml` builds `bin/harbor`,
installs npm deps + the Chromium browser, and runs `npm run test:e2e`. The job
skips gracefully when `web/console/` is absent (directory-missing → SKIP).
