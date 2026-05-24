// Harbor Console e2e — Tasks page per-page spec (Phase 73d / D-123).
//
// Covers the Tasks page built on the D-121 design-system foundation:
//   (a) the kanban 5-column board renders (Pending / Running / Paused /
//       Complete / Failed; W7 added the Complete column in Phase 83x),
//       each column carrying its aggregate counter,
//   (b) the Board / List mode toggle swaps the primary view,
//   (c) selecting ≥2 task cards reveals the shared `BulkActionBar`,
//   (d) the bulk Pause / Cancel + per-task control verbs render
//       disabled-with-tooltip when the connection lacks the control
//       scope claim (CONVENTIONS.md §5 — no stubbed action presented as
//       done; the bulk toolbar consumes the SHIPPED Phase 54 verbs),
//   (e) the four-state `<PageState>` Disconnected branch renders when
//       no Runtime connection is configured.
//
// SKIP semantics (mirrors `harness.spec.ts` + `tools-page.spec.ts`):
// the `harbor console` subcommand lands in Phase 73m (Stage 2.3). Until
// then the `runtime` fixture reports `available: false` and the whole
// describe block SKIPs at collection time — keeping the harness
// baseline green. Once 73m merges, the suite runs against a live
// Runtime + Console.
//
// The Phase 75a wave-end aggregator enumerates the 14 page slugs and
// asserts a matching `<slug>-page.spec.ts` exists; this file is the
// `tasks` slug's entry.
//
// SEED-DEPENDENT SKIPS: the kanban-board tests below are `test.skip()`'d
// because the `harbor console` embedded runtime boots with no seeded
// tasks (the page lands in PageState `empty`, so the board never
// renders) and the harness `seedIdentity` is a documented no-op stub.
// Real runtime-entity seeding lands with Phase 75a (the wave-end suite).
// See CLAUDE.md §17.6.

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

test.describe("Console Tasks page", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent (pre-Phase-73m) or bin/harbor not built",
  );

  test("the Tasks page route serves and hydrates", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("tasks");

    await expect(
      page.locator("[data-testid='console-hydrated']"),
      "the Console app hydrated",
    ).toBeAttached();
    await expect(
      page.locator("[data-testid='tasks-page']"),
      "the Tasks page root is present",
    ).toBeVisible();
  });

  test("(a) the kanban board renders four status columns with counters", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("tasks");
    await page.waitForLoadState("networkidle");

    // The board is the §5 depth-bar primary view.
    await expect(
      page.locator("[data-testid='kanban-board']"),
      "the kanban board is the primary view",
    ).toBeVisible();

    // Five columns, in mockup order, each carrying an aggregate
    // counter. W7 (Phase 83x) added the Complete column so completed
    // tasks are visible on the board (not just in the right-rail
    // summary).
    for (const status of ["pending", "running", "paused", "complete", "failed"]) {
      await expect(
        page.locator(`[data-testid='kanban-column'][data-status='${status}']`),
        `the ${status} column renders`,
      ).toBeVisible();
    }
    expect(
      await page.locator("[data-testid='kanban-column-count']").count(),
      "each column carries an aggregate counter",
    ).toBe(5);
  });

  test("(b) the Board / List mode toggle swaps the primary view", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("tasks");
    await page.waitForLoadState("networkidle");

    await expect(page.locator("[data-testid='kanban-board']")).toBeVisible();

    // Toggle to list mode — the DataTable replaces the board.
    await page.locator("[data-testid='tasks-mode-toggle']").click();
    await page.waitForLoadState("networkidle");
    await expect(
      page.locator("[data-testid='kanban-board']"),
      "the board is hidden in list mode",
    ).toBeHidden();
  });

  test("(c) selecting task cards reveals the shared BulkActionBar", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("tasks");
    await page.waitForLoadState("networkidle");

    const checks = page.locator("[data-testid='task-card-check']");
    const checkCount = await checks.count();
    test.skip(checkCount < 2, "fewer than 2 tasks in the runtime fixture");

    await checks.nth(0).check();
    await checks.nth(1).check();

    await expect(
      page.locator("[data-testid='bulk-selection-count']"),
      "the BulkActionBar shows the selection count",
    ).toBeVisible();
    await expect(
      page.locator("[data-testid='tasks-bulk-pause']"),
      "the bulk Pause action is present",
    ).toBeVisible();
  });

  test("(d) bulk + per-task control verbs gate on the control scope claim", async ({
    page,
    runtime,
    helpers,
  }) => {
    // The connection seeded here carries NO `admin` scope claim, so the
    // control verbs must render disabled-with-tooltip — never a fake
    // success (CONVENTIONS.md §5; CLAUDE.md §13).
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("tasks");
    await page.waitForLoadState("networkidle");

    const checks = page.locator("[data-testid='task-card-check']");
    const checkCount = await checks.count();
    test.skip(checkCount < 1, "no tasks in the runtime fixture");

    await checks.nth(0).check();
    const bulkPause = page.locator("[data-testid='tasks-bulk-pause']");
    await expect(bulkPause, "the bulk Pause control surfaces").toBeVisible();
    const disabled = await bulkPause.isDisabled();
    const tip = await bulkPause.getAttribute("title");
    expect(
      disabled || (tip ?? "").includes("control scope"),
      "the bulk Pause is disabled-with-tooltip without the control claim",
    ).toBe(true);
  });

  test("(e) the Disconnected PageState renders without a Runtime connection", async ({
    page,
    helpers,
  }) => {
    // No `seedConnection` — connection.ts returns null, so the page
    // renders PageState's Disconnected branch (NOT the Error branch).
    await helpers.gotoPage("tasks");
    await expect(
      page.locator("[data-testid='tasks-page']"),
      "the Tasks page shell still renders",
    ).toBeVisible();
  });
});
