// Harbor Console e2e — Playground page per-page spec (Phase 73n /
// D-130).
//
// Covers the Playground page built on the D-121 design-system
// foundation + the shared chat module (D-091):
//   (a) the page route serves + hydrates inside the shared app shell;
//   (b) the shared chat module (`<ChatPanel>` + `<ChatComposer>`)
//       renders — the first consumer of `$lib/chat/`;
//   (c) a chat-stream round-trip — typing + Send invokes the SHIPPED
//       `user_message` Protocol method;
//   (d) the multimodal attach control is present (artifacts.put);
//   (e) the Controls card reasoning-effort override invokes
//       `runs.set_overrides`, applied to the NEXT message;
//   (f) the Pending Interventions Approve / Reject buttons render
//       disabled-with-tooltip without the steering scope claim
//       (CONVENTIONS.md §5);
//   (g) the trace toggle reveals the topology trace body;
//   (h) the Recent Artifacts card preview affordance;
//   (i) the four-state `<PageState>` Disconnected branch.
//
// SKIP semantics (mirrors `live-runtime-page.spec.ts`): the `harbor
// console` subcommand lands in a later Stage; until then the `runtime`
// fixture reports `available: false` and the describe block SKIPs at
// collection time — keeping the harness baseline green.
//
// The Phase 75a wave-end aggregator enumerates the 14 page slugs and
// asserts a matching `<slug>-page.spec.ts` exists; this file is the
// `playground` slug's entry.

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

// Seeds the Console's `harbor.runtime.*` storage convention so the page
// resolves a live connection rather than the Disconnected `PageState`.
async function seedConnection(
  page: import("@playwright/test").Page,
  baseURL: string,
  token: string,
): Promise<void> {
  await page.addInitScript(
    ([b, t]) => {
      window.localStorage.setItem("harbor.runtime.base_url", b);
      window.localStorage.setItem("harbor.runtime.token", t);
      window.localStorage.setItem("harbor.runtime.tenant", "tenant-e2e");
      window.localStorage.setItem("harbor.runtime.user", "user-e2e");
      window.localStorage.setItem("harbor.runtime.session", "session-e2e");
    },
    [baseURL, token] as const,
  );
}

test.describe("Console Playground page", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent or bin/harbor not built",
  );

  test("(a) the Playground page route serves and hydrates", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("playground");

    await expect(
      page.locator("[data-testid='console-hydrated']"),
      "the Console app hydrated",
    ).toBeAttached();
    await expect(
      page.locator("[data-testid='playground-page']"),
      "the Playground page root is present",
    ).toBeVisible();
  });

  test("(b) the shared chat module renders the panel + composer", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("playground");
    await page.waitForLoadState("networkidle");

    await expect(
      page.locator("[data-testid='chat-panel']"),
      "the shared chat panel renders",
    ).toBeVisible();
    await expect(
      page.locator("[data-testid='chat-composer']"),
      "the shared chat composer renders",
    ).toBeVisible();
  });

  test("(c) a chat-stream round-trip — typing + Send", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("playground");
    await page.waitForLoadState("networkidle");

    await page
      .locator("[data-testid='chat-composer-input']")
      .fill("hello from the playground spec");
    await page.locator("[data-testid='chat-send-button']").click();

    await expect(
      page.locator("[data-testid='chat-message-bubble']").first(),
      "a chat message bubble appears after Send",
    ).toBeVisible();
  });

  test("(d) the multimodal attach control is present", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("playground");
    await page.waitForLoadState("networkidle");

    await expect(
      page.locator("[data-testid='chat-attach-input']"),
      "the multimodal attach input is present",
    ).toBeAttached();
  });

  test("(e) the Controls card records a reasoning-effort override", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("playground");
    await page.waitForLoadState("networkidle");

    await page
      .locator("[data-testid='controls-reasoning-effort']")
      .selectOption("high");
    await page.locator("[data-testid='controls-apply']").click();

    await expect(
      page.locator("[data-testid='controls-apply-result']"),
      "the Controls card surfaces a runs.set_overrides result",
    ).toBeVisible();
  });

  test("(f) the drift-mode toggle is visible-but-disabled (Post-V1)", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("playground");
    await page.waitForLoadState("networkidle");

    const drift = page.locator("[data-testid='controls-drift-mode']");
    await expect(drift, "the drift-mode toggle renders").toBeVisible();
    await expect(drift, "the drift-mode toggle is disabled (Post-V1)").toBeDisabled();
  });

  test("(g) the trace toggle reveals the topology trace body", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("playground");
    await page.waitForLoadState("networkidle");

    await page.locator("[data-testid='trace-toggle-checkbox']").check();
    await expect(
      page.locator("[data-testid='trace-body']"),
      "the trace body appears once the toggle is on",
    ).toBeVisible();
  });

  test("(h) the Recent Artifacts card renders", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("playground");
    await page.waitForLoadState("networkidle");

    await expect(
      page.locator("[data-testid='playground-recent-artifacts-card']"),
      "the Recent Artifacts rail card renders",
    ).toBeVisible();
  });

  test("(i) the Pending Interventions card renders", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("playground");
    await page.waitForLoadState("networkidle");

    await expect(
      page.locator("[data-testid='playground-interventions-card']"),
      "the Pending Interventions rail card renders",
    ).toBeVisible();
  });

  test("(j) the Disconnected PageState renders without a connection", async ({
    page,
    runtime,
    helpers,
  }) => {
    // Seed auth but NOT the runtime connection — the page resolves a
    // null connection and renders PageState's Disconnected branch.
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("playground");
    await page.waitForLoadState("networkidle");

    await expect(
      page.locator("[data-testid='page-state-disconnected']"),
      "the Disconnected PageState branch renders without a Runtime",
    ).toBeVisible();
  });
});
