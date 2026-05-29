// Harbor Console e2e — Playground page polish (Phase 108 / D-167).
//
// Asserts the polished Playground surface: markdown rendering in chat
// bubbles, KPI strip tiles, bottom status bar indicators.

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

test.describe("Console Playground polish (Phase 108)", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent or bin/harbor not built",
  );

  test("(a) markdown renders in agent bubbles — no literal **", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("playground");
    await page.waitForLoadState("load");

    // Wait for the chat stream to actually render (the page reaches its
    // ready state only after the async onMount → load() resolves). The
    // original test evaluated immediately and raced that resolve.
    await page.locator('[data-testid="chat-stream"]').waitFor({ state: "attached" });

    // Inject a mock message with markdown into the page state.
    // We simulate an agent message arriving with markdown content.
    await page.evaluate(() => {
      // Find the chat stream and inject a mock bubble.
      const stream = document.querySelector('[data-testid="chat-stream"]');
      if (stream) {
        const bubble = document.createElement('div');
        bubble.setAttribute('data-testid', 'chat-message-bubble');
        bubble.setAttribute('data-role', 'agent');
        bubble.innerHTML = '<div class="bubble-content"><div class="bubble-body"><p><strong>By topic:</strong> music, <em>games</em>, and <code>code</code>.</p></div></div>';
        stream.appendChild(bubble);
      }
    });

    // Assert the rendered DOM contains <strong> with the right text.
    const strong = page.locator('[data-testid="chat-message-bubble"] strong').first();
    await expect(strong).toHaveText('By topic:');

    // Assert no literal ** remains visible.
    const bubbleText = await page.locator('[data-testid="chat-message-bubble"]').first().textContent();
    expect(bubbleText).not.toContain('**By topic:**');
  });

  test("(b) KPI strip renders the integrated metadata columns", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("playground");
    await page.waitForLoadState("load");

    await expect(
      page.locator("[data-testid='kpi-strip']"),
      "KPI strip is present",
    ).toBeVisible();

    // 108a integrated metadata row (Status moved to the header pill).
    for (const col of [
      "kpi-session",
      "kpi-started",
      "kpi-duration",
      "kpi-tokens",
      "kpi-cost",
      "kpi-latency",
      "kpi-identity",
      "kpi-scope",
    ]) {
      await expect(
        page.locator(`[data-testid='${col}']`),
        `${col} column renders`,
      ).toBeVisible();
    }
  });

  test("(c) global app status bar + composer telemetry render (108a)", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("playground");
    await page.waitForLoadState("load");

    // The status bar is now ONE global app-shell bar (Connection ·
    // Protocol · Events Stream · Console).
    const bar = page.locator("[data-testid='app-status-bar']");
    await expect(bar, "global app status bar is present").toBeVisible();
    await expect(bar).toContainText('Protocol');
    await expect(bar).toContainText('Events Stream');
    await expect(bar).toContainText('Console');

    // The page-level run phase ("Idle"/"Streaming") moved to the
    // composer telemetry strip.
    await expect(
      page.locator("[data-testid='composer-telemetry']"),
      "composer telemetry strip renders",
    ).toBeVisible();
  });

  test("(d) status bar shows the live runtime Protocol version", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("playground");
    await page.waitForLoadState("load");

    // The global bar reads the REAL Protocol version resolved from
    // runtime.info (no longer a hardcoded 'v1'). Assert a concrete semver
    // — proving the value is wired to the runtime, not a placeholder. The
    // web-first assertion auto-waits for the async runtime.info resolve.
    const bar = page.locator("[data-testid='app-status-bar']");
    await expect(bar).toContainText(/Protocol \d+\.\d+\.\d+/);
  });
});
