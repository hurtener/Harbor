// Overview page Playwright spec (Phase 73a / D-127).
//
// This is the per-page e2e spec for the Console Overview page. It rides
// on the Phase 75 harness baseline (`tests/fixtures/page.ts`): the
// `runtime` fixture boots a per-run Harbor Runtime + `harbor console`
// instance, and the suite gates on `consoleSubcommandAvailable()`.
//
// SKIP semantics (mirrors the harness): when the `harbor console`
// subcommand is absent (pre-Phase-73m) or `bin/harbor` is not built,
// the whole describe block SKIPs cleanly so the harness baseline stays
// green.
//
// The Overview page is composition over already-shipped Protocol
// surface (Phase 72f `runtime.counters` / `runtime.health`, Phase 72e
// `pause.list`, Phase 60/72 `events.subscribe`, Phase 54 `approve` /
// `reject`). Phase 73a ships NO new Protocol method. The assertions
// below target the page-overview.md §12 mockup-aligned shape — the
// route lives under `(console)/` per CONVENTIONS.md §1 (served at
// `/overview`, no `/console/` prefix; `/` redirects here).
//
// Coverage (acceptance criteria):
//   (a) initial load renders all panel skeletons / the depth-bar shell;
//   (b) counter cards populate (or a documented PageState renders);
//   (c) the intervention queue renders with Approve / Reject hidden-as-
//       disabled when the control scope is absent;
//   (d) Quick Links navigation works;
//   (e) the `+ New` menu deep-links resolve.

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";
import { STORAGE_KEYS } from "../src/lib/connection";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

// Phase 75a (D-131): the runtime-entity seeding gap is closed — the
// `harbor console` binary boots with a deterministic fixture set when
// `HARBOR_DEV_SEED_FIXTURES=1` (set by the harness `runtime` fixture),
// and `seedConnection` below uses the matching `(dev, dev, dev)` triple.
// The counter-row + intervention-queue tests that were parked on the
// seeding gap now run for real.

/**
 * Seed the full `connection.ts` storage convention so the page resolves
 * a live Runtime connection (the harness `seedAuth` only writes the
 * legacy console-token key; the D-121 page resolves through
 * `connection.ts`).
 *
 * The identity triple MUST match the `harbor console` dev token —
 * `(dev, dev, dev)` (cmd/harbor/devauth.go). A mismatched triple makes
 * every identity-scoped Protocol read come back `identity_required` and
 * the page never reaches a rendered state (Phase 75a / D-131).
 */
async function seedConnection(
  page: import("@playwright/test").Page,
  baseURL: string,
  token: string,
): Promise<void> {
  await page.addInitScript(
    ([keys, base, tok]) => {
      window.localStorage.setItem(keys.baseURL, base);
      window.localStorage.setItem(keys.token, tok);
      window.localStorage.setItem(keys.tenant, "dev");
      window.localStorage.setItem(keys.user, "dev");
      window.localStorage.setItem(keys.session, "dev");
    },
    [STORAGE_KEYS, baseURL, token] as const,
  );
}

