// Phase 73k (D-119) — MCP Connections page Playwright spec.
//
// This is the per-page e2e spec for the Console MCP Connections page. It
// rides on the Phase 75 harness baseline (`tests/fixtures/page.ts`): the
// `runtime` fixture boots a per-run Harbor Runtime + `harbor console`
// instance, and the suite gates on `consoleSubcommandAvailable()`.
//
// SKIP semantics (mirrors the harness — the directory-/subcommand-missing
// → SKIP pattern): when the `harbor console` subcommand is absent
// (pre-Phase-73m) or `bin/harbor` is not built, the whole describe block
// SKIPs cleanly so the harness baseline stays green. Once `harbor console`
// lands, these assertions run against the live MCP Connections page.
//
// Coverage (Phase 73k acceptance criteria, AC "Playwright spec"):
//   (a) servers list renders ≥1 row OR the documented empty state;
//   (b) state badges render with the canonical state class;
//   (c) drill-in to per-server detail;
//   (d) each detail tab paints;
//   (e) refresh-discovery as a non-admin surfaces a scope-mismatch error;
//   (f) the raw-HTML toggle is disabled for non-admins;
//   (g) deep-link from the Tools tab targets /console/tools?server=…;
//   (h) a missing-server detail load surfaces the not-found state.

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

test.describe("MCP Connections page", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent (pre-Phase-73m) or bin/harbor not built",
  );

  test("the servers list renders (rows or the documented empty state)", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("mcp-connections");

    // The list section always renders; it shows either a populated table
    // or the documented "no MCP servers configured" empty state.
    await expect(
      page.locator("[data-testid='mcp-connections-list']"),
      "the MCP Connections list section is present",
    ).toBeAttached();

    const table = page.locator("[data-testid='servers-table']");
    const empty = page.locator("[data-testid='list-empty']");
    await expect(table.or(empty), "list shows a table or the empty state").toBeVisible();
  });

  test("state badges render with the canonical state class", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("mcp-connections");

    const table = page.locator("[data-testid='servers-table']");
    if (await table.isVisible()) {
      const firstBadge = page.locator("[data-testid^='status-']").first();
      await expect(firstBadge, "a status badge renders").toBeVisible();
      const cls = await firstBadge.getAttribute("class");
      expect(cls, "the badge carries a canonical chip-<state> class").toMatch(
        /chip-(online|reconnecting|offline|auth_pending|error)/,
      );
    }
  });

  test("drilling into a server opens the detail view with six tabs", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("mcp-connections");

    const firstLink = page.locator("[data-testid^='server-row-'] a.server-link").first();
    if ((await firstLink.count()) === 0) {
      test.skip(true, "no MCP servers configured on the dev runtime");
      return;
    }
    await firstLink.click();
    await page.waitForLoadState("networkidle");

    await expect(
      page.locator("[data-testid='mcp-connections-detail']"),
      "the per-server detail view renders",
    ).toBeAttached();

    for (const tab of ["tools", "resources", "prompts", "oauth", "health", "policy"]) {
      await expect(
        page.locator(`[data-testid='tab-${tab}']`),
        `the ${tab} tab is present`,
      ).toBeVisible();
    }
  });

  test("each detail tab paints when selected", async ({ page, runtime, helpers }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("mcp-connections");

    const firstLink = page.locator("[data-testid^='server-row-'] a.server-link").first();
    if ((await firstLink.count()) === 0) {
      test.skip(true, "no MCP servers configured on the dev runtime");
      return;
    }
    await firstLink.click();
    await page.waitForLoadState("networkidle");

    for (const tab of ["resources", "prompts", "oauth", "health", "policy", "tools"]) {
      await page.locator(`[data-testid='tab-${tab}']`).click();
      await expect(
        page.locator(`[data-testid='tab-body-${tab}']`),
        `the ${tab} tab body paints`,
      ).toBeVisible();
    }
  });

  test("refresh-discovery without the control claim surfaces a scope error", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("mcp-connections");

    const firstLink = page.locator("[data-testid^='server-row-'] a.server-link").first();
    if ((await firstLink.count()) === 0) {
      test.skip(true, "no MCP servers configured on the dev runtime");
      return;
    }
    await firstLink.click();
    await page.waitForLoadState("networkidle");

    await page.locator("[data-testid='refresh-discovery']").click();
    // The dev runtime token carries no admin/control claim, so the
    // control-plane verb returns CodeScopeMismatch — the page renders the
    // mapped action-error message.
    await expect(
      page.locator("[data-testid='action-error']"),
      "refresh-discovery without the control claim surfaces a visible error",
    ).toBeVisible();
  });

  test("the raw-HTML toggle is disabled for a non-admin session", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("mcp-connections");

    const firstLink = page.locator("[data-testid^='server-row-'] a.server-link").first();
    if ((await firstLink.count()) === 0) {
      test.skip(true, "no MCP servers configured on the dev runtime");
      return;
    }
    await firstLink.click();
    await page.waitForLoadState("networkidle");

    const toggle = page.locator("[data-testid='raw-html-toggle']");
    await expect(toggle, "the raw-HTML toggle renders").toBeVisible();
    await expect(
      toggle,
      "the raw-HTML toggle is disabled for a non-admin session",
    ).toBeDisabled();
  });

  test("the Tools tab deep-links to the Tools page scoped to the server", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("mcp-connections");

    const firstLink = page.locator("[data-testid^='server-row-'] a.server-link").first();
    if ((await firstLink.count()) === 0) {
      test.skip(true, "no MCP servers configured on the dev runtime");
      return;
    }
    await firstLink.click();
    await page.waitForLoadState("networkidle");

    await page.locator("[data-testid='tab-tools']").click();
    const deepLink = page.locator("[data-testid='tools-deep-link']");
    await expect(deepLink, "the Tools-tab deep-link renders").toBeVisible();
    const href = await deepLink.getAttribute("href");
    expect(href, "the deep-link targets /console/tools?server=…").toMatch(
      /\/console\/tools\?server=/,
    );
  });

  test("a missing server surfaces the not-found state", async ({ page, runtime }) => {
    const response = await page.goto(
      new URL("/mcp-connections/__nonexistent-server__", runtime.baseURL).toString(),
    );
    expect(response, "navigation returned a response").not.toBeNull();
    // The route loads; the page surfaces the mapped not-found error rather
    // than a 5xx (failure-mode coverage, CLAUDE.md §17.3 #3).
    expect(response!.status(), "the detail route does not 5xx").toBeLessThan(500);
    await page.waitForLoadState("networkidle");
    await expect(
      page.locator("[data-testid='detail-error']"),
      "the not-found state renders for an unknown server",
    ).toBeVisible();
  });
});
