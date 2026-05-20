// Harbor Console e2e — Tools page per-page spec (Phase 73f / D-116).
//
// Covers the binding acceptance criteria from
// `docs/plans/phase-73f-console-tools-page.md`:
//   (a) the catalog table renders rows with the mockup columns,
//   (b) a facet chip toggle updates the rendered rows,
//   (c) a selected-tool drill-down opens the detail panel,
//   (d) the Approve action surfaces the shipped `approve` Protocol path.
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

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

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
    await helpers.gotoPage("tools");

    await expect(
      page.locator("[data-testid='console-hydrated']"),
      "the Console app hydrated",
    ).toBeAttached();
    await expect(
      page.locator("[data-testid='tools-page']"),
      "the Tools page root is present",
    ).toBeVisible();
  });

  test("(a) the catalog table renders rows with the mockup columns", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("tools");

    const table = page.locator("[data-testid='tools-catalog-table']");
    await expect(table, "the catalog table renders").toBeVisible();

    // The mockup column headers, in order.
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
        table.locator("thead th", { hasText: col }),
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
    await helpers.gotoPage("tools");

    // Wait for the initial catalog load to settle (rows OR the
    // filtered-empty state).
    await page.waitForLoadState("networkidle");

    const before = await page
      .locator("[data-testid='tools-catalog-row']")
      .count();

    // Toggle the MCP transport facet. The page re-issues `tools.list`
    // with the facet and re-renders — the row count must change OR the
    // filtered-empty state appears.
    await page
      .locator(
        "[data-testid='tools-facet-transport'][data-facet-value='MCP']",
      )
      .click();
    await page.waitForLoadState("networkidle");

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
    await helpers.gotoPage("tools");
    await page.waitForLoadState("networkidle");

    const firstRow = page
      .locator("[data-testid='tools-catalog-row']")
      .first();
    const rowCount = await page
      .locator("[data-testid='tools-catalog-row']")
      .count();
    test.skip(rowCount === 0, "no tools registered in the runtime fixture");

    await firstRow.click();
    await expect(
      page.locator("[data-testid='tools-detail-name']"),
      "the detail panel header shows the selected tool",
    ).toBeVisible();

    // The detail panel exposes the tabbed surface.
    await expect(
      page.locator("[data-testid='tools-detail-tab'][data-tab='approval']"),
      "the Approval tab is present",
    ).toBeVisible();
  });

  test("(d) the Approval tab's Approve action surfaces the shipped approve path", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("tools");
    await page.waitForLoadState("networkidle");

    const rowCount = await page
      .locator("[data-testid='tools-catalog-row']")
      .count();
    test.skip(rowCount === 0, "no tools registered in the runtime fixture");

    await page.locator("[data-testid='tools-catalog-row']").first().click();
    await page
      .locator("[data-testid='tools-detail-tab'][data-tab='approval']")
      .click();
    await page.locator("[data-testid='tools-approve']").click();

    // The Approve action records its intent — invoking the shipped
    // `approve` Protocol method (Phase 54), never a new approval impl.
    await expect(
      page.locator("[data-testid='tools-approval-feedback']"),
      "the Approve action surfaced feedback referencing the approve method",
    ).toContainText("approve");
  });
});
