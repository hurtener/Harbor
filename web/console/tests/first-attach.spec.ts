// Harbor Console e2e — Phase 105 (V1.2) first-attach UX.
//
// Pins three operator-visible flows:
//
//   (1) AC-5: a cold load against a Console with no Runtime attached
//       redirects to /settings — the only surface where the operator
//       can fix it.
//
//   (2) AC-9 + AC-10: the AttachToLocalCard is visible when
//       disconnected; clicking "Attach to local Runtime" calls the
//       bootstrap endpoint, seeds the connection envelope into
//       localStorage, reloads, and the page comes back attached
//       (footer flips to "Connected" + the card hides).
//
//   (3) AC-1 + AC-2 + AC-4: the manual six-field form submits a
//       full connection envelope. Filling all six fields with the
//       fixture token attaches the Console for real.
//
// Real `harbor console` co-resident Runtime per CLAUDE.md §17.3 — no
// mocks on the seam. The harness fixture provides the live Runtime
// URL + token.

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

// The localStorage keys the Console's `connection.ts` reads.
const STORAGE_BASE_URL = "harbor.runtime.base_url";
const STORAGE_TOKEN = "harbor.runtime.token";
const STORAGE_TENANT = "harbor.runtime.tenant";
const STORAGE_USER = "harbor.runtime.user";
const STORAGE_SESSION = "harbor.runtime.session";

test.describe("Console first-attach UX (Phase 105)", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent or bin/harbor not built",
  );

  test("(AC-5) a cold load with no Runtime attached redirects to /settings", async ({
    page,
    runtime,
    helpers,
  }) => {
    // Seed auth ONLY — no Runtime triple in localStorage. The layout
    // observes `resolveConnection() === null` and redirects to /settings.
    await helpers.seedAuth(runtime.token);

    // Land directly on /overview (would otherwise render the Overview
    // page's Disconnected branch). The layout redirect kicks in on mount.
    await page.goto(new URL("/overview", runtime.baseURL).toString());
    await page.waitForLoadState("load");

    // The URL ends up under /settings.
    await expect.poll(() => new URL(page.url()).pathname).toMatch(/^\/settings(\/.*)?$/);
  });

  test("(AC-9/AC-10) AttachToLocal one-click flow attaches the Console", async ({
    page,
    runtime,
    helpers,
  }) => {
    // Seed auth ONLY — the Console boots disconnected, the layout
    // redirects to /settings, and the AttachToLocalCard renders.
    await helpers.seedAuth(runtime.token);
    await page.goto(new URL("/settings", runtime.baseURL).toString());
    await page.waitForLoadState("load");

    // The card is visible because resolveConnection() === null.
    const attachBtn = page.locator("[data-testid='attach-to-local-runtime']");
    await expect(
      attachBtn,
      "AttachToLocalCard is visible when no Runtime is attached",
    ).toBeVisible();

    // Confirm pre-attach state: no `harbor.runtime.base_url`.
    const baseURLBefore = await page.evaluate(
      (k) => window.localStorage.getItem(k),
      STORAGE_BASE_URL,
    );
    expect(baseURLBefore, "no Runtime attached before the click").toBeNull();

    // Click triggers a bootstrap fetch, then writes the connection
    // envelope to localStorage, then reloads. `waitForLoadState("load")`
    // resolves on the ALREADY-loaded page (the reload navigation hasn't
    // started yet), so polling localStorage is the race-free wait: it
    // retries across the fetch round-trip + reload until the connection
    // is seeded (Execution-context-destroyed mid-reload is swallowed by
    // the poll and retried).
    await attachBtn.click();
    await expect
      .poll(
        () => page.evaluate((k) => window.localStorage.getItem(k), STORAGE_BASE_URL),
        { timeout: 10_000 },
      )
      .toBeTruthy();

    // After the reload the connection envelope is seeded.
    const seeded = await page.evaluate(
      ([base, token, tenant, user, session]) => ({
        baseURL: window.localStorage.getItem(base),
        token: window.localStorage.getItem(token),
        tenant: window.localStorage.getItem(tenant),
        user: window.localStorage.getItem(user),
        session: window.localStorage.getItem(session),
      }),
      [
        STORAGE_BASE_URL,
        STORAGE_TOKEN,
        STORAGE_TENANT,
        STORAGE_USER,
        STORAGE_SESSION,
      ] as const,
    );
    expect(seeded.baseURL, "base_url seeded from bootstrap response").toBeTruthy();
    expect(seeded.token, "token seeded from bootstrap response").toBeTruthy();
    expect(seeded.tenant, "identity.tenant seeded").toBe("dev");
    expect(seeded.user, "identity.user seeded").toBe("dev");
    expect(seeded.session, "identity.session seeded").toBe("dev");

    // AC-10: the card is HIDDEN once a connection is live.
    await expect(
      page.locator("[data-testid='attach-to-local-runtime']"),
      "AttachToLocalCard hides when a Runtime is attached (AC-10)",
    ).toHaveCount(0);
  });

  test("(AC-1/AC-2/AC-4) manual six-field form attaches a remote Runtime", async ({
    page,
    runtime,
    helpers,
  }) => {
    // Seed auth ONLY; land on /settings.
    await helpers.seedAuth(runtime.token);
    await page.goto(new URL("/settings", runtime.baseURL).toString());
    await page.waitForLoadState("load");

    // Open the Add Runtime form.
    const addBtn = page.locator("button:has-text('+ Add Runtime')").first();
    await addBtn.click();

    // Submit with everything empty — validator must surface an error
    // and the page must NOT navigate (AC-4: no silent no-op).
    await page.locator("[data-testid='add-runtime-submit']").click();
    await expect(
      page.locator("[data-testid='add-runtime-error']"),
      "submitting an empty form surfaces inline error text (AC-4)",
    ).toBeVisible();

    // Fill the six fields with the live fixture token + dev identity.
    await page
      .locator("[data-testid='add-runtime-name']")
      .fill("first-attach-e2e");
    await page
      .locator("[data-testid='add-runtime-url']")
      .fill(runtime.baseURL);
    await page.locator("[data-testid='add-runtime-token']").fill(runtime.token);
    await page.locator("[data-testid='add-runtime-tenant']").fill("dev");
    await page.locator("[data-testid='add-runtime-user']").fill("dev");
    await page.locator("[data-testid='add-runtime-session']").fill("dev");

    // Submit reloads the page after writing to localStorage.
    await Promise.all([
      page.waitForLoadState("load"),
      page.locator("[data-testid='add-runtime-submit']").click(),
    ]);

    // After the reload the connection envelope is seeded.
    const seeded = await page.evaluate(
      ([base, token, tenant, user, session]) => ({
        baseURL: window.localStorage.getItem(base),
        token: window.localStorage.getItem(token),
        tenant: window.localStorage.getItem(tenant),
        user: window.localStorage.getItem(user),
        session: window.localStorage.getItem(session),
      }),
      [
        STORAGE_BASE_URL,
        STORAGE_TOKEN,
        STORAGE_TENANT,
        STORAGE_USER,
        STORAGE_SESSION,
      ] as const,
    );
    expect(seeded.baseURL, "manual form wrote base_url").toBeTruthy();
    expect(seeded.token, "manual form wrote token").toBeTruthy();
    expect(seeded.tenant, "manual form wrote tenant").toBe("dev");
    expect(seeded.user, "manual form wrote user").toBe("dev");
    expect(seeded.session, "manual form wrote session").toBe("dev");
  });
});
