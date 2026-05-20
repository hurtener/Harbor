// Harbor Console e2e harness — meta-test (Phase 75 / D-115).
//
// This is the harness's self-test: it proves the harness is alive BEFORE any
// Console page lands. It boots the `runtime` fixture (Runtime + `harbor
// console`), navigates to `/`, and asserts the index serves and the SvelteKit
// app hydrates. It carries ZERO page-specific assertions — those belong in the
// per-page specs (`<slug>-page.spec.ts`, Stage 2) and the wave-end aggregator
// (Phase 75a).
//
// It is also the §17 in-package wiring test: it composes two production
// binaries on the seam (`bin/harbor` Runtime + `bin/harbor console` serving
// the static build) and asserts they wire together.
//
// SKIP semantics (the directory-/subcommand-missing → SKIP pattern, mirroring
// CLAUDE.md §4.2's "404/405/501 → SKIP" for smokes):
//   - `harbor console` absent (pre-Phase-73m) OR `make build` not run
//     → `runtime.available` is false → every test SKIPs cleanly.
//   - generated `src/lib/protocol.ts` absent (pre-Phase-72h)
//     → the Protocol-round-trip test SKIPs; the boot/hydrate tests still run
//       once the `harbor console` subcommand exists.

import {
  test,
  expect,
  consoleSubcommandAvailable,
} from "./fixtures/page";
import { generatedProtocolAvailable } from "./helpers/protocol";
import { DEFAULT_TEST_IDENTITY, makeTestIdentity } from "./helpers/identity";

// Gate the whole suite on the sync `harbor console` probe. Playwright
// instantiates the `page` fixture (launching the browser) BEFORE a test body
// runs, so a body-level `test.skip()` cannot prevent a browser launch on a
// runner with no browser installed. Skipping the describe block at collection
// time is the only way to keep the harness baseline green pre-Phase-73m (the
// `harbor console` subcommand) — the directory-/subcommand-missing → SKIP
// pattern, mirroring CLAUDE.md §4.2's "404/405/501 → SKIP" smoke convention.
const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

test.describe("Console e2e harness baseline", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent (pre-Phase-73m) or bin/harbor not built",
  );

  test("harbor console boots and serves the Console index", async ({
    page,
    runtime,
  }) => {
    const response = await page.goto(runtime.baseURL);
    expect(response, "navigation to / returned a response").not.toBeNull();
    expect(response!.status(), "Console index returns 200").toBe(200);
  });

  test("the Console index hydrates as a SvelteKit app", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("overview");

    // SvelteKit stamps a known hydration marker once the app is interactive.
    // Asserting on it (not a fixed sleep) keeps the meta-test deterministic.
    await expect(
      page.locator("[data-testid='console-hydrated']"),
      "SvelteKit hydration marker is present",
    ).toBeAttached();
  });

  test("the harness exposes the typed Protocol client when generated", async ({
    runtime,
  }) => {
    test.skip(
      !(await generatedProtocolAvailable()),
      "generated src/lib/protocol.ts absent (pre-Phase-72h SvelteKit scaffold)",
    );

    // Identity seam: the harness seeds a deterministic isolation triple. The
    // first real Protocol round-trip consumer is Phase 73a Overview's spec;
    // here we only assert the seam wires without throwing.
    await runtime.seedIdentity(DEFAULT_TEST_IDENTITY);
    await runtime.seedIdentity(makeTestIdentity({ session: "harbor-e2e-alt" }));
  });

  test("an unauthenticated index load does not 500", async ({
    page,
    runtime,
  }) => {
    // Failure mode (CLAUDE.md §17.3 #3): a tokenless load must reach the
    // SvelteKit auth-redirect page, never a server error. The harness clears
    // storage by simply not calling `seedAuth`.
    const response = await page.goto(runtime.baseURL);
    expect(response, "tokenless navigation returned a response").not.toBeNull();
    expect(
      response!.status(),
      "tokenless index load is not a 5xx",
    ).toBeLessThan(500);
  });
});
