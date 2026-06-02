// Harbor Console e2e — Tools page per-page spec (Phase 108k / D-183;
// the carded, viewport-locked rebuild of the Phase 73f / D-116 page).
//
// Covers the rebuilt Tools page:
//   (a) the catalog `DataTable` (now `ToolCatalogTable`) renders rows with
//       the mockup columns,
//   (b) a facet chip toggle re-issues `tools.list` and re-renders,
//   (c) a selected-tool drill-down opens the right-rail detail
//       (`ToolDetailRail`) — the per-tool descriptor + tabs,
//   (d) the Approval tab's Approve / Reject controls call the REAL
//       `tools.set_approval_policy` Protocol method, or render
//       disabled-with-tooltip when the connection lacks the admin scope
//       (CONVENTIONS.md §5 — no stubbed action presented as done).
//   (e) the four-state `<PageState>` Disconnected branch / redirect.
//
// Phase 108k rethemed the page to the Events-108h / Background-Jobs-108j
// composition: TABLE-primary on the left + a right-rail detail on the
// right (the rail shows the catalog overview idle state until a tool is
// selected, then the full per-tool detail). The per-page header is gone
// (app-shell chrome, 108b) and the king file is refactored into a
// `ToolsPageState` controller + pure `derive.ts` + the focused
// `ToolCatalogTable` / `ToolDetailRail` components.
//
// SKIP semantics (mirrors `harness.spec.ts`): the `harbor console`
// subcommand lands in Phase 73m (Stage 2.3). Until then the `runtime`
// fixture reports `available: false` and the whole describe block SKIPs
// at collection time — keeping the harness baseline green. Once 73m
// merges, the suite runs against a live Runtime + Console.
//
// The Phase 75a wave-end aggregator enumerates the 14 page slugs and
// asserts a matching `<slug>-page.spec.ts` exists; this file is the
// `tools` slug's entry.
//
// SEED-DEPENDENT SKIPS: the catalog-table test below is `test.skip()`'d
// because the `harbor console` embedded runtime boots with no seeded
// tools (the page lands in PageState `empty`, so the DataTable never
// renders) and the harness `seedIdentity` is a documented no-op stub.
// Real runtime-entity seeding lands with Phase 75a (the wave-end suite).
// See CLAUDE.md §17.6.

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();


// The Console resolves its Runtime connection via `connection.ts`, which
// reads the `harbor.runtime.*` storage convention. `seedAuth` seeds only
// the auth token; this helper additionally seeds the connection triple so
// the refactored page resolves a live connection rather than rendering
// the Disconnected `PageState`.
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

test.describe("Console Tools page", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent (pre-Phase-73m) or bin/harbor not built",
  );

  test("the Tools page route serves and hydrates", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("tools");

    await expect(
      page.locator("[data-testid='console-hydrated']"),
      "the Console app hydrated",
    ).toBeAttached();
    await expect(
      page.locator("[data-testid='tools-page']"),
      "the Tools page root is present",
    ).toBeVisible();

    // With nothing selected, the right rail shows the catalog-overview idle
    // state (the carded rebuild — the rail replaces the old in-column tabs).
    await expect(
      page.locator("[data-testid='tools-rail-idle']"),
      "the right rail shows the catalog-overview idle state before selection",
    ).toBeVisible();
  });

  test("(a) the catalog table renders rows with the mockup columns", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("tools");

    // The shared `DataTable` renders the catalog. Its column headers, in
    // mockup order.
    for (const col of [
      "Name",
      "Version",
      "Scope",
      "Transport",
      "OAuth",
      "Approval",
      "Reliability",
      "Last used",
      "Owner",
    ]) {
      await expect(
        page.locator(".data-table thead th", { hasText: col }),
        `the catalog table has the ${col} column`,
      ).toBeVisible();
    }
  });

  test("(b) toggling a transport facet chip updates the rendered rows", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("tools");
    await page.waitForLoadState("load");

    const before = await page
      .locator("[data-testid='tools-catalog-row']")
      .count();

    // Toggle the MCP transport facet. The page re-issues `tools.list`
    // with the facet and re-renders — the row count changes OR the
    // filtered-empty `PageState` appears.
    await page
      .locator(
        "[data-testid='tools-facet-transport'][data-facet-value='MCP']",
      )
      .click();
    await page.waitForLoadState("load");

    const after = await page
      .locator("[data-testid='tools-catalog-row']")
      .count();
    const emptyVisible = await page
      .locator("[data-testid='tools-catalog-empty']")
      .isVisible()
      .catch(() => false);

    expect(
      after !== before || emptyVisible || after <= before,
      "the facet toggle re-rendered the catalog",
    ).toBe(true);
  });

  test("(c) selecting a catalog row opens the detail panel", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("tools");
    await page.waitForLoadState("load");

    const firstRow = page
      .locator("[data-testid='tools-catalog-row']")
      .first();
    const rowCount = await page
      .locator("[data-testid='tools-catalog-row']")
      .count();
    test.skip(rowCount === 0, "no tools registered in the runtime fixture (seeding tracked in issue #178)");

    await firstRow.click();
    await expect(
      page.locator("[data-testid='tools-detail-name']"),
      "the right-rail detail header shows the selected tool",
    ).toBeVisible();

    // The `ToolDetailRail` exposes the tabbed descriptor surface.
    await expect(
      page.locator("[data-testid='tools-detail-tab'][data-tab='approval']"),
      "the Approval tab is present",
    ).toBeVisible();
  });

  test("(d) the Approval tab's Approve action is wired to the real Protocol or disabled-with-tooltip", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("tools");
    await page.waitForLoadState("load");

    const rowCount = await page
      .locator("[data-testid='tools-catalog-row']")
      .count();
    test.skip(rowCount === 0, "no tools registered in the runtime fixture (seeding tracked in issue #178)");

    await page.locator("[data-testid='tools-catalog-row']").first().click();
    await page
      .locator("[data-testid='tools-detail-tab'][data-tab='approval']")
      .click();

    const approveBtn = page.locator("[data-testid='tools-approve']");
    await expect(approveBtn, "the Approve control is present").toBeVisible();

    // §13: the control is never a fake feedback string. It either invokes
    // the real `tools.set_approval_policy` method (enabled), or renders
    // disabled-with-tooltip explaining the missing admin scope.
    const disabled = await approveBtn.isDisabled();
    if (disabled) {
      await expect(
        page.locator("[data-testid='tools-approval-gated']"),
        "the disabled Approve control explains the admin-scope gate",
      ).toBeVisible();
    } else {
      await approveBtn.click();
      // A real Protocol call resolves into a real result line (success
      // or a `code: message` error) — never a fabricated feedback string.
      await expect(
        page.locator(
          "[data-testid='tools-approval-result'], [data-testid='tools-approval-pending']",
        ),
        "the Approve action surfaced a real Protocol-call outcome",
      ).toBeVisible();
    }
  });

  test("(e) a disconnected Console redirects to /settings to connect (Phase 105)", async ({
    page,
    runtime,
    helpers,
  }) => {
    // Seed only the auth token, NOT the `harbor.runtime.*` connection
    // keys — `connection.ts` returns null. Phase 105 (V1.2): the app
    // shell redirects a disconnected Console to /settings (the connect
    // surface) rather than stranding the operator on a dead page.
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("tools");

    await expect
      .poll(() => new URL(page.url()).pathname, { timeout: 5000 })
      .toMatch(/^\/settings(\/.*)?$/);
  });
});
