// Harbor Console e2e — agent-config control panel (the consolidated 92h
// consumer).
//
// Covers the panel built against the D-121 design-system foundation
// (the Settings single-section composition — a left sub-nav rail listing
// the five control-plane areas, exactly one area in view at a time):
//   (a) the `/agent-config` route serves + hydrates inside the app shell
//       and the panel root + agent selector render;
//   (b) the sub-nav rail lists the five areas, and selecting one swaps the
//       single section rendered in the right pane;
//   (c) with the admin scope claim, the write surface is live — no
//       read-only scope banner, the Load control is enabled;
//   (d) WITHOUT the admin scope claim, the panel routes to the admin-scope
//       info state (no scary error / Retry) — the agent_config read surface
//       is itself admin-scoped (D-235; the 92b precedent).
//
// SKIP semantics (mirrors `agents-page.spec.ts`): until the `harbor
// console` subcommand + the agent_config surface are present in the
// harness build, the `runtime` fixture reports `available: false` and the
// describe block SKIPs at collection time — keeping the baseline green.
//
// The panel is NOT one of the 14 IA slugs (`gotoPage` only knows those),
// so this spec navigates to `/agent-config` directly against the harness
// Runtime base URL. The header card + agent selector + scope banner render
// OUTSIDE the `<PageState>` async boundary, so the admin/non-admin
// assertions hold even when the dev runtime has no agent_config data.

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";
import type { Page } from "@playwright/test";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

/** Seeds a connection envelope with an explicit scope set (so the
 * non-admin case can drop the `admin` claim the harness default carries). */
async function seedConnectionWithScopes(
  page: Page,
  baseURL: string,
  token: string,
  scopes: string,
): Promise<void> {
  await page.addInitScript(
    ([b, t, s]) => {
      window.localStorage.setItem("harbor.runtime.base_url", b);
      window.localStorage.setItem("harbor.runtime.token", t);
      window.localStorage.setItem("harbor.runtime.tenant", "dev");
      window.localStorage.setItem("harbor.runtime.user", "dev");
      window.localStorage.setItem("harbor.runtime.session", "dev");
      window.localStorage.setItem("harbor.runtime.scopes", s);
    },
    [baseURL, token, scopes] as const,
  );
}

async function gotoPanel(page: Page, baseURL: string): Promise<void> {
  await page.goto(new URL("/agent-config", baseURL).toString());
  await page.waitForLoadState("load");
}

test.describe("Console agent-config control panel", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent or bin/harbor not built",
  );

  test("the panel route serves and hydrates", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.seedConnection();
    await gotoPanel(page, runtime.baseURL);

    await expect(
      page.locator("[data-testid='console-hydrated']"),
      "the Console app hydrated",
    ).toBeAttached();
    await expect(
      page.locator("[data-testid='agent-config-page']"),
      "the panel root is present",
    ).toBeVisible();
    await expect(
      page.locator("[data-testid='agent-config-agent-input']"),
      "the agent selector renders",
    ).toBeVisible();
  });

  test("the sub-nav rail lists the six areas and selecting one swaps the section", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.seedConnection();
    await gotoPanel(page, runtime.baseURL);

    // The rail (outside the async boundary) lists all six control-plane
    // areas in display order (92i adds "Model & sampling" / llm).
    const rail = page.locator("[data-testid='agent-config-subnav']");
    await expect(rail, "the sub-nav rail renders").toBeVisible();
    for (const area of [
      "revisions",
      "prompt",
      "llm",
      "skills",
      "mcp",
      "add-connection",
    ]) {
      await expect(
        page.locator(`[data-testid='agent-config-subnav-${area}']`),
        `the rail lists the ${area} area`,
      ).toBeVisible();
    }

    // The rail is the single-section selector and renders OUTSIDE the
    // `<PageState>` async boundary, so its behaviour holds even when the
    // harness Runtime carries no agent_config data (the active section's
    // CONTENT is data-gated and is asserted in the vitest unit tests).
    // Exactly one rail item is active at a time; Revision history is default.
    await expect(
      page.locator("[data-testid='agent-config-subnav-revisions']"),
      "Revision history is the default active area",
    ).toHaveAttribute("aria-current", "true");

    // Selecting another area moves the active highlight (single-section model).
    await page.locator("[data-testid='agent-config-subnav-skills']").click();
    await expect(
      page.locator("[data-testid='agent-config-subnav-skills']"),
      "selecting Skills makes it the active area",
    ).toHaveAttribute("aria-current", "true");
    await expect(
      page.locator("[data-testid='agent-config-subnav-revisions']"),
      "the previously-active area is no longer active",
    ).toHaveAttribute("aria-current", "false");
  });

  test("the Model & sampling area is selectable and the Save-all bar is absent until edits", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.seedConnection();
    await gotoPanel(page, runtime.baseURL);

    // The 92i "Model & sampling" (llm) rail item selects like any other area
    // (the rail renders OUTSIDE the data-gated <PageState>; the form CONTENT
    // is asserted in vitest, where the client is mockable).
    await page.locator("[data-testid='agent-config-subnav-llm']").click();
    await expect(
      page.locator("[data-testid='agent-config-subnav-llm']"),
      "selecting Model & sampling makes it the active area",
    ).toHaveAttribute("aria-current", "true");

    // The atomic Save-all bar self-gates on staged edits — with the data-less
    // harness Runtime (no agent_config data, nothing staged) it is absent,
    // never a faked always-on control.
    await expect(
      page.locator("[data-testid='agent-config-save-all-bar']"),
      "the Save-all bar is absent with no staged edits",
    ).toHaveCount(0);
  });

  test("with the admin claim the write surface is live (no read-only banner)", async ({
    page,
    runtime,
    helpers,
  }) => {
    // The harness `seedConnection` carries the `admin` claim.
    await helpers.seedAuth(runtime.token);
    await helpers.seedConnection();
    await gotoPanel(page, runtime.baseURL);

    await expect(
      page.locator("[data-testid='agent-config-page']"),
    ).toBeVisible();
    // No read-only scope banner when the connection carries admin.
    await expect(
      page.locator("[data-testid='agent-config-scope-banner']"),
      "no read-only banner with the admin claim",
    ).toHaveCount(0);
    // The Load control is enabled (the agent selector is usable).
    await expect(
      page.locator("[data-testid='agent-config-load']"),
      "the Load control is enabled with a connection",
    ).toBeEnabled();
  });

  test("without the admin claim the panel routes to the admin-scope info state", async ({
    page,
    runtime,
    helpers,
  }) => {
    // Seed a connection that LACKS the admin scope claim. The agent-config
    // READ surface is itself admin-scoped (D-235), so the panel cannot load —
    // it routes to the not-applicable info branch (no scary error, no Retry).
    await helpers.seedAuth(runtime.token);
    await seedConnectionWithScopes(
      page,
      runtime.baseURL,
      runtime.token,
      "console:fleet",
    );
    await gotoPanel(page, runtime.baseURL);

    await expect(
      page.locator("[data-testid='agent-config-page']"),
    ).toBeVisible();
    // The info branch explains the missing scope; no error/Retry surfaces.
    await expect(
      page.locator("[data-testid='page-state-info']"),
      "the admin-scope info state renders for a non-admin",
    ).toBeVisible();
    await expect(
      page.locator("[data-testid='page-state-error']"),
      "no scary error branch for a non-admin",
    ).toHaveCount(0);
  });
});
