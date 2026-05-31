// Overview page Playwright spec (Phase 73a / 108c rebuild).
//
// Rides the Phase 75 harness baseline (`tests/fixtures/page.ts`): the `runtime`
// fixture boots a per-run Harbor Runtime + `harbor console`; the suite gates on
// `consoleSubcommandAvailable()`. The Overview is composition over the shipped
// surface (`runtime.counters`/`runtime.health`/`pause.list`/`events.subscribe`/
// `approve`/`reject`) — Phase 108c rebuilt the canvas to the mock and removed
// the top FilterBar + right DetailRail + the +New menu (search/+New are
// app-shell chrome now — 108b). Assertions target the rebuilt surface.

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";
import { STORAGE_KEYS } from "../src/lib/connection";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

async function seedConnection(
  page: import("@playwright/test").Page,
  baseURL: string,
  token: string,
): Promise<void> {
  await page.addInitScript(
    ([keys, base, tok]) => {
      window.localStorage.setItem(keys.baseURL, base);
      window.localStorage.setItem(keys.token, tok);
      window.localStorage.setItem(keys.tenant, "dev");
      window.localStorage.setItem(keys.user, "dev");
      // D-171 — session blank on the connection (per-request).
      window.localStorage.setItem(keys.session, "");
    },
    [STORAGE_KEYS, baseURL, token] as const,
  );
}

test.describe("Overview page", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent (pre-Phase-73m) or bin/harbor not built",
  );

  test("the page mounts with the rebuilt canvas surfaces", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("overview");

    await expect(page.locator("[data-testid='overview-page']")).toBeAttached();
    // The single global status bar (chrome) still renders under the page.
    await expect(page.locator("[data-testid='connection-footer']").first()).toBeVisible();
    // Row-1: the slim context row (health pill + audit ribbon) — NOT a top
    // FilterBar (108c removed it) and NOT a right detail-rail.
    await expect(page.locator("[data-testid='overview-context-row']")).toBeVisible();
    await expect(page.locator("[data-testid='overview-health-pill']")).toBeVisible();
    await expect(page.locator("[data-testid='overview-audit-ribbon']")).toBeVisible();
    // The removed surfaces must NOT be present.
    await expect(page.locator("[data-testid='overview-window-facet']")).toHaveCount(0);
    await expect(page.locator("[data-testid='overview-activity-search']")).toHaveCount(0);
  });

  test("the counter row populates or a documented PageState renders", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("overview");

    const counters = page.locator("[data-testid='counter-row']");
    const error = page.locator("[data-testid='page-state-error']");
    const loading = page.locator("[data-testid='page-state-loading']");
    const empty = page.locator("[data-testid='page-state-empty']");
    const disconnected = page.locator("[data-testid='page-state-disconnected']");
    const info = page.locator("[data-testid='page-state-info']");
    await expect(
      counters.or(error).or(loading).or(empty).or(disconnected).or(info).first(),
    ).toBeVisible();

    if ((await counters.count()) > 0) {
      for (const id of ["counter-events", "counter-tasks", "counter-jobs", "counter-mcp"]) {
        await expect(page.locator(`[data-testid='${id}']`)).toBeVisible();
      }
    }
  });

  test("the cost panel renders with a model/agent axis selector", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("overview");

    await expect(page.locator("[data-testid='cost-rollup-card']")).toBeVisible();
    const model = page.locator("[data-testid='cost-axis-model']");
    const agent = page.locator("[data-testid='cost-axis-runtime']");
    await expect(model).toBeVisible();
    await expect(agent).toBeVisible();
    // Model is the default active axis; switching to Agent flips it.
    await expect(model).toHaveAttribute("data-active", "true");
    await agent.click();
    await expect(agent).toHaveAttribute("data-active", "true");
  });

  test("the intervention queue renders with scope-gated actions", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("overview");

    const queue = page.locator("[data-testid='intervention-queue']");
    const queueEmpty = page.locator("[data-testid='intervention-queue-state-empty']");
    const error = page.locator("[data-testid='page-state-error']");
    const loading = page.locator("[data-testid='page-state-loading']");
    const empty = page.locator("[data-testid='page-state-empty']");
    const disconnected = page.locator("[data-testid='page-state-disconnected']");
    const info = page.locator("[data-testid='page-state-info']");
    await expect(
      queue.or(queueEmpty).or(error).or(loading).or(empty).or(disconnected).or(info).first(),
    ).toBeVisible();

    const approve = page.locator("[data-testid='intervention-approve']").first();
    if ((await approve.count()) > 0) {
      await expect(approve).toBeDisabled();
    }
  });

  test("Quick Links navigate to their unprefixed routes", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("overview");

    const grid = page.locator("[data-testid='quick-links-grid']");
    await expect(grid).toBeVisible();
    // Exactly six tiles — no Evaluations tile (D-064).
    await expect(grid.locator("a")).toHaveCount(6);
    await page.locator("[data-testid='quick-link-tasks']").click();
    await expect(page).toHaveURL(/\/tasks$/);
  });

  test("a disconnected Console redirects to /settings to connect (Phase 105)", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("overview");
    await expect
      .poll(() => new URL(page.url()).pathname, { timeout: 5000 })
      .toMatch(/^\/settings(\/.*)?$/);
  });
});
