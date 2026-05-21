// Harbor Console — Flows page e2e spec (Phase 73i / D-117, refactored
// onto the design-system foundation — D-121).
//
// Per-page Playwright spec for the Console Flows page. It exercises the
// shared `DataTable` catalog, the four-state `PageState`, the engine
// graph canvas, the `Run flow` scope-claim gate, the run-history →
// run-summary drill, and the heavy-output `Open artifact` link.
//
// SKIP semantics (mirroring the Phase 75 harness baseline + CLAUDE.md
// §4.2's "404/405/501 → SKIP" smoke convention): the `harbor console`
// subcommand lands in Phase 73m. Until then `consoleSubcommandAvailable()`
// is false and the whole describe block SKIPs cleanly — the spec ships
// in THIS phase's PR (the §13 per-page-spec-in-same-PR rule) and flips
// to live once 73m merges.
//
// Phase 75a (D-131): the runtime-entity seeding gap is closed — the
// `harbor console` binary boots a deterministic flow fixture set when
// `HARBOR_DEV_SEED_FIXTURES=1` (set by the harness `runtime` fixture),
// and `seedFlowsConnection` below uses the matching `(dev, dev, dev)`
// triple. The flow-drill tests that were parked on the seeding gap now
// run for real.

import {
  test,
  expect,
  consoleSubcommandAvailable,
} from './fixtures/page';
import { BasePage } from './pages/base-page';
import { STORAGE_KEYS } from '../src/lib/connection';

/**
 * Seed the `connection.ts` storage convention so the D-121 Flows page
 * resolves a live Runtime connection. The triple MUST match the
 * `harbor console` dev token — `(dev, dev, dev)` (cmd/harbor/devauth.go)
 * — the boot-seeded flow fixtures (HARBOR_DEV_SEED_FIXTURES, Phase 75a /
 * D-131) live under that triple.
 */
async function seedFlowsConnection(
  page: import('@playwright/test').Page,
  baseURL: string,
  token: string,
): Promise<void> {
  await page.addInitScript(
    ([keys, base, tok]) => {
      window.localStorage.setItem(keys.baseURL, base);
      window.localStorage.setItem(keys.token, tok);
      window.localStorage.setItem(keys.tenant, 'dev');
      window.localStorage.setItem(keys.user, 'dev');
      window.localStorage.setItem(keys.session, 'dev');
      window.localStorage.setItem(keys.scopes, 'admin');
    },
    [STORAGE_KEYS, baseURL, token] as const,
  );
}


/** Page object for the Console Flows page. */
class FlowsPage extends BasePage {
  readonly selectors = {
    page: "[data-testid='flows-page']",
    // The catalog is now the shared `DataTable`; rows carry the
    // `catalog-row` marker, the page wraps it in `flows-page`.
    catalogRow: "[data-testid='catalog-row']",
    catalogRun: "[data-testid='catalog-run']",
    catalogMetrics: "[data-testid='catalog-metrics']",
    search: "[data-testid='flows-search']",
    searchApply: "[data-testid='flows-search-apply']",
    refresh: "[data-testid='flows-refresh']",
    saveView: "[data-testid='flows-save-view']",
    // The four-state PageState boundary (CONVENTIONS.md §4).
    stateDisconnected: "[data-testid='page-state-disconnected']",
    stateLoading: "[data-testid='page-state-loading']",
    stateError: "[data-testid='page-state-error']",
    stateEmpty: "[data-testid='page-state-empty']",
    retry: "[data-testid='page-state-retry']",
    // Detail-rail metrics card.
    railMetricsEmpty: "[data-testid='rail-metrics-empty']",
    metricsCard: "[data-testid='flow-metrics-card']",
    // Detail route.
    detailPage: "[data-testid='flow-detail-page']",
    detailRun: "[data-testid='detail-run']",
    detailBack: "[data-testid='flow-detail-back']",
    graphCanvas: "[data-testid='engine-graph-canvas']",
    graphNode: "[data-testid='graph-node']",
    runHistory: "[data-testid='run-history']",
    runHistoryRow: "[data-testid='run-history-row']",
    runSummary: "[data-testid='run-summary-panel']",
    runSummaryEmpty: "[data-testid='run-summary-empty']",
    runOpenArtifact: "[data-testid='run-open-artifact']",
    runFlowModal: "[data-testid='run-flow-modal']",
    footer: "[data-testid='connection-footer']",
  } as const;

  async goto(): Promise<void> {
    await this.gotoSlug('flows');
  }

  async gotoDetail(flowID: string): Promise<void> {
    await this.gotoSlug(`flows/${flowID}`);
  }
}

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

