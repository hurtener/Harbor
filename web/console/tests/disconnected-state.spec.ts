// Harbor Console e2e — disconnected-state contract.
//
// Phase 83r + 83s built per-page disconnected hygiene (disabled action
// buttons, desaturated chips, no synthetic `$0.00`). Phase 105 (V1.2)
// then reframed the disconnected contract at the product level: rather
// than strand the operator on a dead page full of disabled controls,
// the app shell ((console)/+layout.svelte) redirects ANY disconnected
// navigation to /settings — the one surface where they can attach a
// Runtime. "When disconnected, take them to where they connect" — so
// they get a working Console, not a guessing game.
//
// This spec pins that redirect across the page catalog. The pre-105
// per-page disabled-control assertions are superseded: those controls
// are no longer reachable by navigation (you are redirected before you
// can see them), so testing them via navigation is no longer
// meaningful. The disabled-control CODE still ships for the brief
// pre-redirect frame and for defence in depth; its unit-level coverage
// lives with the components.
//
// SKIP semantics (mirrors `harness.spec.ts`): the `harbor console`
// subcommand lands in Phase 73m. When absent the whole describe block
// SKIPs at collection time.

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

test.describe("Console disconnected-state contract (Phase 83r/83s + Phase 105 redirect)", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent (pre-Phase-73m) or bin/harbor not built",
  );

  // The pages that redirect to /settings when no Runtime is attached.
  // (Settings itself is excluded — it is the redirect TARGET.)
  const REDIRECTING_PAGES = [
    "overview",
    "live-runtime",
    "sessions",
    "tasks",
    "agents",
    "tools",
    "events",
    "background-jobs",
    "flows",
    "memory",
    "mcp-connections",
    "artifacts",
  ] as const;

  for (const slug of REDIRECTING_PAGES) {
    test(`disconnected navigation to /${slug} redirects to /settings (connect, don't strand)`, async ({
      page,
      runtime,
      helpers,
    }) => {
      // Seed auth ONLY — no Runtime triple, so resolveConnection() is
      // null and the layout redirect fires.
      await helpers.seedAuth(runtime.token);
      const path = slug === "overview" ? "/overview" : `/${slug}`;
      await page.goto(new URL(path, runtime.baseURL).toString());
      await page.waitForLoadState("load");

      // The shell's first-load redirect lands the operator on /settings,
      // where the AttachToLocalCard + Add Runtime form let them connect.
      await expect
        .poll(() => new URL(page.url()).pathname, { timeout: 5000 })
        .toMatch(/^\/settings(\/.*)?$/);
    });
  }

  test("(N2) the disconnected /settings landing renders exactly ONE ConnectionFooter", async ({
    page,
    runtime,
    helpers,
  }) => {
    // A disconnected cold load redirects to /settings; the app shell
    // renders exactly one ConnectionFooter (the pre-83s pages duplicated
    // it via per-page imports).
    await helpers.seedAuth(runtime.token);
    await page.goto(new URL("/overview", runtime.baseURL).toString());
    await expect
      .poll(() => new URL(page.url()).pathname, { timeout: 5000 })
      .toMatch(/^\/settings(\/.*)?$/);
    await expect(
      page.locator("[data-testid='connection-footer']"),
      "the app shell renders exactly one ConnectionFooter (N2)",
    ).toHaveCount(1);
  });

  test("(N7) the saved-view button label is 'Save view' on every page", async ({
    page,
    helpers,
  }) => {
    // Save-view label consistency is a CONNECTED-state concern — a
    // disconnected Console redirects to /settings, where most saved-view
    // sites do not exist. Seed a live connection so the pages render and
    // the label drift ("Save filter" / "Save snapshot" / "Save preset" /
    // bare "Save") is collapsed onto one verb across the catalog.
    await helpers.seedConnection();

    const sites: Array<{
      slug: Parameters<typeof helpers.gotoPage>[0];
      testid: string;
    }> = [
      // Overview (108c) and Live Runtime (108d) dropped their saved-view bars —
      // the mocks have no saved-view strip on those pages — so neither appears
      // in this per-page Save-view label check.
      { slug: "sessions", testid: "sessions-save-view" },
      { slug: "tasks", testid: "tasks-save-filter" },
      { slug: "agents", testid: "agents-save-view" },
      { slug: "tools", testid: "tools-save-filter" },
      { slug: "events", testid: "save-view" },
      { slug: "background-jobs", testid: "bg-save-filter" },
      { slug: "flows", testid: "flows-save-view" },
      { slug: "memory", testid: "memory-save-view" },
      { slug: "artifacts", testid: "save-view" },
      { slug: "mcp-connections", testid: "save-view" },
    ];

    for (const { slug, testid } of sites) {
      await helpers.gotoPage(slug);
      const btn = page.locator(`[data-testid='${testid}']`);
      await expect(
        btn,
        `${slug}: the saved-view button renders`,
      ).toBeVisible();
      await expect(
        btn,
        `${slug}: the saved-view button label is 'Save view' (N7)`,
      ).toHaveText(/Save view/);
    }
  });
});
