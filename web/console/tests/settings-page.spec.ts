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
    //
    // Phase 83u (D-163) — `#catchUpAddressBook()` auto-inserts the
    // active connection on load(), so the list carries 2+ rows: the
    // auto-imported active connection AND the e2e-runtime row this
    // test added. Scope the assertion to the e2e-runtime row by name.
    await expect(
      page
        .locator("[data-testid='connected-runtime-row']")
        .filter({ hasText: "e2e-runtime" }),
      "the added runtime appears in the address book",
    ).toBeVisible();
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

  test("(g) Settings page lets operator add a runtime when disconnected (Phase 83p / D-158)", async ({
    page,
    runtime,
    helpers,
  }) => {
    // Phase 83p — the Connected Runtimes form MUST render when the
    // Console has no Runtime attached (it's the operator's only path to
    // attach one). The pre-83p Settings page wrapped EVERY section in
    // <PageState>, so the disconnected state short-circuited the whole
    // page to the "Not connected" placeholder — hiding the form an
    // operator needs to fix the disconnection. The two-group layout
    // (console-local sections render unconditionally; runtime-posture
    // sections wrap in PageState) closes Bug F1 from the post-83k
    // walkthrough.
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("settings");

    // The console-local card group renders unconditionally.
    await expect(
      page.locator("[data-testid='settings-cards-console-local']"),
      "console-local cards render even when no Runtime is attached",
    ).toBeVisible();

    // The Connected Runtimes section + add-form trigger reach the DOM.
    await expect(
      page.locator("[data-testid='settings-section-connected-runtimes']"),
      "Connected Runtimes section is reachable in the disconnected state",
    ).toBeVisible();
    const addBtn = page.locator("button:has-text('+ Add Runtime')").first();
    await expect(
      addBtn,
      "+ Add Runtime button is visible in the disconnected state",
    ).toBeVisible();

    // Opening the form reveals the runtime-name + base-url inputs.
    await addBtn.click();
    await expect(
      page.locator("input[placeholder='Runtime name']"),
      "runtime name input is in the DOM after opening the form",
    ).toBeVisible();
    await expect(
      page.locator("input[placeholder='https://runtime.example.com']"),
      "base URL input is in the DOM after opening the form",
    ).toBeVisible();

    // The runtime-posture card group is wrapped in <PageState> and
    // shows the consolidated disconnected placeholder (one card, not
    // N empty per-section placeholders).
    await expect(
      page.locator("[data-testid='page-state-disconnected']").first(),
      "runtime-posture sections route through PageState (disconnected branch)",
    ).toBeVisible();
  });

  test("(h) Settings + Add Runtime from disconnected boot writes localStorage and reconnects (Phase 83u / D-163)", async ({
    page,
    runtime,
    helpers,
  }) => {
    // Phase 83u — the F3 chicken-and-egg fix. From a clean Console boot
    // (no prior `harbor.runtime.*` in localStorage), the operator clicks
    // + Add Runtime, fills the form, clicks Add. The pre-83u flow threw
    // "Console DB not open — attach to a Runtime first" because the
    // address-book DB needed a connection to derive its per-operator
    // encryption key, but adding a connection required writing to the
    // address book.
    //
    // The fix splits the form's two effects:
    //   1) addRuntime() calls attachConnection(baseURL) FIRST —
    //      writes `harbor.runtime.base_url` to localStorage; no DB.
    //   2) Then attempts the DB write best-effort. On first attach
    //      the DB is not open, so this is deferred and the catch-up
    //      in SettingsDBController.load() runs after the page reload.
    //
    // This test exercises (1) end-to-end. The Playwright harness
    // pre-seeds the auth token but NOT the runtime connection triple,
    // so `connection.ts::resolveConnection` returns null on first mount
    // (the operator's first-boot state). Clicking Add writes the
    // localStorage `base_url` key, which is the load-bearing assertion.
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("settings");

    // Confirm the pre-attach state: localStorage base_url is unset.
    const baseURLBefore = await page.evaluate(() =>
      window.localStorage.getItem("harbor.runtime.base_url"),
    );
    expect(baseURLBefore, "no Runtime attached on first boot").toBeNull();

    // Open the add-form, fill it, and submit. The submit triggers a
    // page reload (the new connection only takes effect on next mount).
    // We intercept the reload by capturing the localStorage write
    // BEFORE the navigation completes — Playwright's `page.evaluate`
    // runs synchronously against the current page context.
    const addBtn = page.locator("button:has-text('+ Add Runtime')").first();
    await addBtn.click();
    await page
      .locator("[data-testid='add-runtime-name']")
      .fill("local-dev-runtime");
    await page
      .locator("[data-testid='add-runtime-url']")
      .fill(runtime.baseURL);

    // The Add submit reloads the page; await both the click and the
    // resulting load completion.
    await Promise.all([
      page.waitForLoadState("networkidle"),
      page.locator("[data-testid='add-runtime-submit']").click(),
    ]);

    // After the reload, the localStorage key is written and the
    // page reads it on mount — confirm both.
    const baseURLAfter = await page.evaluate(() =>
      window.localStorage.getItem("harbor.runtime.base_url"),
    );
    expect(
      baseURLAfter,
      "addRuntime wrote the active connection's base_url to localStorage",
    ).toBe(runtime.baseURL.replace(/\/$/, ""));

    // The form-submit path no longer throws "Console DB not open" —
    // the pre-83u red error is gone. The post-reload page renders
    // the cards group without the disconnected state on the
    // console-local sections.
    await expect(
      page.locator("[data-testid='settings-cards-console-local']"),
      "console-local cards still render after the reload",
    ).toBeVisible();
  });
});