test.describe('Console Flows page', () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    'harbor console subcommand absent (pre-Phase-73m) or bin/harbor not built',
  );

  // A wide desktop viewport. The Flows page is a two-column grid
  // (`1fr var(--size-rail)`); at the Playwright default 1280px width
  // the catalog table's right-most actions column tucks under the
  // detail rail, so a per-row action button (`catalog-run` /
  // `catalog-metrics`) is pointer-intercepted by the rail. The Console
  // is a desktop control-plane app — a 1600px viewport is realistic and
  // gives the actions column clearance.
  test.use({ viewport: { width: 1600, height: 900 } });

  test('the catalog renders registered flows via the shared DataTable', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedFlowsConnection(page, runtime.baseURL, runtime.token);
    const flows = new FlowsPage(page, runtime.baseURL);
    await flows.goto();
    await expect(page.locator(flows.selectors.page)).toBeVisible();
    await expect(
      page.locator(flows.selectors.catalogRow).first(),
    ).toBeVisible();
    // The shared ConnectionFooter renders inside the app shell.
    await expect(page.locator(flows.selectors.footer)).toBeVisible();
  });

  test('the catalog routes through PageState — rows or the Empty message', async ({
    page,
    runtime,
    helpers,
  }) => {
    // CONVENTIONS.md §4 state 4: a zero-row result renders the
    // page-specific Empty message — never a bare empty table. With the
    // Phase 75a fixture seeding the catalog carries rows; this asserts
    // the catalog resolved into EITHER its rows OR — when no flows are
    // registered — the catalog's own Empty message (scoped to the
    // catalog area, NOT the detail rail's nested PageState which shares
    // the `page-state-empty` testid).
    await helpers.seedAuth(runtime.token);
    await seedFlowsConnection(page, runtime.baseURL, runtime.token);
    const flows = new FlowsPage(page, runtime.baseURL);
    await flows.goto();
    const rows = page.locator(flows.selectors.catalogRow);
    const catalogEmpty = page
      .locator(`${flows.selectors.page} ${flows.selectors.stateEmpty}`)
      .filter({ hasText: 'No flows registered' });
    await expect(
      rows.first().or(catalogEmpty.first()),
      'the flows catalog resolves into rows or the Empty message',
    ).toBeVisible();
  });

  test('selecting a flow opens the detail page + engine graph canvas', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedFlowsConnection(page, runtime.baseURL, runtime.token);
    const flows = new FlowsPage(page, runtime.baseURL);
    await flows.goto();
    await page.locator(flows.selectors.catalogRow).first().click();
    await expect(page.locator(flows.selectors.detailPage)).toBeVisible();
    await expect(page.locator(flows.selectors.graphCanvas)).toBeVisible();
    await expect(
      page.locator(flows.selectors.graphNode).first(),
    ).toBeVisible();
  });

  test('Run flow is enabled with the flows.run scope claim', async ({
    page,
    runtime,
    helpers,
  }) => {
    // The harness seeds an admin-scoped token; the `Run flow` button is
    // enabled and opens the inline runner modal.
    await helpers.seedAuth(runtime.token);
    await seedFlowsConnection(page, runtime.baseURL, runtime.token);
    const flows = new FlowsPage(page, runtime.baseURL);
    await flows.goto();
    const runBtn = page.locator(flows.selectors.catalogRun).first();
    await expect(runBtn).toBeEnabled();
    await runBtn.click();
    await expect(page.locator(flows.selectors.runFlowModal)).toBeVisible();
  });

  test('Run flow carries the scope-claim tooltip and degrades, never vanishes', async ({
    page,
    runtime,
    helpers,
  }) => {
    // D-066: the `Run flow` affordance ALWAYS renders — it degrades to
    // disabled-with-tooltip without the claim, it never vanishes. The
    // button's `title` attribute carries the scope-claim explanation in
    // both states, so the operator always learns why a run is gated.
    await helpers.seedAuth(runtime.token);
    await seedFlowsConnection(page, runtime.baseURL, runtime.token);
    const flows = new FlowsPage(page, runtime.baseURL);
    await flows.goto();
    const runBtn = page.locator(flows.selectors.catalogRun).first();
    await expect(runBtn).toHaveCount(1);
    const title = await runBtn.getAttribute('title');
    expect(title, 'Run flow button always carries a title').toBeTruthy();
  });

  test('the detail rail surfaces flow metrics on demand', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedFlowsConnection(page, runtime.baseURL, runtime.token);
    const flows = new FlowsPage(page, runtime.baseURL);
    await flows.goto();
    // The metrics rail starts in the PageState Empty state.
    await expect(page.locator(flows.selectors.railMetricsEmpty)).toBeVisible();
    await page.locator(flows.selectors.catalogMetrics).first().click();
    await expect(page.locator(flows.selectors.metricsCard)).toBeVisible();
  });

  test('clicking a run-history row loads the run summary panel', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedFlowsConnection(page, runtime.baseURL, runtime.token);
    const flows = new FlowsPage(page, runtime.baseURL);
    await flows.goto();
    await page.locator(flows.selectors.catalogRow).first().click();
    const runRow = page.locator(flows.selectors.runHistoryRow).first();
    await runRow.click();
    await expect(page.locator(flows.selectors.runSummary)).toBeVisible();
  });

  test('a heavy run output surfaces an Open artifact link, never inline bytes', async ({
    page,
    runtime,
    helpers,
  }) => {
    // D-026: a run whose output exceeded the heavy-content threshold is
    // shipped by-reference; the summary panel renders an `Open artifact`
    // link rather than inlining the bytes.
    await helpers.seedAuth(runtime.token);
    await seedFlowsConnection(page, runtime.baseURL, runtime.token);
    const flows = new FlowsPage(page, runtime.baseURL);
    await flows.goto();
    await page.locator(flows.selectors.catalogRow).first().click();
    const heavyRow = page
      .locator(flows.selectors.runHistoryRow)
      .filter({ hasText: 'heavy' });
    if ((await heavyRow.count()) > 0) {
      await heavyRow.first().click();
      await expect(page.locator(flows.selectors.runOpenArtifact)).toBeVisible();
    }
  });

  test('no authoring affordances render — the page is view-only (D-063)', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedFlowsConnection(page, runtime.baseURL, runtime.token);
    const flows = new FlowsPage(page, runtime.baseURL);
    await flows.goto();
    await page.locator(flows.selectors.catalogRow).first().click();
    // `Add node`, `Delete edge`, `Save graph`, `New flow` MUST be absent
    // — not disabled, absent (D-063).
    for (const label of ['Add node', 'Delete edge', 'Save graph', 'New flow']) {
      await expect(
        page.getByRole('button', { name: label }),
      ).toHaveCount(0);
    }
  });
});
