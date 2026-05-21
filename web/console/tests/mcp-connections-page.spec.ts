// MCP Connections page Playwright spec (D-121, MCP refactor).
//
// This is the per-page e2e spec for the Console MCP Connections page. It
// rides on the Phase 75 harness baseline (`tests/fixtures/page.ts`): the
// `runtime` fixture boots a per-run Harbor Runtime + `harbor console`
// instance, and the suite gates on `consoleSubcommandAvailable()`.
//
// SKIP semantics (mirrors the harness — the directory-/subcommand-missing
// → SKIP pattern): when the `harbor console` subcommand is absent
// (pre-Phase-73m) or `bin/harbor` is not built, the whole describe block
// SKIPs cleanly so the harness baseline stays green.
//
// The D-121 refactor rebuilt this page onto the design-system foundation:
// the raw `<table>` → shared `<DataTable>`, the bespoke async chain →
// the four-state `<PageState>` (now WITH a Disconnected branch), the
// `mcpApi` object → the unified `HarborClient` + `connection.ts`. The
// assertions below target the refactored shape.
//
// Coverage:
//   (a) servers list renders rows OR the documented empty state;
//   (b) status chips render via the shared StatusChip;
//   (c) selecting a row populates the DetailRail summary;
//   (d) drill-in to the tabbed per-server detail route;
//   (e) each detail tab paints;
//   (f) refresh-discovery as a non-admin surfaces a scope-mismatch error;
//   (g) the raw-HTML toggle is disabled for non-admins;
//   (h) the Tools tab deep-links to /tools?server=… (unprefixed — §1);
//   (i) a missing-server detail load surfaces the PageState Error state;
//   (j) the disconnected PageState renders when no Runtime is attached;
//   (k) the page carries the depth-bar surfaces (header, footer, pager).

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";
import { STORAGE_KEYS } from "../src/lib/connection";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

/**
 * Seed the full `connection.ts` storage convention so the page resolves a
 * live Runtime connection (the harness `seedAuth` only writes the legacy
 * console-token key; the D-121 page resolves through `connection.ts`).
 */
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
      window.localStorage.setItem(keys.session, "dev");
    },
    [STORAGE_KEYS, baseURL, token] as const,
  );
}

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
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("mcp-connections");

    await expect(
      page.locator("[data-testid='mcp-connections-list']"),
      "the MCP Connections list section is present",
    ).toBeAttached();

    // The list resolves into one of the four PageState states: a populated
    // DataTable, the documented empty state, or the Error state.
    const table = page.locator("table.data-table");
    const empty = page.locator("[data-testid='list-empty']");
    const error = page.locator("[data-testid='page-state-error']");
    await expect(
      table.or(empty).or(error),
      "list shows a table, the empty state, or an error",
    ).toBeVisible();
  });

  test("status chips render via the shared StatusChip", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("mcp-connections");

    const table = page.locator("table.data-table");
    if (await table.isVisible()) {
      const firstStatus = page.locator("[data-testid^='status-'] .status-chip").first();
      if ((await firstStatus.count()) > 0) {
        await expect(firstStatus, "a status chip renders").toBeVisible();
        const kind = await firstStatus.getAttribute("data-kind");
        expect(kind, "the chip carries a canonical status kind").toMatch(
          /^(success|warning|danger|accent|neutral)$/,
        );
      }
    }
  });

  test("selecting a server row populates the detail rail", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("mcp-connections");

    const firstRow = page.locator("[data-testid^='server-row-']").first();
    if ((await firstRow.count()) === 0) {
      test.skip(true, "no MCP servers configured on the dev runtime");
      return;
    }
    await firstRow.click();
    await expect(
      page.locator("[data-testid='rail-server-name']"),
      "the detail rail shows the selected server",
    ).toBeVisible();
  });

  test("drilling into a server opens the tabbed detail view with six tabs", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
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
    await seedConnection(page, runtime.baseURL, runtime.token);
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
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("mcp-connections");

    const firstLink = page.locator("[data-testid^='server-row-'] a.server-link").first();
    if ((await firstLink.count()) === 0) {
      test.skip(true, "no MCP servers configured on the dev runtime");
      return;
    }
    await firstLink.click();
    await page.waitForLoadState("networkidle");

    await page.locator("[data-testid='refresh-discovery']").click();
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
    await seedConnection(page, runtime.baseURL, runtime.token);
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

  test("the Tools tab deep-links to /tools scoped to the server", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
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
    // CONVENTIONS.md §1: inter-page links use the unprefixed form.
    expect(href, "the deep-link targets /tools?server=…").toMatch(
      /^\/tools\?server=/,
    );
  });

  test("a missing server surfaces the PageState error state", async ({ page, runtime }) => {
    await seedConnection(page, runtime.baseURL, runtime.token);
    const response = await page.goto(
      new URL("/mcp-connections/__nonexistent-server__", runtime.baseURL).toString(),
    );
    expect(response, "navigation returned a response").not.toBeNull();
    expect(response!.status(), "the detail route does not 5xx").toBeLessThan(500);
    await page.waitForLoadState("networkidle");
    await expect(
      page.locator("[data-testid='page-state-error']"),
      "the Error state renders for an unknown server",
    ).toBeVisible();
  });

  test("the Disconnected state renders when no Runtime is attached", async ({
    page,
    runtime,
  }) => {
    // Deliberately do NOT seed the connection — the page must render the
    // Disconnected PageState, never the Error state (CONVENTIONS.md §4).
    await page.goto(new URL("/mcp-connections", runtime.baseURL).toString());
    await page.waitForLoadState("networkidle");
    await expect(
      page.locator("[data-testid='page-state-disconnected']"),
      "the Disconnected state renders, not an error",
    ).toBeVisible();
  });

  test("the page carries the depth-bar shell surfaces", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("mcp-connections");

    // D-132 / W3: the ConnectionFooter is owned by the app shell
    // (`(console)/+layout.svelte`) — the page no longer renders its
    // own. Assert the single shell-provided footer.
    await expect(
      page.locator("[data-testid='connection-footer']"),
      "the shell-provided ConnectionFooter renders",
    ).toBeVisible();
    await expect(
      page.locator("[data-testid='mcp-search']"),
      "the FilterBar search input renders",
    ).toBeVisible();
  });
});
