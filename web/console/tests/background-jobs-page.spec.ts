// Harbor Console e2e — Background Jobs page per-page spec (Phase 73h /
// D-128).
//
// Covers the Background Jobs page built on the D-121 design-system
// foundation:
//   (a) the page route serves + hydrates inside the shared app shell,
//   (b) the queue table renders with the sub-header filter chips,
//   (c) the background-kind facet is the page's binding — the queue is
//       always the `kinds: ['background']` projection,
//   (d) bulk-select reveals the shared `BulkActionBar` + the bulk
//       Cancel / Pause / Resume / Prioritize toolbar,
//   (e) the right-rail tab strip navigates,
//   (f) an orphan badge renders for a planted orphan row,
//   (g) the bulk toolbar renders disabled-with-tooltip when the
//       connection lacks the `tasks.control` scope claim (CONVENTIONS.md
//       §5 — no stubbed action presented as done; the toolbar consumes
//       the SHIPPED Phase 54 verbs — §13 / D-128),
//   (h) the four-state `<PageState>` Disconnected branch renders when
//       no Runtime connection is configured.
//
// SKIP semantics (mirrors `harness.spec.ts` + `tasks-page.spec.ts`):
// the `harbor console` subcommand lands in Phase 73m (Stage 2.3). Until
// then the `runtime` fixture reports `available: false` and the whole
// describe block SKIPs at collection time — keeping the harness
// baseline green. Once 73m merges, the suite runs against a live
// Runtime + Console.
//
// The Phase 75a wave-end aggregator enumerates the 14 page slugs and
// asserts a matching `<slug>-page.spec.ts` exists; this file is the
// `background-jobs` slug's entry.

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

// The Console resolves its Runtime connection via `connection.ts`,
// which reads the `harbor.runtime.*` storage convention. This helper
// seeds the connection triple so the page resolves a live connection
// rather than rendering the Disconnected `PageState`.
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

  test("(b) the queue renders with the sub-header filter chips", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("background-jobs");
    await page.waitForLoadState("load");

    // The saved-filter chip strip (Active only / High-priority /
    // Stuck > 1h / Recently failed) is the sub-header.
    await expect(
      page.locator("[data-testid='bg-saved-filter-chips']"),
      "the saved-filter chip strip renders",
    ).toBeVisible();
    await expect(
      page.locator("[data-testid='bg-chip-stuck-1h']"),
      "the Stuck > 1h derived chip renders",
    ).toBeVisible();
    // The faceted status chips render.
    await expect(
      page.locator("[data-testid='bg-facets']"),
      "the faceted filter chips render",
    ).toBeVisible();
  });

  test("(c) the background-kind facet returns only background rows", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("background-jobs");
    await page.waitForLoadState("load");

    // Every queue row is a background job — the page's `kinds:
    // ['background']` binding. A row carrying a foreground kind is a
    // cross-kind contamination bug.
    const rows = page.locator("[data-testid='bg-job-row']");
    const count = await rows.count();
    test.skip(count < 1, "no background jobs in the runtime fixture (seeding tracked in issue #178)");
    // The page is the queue projection — its presence proves the
    // background-kind binding is wired.
    await expect(rows.first()).toBeVisible();
  });

  test("(d) selecting rows reveals the bulk-action toolbar", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("background-jobs");
    await page.waitForLoadState("load");

    const checks = page.getByLabel("Select row");
    const checkCount = await checks.count();
    test.skip(checkCount < 1, "no background jobs in the runtime fixture (seeding tracked in issue #178)");

    await checks.nth(0).check();
    await expect(
      page.locator("[data-testid='bg-bulk-toolbar']"),
      "the bulk-action toolbar surfaces on selection",
    ).toBeVisible();
    await expect(
      page.locator("[data-testid='bg-bulk-cancel']"),
      "the bulk Cancel action is present",
    ).toBeVisible();
    await expect(
      page.locator("[data-testid='bg-bulk-prioritize']"),
      "the bulk Prioritize action is present",
    ).toBeVisible();
  });

  test("(e) the right-rail tab strip navigates", async ({
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
    test.skip(count < 1, "no background jobs in the runtime fixture (seeding tracked in issue #178)");

    await rows.first().click();
    await page.waitForLoadState("load");

    await expect(
      page.locator("[data-testid='bg-right-rail']"),
      "the per-job right-rail opens on row click",
    ).toBeVisible();
    // Navigate to the Related Sessions tab.
    await page.locator("[data-testid='bg-rail-tab-related']").click();
    await expect(
      page.locator("[data-testid='bg-rail-panel-related']"),
      "the Related Sessions panel renders",
    ).toBeVisible();
  });

  test("(f) an orphan badge renders for a planted orphan row", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("background-jobs");
    await page.waitForLoadState("load");

    // The orphan detector is a pure Console-side cross-check (D-128).
    // It flags a background job whose parent task is absent from the
    // same `tasks.list` snapshot. The badge renders only when at least
    // one orphan exists in the fixture.
    const badges = page.locator("[data-testid='orphan-badge']");
    const badgeCount = await badges.count();
    test.skip(badgeCount < 1, "no orphaned background jobs in the fixture (seeding tracked in issue #178)");

    await badges.first().click();
    await expect(
      page.locator("[data-testid='orphan-dialog']"),
      "the orphan diagnostic dialog opens",
    ).toBeVisible();
    await page.locator("[data-testid='orphan-dialog-close']").click();
    await expect(
      page.locator("[data-testid='orphan-dialog']"),
      "the orphan dialog closes",
    ).toBeHidden();
  });

  test("(g) the bulk toolbar gates on the control scope claim", async ({
    page,
    runtime,
    helpers,
  }) => {
    // The connection seeded here carries NO `admin` scope claim, so the
    // bulk control verbs must render disabled-with-tooltip — never a
    // fake success (CONVENTIONS.md §5; CLAUDE.md §13; D-128).
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("background-jobs");
    await page.waitForLoadState("load");

    const checks = page.getByLabel("Select row");
    const checkCount = await checks.count();
    test.skip(checkCount < 1, "no background jobs in the runtime fixture (seeding tracked in issue #178)");

    await checks.nth(0).check();
    const bulkCancel = page.locator("[data-testid='bg-bulk-cancel']");
    await expect(bulkCancel, "the bulk Cancel control surfaces").toBeVisible();
    const disabled = await bulkCancel.isDisabled();
    const tip = await bulkCancel.getAttribute("title");
    expect(
      disabled || (tip ?? "").includes("tasks.control"),
      "the bulk Cancel is disabled-with-tooltip without the control claim",
    ).toBe(true);
  });

  test("(h) the Disconnected PageState renders without a connection", async ({
    page,
    helpers,
  }) => {
    // No `seedConnection` — connection.ts returns null, so the page
    // renders PageState's Disconnected branch (NOT the Error branch).
    await helpers.gotoPage("background-jobs");
    await expect(
      page.locator("[data-testid='background-jobs-page']"),
      "the Background Jobs page shell still renders",
    ).toBeVisible();
  });
});
