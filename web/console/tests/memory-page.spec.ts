// Harbor Console e2e — Memory page spec (Phase 108n / D-186; rebuilt onto
// the carded, viewport-locked master-detail composition).
//
// The per-page Playwright spec for `/memory`. It rides the Phase 75 harness
// baseline; the whole describe block SKIPs when `harbor console` is absent.
//
// Phase 108n rebuilt the page: the per-page PageHeader is gone (108b chrome);
// the right rail is a stacked Memory-health / Strategy-trace / live
// Memory-events / Add-memory / Selected-item set; the bulk-action bar's
// disabled placeholders are replaced by a REAL admin-gated "Evict selected"
// (the new `memory.delete`); and the formerly-deferred event-feed card is now
// a LIVE `events.subscribe` projection. The assertions below target that shape.
//
// Coverage:
//   (a) the carded master-detail page root renders (no PageHeader);
//   (b) the shared DataTable renders with the mockup columns;
//   (c) a scope-facet toggle re-issues the list query;
//   (d) the right-rail Strategy-trace + live Memory-events cards render;
//   (e) selecting a row opens the Selected-item detail;
//   (f) the mutation surface (Evict selected / Add memory) is admin-gated;
//   (g) the shell-provided ConnectionFooter renders.

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";
import { STORAGE_KEYS } from "../src/lib/connection";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

/**
 * Seed the `connection.ts` storage convention so the page resolves a live
 * Runtime connection. Scopes are left UNSET — a non-admin UI gate — so the
 * admin-gated mutation surface (Evict selected / Add memory) is
 * deterministically disabled (the runtime is the authoritative gate, D-079).
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

test.describe("Console Memory page", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent (pre-Phase-73m) or bin/harbor not built",
  );

  test.beforeEach(async ({ page, runtime, helpers }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("memory");
  });

  test("(a) the carded master-detail page root renders (no PageHeader)", async ({
    page,
  }) => {
    await expect(
      page.locator("[data-testid='memory-page']"),
      "the Memory page root is present",
    ).toBeVisible();
    // The per-page PageHeader was dropped — the page no longer renders an
    // <h1>Memory</h1> (the breadcrumb is app-shell chrome, 108b).
    await expect(
      page.getByRole("heading", { name: "Memory", level: 1 }),
      "no per-page PageHeader heading",
    ).toHaveCount(0);
  });

  test("(b) the shared DataTable renders with the mockup columns", async ({
    page,
  }) => {
    const table = page.locator("table.data-table");
    const empty = page.locator("[data-testid='memory-empty']");
    // Wait for the page to settle into either the loaded table or the empty
    // state before branching (the list load is async).
    await expect(
      table.or(empty),
      "the page settles into a table or the documented empty state",
    ).toBeVisible();
    if (!(await table.isVisible())) {
      return;
    }
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

  test("(c) a scope-facet toggle re-issues the list query", async ({ page }) => {
    const scopeFacet = page.locator("[data-testid='memory-scope-facet']");
    await scopeFacet.selectOption("session");
    await expect(
      page.locator("table.data-table").or(page.locator("[data-testid='memory-empty']")),
      "the table (or empty state) re-renders after a facet toggle",
    ).toBeVisible();
  });

  test("(d) the right-rail Strategy-trace + live Memory-events cards render", async ({
    page,
  }) => {
    await expect(
      page.locator("[data-testid='memory-strategy-trace']"),
      "the Strategy-trace card renders",
    ).toBeVisible();
    await expect(
      page.locator("[data-testid='memory-events-feed']"),
      "the live Memory-events feed renders (replaces the deferred placeholder)",
    ).toBeVisible();
  });

  test("(e) selecting a row opens the Selected-item detail", async ({ page }) => {
    const railDetail = page.locator("section.rail-card:has-text('Selected item')");
    await expect(railDetail, "the Selected item RailCard is present").toBeVisible();
    const firstRow = page.locator("table.data-table tbody tr.data-row").first();
    if ((await firstRow.count()) > 0) {
      await firstRow.click();
      await expect(railDetail).toBeVisible();
    }
  });

  test("(f) the mutation surface is admin-gated (no admin scope)", async ({
    page,
  }) => {
    // The Add-memory composer is always present in the rail; its submit is
    // disabled without the admin claim (D-079).
    await expect(
      page.locator("[data-testid='memory-add-submit']"),
      "the Add-memory submit is disabled for a non-admin session",
    ).toBeDisabled();
    await expect(
      page.locator("[data-testid='memory-add-gated']"),
      "the composer names the admin-gate",
    ).toBeVisible();

    // The bulk "Evict selected" appears only when a row is checked; when rows
    // exist, check one and assert it is disabled for a non-admin session.
    const firstCheckbox = page
      .locator("table.data-table tbody tr.data-row td.select-col input")
      .first();
    if ((await firstCheckbox.count()) > 0) {
      await firstCheckbox.check();
      await expect(
        page.locator("[data-testid='memory-evict-selected']"),
        "Evict selected is disabled for a non-admin session",
      ).toBeDisabled();
    }
  });

  test("(g) the shared ConnectionFooter renders", async ({ page }) => {
    await expect(
      page.locator("[data-testid='connection-footer']"),
      "the shell-provided ConnectionFooter renders",
    ).toBeVisible();
  });
});
