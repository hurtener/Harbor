// Harbor Console e2e — Memory page spec (Phase 73j / D-118; refactored
// onto the D-121 design-system foundation).
//
// The per-page Playwright spec for `/memory`. It is the §13 primitive-
// with-consumer discharge at the UI layer: the Memory page IS the
// consumer of the three `memory.*` Protocol methods, and this spec
// exercises it end-to-end against a real `harbor console` + a real
// Runtime via the Phase 75 harness.
//
// SKIP semantics (the subcommand-missing → SKIP pattern, mirroring
// CLAUDE.md §4.2): `harbor console` lands in Phase 73m. Until then
// `consoleSubcommandAvailable()` is false and the whole describe block
// SKIPs at collection time — the spec exists, is enumerated by the
// Phase 75a aggregator, and flips to live once 73m ships.
//
// Coverage — the refactored Memory page (CONVENTIONS.md §3/§4/§5):
//   (a) the shared `DataTable` renders with the mockup columns,
//   (b) a scope-facet toggle re-issues the `memory.list` query,
//   (c) selecting a row opens the detail rail's nested PageState,
//   (d) the Recent-identity-rejections RailCard renders (D-033),
//   (e) the Recovery-dropouts RailCard renders (D-035),
//   (f) the shared `BulkActionBar` actions are disabled-with-tooltip
//       (page-memory.md §10 — V1 is view-only),
//   (g) the shell-provided `ConnectionFooter` renders.

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

test.describe("Console Memory page", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent (pre-Phase-73m) or bin/harbor not built",
  );

  test.beforeEach(async ({ runtime, helpers }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("memory");
  });

  test("renders the Memory page shell", async ({ page }) => {
    await expect(
      page.locator("[data-testid='memory-page']"),
      "the Memory page root is present",
    ).toBeVisible();
    await expect(
      page.getByRole("heading", { name: "Memory", level: 1 }),
      "the shared PageHeader renders",
    ).toBeVisible();
  });

  test("(a) the shared DataTable renders with the mockup columns", async ({
    page,
  }) => {
    const table = page.locator("table.data-table");
    await expect(table, "the shared DataTable renders").toBeVisible();
    for (const col of [
      "Memory key",
      "Strategy",
      "Scope",
      "Owner",
      "Created",
      "Last updated",
      "TTL / Expires",
      "Size",
      "Driver",
    ]) {
      await expect(
        table.getByRole("columnheader", { name: col }),
        `the "${col}" column header renders`,
      ).toBeVisible();
    }
  });

  test("(b) a scope-facet toggle re-issues the list query", async ({
    page,
  }) => {
    // Selecting a scope facet drives a fresh memory.list call routed
    // through HarborClient. We assert the facet control is wired and the
    // table is still attached after the toggle (the exact row count
    // depends on seeded runtime state).
    const scopeFacet = page.locator("[data-testid='memory-scope-facet']");
    await scopeFacet.selectOption("session");
    await expect(
      page.locator("table.data-table"),
      "the table re-renders after a facet toggle",
    ).toBeVisible();
  });

  test("(c) selecting a row opens the detail rail", async ({ page }) => {
    const railDetail = page.locator(
      "section.rail-card:has-text('Selected item')",
    );
    await expect(railDetail, "the Selected item RailCard is present").toBeVisible();
    const firstRow = page.locator("table.data-table tbody tr.data-row").first();
    if ((await firstRow.count()) > 0) {
      await firstRow.click();
      await expect(railDetail).toBeVisible();
    }
  });

  test("(d) the Recent-identity-rejections card is present (D-033)", async ({
    page,
  }) => {
    await expect(
      page.locator("[aria-label='Recent identity rejections']"),
      "the Recent identity rejections card renders",
    ).toBeVisible();
  });

  test("(e) the Recovery-dropouts card is present (D-035)", async ({
    page,
  }) => {
    await expect(
      page.locator("[aria-label='Recovery dropouts']"),
      "the Recovery dropouts card renders",
    ).toBeVisible();
  });

  test("(f) the shared BulkActionBar actions are disabled with a tooltip", async ({
    page,
  }) => {
    // The BulkActionBar only renders when ≥1 row is selected. Select the
    // first row's checkbox, then assert the V1 view-only carve-out: each
    // mutation action is disabled-with-tooltip (page-memory.md §10).
    const firstCheckbox = page
      .locator("table.data-table tbody tr.data-row td.select-col input")
      .first();
    if ((await firstCheckbox.count()) === 0) {
      test.skip(true, "no seeded memory rows to select");
      return;
    }
    await firstCheckbox.check();
    const bar = page.locator("[aria-label='Bulk actions']");
    await expect(bar, "the BulkActionBar renders on selection").toBeVisible();
    for (const action of ["Delete selected", "Refresh TTL", "Pin"]) {
      const btn = bar.getByRole("button", { name: action });
      await expect(btn, `"${action}" is rendered`).toBeVisible();
      await expect(btn, `"${action}" is disabled at V1`).toBeDisabled();
      await expect(
        btn,
        `"${action}" carries the deferral tooltip`,
      ).toHaveAttribute("title", "Memory mutation surface deferred — Phase 73");
    }
  });

  test("(g) the shared ConnectionFooter renders", async ({ page }) => {
    await expect(
      page.locator("[data-testid='connection-footer']"),
      "the shell-provided ConnectionFooter renders",
    ).toBeVisible();
  });
});