test.describe("Overview page", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent (pre-Phase-73m) or bin/harbor not built",
  );

  test("the page mounts with the depth-bar shell surfaces", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("overview");

    await expect(
      page.locator("[data-testid='overview-page']"),
      "the Overview page section is present",
    ).toBeAttached();
    // Phase 83s (D-161) — the per-page inline `connection-footer` was
    // removed in favour of the single viewport-fixed `ConnectionFooter`
    // rendered once by `(console)/+layout.svelte`. The assertion now
    // looks for the footer at the layout level (no `overview-page`
    // ancestor selector).
    await expect(
      page.locator("[data-testid='connection-footer']").first(),
      "the shared viewport ConnectionFooter renders",
    ).toBeVisible();
    await expect(
      page.locator("[data-testid='overview-activity-search']"),
      "the FilterBar search input renders",
    ).toBeVisible();
    await expect(
      page.locator("[data-testid='overview-window-facet']"),
      "the counter-window facet renders",
    ).toBeVisible();
    await expect(
      page.locator("[data-testid='overview-footer']"),
      "the page footer (runtime + Protocol + stream + Console version) renders",
    ).toBeVisible();
  });

  test("the counter row populates or a documented PageState renders", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("overview");

    const counters = page.locator("[data-testid='counter-row']");
    const error = page.locator("[data-testid='page-state-error']");
    const loading = page.locator("[data-testid='page-state-loading']");
    const empty = page.locator("[data-testid='page-state-empty']");
    const disconnected = page.locator("[data-testid='page-state-disconnected']");
    const info = page.locator("[data-testid='page-state-info']");
    await expect(
      counters.or(error).or(loading).or(empty).or(disconnected).or(info),
      "the page resolves into the counter row or a documented PageState",
    ).toBeVisible();

    // When the counter row resolved, all four cards are present.
    if ((await counters.count()) > 0) {
      for (const id of [
        "counter-events",
        "counter-tasks",
        "counter-jobs",
        "counter-mcp",
      ]) {
        await expect(
          page.locator(`[data-testid='${id}']`),
          `the ${id} card renders`,
        ).toBeVisible();
      }
    }
  });

  test("the intervention queue renders with scope-gated actions", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("overview");

    // The queue resolves into the table, its empty state, or a
    // PageState — every branch is documented (CONVENTIONS.md §4).
    const queue = page.locator("[data-testid='intervention-queue']");
    const queueEmpty = page.locator(
      "[data-testid='intervention-queue-state-empty']",
    );
    const error = page.locator("[data-testid='page-state-error']");
    const loading = page.locator("[data-testid='page-state-loading']");
    const empty = page.locator("[data-testid='page-state-empty']");
    const disconnected = page.locator("[data-testid='page-state-disconnected']");
    const info = page.locator("[data-testid='page-state-info']");
    await expect(
      queue.or(queueEmpty).or(error).or(loading).or(empty).or(disconnected).or(info),
      "the intervention queue resolves into a documented state",
    ).toBeVisible();

    // When the queue carries rows, the Approve button is present but
    // DISABLED for the dev runtime's non-admin token (D-066 — the
    // control-scope-gated verbs degrade to disabled-with-tooltip, never
    // a fake-success stub).
    const approve = page.locator("[data-testid='intervention-approve']").first();
    if ((await approve.count()) > 0) {
      await expect(
        approve,
        "Approve is disabled without the admin control-scope claim",
      ).toBeDisabled();
    }
  });

  test("Quick Links navigate to their unprefixed routes", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("overview");

    const grid = page.locator("[data-testid='quick-links-grid']");
    await expect(grid, "the Quick Links grid renders").toBeVisible();

    // Exactly six tiles — no Evaluations tile (D-064).
    await expect(
      grid.locator("a"),
      "the grid carries exactly six tiles (D-064 — no Evaluations)",
    ).toHaveCount(6);

    // Navigating the Tasks tile lands on the unprefixed /tasks route.
    await page.locator("[data-testid='quick-link-tasks']").click();
    await expect(page).toHaveURL(/\/tasks$/);
  });

  test("the + New menu deep-links resolve", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("overview");

    await page.locator("[data-testid='new-menu-trigger']").click();
    await expect(
      page.locator("[data-testid='new-menu-list']"),
      "the + New menu opens",
    ).toBeVisible();

    // The Playground item deep-links into the unprefixed /playground
    // route — the create flow itself is owned by 73n. The Playground
    // ships a `[session_id]` deep-link route (D-130), so when a session
    // is seeded the menu resolves to `/playground/<session_id>`; accept
    // either the bare route or the deep-linked form.
    await page.locator("[data-testid='new-menu-playground']").click();
    await expect(page).toHaveURL(/\/playground(\/[^/]+)?$/);
  });

  test("the Disconnected PageState renders when no Runtime is attached", async ({
    page,
    runtime,
    helpers,
  }) => {
    // Seed only the harness token — NOT the connection.ts storage
    // convention — so `resolveConnection()` returns null and the page
    // renders the Disconnected state (never conflated with Error).
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("overview");

    await expect(
      page.locator("[data-testid='page-state-disconnected']").first(),
      "the Disconnected PageState renders, distinct from Error",
    ).toBeVisible();
    await expect(
      page.locator("[data-testid='page-state-error']"),
      "the Error PageState is NOT shown for an unattached Console",
    ).toHaveCount(0);
  });
});
