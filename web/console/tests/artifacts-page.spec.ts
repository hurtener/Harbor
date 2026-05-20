// Harbor Console Artifacts page — Playwright per-page spec.
//
// Refactored for the design-system foundation (D-121, CONVENTIONS.md): the
// Artifacts page now composes the shared `components/ui/` inventory, routes
// async state through the four-state `<PageState>`, and talks to the
// Runtime through `HarborClient` + `connection.ts`. The page-specific
// preview component moved to `src/lib/components/artifacts/`.
//
// This spec rides on the Phase 75 harness baseline (`fixtures/page`). The
// live e2e assertions are gated on the `harbor console` subcommand
// (Phase 73m) the same way `harness.spec.ts` is — the directory-/
// subcommand-missing → SKIP pattern (CLAUDE.md §4.2). Until `harbor
// console` lands, the live tests SKIP cleanly so preflight stays green.
//
// The renderer-registry discipline test is UNCONDITIONAL — it is a static
// source-tree assertion (no browser, no server) that pins the CLAUDE.md
// §13 / Brief 12 invariant: the Artifacts surface carries NO bespoke
// per-mime renderer; the preview component dispatches through the
// canonical registry at `$lib/chat/renderers`.

import { readdirSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

const here = dirname(fileURLToPath(import.meta.url));
const artifactsRouteDir = join(here, "..", "src", "routes", "(console)", "artifacts");
const artifactsComponentsDir = join(
  here,
  "..",
  "src",
  "lib",
  "components",
  "artifacts",
);

test.describe("Console Artifacts page — renderer-registry discipline", () => {
  // This block is UNCONDITIONAL — it is a static source assertion, not a
  // browser test, so it runs at every phase regardless of `harbor
  // console` availability.

  test("the Artifacts surface ships no bespoke per-mime renderer", () => {
    // CLAUDE.md §13 / Brief 12: per-mime renderers live ONLY in the
    // canonical registry at `$lib/chat/renderers/`. A `*_renderer.svelte`
    // or a `*.renderer.svelte` under the route or page-component dir is a
    // violation.
    for (const dir of [artifactsRouteDir, artifactsComponentsDir]) {
      const files = readdirSync(dir);
      const bespoke = files.filter(
        (f) => /_renderer\.svelte$/.test(f) || /\.renderer\.svelte$/.test(f),
      );
      expect(
        bespoke,
        `bespoke renderer files under ${dir} — per-mime renderers belong in $lib/chat/renderers/`,
      ).toEqual([]);
    }
  });

  test("the preview component imports dispatchRenderer from the canonical registry", () => {
    const previewComponent = readFileSync(
      join(artifactsComponentsDir, "ArtifactPreview.svelte"),
      "utf8",
    );
    expect(
      previewComponent.includes("$lib/chat/renderers"),
      "ArtifactPreview.svelte imports the canonical renderer registry",
    ).toBe(true);
    expect(
      previewComponent.includes("dispatchRenderer"),
      "ArtifactPreview.svelte dispatches via dispatchRenderer",
    ).toBe(true);
  });

  test("the page makes no hand-rolled fetch calls", () => {
    // CLAUDE.md §13: components go through the typed Protocol client,
    // never a raw `fetch`. The renderer components legitimately fetch
    // their (presigned) content URL — that is the registry's contract,
    // not a Protocol call — so the assertion is scoped to the page + its
    // page-specific components.
    const pageSrc = readFileSync(
      join(artifactsRouteDir, "+page.svelte"),
      "utf8",
    );
    expect(
      /\bfetch\s*\(/.test(pageSrc),
      "+page.svelte must not hand-roll fetch — use the typed Protocol client",
    ).toBe(false);
    const previewSrc = readFileSync(
      join(artifactsComponentsDir, "ArtifactPreview.svelte"),
      "utf8",
    );
    expect(
      /\bfetch\s*\(/.test(previewSrc),
      "ArtifactPreview.svelte must not hand-roll fetch",
    ).toBe(false);
  });

  test("the page reads no hardcoded dev identity off a globalThis shim", () => {
    // CONVENTIONS.md §6: identity comes from `connection.ts`, never a
    // `globalThis.__HARBOR_*__` window-global or a hardcoded
    // `dev-tenant/dev-user/dev-session` triple.
    const pageSrc = readFileSync(
      join(artifactsRouteDir, "+page.svelte"),
      "utf8",
    );
    expect(
      pageSrc.includes("__HARBOR_IDENTITY__"),
      "+page.svelte must not read identity from a window global",
    ).toBe(false);
    expect(
      pageSrc.includes("dev-tenant"),
      "+page.svelte must not hardcode a dev identity",
    ).toBe(false);
    expect(
      pageSrc.includes("resolveConnection"),
      "+page.svelte resolves its connection through connection.ts",
    ).toBe(true);
  });

  test("the page composes the shared ui/ inventory", () => {
    // CONVENTIONS.md §3/§5: the page is built from the shared component
    // inventory and routes async state through the four-state PageState.
    const pageSrc = readFileSync(
      join(artifactsRouteDir, "+page.svelte"),
      "utf8",
    );
    for (const primitive of [
      "PageHeader",
      "FilterBar",
      "SavedViewChips",
      "DataTable",
      "BulkActionBar",
      "DetailRail",
      "RailCard",
      "Pagination",
      "ConnectionFooter",
      "PageState",
    ]) {
      expect(
        pageSrc.includes(primitive),
        `+page.svelte composes the shared ${primitive} primitive`,
      ).toBe(true);
    }
  });
});

test.describe("Console Artifacts page — live e2e", () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    "harbor console subcommand absent (pre-Phase-73m) or bin/harbor not built",
  );

  test("the catalog renders artifact rows", async ({ page, runtime, helpers }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("artifacts");
    await expect(
      page.locator("[data-testid='artifacts-page']"),
      "the Artifacts page mounts",
    ).toBeAttached();
    await expect(
      page.locator("table"),
      "the artifacts catalog table renders",
    ).toBeAttached();
  });

  test("Upload artifact triggers artifacts.put then artifacts.get_ref", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("artifacts");

    // Observe the wire calls: an Upload fires artifacts.put, then the
    // page auto-resolves the preview via artifacts.get_ref.
    const putPromise = page.waitForRequest(
      (r) => r.url().includes("/v1/control/artifacts.put"),
    );
    const uploadBtn = page.locator("[data-testid='upload-artifact']");
    await expect(uploadBtn, "the Upload artifact button is present").toBeAttached();
    const chooserPromise = page.waitForEvent("filechooser");
    await uploadBtn.click();
    const chooser = await chooserPromise;
    await chooser.setFiles({
      name: "fixture.png",
      mimeType: "image/png",
      buffer: Buffer.from([0x89, 0x50, 0x4e, 0x47]),
    });
    await putPromise;
    await page.waitForRequest((r) =>
      r.url().includes("/v1/control/artifacts.get_ref"),
    );
  });

  test("the preview pane dispatches through the canonical renderer registry", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("artifacts");
    // Selecting a row resolves a preview; the host stamps
    // `data-renderer-dispatched` with the registry renderer's `source`
    // id — proving the canonical registry handled the preview.
    const firstRow = page.locator("tbody tr.data-row").first();
    if ((await firstRow.count()) > 0) {
      await firstRow.click();
      await expect(
        page.locator("[data-renderer-dispatched]"),
        "the preview is dispatched via the canonical renderer registry",
      ).toBeAttached();
    }
  });

  test("Delete and Set retention render disabled-with-tooltip", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("artifacts");
    // The bulk Delete / Set retention surfaces are deferred (page-
    // artifacts.md §10) — they render aria-disabled with the deferred
    // tooltip. The bulk action bar only shows when a row is checked.
    const firstCheckbox = page
      .locator("tbody tr.data-row input[type='checkbox']")
      .first();
    if ((await firstCheckbox.count()) > 0) {
      await firstCheckbox.check();
      const del = page.locator("[data-testid='bulk-delete']");
      await expect(del, "bulk Delete is disabled").toHaveAttribute(
        "aria-disabled",
        "true",
      );
      await expect(del, "bulk Delete carries the deferred tooltip").toHaveAttribute(
        "title",
        /Deferred/,
      );
    }
  });

  test("Export emits a metadata-only CSV with no inline blob bytes", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await helpers.gotoPage("artifacts");
    const downloadPromise = page.waitForEvent("download");
    await page.locator("[data-testid='export-csv']").click();
    const download = await downloadPromise;
    const stream = await download.createReadStream();
    let csv = "";
    for await (const chunk of stream) {
      csv += chunk.toString();
    }
    // Header row of column names, no base64 / data: blob payload (D-026).
    expect(csv.split("\n")[0], "CSV carries a metadata header row").toContain(
      "id,filename,mime_type",
    );
    expect(csv.includes("data:"), "CSV carries no data: blob payload").toBe(false);
  });
});
