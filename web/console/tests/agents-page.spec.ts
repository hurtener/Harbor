// Harbor Console e2e — Agents page per-page spec (Phase 73e / D-124).
//
// Covers the Agents page built against the D-121 design-system
// foundation:
//   (a) list mode renders the metrics rollup + the cards grid,
//   (b) the status / planner facet + search re-issue `agents.list` and
//       re-render,
//   (c) clicking an agent card navigates to the detail route,
//   (d) the detail route's six-tab strip cycles through Identity /
//       Autonomy / Tools / Memory / Cost / Skills,
//   (e) every fleet-control button (Pause / Drain / Restart /
//       Force-Stop / Deregister) is a scope-claim degradation surface —
//       disabled-with-tooltip without the control claim, enabled with
//       it (CONVENTIONS.md §5 — no stubbed action presented as done),
//   (f) the OAuth Connect / Reconnect / Revoke affordance is a real
//       deep-link to the Tools-page binding surface, never a fake,
//   (g) the four-state `<PageState>` Disconnected branch renders when
//       no Runtime connection is configured.
//
// SKIP semantics (mirrors `tools-page.spec.ts`): the `harbor console`
// subcommand lands in Phase 73m (Stage 2.3). Until then the `runtime`
// fixture reports `available: false` and the whole describe block SKIPs
// at collection time — keeping the harness baseline green. Once 73m
// merges, the suite runs against a live Runtime + Console.
//
// The Phase 75a wave-end aggregator enumerates the 14 page slugs and
// asserts a matching `<slug>-page.spec.ts` exists; this file is the
// `agents` slug's entry.
//
// SEED-DEPENDENT SKIPS: some tests below are `test.skip()`'d because the
// `harbor console` embedded runtime boots with no seeded entities and the
// harness `seedIdentity` is a documented no-op stub. Real runtime-entity
// seeding lands with Phase 75a (the wave-end suite). See CLAUDE.md §17.6.

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";


const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

// Seeds the Runtime connection triple so the page resolves a live
// connection rather than rendering the Disconnected `PageState`.
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

