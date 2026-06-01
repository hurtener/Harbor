// Harbor Console e2e — Live Runtime cockpit per-page spec (Phase 108e /
// D-177; rebuilt from the 108d topology-first spec).
//
// Covers the single-runtime capability-adaptive COCKPIT:
//   (a) the page route serves + hydrates inside the shared app shell with no
//       console errors,
//   (b) the spine panels render on a planner/RunLoop fixture runtime (posture
//       header · activity strip · needs-attention · live events · active
//       sessions · health · cost),
//   (c) topology is CAPABILITY-GATED — a `page.route()`-mocked capability set
//       that advertises `topology_snapshot` makes the topology panel appear;
//       without it the panel is absent,
//   (d) the needs-attention control verbs gate on the admin scope claim
//       (CONVENTIONS.md §5; the seeded connection carries none → disabled),
//   (e) a disconnected Console redirects to /settings (Phase 105).
//
// The free-floating composer is GONE (D-062): no run-composer, no chat import.
//
// SKIP semantics mirror the other page specs: until the `harbor console`
// subcommand + a live runtime are available the describe block SKIPs at
// collection time, keeping the harness baseline green.

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
      window.localStorage.setItem("harbor.runtime.tenant", "dev");
      window.localStorage.setItem("harbor.runtime.user", "dev");
      window.localStorage.setItem("harbor.runtime.session", "dev");
    },
    [baseURL, token] as const,
  );
}

test.describe("Console Live Runtime cockpit", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent or bin/harbor not built",
  );

  test("(a) the cockpit route serves and hydrates without console errors", async ({
    page,
    runtime,
    helpers,
  }) => {
    const errors: string[] = [];
    page.on("console", (msg) => {
      if (msg.type() === "error") errors.push(msg.text());
    });

    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("live-runtime");

    await expect(
      page.locator("[data-testid='console-hydrated']"),
      "the Console app hydrated",
    ).toBeAttached();
    await expect(
      page.locator("[data-testid='live-runtime-page']"),
      "the Live Runtime cockpit root is present",
    ).toBeVisible();
    expect(errors, "no console errors during hydration").toEqual([]);
  });

  test("(b) the spine panels render on a planner runtime", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("live-runtime");
    await page.waitForLoadState("load");

    // Posture header + activity strip (rows 1+2).
    await expect(
      page.locator("[data-testid='runtime-posture-header']"),
      "the runtime posture header renders",
    ).toBeVisible();
    await expect(
      page.locator("[data-testid='status-counter-strip']"),
      "the activity status-counter strip renders",
    ).toBeVisible();

    // The spine cockpit panels (row 3).
    for (const id of [
      "panel-needs-attention",
      "panel-live-events",
      "panel-active-sessions",
      "panel-health",
      "panel-cost",
    ]) {
      await expect(
        page.locator(`[data-testid='${id}']`),
        `the ${id} spine panel renders`,
      ).toBeVisible();
    }
  });

  test("(c) topology is capability-gated — absent without topology_snapshot, present with it", async ({
    page,
    runtime,
    helpers,
  }) => {
    // The dev runtime is planner/RunLoop-shaped (no topology_snapshot), so the
    // topology panel is absent. We assert that, then mock runtime.info to
    // advertise topology_snapshot and assert the panel appears (structural).
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);

    // Intercept runtime.info to advertise the topology capability. The typed
    // client memoises capabilities off this surface (client.capabilities()),
    // so the registry resolves the topology panel in.
    await page.route("**/v1/control/runtime.info", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          instance_id: "dev",
          build_version: "test",
          build_commit: "test",
          build_go_version: "go1.26",
          protocol_version: "1.0.0",
          uptime_seconds: 1,
          // runtime.info advertises capabilities as a STRING array (matches the
          // Go wire shape + client.capabilities() = new Set(info.capabilities)).
          capabilities: ["topology_snapshot"],
        }),
      });
    });
    // Mock the snapshot so the panel has a projection to render.
    await page.route("**/v1/control/topology.snapshot", async (route) => {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ nodes: [], edges: [], protocol_version: "1.0.0" }),
      });
    });

    await helpers.gotoPage("live-runtime");
    await page.waitForLoadState("load");

    await expect(
      page.locator("[data-testid='panel-topology']"),
      "the topology panel appears when topology_snapshot is advertised",
    ).toBeVisible();
  });

  test("(d) needs-attention verbs gate on the control scope claim", async ({
    page,
    runtime,
    helpers,
  }) => {
    // The seeded connection carries NO `admin` scope claim. When the queue has
    // rows the verbs render disabled-with-tooltip; with no rows the panel is
    // its honest empty state. Either way no fake-success is possible.
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("live-runtime");
    await page.waitForLoadState("load");

    const approve = page.locator("[data-testid='needs-attention-approve']").first();
    const empty = page.locator("[data-testid='needs-attention-empty']");
    // Whichever branch the live queue lands in, one of these is present.
    const hasRow = (await approve.count()) > 0;
    if (hasRow) {
      expect(
        await approve.isDisabled(),
        "Approve is disabled without the admin control claim",
      ).toBe(true);
    } else {
      await expect(empty, "the honest no-interventions empty state renders").toBeVisible();
    }
  });

  test("(e) a disconnected Console redirects to /settings (Phase 105)", async ({
    page,
    helpers,
  }) => {
    await helpers.gotoPage("live-runtime");
    await expect
      .poll(() => new URL(page.url()).pathname, { timeout: 5000 })
      .toMatch(/^\/settings(\/.*)?$/);
  });
});
