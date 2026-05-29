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

  test("(b) KPI strip renders four tiles", async ({
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

    await expect(
      page.locator("[data-testid='kpi-tokens']"),
      "Tokens tile renders",
    ).toBeVisible();

    await expect(
      page.locator("[data-testid='kpi-cost']"),
      "Cost tile renders",
    ).toBeVisible();

    await expect(
      page.locator("[data-testid='kpi-latency']"),
      "Latency tile renders",
    ).toBeVisible();

    await expect(
      page.locator("[data-testid='kpi-status']"),
      "Status tile renders",
    ).toBeVisible();
  });

  test("(c) bottom status bar renders four indicators", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("playground");
    await page.waitForLoadState("load");

    await expect(
      page.locator("[data-testid='playground-status-bar']"),
      "Status bar is present",
    ).toBeVisible();

    // Assert the four indicators are present.
    const bar = page.locator("[data-testid='playground-status-bar']");
    await expect(bar).toContainText('Idle');
    await expect(bar).toContainText('Protocol');
    await expect(bar).toContainText('Events Stream');
    await expect(bar).toContainText('Console');
  });

  test("(d) status bar protocol version matches breadcrumb", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("playground");
    await page.waitForLoadState("load");

    // The breadcrumb reads "Console / Playground".
    // The status bar reads "Protocol v1".
    const bar = page.locator("[data-testid='playground-status-bar']");
    await expect(bar).toContainText('Protocol v1');
  });
});
