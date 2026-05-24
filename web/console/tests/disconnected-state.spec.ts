// Harbor Console e2e — Phase 83r + 83s disconnected-state hygiene.
//
// One spec covers the cross-page disconnected contract — the
// post-83k visual walkthrough pinned eight bug shapes (W1/W2/W3 +
// N2/N4/N5/N8/N9/N10) that each page handled inconsistently. The
// hygiene pass standardises them: action buttons disable, the Cost
// Rollup card stops rendering `$0.00`, the Tools page shows ONE empty
// state (not two), the Live Runtime composer disables, the MCP status
// chips desaturate, the Artifacts subtitle reads "no Runtime
// attached", and every page surfaces a single ConnectionFooter
// (rendered by the app shell only — N2).
//
// SKIP semantics (mirrors `harness.spec.ts`): the `harbor console`
// subcommand lands in Phase 73m. When absent the whole describe block
// SKIPs at collection time.

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

test.describe("Console disconnected-state hygiene (Phase 83r + 83s)", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent (pre-Phase-73m) or bin/harbor not built",
  );

  test("(W2) Live Runtime composer disables in the disconnected state", async ({
    page,
    runtime,
    helpers,
  }) => {
    // Seed auth ONLY — connection.ts returns null, so the page is
    // disconnected. The composer textarea + every verb must disable;
    // hovering any of them carries the shared tooltip.
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("live-runtime");

    const textarea = page.locator("[data-testid='composer-textarea']");
    await expect(
      textarea,
      "the composer textarea disables when no Runtime is attached",
    ).toBeDisabled();

    for (const verb of [
      "composer-start",
      "composer-user-message",
      "composer-redirect",
      "composer-inject",
      "composer-pause",
      "composer-resume",
      "composer-cancel",
    ]) {
      await expect(
        page.locator(`[data-testid='${verb}']`),
        `${verb} disables when no Runtime is attached`,
      ).toBeDisabled();
    }

    // The Refresh button in the page header also disables.
    await expect(
      page.locator("[data-testid='live-runtime-refresh']"),
      "the page-header Refresh button disables in the disconnected state",
    ).toBeDisabled();
  });

  test("(W3) Tools page disables action + filter controls in the disconnected state", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("tools");

    for (const id of [
      "tools-refresh",
      "tools-search",
      "tools-search-apply",
      "tools-filter-clear",
      "tools-save-filter",
    ]) {
      await expect(
        page.locator(`[data-testid='${id}']`),
        `${id} disables when no Runtime is attached`,
      ).toBeDisabled();
    }
  });

  test("(N5) Tools page renders ONE empty-state message when disconnected, not two", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("tools");

    // The PageState disconnected branch is visible.
    await expect(
      page.locator("[data-testid='page-state-disconnected']"),
      "the Disconnected PageState renders",
    ).toBeVisible();

    // The secondary `tools-detail-empty` MUST NOT also render — the
    // pre-83r page stacked both and showed two empty messages.
    await expect(
      page.locator("[data-testid='tools-detail-empty']"),
      "the secondary ToolDetailTabs empty does NOT also render (N5)",
    ).toHaveCount(0);
  });

  test("(W1) Overview Cost Rollup card does not render synthetic data when disconnected", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("overview");

    // The disconnected branch renders the consolidated placeholder.
    await expect(
      page.locator("[data-testid='cost-rollup-disconnected']"),
      "the Cost Rollup card renders the disconnected placeholder (W1)",
    ).toBeVisible();

    // The synthetic `$0.00` total + the "No cost recorded" empty row
    // are absent — the pre-83r card rendered both even when there was
    // no Runtime to source data from.
    await expect(
      page.locator("[data-testid='cost-rollup-total']"),
      "no synthetic $0.00 total when disconnected (W1)",
    ).toHaveCount(0);
    await expect(
      page.locator("[data-testid='cost-rollup-empty']"),
      "no 'No cost recorded' line when disconnected (W1)",
    ).toHaveCount(0);

    // The Overview Refresh button is also disabled in the header.
    await expect(
      page.locator("[data-testid='overview-refresh']"),
      "Refresh disables in the disconnected state",
    ).toBeDisabled();
  });

  test("(N8) MCP Connections status chips desaturate in the disconnected state", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("mcp-connections");

    // The State facet chip row is the most reliable site to assert
    // chip desaturation — it renders the fixed 6-element STATE_FACETS
    // even with no Runtime in scope. The desaturated chip has
    // `data-kind="neutral"` and `data-desaturated="true"` regardless
    // of its semantic kind.
    const onlineFacet = page.locator(
      "[data-testid='filter-online'] .status-chip",
    );
    await expect(
      onlineFacet,
      "the Online facet chip is rendered",
    ).toBeVisible();
    await expect(
      onlineFacet,
      "the Online facet chip is desaturated when disconnected (N8)",
    ).toHaveAttribute("data-desaturated", "true");
    await expect(
      onlineFacet,
      "the desaturated chip resolves to the neutral kind (N8)",
    ).toHaveAttribute("data-kind", "neutral");

    // The Save view button is also disabled.
    await expect(
      page.locator("[data-testid='save-view']"),
      "Save view disables when disconnected",
    ).toBeDisabled();
  });

  test("(N9) Artifacts subtitle reads 'no Runtime attached' when disconnected", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("artifacts");

    // The page subtitle (inside PageHeader) carries the
    // disconnected-aware copy — no synthetic "0 artifacts" claim.
    await expect(
      page.locator("[data-testid='artifacts-page']"),
      "the Artifacts page root renders",
    ).toBeVisible();
    await expect(
      page.locator("[data-testid='artifacts-page']"),
      "the subtitle reads 'no Runtime attached' when disconnected (N9)",
    ).toContainText("no Runtime attached");

    // Upload + Export are also disabled.
    await expect(
      page.locator("[data-testid='upload-artifact']").first(),
      "Upload artifact disables when disconnected",
    ).toBeDisabled();
  });

  test("(N2) every page renders exactly ONE ConnectionFooter", async ({
    page,
    runtime,
    helpers,
  }) => {
    // Walk the catalog of top-level pages and assert each lands with
    // exactly one ConnectionFooter (rendered by the app shell). The
    // pre-83s pages duplicated the footer via per-page imports.
    await helpers.seedAuth(runtime.token);

    for (const slug of [
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
    ] as const) {
      await helpers.gotoPage(slug);
      await expect(
        page.locator("[data-testid='connection-footer']"),
        `${slug}: exactly one ConnectionFooter (N2)`,
      ).toHaveCount(1);
    }
  });

  test("(N7) the saved-view button label is 'Save view' on every page", async ({
    page,
    runtime,
    helpers,
  }) => {
    // The label drift across pages — "Save filter" / "Save snapshot" /
    // "Save preset" / bare "Save" — is collapsed onto one verb. Walk
    // the saved-view sites and assert the literal button text.
    await helpers.seedAuth(runtime.token);

    const sites: Array<{ slug: Parameters<typeof helpers.gotoPage>[0]; testid: string }> = [
      { slug: "overview", testid: "overview-save-view" },
      { slug: "live-runtime", testid: "live-runtime-save-view" },
      { slug: "sessions", testid: "sessions-save-view" },
      { slug: "tasks", testid: "tasks-save-filter" },
      { slug: "agents", testid: "agents-save-view" },
      { slug: "tools", testid: "tools-save-filter" },
      { slug: "events", testid: "save-view" },
      { slug: "background-jobs", testid: "bg-save-filter" },
      { slug: "flows", testid: "flows-save-view" },
      { slug: "memory", testid: "memory-save-view" },
      { slug: "artifacts", testid: "save-view" },
      { slug: "mcp-connections", testid: "save-view" },
    ];

    for (const { slug, testid } of sites) {
      await helpers.gotoPage(slug);
      const btn = page.locator(`[data-testid='${testid}']`);
      await expect(
        btn,
        `${slug}: the saved-view button renders`,
      ).toBeVisible();
      await expect(
        btn,
        `${slug}: the saved-view button label is 'Save view' (N7)`,
      ).toHaveText(/Save view/);
    }
  });
});
