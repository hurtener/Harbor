// Harbor Console e2e — Background Jobs page per-page spec (Phase 108j /
// D-182; supersedes the Phase 73h / D-128 pre-chrome spec).
//
// Covers the rebuilt Background Jobs page — the carded, viewport-locked
// Events-108h composition (TABLE-primary + a right-rail detail; no
// mode-switch):
//   (a) the page route serves + hydrates inside the shared app shell,
//   (b) the carded filter strip renders the saved-filter + faceted chips,
//   (c) the queue is the `kinds: ['background']` projection (TABLE-primary),
//   (d) bulk-select reveals the shared BulkActionBar + the bulk toolbar,
//   (e) clicking a row opens the right-rail detail + its tab strip navigates,
//   (f) an orphan badge renders for a planted orphan row,
//   (g) the bulk toolbar renders disabled-with-tooltip without the control
//       scope claim (CONVENTIONS.md §5 — no stubbed action; the toolbar
//       consumes the SHIPPED Phase 54 verbs — §13 / D-128),
//   (h) the document is viewport-locked (no full-page scroll),
//   (i) a disconnected Console redirects to /settings (Phase 105).
//
// SKIP semantics (mirrors `harness.spec.ts` + `tasks-page.spec.ts`): the
// `harbor console` subcommand lands in Phase 73m; until then the `runtime`
// fixture reports `available: false` and the describe block SKIPs at
// collection time. The `<slug>-page.spec.ts` aggregator (Phase 75a)
// expects this file for the `background-jobs` slug.

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

// The Console resolves its Runtime connection via `connection.ts`, which
// reads the `harbor.runtime.*` storage convention. Seed the triple so the
// page resolves a live connection rather than the Disconnected redirect.
async function seedConnection(
  page: import("@playwright/test").Page,
  baseURL: string,
  token: string,
): Promise<void> {
  await page.addInitScript(
    ([b, t]) => {
      window.localStorage.setItem("harbor.runtime.base_url", b);
      window.localStorage.setItem("harbor.runtime.token", t);
      window.localStorage.setItem("harbor.runtime.tenant", "dev");
      window.localStorage.setItem("harbor.runtime.user", "dev");
      window.localStorage.setItem("harbor.runtime.session", "dev");
    },
    [baseURL, token] as const,
  );
}