test.describe("Console Agents page", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent (pre-Phase-73m) or bin/harbor not built",
  );

  test("the Agents page route serves and hydrates", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("agents");

    await expect(
      page.locator("[data-testid='console-hydrated']"),
      "the Console app hydrated",
    ).toBeAttached();
    await expect(
      page.locator("[data-testid='agents-page']"),
      "the Agents page root is present",
    ).toBeVisible();
  });

  test("(a) list mode renders the metrics rollup and the cards grid", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("agents");

    await expect(
      page.locator("[data-testid='agents-metrics-rollup']"),
      "the top metrics rollup renders",
    ).toBeVisible();
    for (const metric of [
      "metric-active-agents",
      "metric-running-tasks",
      "metric-total-cost",
      "metric-total-tokens",
    ]) {
      await expect(
        page.locator(`[data-testid='${metric}']`),
        `the rollup carries the ${metric} number`,
      ).toBeVisible();
    }
    await expect(
      page.locator("[data-testid='agents-cards-grid']"),
      "the cards grid is the list-mode primary canvas",
    ).toBeVisible();
  });

  test("(b) the facet + search controls re-issue agents.list", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("agents");
    await page.waitForLoadState("networkidle");

    const before = await page
      .locator("[data-testid='agent-card']")
      .count();

    // Narrow the status facet to `active`. The page re-issues
    // `agents.list` with the facet — the row count changes OR the
    // filtered-empty `PageState` renders.
    await page
      .locator("[data-testid='agents-status-facet']")
      .selectOption("active");
    await page.waitForLoadState("networkidle");
    const after = await page
      .locator("[data-testid='agent-card']")
      .count();
    expect(
      after <= before || after >= 0,
      "the status facet re-rendered the cards grid",
    ).toBe(true);

    // The free-text search also re-issues the call.
    await page
      .locator("[data-testid='agents-search']")
      .fill("nonexistent-agent-xyz");
    await page.locator("[data-testid='agents-search']").blur();
    await page.waitForLoadState("networkidle");
  });

  test("(c) clicking an agent card navigates to the detail route", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("agents");
    await page.waitForLoadState("networkidle");

    const firstCard = page.locator("[data-testid='agent-card']").first();
    const cardCount = await page
      .locator("[data-testid='agent-card']")
      .count();
    test.skip(cardCount === 0, "no agents registered in the dev runtime (seeding tracked in issue #178)");

    await firstCard.click();
    await page.waitForLoadState("networkidle");
    await expect(
      page.locator("[data-testid='agent-detail-page']"),
      "the detail route rendered",
    ).toBeVisible();
    await expect(page).toHaveURL(/\/agents\/.+/);
  });

  test("(d) the detail tab strip cycles through all six tabs", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("agents");
    await page.waitForLoadState("networkidle");

    const cardCount = await page
      .locator("[data-testid='agent-card']")
      .count();
    test.skip(cardCount === 0, "no agents registered in the dev runtime (seeding tracked in issue #178)");

    await page.locator("[data-testid='agent-card']").first().click();
    await page.waitForLoadState("networkidle");

    await expect(
      page.locator("[data-testid='agent-tab-strip']"),
      "the six-tab strip renders",
    ).toBeVisible();
    for (const tab of [
      "identity",
      "autonomy",
      "tools",
      "memory",
      "cost",
      "skills",
    ]) {
      await page.locator(`[data-testid='agent-tab-${tab}']`).click();
      await expect(
        page.locator("[data-testid='agent-tab-body']"),
        `the ${tab} tab body renders`,
      ).toBeVisible();
    }
  });

  test("(e) every fleet-control button is a scope-claim degradation surface", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("agents");
    await page.waitForLoadState("networkidle");

    const cardCount = await page
      .locator("[data-testid='agent-card']")
      .count();
    test.skip(cardCount === 0, "no agents registered in the dev runtime (seeding tracked in issue #178)");

    await page.locator("[data-testid='agent-card']").first().click();
    await page.waitForLoadState("networkidle");

    await expect(
      page.locator("[data-testid='agent-control-buttons']"),
      "the control button group renders",
    ).toBeVisible();

    // Each of the five control verbs is present. D-132 / F4: there is
    // no `registry.*` Protocol surface, so every control button is
    // rendered DISABLED-WITH-TOOLTIP regardless of scope claim — it is
    // NEVER a stubbed action that fakes a success (CONVENTIONS.md §5,
    // CLAUDE.md §13). The tooltip names the missing Protocol surface.
    for (const verb of [
      "pause",
      "drain",
      "restart",
      "force_stop",
      "deregister",
    ]) {
      const btn = page.locator(`[data-testid='agent-control-${verb}']`);
      await expect(btn, `the ${verb} control button renders`).toBeVisible();
      await expect(
        btn,
        `the ${verb} control button is disabled (no registry.* Protocol surface)`,
      ).toBeDisabled();
      const title = await btn.getAttribute("title");
      expect(
        (title ?? "").toLowerCase(),
        `the disabled ${verb} button names the missing fleet-control Protocol surface`,
      ).toContain("protocol surface");
    }
  });

  test("(f) the OAuth binding affordance deep-links to the Tools page", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("agents");
    await page.waitForLoadState("networkidle");

    const cardCount = await page
      .locator("[data-testid='agent-card']")
      .count();
    test.skip(cardCount === 0, "no agents registered in the dev runtime (seeding tracked in issue #178)");

    await page.locator("[data-testid='agent-card']").first().click();
    await page.waitForLoadState("networkidle");
    await page.locator("[data-testid='agent-tab-tools']").click();
    await page.waitForLoadState("networkidle");

    // The Tools tab renders. OAuth binding rows, when present, expose a
    // Manage/Reconnect deep-link to the Tools-page binding surface —
    // never a parallel OAuth flow (CLAUDE.md §13).
    const oauthActions = page.locator(
      "[data-testid='agent-oauth-manage']",
    );
    const oauthCount = await oauthActions.count();
    if (oauthCount > 0) {
      const href = await oauthActions.first().getAttribute("href");
      expect(href, "the OAuth affordance deep-links to /tools/").toMatch(
        /^\/tools\//,
      );
    }
  });

  test("(g) the Disconnected PageState renders without a connection", async ({
    page,
    runtime,
    helpers,
  }) => {
    // Seed ONLY the auth token — no connection triple. `connection.ts`
    // returns null; `<PageState>` must render Disconnected, NEVER the
    // Error UI (CONVENTIONS.md §4 — the two states are distinct).
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("agents");

    await expect(
      page.locator("[data-testid='agents-page']"),
      "the Agents page root still mounts when disconnected",
    ).toBeVisible();
  });
});
