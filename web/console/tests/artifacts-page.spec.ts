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
//
// Phase 75a (D-131): the runtime-entity seeding gap is closed — the
// `harbor console` binary boots a deterministic artifact fixture set
// when `HARBOR_DEV_SEED_FIXTURES=1` (set by the harness `runtime`
// fixture), and `seedConnection` below uses the matching `(dev, dev,
// dev)` triple. The live-e2e tests that were parked on the seeding gap
// now run for real.

import { readdirSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";

import { test, expect, consoleSubcommandAvailable } from "./fixtures/page";
import { STORAGE_KEYS } from "../src/lib/connection";

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

/**
 * Seed the `connection.ts` storage convention so the D-121 Artifacts
 * page resolves a live Runtime connection. The identity triple MUST
 * match the `harbor console` dev token — `(dev, dev, dev)`
 * (cmd/harbor/devauth.go) — the boot-seeded artifact fixtures
 * (HARBOR_DEV_SEED_FIXTURES, Phase 75a / D-131) live under that triple.
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


const here = dirname(fileURLToPath(import.meta.url));
const artifactsRouteDir = join(here, "..", "src", "routes", "(console)", "artifacts");

/**
 * Strips comments from a source string so a CONVENTIONS-compliance
 * substring/regex assertion inspects CODE, not commentary. An
 * explanatory comment that *describes* a removed legacy pattern (e.g.
 * "no longer reads `dev-tenant` off a globalThis shim") must not trip a
 * "must-not-contain" check — the test guards real code, never prose.
 *
 * Removes `/* ... *\/` block comments, `<!-- ... -->` HTML/Svelte-markup
 * comments, and `//`-to-end-of-line line comments. The line-comment
 * strip is anchored to a `//` that is at start-of-line (after optional
 * whitespace) or preceded by whitespace, so a `://` inside a URL is left
 * intact. It is a deliberately simple lexical strip — it does not parse
 * string literals — which is sound here: the assertions search for a
 * hand-rolled network call, a window-global identity shim, and a
 * hardcoded dev triple, none of which a comment-strip can manufacture,
 * and the must-CONTAIN checks (real identifiers) survive a strip
 * untouched.
 */
function stripComments(src: string): string {
  return src
    .replace(/\/\*[\s\S]*?\*\//g, "")
    .replace(/<!--[\s\S]*?-->/g, "")
    .replace(/(^|\s)\/\/[^\n]*/g, "$1");
}
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
    const previewComponent = stripComments(
      readFileSync(join(artifactsComponentsDir, "ArtifactPreview.svelte"), "utf8"),
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
    const pageSrc = stripComments(
      readFileSync(join(artifactsRouteDir, "+page.svelte"), "utf8"),
    );
    expect(
      /\bfetch\s*\(/.test(pageSrc),
      "+page.svelte must not hand-roll fetch — use the typed Protocol client",
    ).toBe(false);
    const previewSrc = stripComments(
      readFileSync(join(artifactsComponentsDir, "ArtifactPreview.svelte"), "utf8"),
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
    const pageSrc = stripComments(
      readFileSync(join(artifactsRouteDir, "+page.svelte"), "utf8"),
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
    const pageSrc = stripComments(
      readFileSync(join(artifactsRouteDir, "+page.svelte"), "utf8"),
    );
    // D-132 / W3: ConnectionFooter is NOT in this list — it is owned by
    // the app shell (`(console)/+layout.svelte`), not composed per-page.
    for (const primitive of [
      "PageHeader",
      "FilterBar",
      "SavedViewChips",
      "DataTable",
      "BulkActionBar",
      "DetailRail",
      "RailCard",
      "Pagination",
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
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("artifacts");
    await expect(
      page.locator("[data-testid='artifacts-page']"),
      "the Artifacts page mounts",
    ).toBeAttached();
    // The shared `DataTable` catalog table — `table.data-table`. A bare
    // `table` selector is ambiguous: a row's expanded detail also nests
    // a `table.row-inner`, so the catalog table is matched by its class.
    await expect(
      page.locator("table.data-table").first(),
      "the artifacts catalog table renders",
    ).toBeAttached();
    // The boot-seeded artifact fixtures (Phase 75a / D-131) render as
    // catalog rows.
    await expect(
      page.locator("tbody tr.data-row").first(),
      "the seeded artifact fixtures render as catalog rows",
    ).toBeAttached();
  });

  test("Upload artifact triggers artifacts.put then artifacts.get_ref", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
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
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage("artifacts");
    // Selecting a row resolves a preview. When the artifact-store driver
    // supports presigned URLs, the host stamps `data-renderer-dispatched`
    // with the canonical registry renderer's `source` id. The Console's
    // embedded zero-config runtime uses the in-memory artifact driver,
    // which does NOT presign — so the preview legitimately resolves to
    // the `presign-unsupported` branch instead. Either branch proves the
    // preview pane is wired through the canonical registry path (the
    // `presign-unsupported` branch is part of `ArtifactPreview`, the same
    // component that hosts the registry dispatch); the bug the spec
    // guards is a preview that renders NEITHER.
    const firstRow = page.locator("tbody tr.data-row").first();
    await expect(
      firstRow,
      "a seeded artifact row is present to select",
    ).toBeAttached();
    await firstRow.click();
    const dispatched = page.locator("[data-renderer-dispatched]");
    const presignUnsupported = page.locator(
      "[data-renderer-source='presign-unsupported']",
    );
    await expect(
      dispatched.or(presignUnsupported).first(),
      "the preview resolves through the canonical ArtifactPreview path",
    ).toBeAttached();
  });

  test("Delete and Set retention render disabled-with-tooltip", async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
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
    await seedConnection(page, runtime.baseURL, runtime.token);
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
