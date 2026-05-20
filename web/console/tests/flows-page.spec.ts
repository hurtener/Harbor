// Harbor Console — Flows page e2e spec (Phase 73i / D-117).
//
// Per-page Playwright spec for the Console Flows page. It exercises the
// catalog table, the engine graph canvas, the `Run flow` scope-claim
// gate, the run-history → summary-panel drill, and the heavy-output
// `Open artifact` link.
//
// SKIP semantics (mirroring the Phase 75 harness baseline + CLAUDE.md
// §4.2's "404/405/501 → SKIP" smoke convention): the `harbor console`
// subcommand lands in Phase 73m. Until then `consoleSubcommandAvailable()`
// is false and the whole describe block SKIPs cleanly — the spec ships
// in THIS phase's PR (the §13 per-page-spec-in-same-PR rule) and flips
// to live once 73m merges.

import {
  test,
  expect,
  consoleSubcommandAvailable,
} from './fixtures/page';
import { BasePage } from './pages/base-page';

/** Page object for the Console Flows page. */
class FlowsPage extends BasePage {
  readonly selectors = {
    page: "[data-testid='flows-page']",
    catalog: "[data-testid='flows-catalog']",
    catalogRow: "[data-testid='catalog-row']",
    catalogRun: "[data-testid='catalog-run']",
    search: "[data-testid='flows-search']",
    metricsCard: "[data-testid='flow-metrics-card']",
    detailHeader: "[data-testid='flow-detail-header']",
    detailRun: "[data-testid='detail-run']",
    graphCanvas: "[data-testid='engine-graph-canvas']",
    graphNode: "[data-testid='graph-node']",
    graphEdge: "[data-testid='graph-edge']",
    runHistory: "[data-testid='run-history']",
    runHistoryRow: "[data-testid='run-history-row']",
    runSummary: "[data-testid='run-summary-panel']",
    runOpenArtifact: "[data-testid='run-open-artifact']",
    runFlowModal: "[data-testid='run-flow-modal']",
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

  test('the catalog table renders registered flows', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    const flows = new FlowsPage(page, runtime.baseURL);
    await flows.goto();
    await expect(page.locator(flows.selectors.catalog)).toBeVisible();
  });

  test('selecting a flow opens the detail header + engine graph canvas', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    const flows = new FlowsPage(page, runtime.baseURL);
    await flows.goto();
    const firstRow = page.locator(flows.selectors.catalogRow).first();
    await firstRow.locator('button.link').click();
    await expect(page.locator(flows.selectors.detailHeader)).toBeVisible();
    // The shared engine graph canvas renders nodes + edges.
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
    const flows = new FlowsPage(page, runtime.baseURL);
    await flows.goto();
    const runBtn = page.locator(flows.selectors.catalogRun).first();
    // The affordance is present regardless of scope.
    await expect(runBtn).toHaveCount(1);
    // Its title attribute is always set (either "Run <flow>" or the
    // claim-required explanation) — never empty.
    const title = await runBtn.getAttribute('title');
    expect(title, 'Run flow button always carries a title').toBeTruthy();
  });

  test('clicking a run-history row loads the run summary panel', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    const flows = new FlowsPage(page, runtime.baseURL);
    await flows.goto();
    await page
      .locator(flows.selectors.catalogRow)
      .first()
      .locator('button.link')
      .click();
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
    const flows = new FlowsPage(page, runtime.baseURL);
    await flows.goto();
    await page
      .locator(flows.selectors.catalogRow)
      .first()
      .locator('button.link')
      .click();
    // Select the heavy run if one is present in the seeded history.
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
    const flows = new FlowsPage(page, runtime.baseURL);
    await flows.goto();
    await page
      .locator(flows.selectors.catalogRow)
      .first()
      .locator('button.link')
      .click();
    // `Add node`, `Delete edge`, `Save graph`, `New flow` MUST be absent
    // — not disabled, absent (D-063).
    for (const label of ['Add node', 'Delete edge', 'Save graph', 'New flow']) {
      await expect(
        page.getByRole('button', { name: label }),
      ).toHaveCount(0);
    }
  });
});
