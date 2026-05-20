// Harbor Console e2e harness — page-object base class (Phase 75 / D-115).
//
// Per-page specs subclass `BasePage` to keep their selectors typed and their
// navigation + hydration-wait logic in one place. The base class encodes the
// harness conventions:
//   - a11y-ID-first selectors (`data-testid` / role) — never brittle CSS paths
//   - SvelteKit hydration is awaited via `networkidle` + an explicit
//     hydration marker the Console root layout stamps once interactive
//
// Example per-page subclass (lands in Phase 73a, NOT this phase):
//
//   class OverviewPage extends BasePage {
//     readonly selectors = { counterCards: "[data-testid='overview-counters']" };
//     async goto() { await this.gotoSlug("overview"); }
//   }

import type { Page } from "@playwright/test";

/**
 * The `data-testid` the Console root layout (`src/routes/+layout.svelte`,
 * shipped by the Stage-1 scaffold phase 72h) stamps on the document body once
 * SvelteKit has hydrated and the app is interactive. The harness waits on this
 * rather than a fixed timeout, so specs are deterministic.
 */
export const HYDRATION_MARKER = "[data-testid='console-hydrated']";

/**
 * Page-object base class. Per-page specs extend this; the harness baseline
 * ships it as the substrate (Phase 75 non-goal: per-page specs).
 */
export abstract class BasePage {
  constructor(
    protected readonly page: Page,
    protected readonly baseURL: string,
  ) {}

  /**
   * Typed selector map. Subclasses override with their page's a11y IDs.
   * Declared here so the harness convention (a11y-ID-first) is visible on the
   * base type rather than re-invented per page.
   */
  abstract readonly selectors: Readonly<Record<string, string>>;

  /** Navigate to a Console page by IA slug, then wait for hydration. */
  async gotoSlug(slug: string): Promise<void> {
    const path = slug === "overview" ? "/" : `/${slug}`;
    await this.page.goto(new URL(path, this.baseURL).toString());
    await this.waitForHydration();
  }

  /**
   * Wait until the SvelteKit app has hydrated and is interactive. Combines
   * Playwright's `networkidle` (no in-flight requests) with the explicit
   * hydration marker the root layout stamps — `networkidle` alone can settle
   * before client-side hydration finishes.
   */
  async waitForHydration(): Promise<void> {
    await this.page.waitForLoadState("networkidle");
    await this.page.waitForSelector(HYDRATION_MARKER, { state: "attached" });
  }
}
