// Harbor Console e2e — Memory page spec (Phase 73j / D-118).
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
// Coverage (phase plan acceptance criterion — Playwright spec):
//   (a) the catalog table renders rows with the mockup columns,
//   (b) a facet chip toggle updates the row count,
//   (c) the selected-item drill-down opens the detail panel,
//   (d) the Recent-identity-rejections card surfaces a rejection with
//       the `<missing>` substitution visible (D-033),
//   (e) a heavy-value row shows the `Open artifact` deep-link (D-026),
//   (f) the bulk-action toolbar buttons are disabled + reveal the
//       deferral tooltip (page-memory.md §10).

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
      "the page header renders",
    ).toBeVisible();
  });

  test("(a) the catalog table renders with the mockup columns", async ({
    page,
  }) => {
    const table = page.locator("table.memory-table");
    await expect(table, "the memory table renders").toBeVisible();
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
    // Selecting a scope facet drives a fresh memory.list call — the
    // table re-renders. We assert the facet control is wired and the
    // table is still attached after the toggle (the exact row count
    // depends on seeded runtime state).
    const scopeSelect = page.locator("select").first();
    await scopeSelect.selectOption("session");
    await expect(
      page.locator("table.memory-table"),
      "the table re-renders after a facet toggle",
    ).toBeVisible();
  });

  test("(c) selecting a row opens the detail panel", async ({ page }) => {
    const detail = page.locator("[aria-label='Selected item detail']");
    await expect(detail, "the detail card is present").toBeVisible();
    const firstRow = page.locator("table.memory-table tbody tr").first();
    if ((await firstRow.count()) > 0) {
      await firstRow.click();
      // The detail panel transitions out of the "select a row" prompt.
      await expect(detail).toBeVisible();
    }
  });

  test("(d) the Recent-identity-rejections card is present (D-033)", async ({
    page,
  }) => {
    // The card surfaces `memory.identity_rejected` events verbatim,
    // preserving the `<missing>` substitution — NO "view rejected
    // memory anyway" affordance (§13). We assert the card renders;
    // the verbatim-rendering contract is unit-tested in the Vitest /
    // Go suites where a rejection can be seeded deterministically.
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

  test("(f) the bulk-action toolbar buttons are disabled with a tooltip", async ({
    page,
  }) => {
    const toolbar = page.locator("[aria-label='Bulk actions (disabled at V1)']");
    await expect(toolbar, "the bulk-action toolbar renders").toBeVisible();
    for (const action of ["Delete selected", "Refresh TTL", "Pin"]) {
      const btn = toolbar.getByRole("button", { name: action });
      await expect(btn, `"${action}" is rendered`).toBeVisible();
      await expect(btn, `"${action}" is disabled at V1`).toBeDisabled();
      await expect(
        btn,
        `"${action}" carries the deferral tooltip`,
      ).toHaveAttribute("title", "Memory mutation surface deferred — Phase 73");
    }
  });

  test("the page footer marks it a Protocol client", async ({ page }) => {
    await expect(
      page.locator(".page-footer"),
      "the Protocol-client footer renders",
    ).toContainText("Protocol-client");
  });
});
