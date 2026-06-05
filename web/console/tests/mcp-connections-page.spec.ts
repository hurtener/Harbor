// MCP Connections page Playwright spec (Phase 108m / D-185).
//
// This is the per-page e2e spec for the Console MCP Connections page. It
// rides on the Phase 75 harness baseline (`tests/fixtures/page.ts`): the
// `runtime` fixture boots a per-run Harbor Runtime + `harbor console`
// instance, and the suite gates on `consoleSubcommandAvailable()`.
//
// SKIP semantics (mirrors the harness — the directory-/subcommand-missing
// → SKIP pattern): when the `harbor console` subcommand is absent or
// `bin/harbor` is not built, the whole describe block SKIPs cleanly so the
// harness baseline stays green.
//
// The Phase 108m rebuild rethemed the page to the carded, viewport-locked
// master-detail composition: the servers TABLE on the left + a right-rail
// server detail on the right (the deepened header + five tabs + live
// Recent-events card). The separate tabbed-detail route was DROPPED — the
// rail is the single detail surface (§13: no two parallel implementations).
// The assertions below target that rebuilt shape.
//
// Coverage:
//   (a) servers list renders rows OR the documented empty/error state;
//   (b) status chips render via the shared StatusChip;
//   (c) selecting a row populates the right-rail detail;
//   (d) the rail carries the five tabs and each paints in place;
//   (e) the Tools tab deep-links to /tools?server=… (unprefixed — §1);
//   (f) the raw-HTML toggle is disabled for a non-admin session;
//   (g) the disconnected Console redirects to /settings (Phase 105);
//   (h) the page carries the depth-bar shell surfaces (footer, search).

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";
import { STORAGE_KEYS } from "../src/lib/connection";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

/**
 * Seed the full `connection.ts` storage convention so the page resolves a
 * live Runtime connection (the harness `seedAuth` only writes the legacy
 * console-token key; the page resolves through `connection.ts`). Scopes are
 * left UNSET — a non-admin UI gate — so the admin-gated raw-HTML toggle is
 * deterministically disabled (the runtime token is the authoritative gate).
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

const NO_ROWS_SKIP =
  "no MCP servers configured on the dev runtime (runtime-fixture seeding tracked in issue #178)";

test.describe("MCP Connections page", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent or bin/harbor not built",
  );

  test("the servers list renders (rows or the documented empty/error state)", async ({
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

  test("selecting a server row populates the right-rail detail", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("mcp-connections");

    const firstRow = page.locator("[data-testid^='server-row-']").first();
    if ((await firstRow.count()) === 0) {
      test.skip(true, NO_ROWS_SKIP);
      return;
    }
    await firstRow.click();
    await expect(
      page.locator("[data-testid='rail-server-name']"),
      "the right-rail detail shows the selected server",
    ).toBeVisible();
  });

  test("the detail rail carries the five tabs and each paints in place", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("mcp-connections");

    const firstRow = page.locator("[data-testid^='server-row-']").first();
    if ((await firstRow.count()) === 0) {
      test.skip(true, NO_ROWS_SKIP);
      return;
    }
    await firstRow.click();
    await expect(page.locator("[data-testid='mcp-detail-rail']")).toBeVisible();

    for (const tab of ["tools", "resources", "prompts", "oauth", "policy"]) {
      await expect(
        page.locator(`[data-testid='tab-${tab}']`),
        `the ${tab} tab is present`,
      ).toBeVisible();
    }
    for (const tab of ["resources", "prompts", "oauth", "policy", "tools"]) {
      await page.locator(`[data-testid='tab-${tab}']`).click();
      await expect(
        page.locator(`[data-testid='tab-body-${tab}']`),
        `the ${tab} tab body paints`,
      ).toBeVisible();
    }
  });

  test("the Tools tab deep-links to /tools scoped to the server", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("mcp-connections");

    const firstRow = page.locator("[data-testid^='server-row-']").first();
    if ((await firstRow.count()) === 0) {
      test.skip(true, NO_ROWS_SKIP);
      return;
    }
    await firstRow.click();
    await page.locator("[data-testid='tab-tools']").click();

    const deepLink = page.locator("[data-testid='tools-deep-link']");
    await expect(deepLink, "the Tools-tab deep-link renders").toBeVisible();
    const href = await deepLink.getAttribute("href");
    // CONVENTIONS.md §1: inter-page links use the unprefixed form.
    expect(href, "the deep-link targets /tools?server=…").toMatch(
      /^\/tools\?server=/,
    );
  });

  test("the raw-HTML toggle is disabled for a non-admin session", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("mcp-connections");

    const firstRow = page.locator("[data-testid^='server-row-']").first();
    if ((await firstRow.count()) === 0) {
      test.skip(true, NO_ROWS_SKIP);
      return;
    }
    await firstRow.click();

    const toggle = page.locator("[data-testid='raw-html-toggle']");
    await expect(toggle, "the raw-HTML toggle renders").toBeVisible();
    await expect(
      toggle,
      "the raw-HTML toggle is disabled for a non-admin session",
    ).toBeDisabled();
  });

  test("a disconnected Console redirects to /settings to connect (Phase 105)", async ({
    page,
    runtime,
  }) => {
    // Deliberately do NOT seed the connection. Phase 105 (V1.2): the app
    // shell redirects a disconnected Console to /settings (the connect
    // surface) rather than stranding the operator on a dead page.
    await page.goto(new URL("/mcp-connections", runtime.baseURL).toString());
    await page.waitForLoadState("load");
    await expect
      .poll(() => new URL(page.url()).pathname, { timeout: 5000 })
      .toMatch(/^\/settings(\/.*)?$/);
  });

  test("the page carries the depth-bar shell surfaces", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("mcp-connections");

    // D-132 / W3: the ConnectionFooter is owned by the app shell — the page
    // no longer renders its own. Assert the single shell-provided footer.
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
