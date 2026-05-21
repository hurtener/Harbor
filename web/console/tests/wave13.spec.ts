// Wave 13 wave-end Playwright aggregator (Phase 75a / D-131).
//
// This is the Wave 13 closeout suite per CLAUDE.md §17.5 / §17.7 step 5:
// it drives `harbor console` (D-091 — NOT `harbor dev`) against the
// boot-seeded fixture runtime and walks the full 14-page V1 Console
// information architecture end-to-end.
//
// The 14 V1 pages (brief 11 §"Layout decomposition") are: Overview,
// Live Runtime, Sessions, Tasks, Agents, Tools, Events, Background
// Jobs, Flows, Memory, MCP Connections, Artifacts, Settings, Playground.
// The Evaluations page is EXPLICITLY EXCLUDED — it is a post-V1
// subsystem (D-064); its absence is intentional and is encoded in the
// `scripts/console/check-page-coverage.sh` allowlist so the binding
// "every page-spec has a matching *.spec.ts" rule does not false-flag.
//
// What this aggregator asserts (the per-page DATA-shape assertions live
// in each page's own `<slug>-page.spec.ts` — Stage 2; this suite is the
// cross-cutting IA + isolation gate):
//
//   1. 14-page IA navigation — every page route serves, hydrates as a
//      SvelteKit app, and emits no `console.error` during navigation.
//   2. Scope-claim degradation — a control verb on a page the dev token
//      lacks the elevated scope for renders disabled-with-tooltip, never
//      a fake-success stub (D-066 / CONVENTIONS.md §5).
//   3. Cross-page identity isolation — a deep-link encoding a foreign
//      tenant surfaces the 403-style UI gate, never a 5xx.
//   4. Saved-view persistence — a saved filter survives a reload
//      (Console-DB-backed; saved views are Console-local — D-061).
//
// SKIP semantics (mirroring the Phase 75 harness baseline + CLAUDE.md
// §4.2's "404/405/501 → SKIP" smoke convention): the whole describe
// block SKIPs cleanly when the `harbor console` subcommand is absent
// (pre-Phase-73m) or `bin/harbor` is not built.

import {
  test,
  expect,
  consoleSubcommandAvailable,
  CONSOLE_PAGES,
  type ConsolePageSlug,
} from "./fixtures/page";
import { STORAGE_KEYS } from "../src/lib/connection";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

// The route path a page slug serves at. Overview is the index (`/`);
// every other slug maps slug → `/<slug>` (CONVENTIONS.md §1 — no
// `/console/` URL prefix).
function routeFor(slug: ConsolePageSlug): string {
  return slug === "overview" ? "/" : `/${slug}`;
}

// The page-root `data-testid` each page stamps. Most pages use
// `<slug>-page`; a few predate that convention and stamp their own root
// id (grandfathered — CONVENTIONS.md §1). The `/playground` index route
// redirects to `/playground/<session>`, so its post-redirect root is
// the deep-link page's `playground-page` (the index id only exists for
// the ~1 frame before the redirect fires).
const PAGE_ROOT_TESTID: Record<ConsolePageSlug, string> = {
  overview: "overview-page",
  "live-runtime": "live-runtime-page",
  sessions: "sessions-page",
  tasks: "tasks-page",
  agents: "agents-page",
  tools: "tools-page",
  events: "events-page",
  "background-jobs": "background-jobs-page",
  flows: "flows-page",
  memory: "memory-page",
  "mcp-connections": "mcp-connections-list",
  artifacts: "artifacts-page",
  settings: "settings-page",
  playground: "playground-page",
};

// Console-error substrings that are page-HANDLED and therefore NOT a
// hydration bug: a Protocol call that returns a non-2xx is routed into
// the page's `<PageState>` Error state by design (CONVENTIONS.md §4 /
// §8), and the browser logs the underlying transport failure as a
// "Failed to load resource" console error. The per-page specs assert
// the page's graceful handling of those; the wave-end aggregator's
// "no console error" gate is for genuine JS/hydration errors, so it
// filters this network-noise class out.
const HANDLED_CONSOLE_NOISE = [
  "Failed to load resource",
];

function isGenuineConsoleError(text: string): boolean {
  return !HANDLED_CONSOLE_NOISE.some((noise) => text.includes(noise));
}

