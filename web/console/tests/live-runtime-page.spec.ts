// Harbor Console e2e — Live Runtime page per-page spec (Phase 73b /
// D-126).
//
// Covers the Live Runtime page built on the D-121 design-system
// foundation:
//   (a) the page route serves + hydrates inside the shared app shell,
//   (b) the header status-counter strip renders the five chips
//       (pending / running / completed / paused / failed),
//   (c) the main-canvas tab strip (Topology / Timeline / Metrics /
//       Health) swaps the primary view; Metrics + Health render the
//       72f-pointer empty state,
//   (d) the topology canvas (the §5 depth-bar primary view, in place of
//       a DataTable) renders via the shared `<EngineGraphCanvas>`,
//   (e) the bottom-dock Event Stream + the Skeleton-primitive composer
//       render (NO chat-module dependency — D-091),
//   (f) the composer's elevated steering verbs render
//       disabled-with-tooltip without the control scope claim
//       (CONVENTIONS.md §5),
//   (g) the four-state `<PageState>` Disconnected branch renders when
//       no Runtime connection is configured.
//
// SKIP semantics (mirrors `harness.spec.ts` + `tasks-page.spec.ts`):
// the `harbor console` subcommand lands in a later Stage; until then
// the `runtime` fixture reports `available: false` and the whole
// describe block SKIPs at collection time — keeping the harness
// baseline green. Once `harbor console` merges, the suite runs against
// a live Runtime + Console.
//
// The Phase 75a wave-end aggregator enumerates the 14 page slugs and
// asserts a matching `<slug>-page.spec.ts` exists; this file is the
// `live-runtime` slug's entry.
//
// SEED-DEPENDENT SKIPS: the tab-content tests below are `test.skip()`'d
// because the `harbor console` embedded runtime boots with no seeded
// topology nodes (the page lands in PageState `empty`, so the tab bodies
// never render) and the harness `seedIdentity` is a documented no-op
// stub. Real runtime-entity seeding lands with Phase 75a (the wave-end
// suite). See CLAUDE.md §17.6.

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

test.describe("Console Live Runtime page", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent or bin/harbor not built",
  );

  test("the Live Runtime page route serves and hydrates", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("live-runtime");

    await expect(
      page.locator("[data-testid='console-hydrated']"),
      "the Console app hydrated",
    ).toBeAttached();
    await expect(
      page.locator("[data-testid='live-runtime-page']"),
      "the Live Runtime page root is present",
    ).toBeVisible();
  });

  test("(b) the header status-counter strip renders five chips", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("live-runtime");
    await page.waitForLoadState("networkidle");

    await expect(
      page.locator("[data-testid='status-counter-strip']"),
      "the header status-counter strip renders",
    ).toBeVisible();

    for (const key of ["pending", "running", "completed", "paused", "failed"]) {
      await expect(
        page.locator(`[data-testid='counter-${key}']`),
        `the ${key} counter chip renders`,
      ).toBeVisible();
    }
  });

  test("(c) the tab strip swaps the primary view; Metrics/Health point to 72f", async ({
    page,
    runtime,
    helpers,
  }) => {
    // §17.6 deferral — NOT a seeding-gap skip. The Phase 75a fixture
    // seeder (D-131) closes the runtime-entity gap (sessions / agents /
    // tasks / artifacts / tools / flows / memory). The Live Runtime
    // page's tab content renders inside `<PageState>`, which only
    // renders children when `status === 'ready'`; `ready` requires a
    // non-empty `topology.snapshot`, and topology is projected from a
    // live engine run (`internal/runtime/engine/topology.go`) — NOT
    // from registry fixtures. Exercising it needs a real planner/engine
    // run fixture (a larger seam than entity seeding). Tracked as a
    // tracked in issue #178 (live-planner-run trajectory fixtures).
    test.skip(
      true,
      "deferred: needs a live engine-run topology fixture (not entity " +
        "seeding) — tracked in issue #178. See CLAUDE.md §17.6.",
    );
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("live-runtime");
    await page.waitForLoadState("networkidle");

    await expect(
      page.locator("[data-testid='live-runtime-tab-strip']"),
      "the main-canvas tab strip renders",
    ).toBeVisible();

    // The Metrics tab renders the 72f-pointer empty state.
    await page.locator("[data-testid='tab-metrics']").click();
    await expect(
      page.locator("[data-testid='metrics-tab-empty']"),
      "the Metrics tab shows the 72f-pointer empty state",
    ).toBeVisible();

    // The Health tab likewise.
    await page.locator("[data-testid='tab-health']").click();
    await expect(
      page.locator("[data-testid='health-tab-empty']"),
      "the Health tab shows the 72f-pointer empty state",
    ).toBeVisible();
  });

  test("(d) the topology canvas renders as the depth-bar primary view", async ({
    page,
    runtime,
    helpers,
  }) => {
    // §17.6 deferral — NOT a seeding-gap skip. The topology canvas is
    // the `<PageState>` `ready`-state primary view; `ready` requires a
    // non-empty `topology.snapshot`, projected from a live engine run
    // (not registry fixtures). Tracked in issue #178 — see
    // the (c) test's comment and issue #178.
    test.skip(
      true,
      "deferred: needs a live engine-run topology fixture (not entity " +
        "seeding) — tracked in issue #178. See CLAUDE.md §17.6.",
    );
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("live-runtime");
    await page.waitForLoadState("networkidle");

    // The Topology tab is the default; the canvas is the §5 primary view.
    await page.locator("[data-testid='tab-topology']").click();
    await expect(
      page.locator("[data-testid='topology-canvas']"),
      "the topology canvas renders on the Topology tab",
    ).toBeVisible();
  });

  test("(e) the bottom dock renders the Event Stream + Skeleton-primitive composer", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("live-runtime");
    await page.waitForLoadState("networkidle");

    await expect(
      page.locator("[data-testid='event-stream-dock']"),
      "the bottom-dock Event Stream pane renders",
    ).toBeVisible();
    // With no node selected, the composer (non-chat Skeleton primitives,
    // D-091) renders rather than the per-task detail pane.
    await expect(
      page.locator("[data-testid='run-composer']"),
      "the Skeleton-primitive composer renders when no node is selected",
    ).toBeVisible();
  });

  test("(f) elevated composer verbs gate on the control scope claim", async ({
    page,
    runtime,
    helpers,
  }) => {
    // The seeded connection carries NO `admin` scope claim, so the
    // elevated steering verbs render disabled-with-tooltip — never a
    // fake success (CONVENTIONS.md §5; CLAUDE.md §13).
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("live-runtime");
    await page.waitForLoadState("networkidle");

    const pauseBtn = page.locator("[data-testid='composer-pause']");
    await expect(pauseBtn, "the composer Pause verb surfaces").toBeVisible();
    const disabled = await pauseBtn.isDisabled();
    const tip = await pauseBtn.getAttribute("title");
    expect(
      disabled || (tip ?? "").includes("control scope"),
      "Pause is disabled-with-tooltip without the control claim",
    ).toBe(true);
  });

  test("(g) the Disconnected PageState renders without a Runtime connection", async ({
    page,
    helpers,
  }) => {
    // No `seedConnection` — connection.ts returns null, so the page
    // renders PageState's Disconnected branch (NOT the Error branch).
    await helpers.gotoPage("live-runtime");
    await expect(
      page.locator("[data-testid='live-runtime-page']"),
      "the Live Runtime page shell still renders",
    ).toBeVisible();
  });
});
