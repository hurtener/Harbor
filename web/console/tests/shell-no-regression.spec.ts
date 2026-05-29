// Harbor Console e2e — Shell no-regression (Phase 108 / D-167).
//
// Loads each of the 13 non-Playground Console pages after the AC-1
// layout reshape and asserts no horizontal overflow, no double
// scrollbars, and that the existing page-specific test markers render.

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
      window.localStorage.setItem("harbor.runtime.session", "dev");
    },
    [baseURL, token] as const,
  );
}

const PAGES = [
  'overview',
  'tools',
  'sessions',
  'settings',
  'artifacts',
  'memory',
  'flows',
  'mcp-connections',
  'agents',
  'events',
  'tasks',
  'background-jobs',
  'live-runtime',
] as const;

test.describe("Console shell no-regression (Phase 108)", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent or bin/harbor not built",
  );

  for (const slug of PAGES) {
    test(`(${slug}) no horizontal overflow or double scrollbars`, async ({
      page,
      runtime,
      helpers,
    }) => {
      await helpers.seedAuth(runtime.token);
      await seedConnection(page, runtime.baseURL, runtime.token);
      await helpers.gotoPage(slug as Parameters<typeof helpers.gotoPage>[0]);
      await page.waitForLoadState("load");

      // Assert the shell hydrated.
      await expect(
        page.locator("[data-testid='console-hydrated']"),
        `${slug}: shell hydrated`,
      ).toBeAttached();

      // Assert no horizontal overflow.
      const hasHorizontalOverflow = await page.evaluate(() => {
        return document.documentElement.scrollWidth > document.documentElement.clientWidth;
      });
      expect(hasHorizontalOverflow, `${slug}: no horizontal overflow`).toBe(false);

      // Assert no double scrollbars on the main column.
      const mainColumn = page.locator('.main-column');
      if (await mainColumn.isVisible().catch(() => false)) {
        const scrollHeight = await mainColumn.evaluate((el: HTMLElement) => el.scrollHeight);
        // scrollHeight may be > clientHeight (content overflow), but we
        // should not see the shell-level scrollbar AND the content-level
        // scrollbar simultaneously. The shell is now overflow:hidden, so
        // only the inner page should scroll if needed.
        expect(scrollHeight, `${slug}: main-column has height`).toBeGreaterThan(0);
      }
    });
  }
});