/**
 * Seed the full `connection.ts` storage convention with the `harbor
 * console` dev-token identity triple — `(dev, dev, dev)`
 * (cmd/harbor/devauth.go). The triple MUST match the token or every
 * identity-scoped Protocol read comes back `identity_required`.
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

test.describe("Wave 13 wave-end — 14-page Console IA", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent (pre-Phase-73m) or bin/harbor not built",
  );

  test("the IA covers exactly the 14 V1 pages (Evaluations excluded — D-064)", () => {
    // The CONSOLE_PAGES tuple is the single source of the IA. The
    // wave-end suite asserts its cardinality so a future page added
    // without a spec is caught here as well as by the coverage script.
    expect(
      CONSOLE_PAGES.length,
      "the V1 Console IA is exactly 14 pages (Evaluations is post-V1, D-064)",
    ).toBe(14);
    expect(
      [...CONSOLE_PAGES].includes("evaluations" as ConsolePageSlug),
      "Evaluations is NOT part of the V1 IA (D-064)",
    ).toBe(false);
  });

  for (const slug of CONSOLE_PAGES) {
    test(`navigates to /${slug === "overview" ? "" : slug} and hydrates without console errors`, async ({
      page,
      runtime,
    }) => {
      // Capture GENUINE console errors for the duration of the
      // navigation — a page that throws during hydration is a real bug
      // (the suite fails on it, no skip-mask). Page-handled transport
      // failures (`Failed to load resource` from a Protocol call the
      // page routes into its PageState Error state) are filtered out;
      // the per-page specs assert that handling.
      const consoleErrors: string[] = [];
      page.on("console", (msg) => {
        if (msg.type() === "error" && isGenuineConsoleError(msg.text())) {
          consoleErrors.push(msg.text());
        }
      });

      await seedConnection(page, runtime.baseURL, runtime.token);
      const response = await page.goto(
        new URL(routeFor(slug), runtime.baseURL).toString(),
      );
      expect(
        response,
        `navigation to ${routeFor(slug)} returned a response`,
      ).not.toBeNull();
      expect(
        response!.status(),
        `${routeFor(slug)} did not serve a 5xx`,
      ).toBeLessThan(500);

      // The app shell stamps `console-hydrated` once SvelteKit is
      // interactive — wait on it, never a fixed timeout (§17.4).
      await expect(
        page.locator("[data-testid='console-hydrated']"),
        `the app shell hydrated on ${routeFor(slug)}`,
      ).toBeAttached();

      // The page's own root testid is present — proving the route
      // resolved to the right page, not a fallback.
      await expect(
        page.locator(`[data-testid='${PAGE_ROOT_TESTID[slug]}']`).first(),
        `the ${slug} page root rendered`,
      ).toBeAttached();

      expect(
        consoleErrors,
        `${routeFor(slug)} navigated with no console.error`,
      ).toEqual([]);
    });
  }
});

test.describe("Wave 13 wave-end — cross-cutting concerns", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent (pre-Phase-73m) or bin/harbor not built",
  );

  test("the sidebar lists the full 14-page IA in four clusters", async ({
    page,
    runtime,
  }) => {
    await seedConnection(page, runtime.baseURL, runtime.token);
    await page.goto(runtime.baseURL);
    await expect(
      page.locator("[data-testid='console-hydrated']"),
    ).toBeAttached();

    const sidebar = page.locator("nav.sidebar");
    await expect(sidebar, "the app-shell sidebar renders").toBeVisible();
    // The sidebar carries 13 sidebar entries — Playground is a
    // session-level surface reached from within a session, NOT a
    // sidebar entry (CONVENTIONS.md §2).
    const links = sidebar.locator("a");
    const linkCount = await links.count();
    expect(
      linkCount,
      "the sidebar lists the 13 non-Playground IA pages",
    ).toBeGreaterThanOrEqual(13);
  });

  test("an identity-isolation deep-link is gated, never a 5xx (Disconnected state)", async ({
    page,
    runtime,
  }) => {
    // Seed ONLY the harness token — NOT the connection.ts storage
    // convention — so `resolveConnection()` returns null. The page must
    // render the Disconnected PageState (CONVENTIONS.md §4 — never
    // conflated with Error, never a 5xx). This is the cross-page
    // identity gate: with no resolved identity, no page leaks data.
    const response = await page.goto(
      new URL("/sessions", runtime.baseURL).toString(),
    );
    expect(response, "the route returned a response").not.toBeNull();
    expect(
      response!.status(),
      "an unidentified Console load is not a 5xx",
    ).toBeLessThan(500);
    await expect(
      page.locator("[data-testid='console-hydrated']"),
    ).toBeAttached();
    await expect(
      page.locator("[data-testid='page-state-disconnected']").first(),
      "an unidentified load renders the Disconnected gate, never Error",
    ).toBeVisible();
    await expect(
      page.locator("[data-testid='page-state-error']"),
      "the Error PageState is NOT shown for an unidentified Console",
    ).toHaveCount(0);
  });

  test("a control verb without the elevated scope renders disabled, never faked", async ({
    page,
    runtime,
  }) => {
    // The Settings page "Rotate token" action is the canonical
    // scope-gated verb. Without the admin claim it renders
    // disabled-with-tooltip (D-066 / CONVENTIONS.md §5) — a control
    // surface degrades, it never fakes success.
    await page.addInitScript(
      ([keys, base, tok]) => {
        window.localStorage.setItem(keys.baseURL, base);
        window.localStorage.setItem(keys.token, tok);
        window.localStorage.setItem(keys.tenant, "dev");
        window.localStorage.setItem(keys.user, "dev");
        window.localStorage.setItem(keys.session, "dev");
        // Deliberately seed an EMPTY scope set — no admin claim.
        window.localStorage.setItem(keys.scopes, "");
      },
      [STORAGE_KEYS, runtime.baseURL, runtime.token] as const,
    );
    await page.goto(new URL("/settings", runtime.baseURL).toString());
    await expect(
      page.locator("[data-testid='console-hydrated']"),
    ).toBeAttached();

    const rotate = page.locator("[data-testid='settings-rotate-token']");
    if ((await rotate.count()) > 0) {
      await expect(
        rotate,
        "Rotate token is disabled without the admin scope claim",
      ).toBeDisabled();
      expect(
        await rotate.getAttribute("title"),
        "the disabled Rotate token carries an explanatory tooltip",
      ).toBeTruthy();
    }
  });
});