test.describe("Console Background Jobs page", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent (pre-Phase-73m) or bin/harbor not built",
  );

  test("(a) the Background Jobs route serves and hydrates", async ({ page, runtime, helpers }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("background-jobs");

    await expect(
      page.locator("[data-testid='console-hydrated']"),
      "the Console app hydrated",
    ).toBeAttached();
    await expect(
      page.locator("[data-testid='background-jobs-page']"),
      "the Background Jobs page root is present",
    ).toBeVisible();
  });

  test("(b) the carded filter strip renders the chips", async ({ page, runtime, helpers }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("background-jobs");
    await page.waitForLoadState("load");

    await expect(
      page.locator("[data-testid='bg-saved-filter-chips']"),
      "the saved-filter chip strip renders",
    ).toBeVisible();
    await expect(
      page.locator("[data-testid='bg-chip-stuck-1h']"),
      "the Stuck > 1h derived chip renders",
    ).toBeVisible();
    await expect(
      page.locator("[data-testid='bg-facets']"),
      "the faceted filter chips render",
    ).toBeVisible();
    // The idle right rail renders its hint until a row is selected.
    await expect(
      page.locator("[data-testid='bg-rail-idle']").or(page.locator("[data-testid='bg-right-rail']")).first(),
      "the right column renders the idle hint or a selected-job detail",
    ).toBeVisible();
  });

  test("(c) the queue is the background-kind projection", async ({ page, runtime, helpers }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("background-jobs");
    await page.waitForLoadState("load");

    const rows = page.locator("[data-testid='bg-job-row']");
    const count = await rows.count();
    test.skip(count < 1, "no background jobs in the runtime fixture (seed with HARBOR_DEV_SEED_FIXTURES=1)");
    await expect(rows.first()).toBeVisible();
    // Every row carries a derived type badge (honest — never a fabricated agent).
    await expect(page.locator("[data-testid='bg-job-type']").first()).toBeVisible();
  });

  test("(d) selecting rows reveals the bulk-action toolbar", async ({ page, runtime, helpers }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("background-jobs");
    await page.waitForLoadState("load");

    const checks = page.getByLabel("Select row");
    const checkCount = await checks.count();
    test.skip(checkCount < 1, "no background jobs in the runtime fixture (seed with HARBOR_DEV_SEED_FIXTURES=1)");

    await checks.nth(0).check();
    await expect(
      page.locator("[data-testid='bg-bulk-toolbar']"),
      "the bulk-action toolbar surfaces on selection",
    ).toBeVisible();
    await expect(page.locator("[data-testid='bg-bulk-cancel']")).toBeVisible();
    await expect(page.locator("[data-testid='bg-bulk-prioritize']")).toBeVisible();
  });

  test("(e) clicking a row opens the right-rail detail + tabs navigate", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("background-jobs");
    await page.waitForLoadState("load");

    const rows = page.locator("[data-testid='bg-job-row']");
    const count = await rows.count();
    test.skip(count < 1, "no background jobs in the runtime fixture (seed with HARBOR_DEV_SEED_FIXTURES=1)");

    await rows.first().click();
    await expect(
      page.locator("[data-testid='bg-right-rail']"),
      "the per-job right-rail opens on row click",
    ).toBeVisible();
    // Navigate to the Progress tab, then Events.
    await page.locator("[data-testid='bg-rail-tab-progress']").click();
    await expect(page.locator("[data-testid='bg-rail-panel-progress']")).toBeVisible();
    await page.locator("[data-testid='bg-rail-tab-events']").click();
    await expect(page.locator("[data-testid='bg-rail-panel-events']")).toBeVisible();
  });

  test("(f) an orphan badge renders for a planted orphan row", async ({ page, runtime, helpers }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("background-jobs");
    await page.waitForLoadState("load");

    const badges = page.locator("[data-testid='orphan-badge']");
    const badgeCount = await badges.count();
    test.skip(badgeCount < 1, "no orphaned background jobs in the fixture (seed with HARBOR_DEV_SEED_FIXTURES=1)");

    await badges.first().click();
    await expect(page.locator("[data-testid='orphan-dialog']")).toBeVisible();
    await page.locator("[data-testid='orphan-dialog-close']").click();
    await expect(page.locator("[data-testid='orphan-dialog']")).toBeHidden();
  });

  test("(g) the bulk toolbar gates on the control scope claim", async ({ page, runtime, helpers }) => {
    // The connection seeded here carries NO `admin` scope claim, so the
    // bulk control verbs must render disabled-with-tooltip — never a fake
    // success (CONVENTIONS.md §5; CLAUDE.md §13; D-128).
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("background-jobs");
    await page.waitForLoadState("load");

    const checks = page.getByLabel("Select row");
    const checkCount = await checks.count();
    test.skip(checkCount < 1, "no background jobs in the runtime fixture (seed with HARBOR_DEV_SEED_FIXTURES=1)");

    await checks.nth(0).check();
    const bulkCancel = page.locator("[data-testid='bg-bulk-cancel']");
    await expect(bulkCancel).toBeVisible();
    const disabled = await bulkCancel.isDisabled();
    const tip = await bulkCancel.getAttribute("title");
    expect(
      disabled || (tip ?? "").includes("tasks.control"),
      "the bulk Cancel is disabled-with-tooltip without the control claim",
    ).toBe(true);
  });

  test("(h) the page is viewport-locked (no full-page scroll)", async ({ page, runtime, helpers }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("background-jobs");
    await page.waitForLoadState("load");
    await expect(page.locator("[data-testid='background-jobs-page']")).toBeVisible();

    // The document must not full-page-scroll — only the page's own regions
    // (the queue table + the right rail) scroll internally (PAGE-POLISH §6).
    const overflow = await page.evaluate(
      () => document.documentElement.scrollHeight - document.documentElement.clientHeight,
    );
    expect(overflow, "the document does not full-page-scroll").toBeLessThanOrEqual(2);
  });

  test("(i) a disconnected Console redirects to /settings to connect (Phase 105)", async ({
    page,
    helpers,
  }) => {
    // No connection seeded — connection.ts returns null. The app shell
    // redirects a disconnected Console to /settings rather than stranding it.
    await helpers.gotoPage("background-jobs");
    await expect
      .poll(() => new URL(page.url()).pathname, { timeout: 5000 })
      .toMatch(/^\/settings(\/.*)?$/);
  });
});
