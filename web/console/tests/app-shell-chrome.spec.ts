// Harbor Console e2e — app-shell chrome (Phase 108b).
//
// Verifies the chrome that renders on EVERY page: the compact icon sidebar,
// the two-line brand, the collapse toggle (+ persistence), the top-bar global
// search launcher (real `search.query` round-trip against the fixture
// runtime), the identity avatar, and the SINGLE global status bar (the
// page-local footer strips were removed). Follows the harness fixture pattern
// (boots `bin/harbor console` + Runtime); SKIPs cleanly when the subcommand /
// binary is absent (CLAUDE.md §4.2 convention).

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

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
      // D-171 — session blank on the connection (per-request).
      window.localStorage.setItem("harbor.runtime.session", "");
      window.localStorage.setItem("harbor.runtime.scopes", "admin");
    },
    [baseURL, token] as const,
  );
}

test.describe("Console app-shell chrome (Phase 108b)", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent or bin/harbor not built",
  );

  test("sidebar is compact, every nav item has an icon, brand is two-line", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("overview");
    await expect(page.locator("[data-testid='console-hydrated']")).toBeAttached();

    // 14 nav items, each with a (lucide) svg icon.
    const items = page.locator(".nav-cluster a");
    await expect(items).toHaveCount(14);
    const icons = page.locator(".nav-cluster a .nav-icon svg");
    await expect(icons).toHaveCount(14);

    // Two-line brand lockup.
    await expect(page.locator(".brand-name")).toHaveText("Harbor");
    await expect(page.locator(".brand-sub")).toHaveText("CONSOLE");

    // Compact width — the sidebar uses --size-nav (13.5rem), NOT the
    // detail-rail --size-rail (22rem) it borrowed before 108b.
    const width = await page
      .locator(".sidebar")
      .evaluate((el) => Math.round(el.getBoundingClientRect().width));
    expect(width, "sidebar compact width").toBeGreaterThan(190);
    expect(width, "sidebar compact width").toBeLessThan(270);
  });

  test("hamburger collapses the sidebar to icons-only and persists across reload", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("overview");
    await expect(page.locator("[data-testid='console-hydrated']")).toBeAttached();

    await page.locator("[data-testid='nav-collapse-toggle']").click();
    await expect(page.locator(".console-shell.collapsed")).toBeAttached();
    // Labels hidden, icons remain.
    await expect(page.locator(".nav-label")).toHaveCount(0);
    await expect(page.locator(".nav-cluster a .nav-icon svg")).toHaveCount(14);

    // Persisted: reload still collapsed.
    await page.reload();
    await expect(page.locator("[data-testid='console-hydrated']")).toBeAttached();
    await expect(page.locator(".console-shell.collapsed")).toBeAttached();
  });

  test("⌘K global search round-trips against the runtime", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("overview");
    await expect(page.locator("[data-testid='console-hydrated']")).toBeAttached();

    await page.locator("[data-testid='global-search-trigger']").click();
    await expect(page.locator("[data-testid='global-search-overlay']")).toBeVisible();

    // Typing fires search.query — the palette resolves to either real rows
    // or the intentional empty copy (never a hang, never a fabricated row).
    await page.locator("[data-testid='global-search-input']").fill("a");
    const results = page.locator("[data-testid='global-search-result']");
    const empty = page.locator("[data-testid='global-search-empty']");
    await expect(results.first().or(empty)).toBeVisible({ timeout: 5000 });

    // Escape closes it.
    await page.keyboard.press("Escape");
    await expect(page.locator("[data-testid='global-search-overlay']")).toHaveCount(0);
  });

  test("top bar shows the identity avatar; exactly one global status bar", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("overview");
    await expect(page.locator("[data-testid='console-hydrated']")).toBeAttached();

    await expect(page.locator("[data-testid='identity-avatar']")).toBeVisible();

    // The status bar is chrome — rendered ONCE, not duplicated per page.
    await expect(page.locator("[data-testid='connection-footer']")).toHaveCount(1);
  });

  test("disconnected: search disabled, avatar muted", async ({ page, helpers }) => {
    // No connection seeded → the shell redirects to /settings and the chrome
    // degrades honestly (D-160) instead of faking data.
    await helpers.gotoPage("settings");
    await expect(page.locator("[data-testid='console-hydrated']")).toBeAttached();

    await expect(page.locator("[data-testid='global-search-trigger']")).toBeDisabled();
    await expect(page.locator("[data-testid='identity-avatar'] .dot.disconnected")).toBeAttached();
  });
});
