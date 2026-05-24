// Harbor Console e2e harness — extended test/expect (Phase 75 / D-115).
//
// This is the SINGLE import every per-page spec uses:
//
//   import { test, expect } from "../fixtures/page";
//
// It re-exports an extended Playwright `test` with the harness pre-wired:
//   - the `runtime` fixture (boots Runtime + `harbor console` — harbor-runtime.ts)
//   - `seedAuth`        — pre-populate the auth-storage token (D-091)
//   - `gotoPage(slug)`  — navigate to a Console page by IA slug
//
// Per-page specs NEVER import from `@playwright/test` directly — that bypasses
// the harness and re-introduces the hand-rolled-setup drift the harness exists
// to close (Phase 75 plan, acceptance criteria).

import {
  test as runtimeTest,
  consoleSubcommandAvailable,
  type RuntimeFixture,
} from "./harbor-runtime";
import type { Page } from "@playwright/test";

export { consoleSubcommandAvailable };

/**
 * The Console IA — the 14 V1 page slugs (Evaluations excluded per D-064).
 * `gotoPage` accepts these; the Phase 75a aggregator enumerates them.
 */
export const CONSOLE_PAGES = [
  "overview",
  "live-runtime",
  "sessions",
  "tasks",
  "agents",
  "tools",
  "events",
  "background-jobs",
  "flows",
  "memory",
  "mcp-connections",
  "artifacts",
  "settings",
  "playground",
] as const;

export type ConsolePageSlug = (typeof CONSOLE_PAGES)[number];

/** Harness navigation + auth helpers injected into every spec. */
export type ConsoleHelpers = {
  /**
   * Seed the auth-storage token so the SvelteKit app boots authenticated.
   * D-091: the Console persists a WebCrypto-encrypted JWT in storage; the
   * harness writes the raw token into `localStorage` under the well-known
   * key the Console reads. The encryption envelope is the Console's concern;
   * the harness seeds the pre-encryption token and lets the app re-wrap it.
   */
  seedAuth(token: string): Promise<void>;
  /** Navigate to a Console page by IA slug and wait for it to settle. */
  gotoPage(slug: ConsolePageSlug): Promise<void>;
};

/** The storage key the Console reads its session token from (D-091). */
export const AUTH_STORAGE_KEY = "harbor.console.token";

async function seedAuthImpl(page: Page, token: string): Promise<void> {
  // `addInitScript` runs before any page script, so the token is present
  // by the time the SvelteKit app's auth guard reads storage.
  await page.addInitScript(
    ([key, value]) => {
      window.localStorage.setItem(key, value);
    },
    [AUTH_STORAGE_KEY, token] as const,
  );
}

async function gotoPageImpl(
  page: Page,
  runtime: RuntimeFixture,
  slug: ConsolePageSlug,
): Promise<void> {
  // Console routes are SvelteKit file-based routes under `/`. Overview is the
  // index; the rest map slug → `/<slug>`.
  const path = slug === "overview" ? "/" : `/${slug}`;
  await page.goto(new URL(path, runtime.baseURL).toString());
  // "load" not "networkidle": every Console page opens a long-lived SSE
  // `EventSource` (events.subscribe) on mount. `networkidle` requires
  // 500ms of zero in-flight requests, which never fires while SSE holds
  // the connection open — every test hits its 30s timeout. The fix that
  // exposed this (PR #226 / round-3 SSE access_token shim) made SSE
  // actually work cross-origin; pre-fix the SSE 401'd instantly and
  // `networkidle` fired by accident. `load` is the correct end-of-
  // page-load signal here.
  await page.waitForLoadState("load");
}

/**
 * The extended `test`. Carries the `runtime` worker fixture plus the
 * test-scoped `helpers` object (`seedAuth`, `gotoPage`).
 */
export const test = runtimeTest.extend<{ helpers: ConsoleHelpers }>({
  helpers: async ({ page, runtime }, use) => {
    await use({
      seedAuth: (token: string) => seedAuthImpl(page, token),
      gotoPage: (slug: ConsolePageSlug) => gotoPageImpl(page, runtime, slug),
    });
  },
});

export { expect } from "@playwright/test";
export type { RuntimeFixture } from "./harbor-runtime";
