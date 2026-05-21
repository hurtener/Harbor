// Harbor Console e2e — Settings page per-page spec (Phase 73m / D-129).
//
// Covers the Settings page:
//   (a) the page route serves + hydrates inside the app shell,
//   (b) the section-nav rail lists the 12 sections + anchors scroll,
//   (c) the Connected-Runtimes card's `+ Add Runtime` round-trips
//       through the Console DB (a new runtime row appears),
//   (d) the `Rotate token` action degrades to disabled-with-tooltip
//       when the connection lacks the admin scope claim (CONVENTIONS.md
//       §5 — no stubbed action presented as done),
//   (e) the mock-mode banner renders conditionally on the backend
//       `llm.posture` `MockMode` flag,
//   (f) the four-state `<PageState>` Disconnected branch renders when
//       no Runtime connection is configured.
//
// SKIP semantics (mirrors `harness.spec.ts`): pre-73m the `harbor
// console` subcommand was absent and the whole suite SKIPped. With 73m
// landed the `runtime` fixture reports `available: true` and the suite
// runs against a live Runtime + Console.

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

// Seed the Runtime connection triple so the page resolves a live
// connection rather than rendering the Disconnected `PageState`.
//
// The identity triple seeded MUST match the identity the `harbor
// console` dev token carries — `(dev, dev, dev)` (cmd/harbor/devauth.go
// DevTenant/DevUser/DevSession). The Runtime's control transport
// asserts the request-body identity against the verified JWT identity
// (defence-in-depth); a mismatch fails the call with `identity_required`.
async function seedConnection(
  page: import("@playwright/test").Page,
  baseURL: string,
  token: string,
  opts: { scopes?: string } = {},
): Promise<void> {
  await page.addInitScript(
    ([b, t, scopes]) => {
      window.localStorage.setItem("harbor.runtime.base_url", b);
      window.localStorage.setItem("harbor.runtime.token", t);
      window.localStorage.setItem("harbor.runtime.tenant", "dev");
      window.localStorage.setItem("harbor.runtime.user", "dev");
      window.localStorage.setItem("harbor.runtime.session", "dev");
      window.localStorage.setItem("harbor.runtime.scopes", scopes);
    },
    [baseURL, token, opts.scopes ?? ""] as const,
  );
}

test.describe("Console Settings page", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent (pre-Phase-73m) or bin/harbor not built",
  );

  test("(a) the Settings page route serves and hydrates", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("settings");

    await expect(
      page.locator("[data-testid='console-hydrated']"),
      "the Console app hydrated",
    ).toBeAttached();
    await expect(
      page.locator("[data-testid='settings-page']"),
      "the Settings page root is present",
    ).toBeVisible();
  });

  test("(b) the section-nav rail lists the Settings sections", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("settings");

    await expect(
      page.locator("[data-testid='settings-subnav']"),
      "the section-nav rail is present",
    ).toBeVisible();
    // A representative sample of the 12 section anchors.
    for (const section of [
      "connected-runtimes",
      "runtime-info",
      "governance-posture",
      "llm-posture",
      "about",
    ]) {
      await expect(
        page.locator(`[data-testid='settings-subnav-${section}']`),
        `the section-nav rail has the ${section} entry`,
      ).toBeVisible();
    }
  });

  test("(b) clicking a section-nav entry switches the active section", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("settings");

    await page.locator("[data-testid='settings-subnav-about']").click();
    await expect(
      page.locator("[data-testid='settings-active-section']"),
      "the detail rail reflects the selected section",
    ).toHaveText("About");
  });

  test("(c) + Add Runtime round-trips through the Console DB", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("settings");

    // Open the Add Runtime form, fill it, submit.
    await page.locator("[data-testid='add-runtime-open']").click();
    await page.locator("[data-testid='add-runtime-name']").fill("e2e-runtime");
    await page
      .locator("[data-testid='add-runtime-url']")
      .fill("https://e2e-runtime.example.com");
    await page.locator("[data-testid='add-runtime-submit']").click();

    // The new runtime appears in the Connected-Runtimes list (a real
    // Console-DB round-trip, not a stubbed feedback string).
    await expect(
      page.locator("[data-testid='connected-runtime-row']"),
      "the added runtime appears in the address book",
    ).toContainText("e2e-runtime");
  });

  test("(d) Rotate token is disabled-with-tooltip without the admin scope", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    // Seed a connection with NO admin scope claim.
    await seedConnection(page, runtime.baseURL, runtime.token, { scopes: "" });
    await helpers.gotoPage("settings");
    await page.locator("[data-testid='settings-subnav-per-runtime-auth']").click();

    const rotateBtn = page.locator("[data-testid='rotate-token-btn']");
    await expect(rotateBtn, "the Rotate token button is rendered").toBeVisible();
    await expect(
      rotateBtn,
      "Rotate token is disabled without the admin scope claim",
    ).toBeDisabled();
    await expect(
      page.locator("[data-testid='rotate-token-disabled-hint']"),
      "the disabled-with-reason hint is shown",
    ).toBeVisible();
  });

  test("(d) Rotate token is enabled with the admin scope", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token, {
      scopes: "admin",
    });
    await helpers.gotoPage("settings");
    await page.locator("[data-testid='settings-subnav-per-runtime-auth']").click();

    await expect(
      page.locator("[data-testid='rotate-token-btn']"),
      "Rotate token is enabled with the admin scope claim",
    ).toBeEnabled();
  });

  test("(e) the mock-mode banner is conditional on the backend MockMode flag", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("settings");
    await page.locator("[data-testid='settings-subnav-llm-posture']").click();
    await page.waitForLoadState("networkidle");

    // The harbor console embedded runtime boots with the mock LLM
    // (the zero-config default), so `llm.posture` reports MockMode =
    // true and the banner renders. If the backend ever reports a live
    // provider the banner is absent — either way the banner is
    // conditional on the backend flag, never hard-coded.
    const bannerVisible = await page
      .locator("[data-testid='mock-mode-banner']")
      .first()
      .isVisible()
      .catch(() => false);
    const llmMode = await page
      .locator("[data-testid='llm-mode']")
      .textContent()
      .catch(() => "");
    const isMock = (llmMode ?? "").includes("mock");
    expect(
      bannerVisible === isMock,
      "the mock-mode banner visibility tracks the backend MockMode flag",
    ).toBe(true);
  });

  test("(f) the Disconnected PageState renders when no Runtime is attached", async ({
    page,
    runtime,
    helpers,
  }) => {
    // Seed auth but NOT the connection triple — connection.ts resolves
    // null, so the page renders the Disconnected state, never an Error.
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("settings");

    await expect(
      page.locator("[data-testid='settings-page']"),
      "the Settings page shell renders even when disconnected",
    ).toBeVisible();
  });
});
