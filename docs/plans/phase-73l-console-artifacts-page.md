# Phase 73l — Console Artifacts page

## Summary

Phase 73l ships the Console **Artifacts** page — the operator's catalog
and preview surface over Harbor's content-addressed artifact store.
The page lands the Wave-13 page UI plus the `artifacts.list` filter
extensions (mime / source / size / task_id), the `artifacts.put`
upload pipeline that Brief 11 §PG-2 requires, and the `PresignGet`
resolver call site that materialises read-side presigned URLs through
the Protocol — and it **ships the canonical renderer registry SKELETON**
at `web/console/src/lib/chat/renderers/` (dispatch table + mime
renderers — markdown, code, image, pdf, audio, json). Phase 73l Stage
2.1 is the FIRST in-staging consumer (the introducer); Phase 73n Stage
2.3 Playground is the second consumer and extends the same directory
with chat-bubble / tool-call / diff renderers under the same dispatch
contract. All mutation
surfaces (Delete / Set retention) are rendered disabled-with-tooltip
per the spec's §10 deferred list.

## RFC anchor

- RFC §5.2
- RFC §6.10
- RFC §7

## Briefs informing this phase

- brief 11
- brief 12

## Brief findings incorporated

- **brief 11 §"Artifacts view"** — the page is "Filter by mime type,
  size, identity, session, source-task, time. Per-row: filename, size,
  age, source-task, content-hash. Click → preview (per-mime-type
  renderer). Download / share / delete (admin-only). 'Where used' →
  which sessions / tasks reference this artifact." This phase ships
  the filter / row / preview / Download / Share surface from V1; the
  Delete + per-artifact mutation surfaces ship disabled-with-tooltip
  per `page-artifacts.md` §10.
- **brief 11 §PG-2 (File upload — multimodal input)** — "Files become
  `ArtifactRef`s in the session's artifact store (Phases 17–19).
  LLM-edge translation (Phase 33 / D-021) maps `ArtifactRef`s to
  multimodal `ContentPart`s for the LLM. Per-attachment audit-redaction
  pass before send (Phase 3) — even Playground inputs are redacted into
  events." This phase ships the `artifacts.put` Protocol method that
  the Console's Upload button consumes; the Playground (phase 73n)
  is the upload pipeline's second consumer.
- **brief 11 §PG-4 (Rich output rendering)** — "Markdown, code blocks,
  JSON tree, CSV table, citations, ArtifactRef previews." This phase
  **ships the canonical renderer registry SKELETON** (dispatch table +
  the mime renderers Artifacts needs: markdown, code, image, pdf,
  audio, json) at `web/console/src/lib/chat/renderers/` per D-062.
  Phase 73l is the FIRST in-staging consumer (Stage 2.1) and therefore
  the introducer. Phase 73n Playground (Stage 2.3) is the second
  consumer and EXTENDS the same registry with chat-bubble-specific
  renderers (tool-call trace cards, diff-view cards, artifact-reference
  cards) — those additions live under the same
  `web/console/src/lib/chat/renderers/` path and reuse the dispatch
  contract this phase establishes.
- **brief 12 §"The shared chat / playground library"** — "The MCP-Apps
  renderer registry lives at `web/console/src/lib/chat/renderers/`."
  This phase establishes that location with the dispatch contract
  plus mime renderers; future renderer additions (chat-bubble renderers
  in 73n, post-V1 new mime renderers) land in the same module under
  the same dispatch contract.
- **brief 11 §CC-4 (search)** — "moderate-cardinality search runs as a
  Console-side index over `artifacts.list` rows; high-cardinality
  search lands as `search.artifacts` (Wave 13 to decide)." This phase
  ships the Console-side index for V1; `search.artifacts` is deferred
  to a follow-up (the spec §3 marks the runtime-side path as `[shipped]`
  only because Wave 13 decomposition deferred the high-cardinality case
  — see §11 below).
- **brief 11 §"Open architectural questions" — Console DB scope** —
  Saved-view chips, the Save (pin) action, sort prefs, and the bulk
  Download (zip) flow are Console-local per D-061; this phase persists
  them in `web/console/src/lib/db/` consuming the schema 72h
  ships (`saved_views`, `saved_filters`). No artifact mutation lands
  in the Console DB.

## Findings I'm departing from (if any)

None. The page-artifacts.md spec §12 captures every binding refinement
from the canonical 2026-05-18 mockup; this phase implements that spec
verbatim. The decomposition's row 73l in `docs/plans/wave-13-decomposition.md`
§5 is followed.

## Goals

- Land the `Console Artifacts` page at `web/console/src/routes/console/artifacts/+page.svelte`
  with: virtualised artifacts table, faceted filter chips (MIME /
  Source / Size / Tenant / Session / Task / Created / More filters),
  saved-view chip row (Console-local), Upload artifact button,
  Export ▾ (metadata-only CSV), selected-artifact right rail
  (Preview / Actions / Metadata / Tags), bulk-action toolbar with
  Download (zip) and Copy refs enabled and Delete / Set retention
  disabled-with-tooltip, footer block per spec §12.
- Land the `artifacts.list` filter-shape extensions on the Protocol —
  `mime_type`, `source` (`tool` | `planner` | `user-upload` |
  `system`), `size_range` (min / max bytes), `task_id`, `created_range`
  (after / before timestamps), `tags`, `tenant_id` (gated by
  `auth.ScopeAdmin`). The pre-existing `artifacts.list` method signature
  (Phase 73 acceptance — currently `Pending`; Phase 73l ships the
  filter extensions as part of the same wire-type as Phase 73 lands
  the base shape) is extended via additive optional fields.
- Land the `artifacts.put` Protocol method per Brief 11 §PG-2. Accepts
  `(scope, bytes, opts)` where `opts` carries `mime_type`, `filename`,
  `namespace`, `source`, `tags`. Returns the canonical `ArtifactRef`.
  Identity is mandatory at the boundary; missing tenant/user/session
  returns `CodeIdentityRequired` (loud, never silent). Cross-tenant
  writes (a request whose payload scope.TenantID disagrees with the
  JWT's tenant claim, absent `ScopeAdmin`) return `CodeScopeMismatch`.
  Body size is bounded; `HeavyOutputThreshold` is the floor for the
  `oversize → 413` rejection path (configurable via
  `config.ProtocolConfig.MaxRequestBytes`).
- Land the `PresignGet` resolver call site as a Protocol method that
  invokes `artifacts.Presigner.PresignGet(ctx, scope, id, expiry)` per
  D-022 / D-026: heavy bytes never travel inline through the Protocol;
  the Preview pane, Download button, Share button, and bulk Download
  (zip) ALL go through this single resolver. Drivers that do not
  implement `Presigner` (in-mem / fs / sqlite-blob / postgres-blob)
  return `CodePresignUnsupported`; only Phase 19's S3 driver implements
  the capability, so the resolver fails loudly with a typed error on
  non-S3 backends (per the Brief 05 §3 + D-022 fail-loud posture).
- **Ship the canonical renderer registry SKELETON** at
  `web/console/src/lib/chat/renderers/` — dispatch table
  (`index.ts`) plus the mime renderers Artifacts needs (markdown,
  code, image, pdf, audio, json). Phase 73l Stage 2.1 is the FIRST
  in-staging consumer and therefore the introducer; Phase 73n Stage
  2.3 Playground extends the SAME directory with chat-bubble /
  tool-call / diff renderers under the SAME dispatch contract. The
  Preview pane in this phase wires through the dispatch table; the
  file `web/console/src/routes/console/artifacts/preview_pane.svelte`
  imports `dispatchRenderer` from the registry and dispatches by
  mime type. NO bespoke per-mime renderer lives in
  `web/console/src/routes/console/artifacts/`.
- Land `web/console/tests/artifacts-page.spec.ts` (Playwright)
  covering: catalog rows render, Upload artifact triggers `artifacts.put`
  followed by `PresignGet` to render the just-uploaded image, Preview
  pane uses the canonical renderer registry (a regression assert
  imports the registry path and verifies no other renderer module
  exists under `routes/console/artifacts/`).
- Land `test/integration/artifacts_page_test.go` — Go-side integration
  test exercising the Protocol round-trip across the in-mem, sqlite,
  and fs artifact drivers. Real `ArtifactStore` + real Protocol
  transport + `PresignGet` resolver round-trip (against a stub
  presigner registered into the in-mem driver only for this test,
  emitting deterministic URLs — clearly marked test-only per §13's
  test-stubs-as-defaults posture; production code never resolves
  PresignGet on non-S3 drivers). Identity propagation asserted on
  every call; cross-tenant `artifacts.list` rejection asserted; under
  `-race`.
- D-025 concurrent-reuse coverage: N≥100 concurrent `artifacts.list`
  calls against a single shared protocol handler + ArtifactStore under
  `-race`, asserting no data race, no goroutine leak (baseline
  goroutine count restored), no cross-tenant bleed.

## Non-goals

- `artifacts.delete` UI (the row-action menu and bulk toolbar render
  Delete as **disabled-with-tooltip** "Deferred — Phase 73"). The
  Protocol-side `artifacts.delete` method ships in Phase 73; this
  phase merely consumes it in disabled state. Wiring the live Delete
  button is post-V1 per the spec's §10 "Out of V1 (deferred)" list.
- `artifacts.set_retention` UI (same posture).
- `search.artifacts` runtime-side high-cardinality search — Wave 13
  to decide later. This phase ships the Console-side index over the
  page's loaded `artifacts.list` rows only.
- Write-side presigned URLs (`PresignPut`) — the V1 attack-surface
  carve-out per the `Presigner` interface godoc and the spec §10
  "Out of V1 (deferred)" list.
- Cross-runtime aggregator — D-091 / D-061 post-V1.
- Per-artifact annotations (tag / note / star) — Console-local
  possible per D-061 but explicitly deferred to keep V1 lean per spec
  §10.
- Server-side artifact transformation (rename, attach metadata) —
  artifacts are immutable at V1.

## Console consistency

This is a Console page phase. It is **binding** on the shared Console
design-system foundation defined in `docs/design/console/CONVENTIONS.md`
(D-121 in `docs/decisions.md`). `CONVENTIONS.md` is the cross-cutting
authority for every Console page; a page PR that diverges from a convention
below is **rejected on sight**. The Artifacts page is a catalog surface with
a selected-artifact detail rail and preview pane; it mounts inside the shared
app shell and clears the §5 depth bar like every other page.

The page MUST:

- **Route under `(console)/`.** The page lives at
  `web/console/src/routes/(console)/artifacts/` and is served at `/artifacts`
  with **no `/console/` URL prefix** (the `(console)` route group is a
  layout-grouping device and does not appear in the URL). Detail views live at
  `(console)/artifacts/[id]/` and are served at `/artifacts/<id>`. All
  inter-page links use the unprefixed form; a link to `/console/<anything>`
  is a bug.
- **Render inside the shared app shell.** The page renders as a child of
  `(console)/+layout.svelte` — the single app shell carrying the sidebar,
  breadcrumb, identity/connection indicator, and footer. It never ships a
  standalone layout.
- **Use the shared `components/ui/` inventory.** It composes the cross-page
  primitives in `web/console/src/lib/components/ui/` — `PageHeader`,
  `FilterBar`, `DataTable`, `DetailRail`/`RailCard`, `BulkActionBar`,
  `SavedViewChips`, `Pagination`, `StatusChip`, `ConnectionFooter`,
  `PageState`. It **never forks a primitive that already exists**;
  page-specific components go in `components/artifacts/`.
- **Route all async state through the four-state `<PageState>`.** Every async
  surface flows through `<PageState>`'s four mutually-exclusive states —
  Disconnected / Loading / Error / Empty. The Error state ships a working
  **Retry** that re-invokes the loader and suppresses any stale primary view;
  **Disconnected** ("no Runtime attached") is detected via `connection.ts`
  returning `null` and is **never conflated with Error**.
- **Clear the §5 depth bar.** The page is not "done" until it has all of:
  a `PageHeader`; a `FilterBar`; a primary `DataTable` or canvas; a
  `DetailRail` or a tabbed detail route; Console-DB-backed `SavedViewChips`;
  real `Pagination` (page / size / total, prev / next — not a fake "load
  more"); a `ConnectionFooter`; and the full four-state `PageState`.
- **Talk to the Runtime only through `HarborClient` + `connection.ts`.** All
  Protocol calls go through the single typed `HarborClient` (adding a
  namespace, never a new top-level client); the connection resolves through
  `web/console/src/lib/connection.ts`. **No `fetch` in `.svelte` files, no
  direct `localStorage` access, no hand-rolled per-page client.**
- **Introduce no raw token literals.** No raw color / spacing / type-scale
  literals in `.svelte` files — design tokens from `tokens.css` only
  (Stylelint enforces this; `npm run lint` fails CI on a violation).
- **Ship no stubbed action presented as done.** Every action either invokes
  the real Protocol method or renders **disabled-with-tooltip** explaining
  why. A button that fakes success with a feedback string is a §13-class
  silent-degradation violation.

See `docs/design/console/CONVENTIONS.md` §9 for the per-phase callout
contract and D-121 for the rationale.

## Acceptance criteria

- [ ] `internal/protocol/methods/methods.go` adds three constants:
      `MethodArtifactsList`, `MethodArtifactsPut`, `MethodArtifactsGetRef`.
      The map is extended; `IsValidMethod` returns true for all three;
      `Methods()` returns them in lexicographic order. (Note: the
      `MethodArtifactsList`, `MethodArtifactsGet`, `MethodArtifactsGetRef`,
      and `MethodArtifactsDelete` constants are added by Phase 73 — the
      parent — when both phases land in the same wave; if Phase 73 has
      not yet merged when this phase is implemented, Phase 73l adds
      the four constants and Phase 73 inherits them.) `MethodArtifactsPut`
      is owned by this phase regardless.
- [ ] `internal/protocol/types/artifacts.go` adds the wire types for
      the four artifacts methods: `ArtifactsListRequest`
      (carrying the extended filter — `MimeType []string`,
      `Source []ArtifactSource`, `SizeRange *SizeRange`, `TaskID string`,
      `CreatedRange *TimeRange`, `Tags []string`, `TenantID string`),
      `ArtifactsListResponse` (rows of `ArtifactRow` carrying
      `ArtifactRef` + `Tags []string` + `Driver string` + `CreatedAt time.Time`),
      `ArtifactsPutRequest` (`Scope`, `Bytes []byte`, `Opts`),
      `ArtifactsPutResponse` (`Ref ArtifactRef`),
      `ArtifactsGetRefRequest` (`Scope`, `ID`, `Expiry time.Duration`),
      `ArtifactsGetRefResponse` (`PresignedURL string`, `Ref ArtifactRef`).
      The `ArtifactSource` enum is `tool` | `planner` | `user_upload`
      | `system`; deserialisation of an unknown source returns
      `CodeInvalidArgument` loudly.
- [ ] `internal/protocol/errors/errors.go` (or the existing errors map)
      adds `CodePresignUnsupported` (driver does not implement
      `Presigner`). All other artifacts error codes are pre-existing.
- [ ] The `artifacts.list` server handler at
      `internal/protocol/handlers/artifacts.go` accepts the extended
      filter shape and projects to `artifacts.ArtifactStore.List`,
      applying the additional fields (`MimeType`, `Source`, `SizeRange`,
      `Tags`, `CreatedRange`) as a Go-side filter pass over the
      driver's returned slice (the V1 driver interface does not extend
      the filter signature — the projection is in the handler to keep
      driver conformance untouched). The handler validates identity
      via `identity.MustFrom(ctx)` and bumps `CodeIdentityRequired`
      loudly when any of tenant/user/session is empty. Cross-tenant
      `list` calls (`TenantID` in the request differs from the JWT's
      tenant claim) require `ScopeAdmin` per RFC §6.13; absent the
      scope, the handler returns `CodeScopeMismatch`.
- [ ] The `artifacts.put` server handler routes the request through
      `audit.Redactor` (D-007), then through `artifacts.ArtifactStore.PutBytes`
      with `PutOpts` derived from the request. Identity is mandatory;
      a request whose body scope disagrees with the JWT identity is
      rejected with `CodeScopeMismatch` (no silent rewrite — Identity
      is mandatory at this boundary). Body size > `MaxRequestBytes`
      returns `CodeRequestTooLarge`. The handler emits
      `audit.artifact_uploaded` per the bus contract.
- [ ] The `artifacts.get_ref` handler invokes `Presigner.PresignGet`
      on the underlying `ArtifactStore` via type-assertion. A driver
      that does NOT implement `Presigner` returns `CodePresignUnsupported`
      loudly; the Console renders the typed error as
      "Preview not available — driver does not support presigned URLs"
      and falls back to a Download link that proxies through the
      runtime (per the spec §7 unsupported-mime fallback). Identity
      is mandatory at this boundary; missing identity returns
      `CodeIdentityRequired`. Expiry is bounded `[1m, 7d]`; out-of-range
      expiries return `CodeInvalidArgument` loudly (matches the
      `Presigner` interface's fail-loud contract).
- [ ] The Console page `web/console/src/routes/console/artifacts/+page.svelte`
      renders the full §12 anatomy: filter chips row + saved-view chip
      row + Upload artifact button + Export ▾ + artifacts table
      (virtualised, columns per §12) + right rail (Preview / Actions /
      Metadata / Tags) + bulk-action toolbar (Download(zip) / Copy refs
      enabled; Delete / Set retention disabled-with-tooltip) + footer.
- [ ] The Console page imports the typed Protocol client from
      `web/console/src/lib/protocol.ts` — generated per D-093, never
      hand-edited. The page makes ZERO hand-rolled `fetch` calls
      (§13 forbidden practice).
- [ ] The Preview pane at
      `web/console/src/routes/console/artifacts/preview_pane.svelte`
      imports `dispatchRenderer` from
      `web/console/src/lib/chat/renderers/index.ts` and dispatches by
      mime type. The route directory contains NO bespoke per-mime
      `.svelte` renderer (Playwright spec asserts this).
- [ ] The Upload artifact button consumes `artifacts.put`, then
      auto-issues `artifacts.get_ref` for the just-uploaded artifact,
      then renders the Preview pane via the canonical renderer registry.
      File-size enforcement is browser-side (against the configured
      `MaxRequestBytes`) plus server-side (the handler enforces
      authoritatively); the browser-side check is a UX nicety only.
- [ ] The saved-view chips + the Save (pin) action + sort prefs +
      bulk Download (zip) ride on the Console DB (D-061) — schema is
      Phase 72h's, consumed here. Schema diffs are zero in this phase
      (72h ships the schema).
- [ ] Export ▾ emits **metadata-only** CSV. No blob bytes are
      inlined; the Playwright spec asserts the CSV mime contains
      `text/csv` and a header row of column names but no `b64` /
      `data:` payload.
- [ ] Cross-tenant `artifacts.list` is rejected without `ScopeAdmin`
      (integration test asserts; Playwright spec asserts the error
      banner renders for a non-admin operator).
- [ ] `web/console/tests/artifacts-page.spec.ts` covers: catalog
      renders rows (using a fixture that pre-seeds 3 artifacts);
      Upload artifact triggers `artifacts.put` then `PresignGet` to
      render an inline preview; Preview pane uses the canonical
      renderer registry (asserts the registry import path is reached
      by walking the network requests + DOM); Delete / Set retention
      render as `aria-disabled="true"` with the deferred tooltip text.
- [ ] `test/integration/artifacts_page_test.go` is added under
      `test/integration/`, uses the real in-mem + sqlite + fs
      `ArtifactStore` drivers, the real Protocol transport
      (`internal/protocol/transports/...`), and asserts: (a) the
      identity quadruple propagates through every call; (b) cross-tenant
      `artifacts.list` is rejected; (c) `artifacts.put` round-trips
      to `artifacts.get_ref` against the in-mem driver (the test
      registers a test-only presigner per the integration-test
      stub-presigner carve-out documented in §11 below); (d) under
      `-race`.
- [ ] D-025 concurrent-reuse test
      `TestArtifactsHandler_ConcurrentReuse_NoCrossTalk` lives in
      `internal/protocol/handlers/artifacts_concurrent_test.go`:
      N=100 concurrent `artifacts.list` calls against a single shared
      handler + shared `ArtifactStore`, each goroutine using a distinct
      identity quadruple; the test asserts every returned list carries
      only the caller's-tenant rows, baseline goroutine count is
      restored at teardown, and `-race` is clean.
- [ ] `scripts/smoke/phase-73l.sh` ships with `# PREFLIGHT_REQUIRES: live-server`
      and covers: `artifacts.list` returns 200 with an empty array
      when called with an empty store; `artifacts.put` returns 201
      with a canonical ref ID; `artifacts.get_ref` returns 200 with
      a presigned URL (or `CodePresignUnsupported` on the FS dev
      driver — SKIPped per the 404/405/501 convention); cross-tenant
      `artifacts.list` without `ScopeAdmin` returns 403.
- [ ] Coverage on `internal/protocol/handlers/artifacts*.go` ≥ 80%.
- [ ] Coverage on the Console route under
      `web/console/src/routes/console/artifacts/` is gated by
      `svelte-check --fail-on-warnings` + Playwright pass (Svelte
      surfaces don't carry Go-style coverage; lint and e2e are the
      gates).
- [ ] README Status row Phase 73l → Shipped + a one-line pointer in
      the Console section.
- [ ] `docs/plans/README.md` Phase 73l row added under the 73-suffix
      block with `Pending` → `Shipped` flipped in the same PR; the
      master plan's detail-block entry for Phase 73 carries a
      cross-reference to 73l for the page-side consumer.
- [ ] Glossary entries added for the four new surface terms
      (see §"Glossary additions").

## Files added or changed

```text
internal/protocol/methods/methods.go       # add MethodArtifactsPut (+List/GetRef if not yet added by 73)
internal/protocol/types/artifacts.go       # NEW — wire types for the four methods
internal/protocol/errors/errors.go         # add CodePresignUnsupported
internal/protocol/handlers/artifacts.go    # NEW — handler bodies for list / put / get_ref
internal/protocol/handlers/artifacts_test.go
internal/protocol/handlers/artifacts_concurrent_test.go  # D-025 N=100

web/console/                              # FIRST creation of this tree IF a prior phase has not landed it
  src/
    routes/console/artifacts/
      +page.svelte
      filter_bar.svelte
      artifacts_table.svelte
      right_rail.svelte
      preview_pane.svelte
      bulk_toolbar.svelte
    lib/protocol.ts                        # generated; this phase regenerates it
    lib/chat/renderers/                    # **INTRODUCED here** (Stage 2.1, first consumer): index.ts dispatch table + markdown.ts + code.ts + image.ts + pdf.ts + audio.ts + json.ts. Phase 73n Stage 2.3 extends this directory with chat-bubble / tool-call / diff renderers under the same dispatch contract.
    lib/db/                                 # consumed (72h introduces)
  tests/artifacts-page.spec.ts             # NEW

test/integration/artifacts_page_test.go    # NEW

docs/plans/phase-73l-console-artifacts-page.md  # this file
docs/plans/README.md                        # row addition + Phase 73 cross-ref
docs/glossary.md                            # four new terms
README.md                                   # Status row + Console pointer
scripts/smoke/phase-73l.sh                  # NEW (chmod +x)
```

The phase does **not** introduce any new top-level directory beyond
`web/console/` (which CLAUDE.md §4.5 already documents). The
`test/integration/` placement matches CLAUDE.md §17.2.

## Public API surface

```go
// internal/protocol/methods/methods.go
const (
    MethodArtifactsList   Method = "artifacts.list"
    MethodArtifactsPut    Method = "artifacts.put"
    MethodArtifactsGetRef Method = "artifacts.get_ref"
    // MethodArtifactsGet + MethodArtifactsDelete owned by Phase 73.
)

// internal/protocol/types/artifacts.go
type ArtifactSource string
const (
    ArtifactSourceTool       ArtifactSource = "tool"
    ArtifactSourcePlanner    ArtifactSource = "planner"
    ArtifactSourceUserUpload ArtifactSource = "user_upload"
    ArtifactSourceSystem     ArtifactSource = "system"
)

type SizeRange struct {
    MinBytes *int64
    MaxBytes *int64
}

type TimeRange struct {
    After  *time.Time
    Before *time.Time
}

type ArtifactsListRequest struct {
    Scope        ArtifactScope     // (Tenant/User/Session/Task) — tenant subset gated by ScopeAdmin
    MimeType     []string          // OR-set; empty == wildcard
    Source       []ArtifactSource  // OR-set; empty == wildcard
    SizeRange    *SizeRange        // optional
    CreatedRange *TimeRange        // optional
    Tags         []string          // OR-set
    Limit        int               // page size (default 100, max 1000)
    Cursor       string            // forward-only opaque cursor
}

type ArtifactRow struct {
    Ref       ArtifactRef
    Tags      []string
    Driver    string             // "inmem" | "fs" | "sqlite" | "postgres" | "s3"
    CreatedAt time.Time
}

type ArtifactsListResponse struct {
    Rows       []ArtifactRow
    NextCursor string
}

type ArtifactsPutRequest struct {
    Scope ArtifactScope
    Bytes []byte
    Opts  ArtifactsPutOpts
}

type ArtifactsPutOpts struct {
    MimeType  string
    Filename  string
    Namespace string
    Source    ArtifactSource     // defaults to "user_upload"
    Tags      []string
}

type ArtifactsPutResponse struct {
    Ref ArtifactRef
}

type ArtifactsGetRefRequest struct {
    Scope  ArtifactScope
    ID     string
    Expiry time.Duration         // bounded [1m, 7d]
}

type ArtifactsGetRefResponse struct {
    Ref          ArtifactRef
    PresignedURL string
    ExpiresAt    time.Time
}
```

```ts
// web/console/src/lib/protocol.ts (generated)
//   regenerated to expose:
//     - artifactsList(req: ArtifactsListRequest): Promise<ArtifactsListResponse>
//     - artifactsPut(req: ArtifactsPutRequest): Promise<ArtifactsPutResponse>
//     - artifactsGetRef(req: ArtifactsGetRefRequest): Promise<ArtifactsGetRefResponse>
```

```svelte
<!-- web/console/src/routes/console/artifacts/preview_pane.svelte -->
<script lang="ts">
  import { dispatchRenderer } from '$lib/chat/renderers';
  // ...
</script>
```

## Test plan

- **Unit:**
  - `TestArtifactsListHandler_FilterShape_Extends` — pass each new
    filter field; assert the handler projects correctly over the
    driver's returned slice.
  - `TestArtifactsListHandler_RejectsCrossTenant_WithoutAdmin` —
    JWT identity tenant=A, request scope.TenantID=B, no `ScopeAdmin`:
    handler returns `CodeScopeMismatch`.
  - `TestArtifactsListHandler_AllowsCrossTenant_WithAdmin` — same as
    above with `ScopeAdmin`; handler returns the cross-tenant rows.
  - `TestArtifactsPutHandler_RoundTrip_InMem` — put then list the
    same scope; the new ref appears with the expected MimeType /
    SizeBytes / Source.
  - `TestArtifactsPutHandler_RejectsScopeMismatch` — body
    scope.TenantID differs from JWT tenant; handler returns
    `CodeScopeMismatch`.
  - `TestArtifactsPutHandler_RejectsOversizeBody` — body length >
    `MaxRequestBytes` → `CodeRequestTooLarge`.
  - `TestArtifactsPutHandler_RoutesThroughAuditRedactor` — install a
    test redactor with a side-effect counter; assert the put body
    walked the redactor once before reaching the store.
  - `TestArtifactsPutHandler_EmitsAuditArtifactUploaded` — assert
    `audit.artifact_uploaded` event published on success.
  - `TestArtifactsGetRefHandler_ReturnsPresigned_S3LikeDriver` —
    register a test-only presigner on the in-mem driver (per the
    integration carve-out below); assert the presigned URL is
    returned.
  - `TestArtifactsGetRefHandler_ReturnsPresignUnsupported_FSDriver` —
    use the fs driver (no `Presigner` impl); handler returns
    `CodePresignUnsupported`.
  - `TestArtifactsGetRefHandler_RejectsOutOfRangeExpiry` — expiry =
    30s (below the 1m floor) or 14d (above the 7d ceiling); returns
    `CodeInvalidArgument`.
  - `TestArtifactsGetRefHandler_RejectsMissingIdentity` — empty
    tenant/user/session → `CodeIdentityRequired`.
- **Integration:** `test/integration/artifacts_page_test.go` —
  composes real `artifacts.ArtifactStore` (in-mem + sqlite + fs),
  real Protocol transport (`internal/protocol/transports/http`),
  real `audit.Redactor`, asserts identity propagation across every
  call, exercises a put → list → get_ref round-trip, and asserts
  cross-tenant rejection without `ScopeAdmin`. Run under `-race`
  with N=10 concurrent producer goroutines per CLAUDE.md §17.3.
- **Conformance:** N/A — this phase does not introduce a new driver;
  it consumes the existing `ArtifactStore` interface and its already-
  shipped conformance suite.
- **Concurrency / leak:** `TestArtifactsHandler_ConcurrentReuse_NoCrossTalk`
  — N=100 concurrent goroutines, each issuing `artifacts.list` against
  a single shared handler + `ArtifactStore`, each with a distinct
  identity quadruple. Asserts:
  - No data race (`-race` clean).
  - Each goroutine's response carries only its own tenant's rows
    (no cross-tenant bleed).
  - Cancelling one goroutine's `ctx` does not affect any other's
    (no cancellation cross-talk).
  - Baseline `runtime.NumGoroutine()` restored after all goroutines
    return (no goroutine leak).
- **Frontend (Playwright):** `web/console/tests/artifacts-page.spec.ts`
  exercises the page against the Phase 75 baseline harness:
  - Catalog renders rows from a fixture-seeded `artifacts.list`.
  - Upload artifact opens the file picker; selecting a fixture file
    fires `artifacts.put` (observed via the network recorder), then
    fires `artifacts.get_ref`, then renders the Preview pane.
  - Preview pane uses the canonical renderer registry — assert by
    walking the resolved module path of the renderer that handled
    the preview (the page DOM exposes a `data-renderer-source` attr
    set by the registry).
  - The `routes/console/artifacts/` directory contains NO bespoke
    per-mime `.svelte` renderer — assert by globbing the directory at
    test setup and failing if any `*_renderer.svelte` or similar
    file is found that doesn't import from
    `$lib/chat/renderers/`.
  - Delete row-action and bulk Delete / Set retention render
    `aria-disabled="true"` with the deferred tooltip.

## Smoke script additions

`scripts/smoke/phase-73l.sh` is `# PREFLIGHT_REQUIRES: live-server`
and runs the following assertions against the booted dev server:

- `artifacts.list` happy path: `protocol_call artifacts.list
  '{"scope":{...}}'` returns 200 with `.rows` array (empty on a
  fresh dev store).
- `artifacts.put` happy path: POST a small text artifact; assert
  201 + the returned `ref.id` matches the content-addressed pattern.
- `artifacts.get_ref` SKIP-or-OK: if the dev store's driver is fs
  (no `Presigner` impl), expect `CodePresignUnsupported` (SKIPped via
  the 404/405/501-shaped header for the dev path); if it's s3,
  expect 200 with a non-empty `presigned_url`.
- Cross-tenant rejection: with a non-admin dev token, `artifacts.list`
  against a different `tenant_id` returns 403.
- Identity-required path: an `artifacts.list` request with an empty
  scope returns 400 with `CodeIdentityRequired`.

## Coverage target

- `internal/protocol/handlers/artifacts*.go`: 80%.
- `internal/protocol/types/artifacts.go`: 80% (round-trip serialisation
  tests).
- `web/console/src/routes/console/artifacts/`: gated by `svelte-check
  --fail-on-warnings` + Playwright pass (no statement coverage gate
  per CLAUDE.md §4.5).

## Dependencies

- **Phase 73** (parent, currently `Pending`) — ships the base
  `artifacts.list`, `artifacts.get`, `artifacts.get_ref`,
  `artifacts.delete` Protocol surfaces. **This phase extends
  `artifacts.list`'s filter shape and adds `artifacts.put`.** Per
  the §13 primitive-with-consumer rule, the page IS the consumer of
  the extended filter and the new put method, landed in the same
  wave.
- **Phase 75** (Playwright harness baseline, Stage 1) — the
  Playwright runner this phase's spec rides on.
- **Phase 72** / **72a** — `events.subscribe` posture (for the live
  artifact updates the right rail observes via the bus, though the
  page does not subscribe directly in V1 — it polls `artifacts.list`
  on user-triggered refresh per the spec's "moderate-cardinality"
  posture).
- **Phase 73n** (Stage 2.3, AFTER this phase in staging) — extends the
  canonical renderer registry this phase introduces with chat-bubble
  / tool-call / diff renderers under the same dispatch contract. The
  Wave 13 decomposition stages 73n in 2.3 and 73l in 2.1, so 73l
  always lands first; this phase is the registry INTRODUCER per the
  encapsulate-first-extract-on-second-consumer pattern (Brief 12 §"The
  shared chat / playground library"). This phase ships the dispatch
  table (`index.ts`) plus mime renderers (markdown, code, image, pdf,
  audio, json); 73n adds the chat-bubble renderers on top without
  re-shipping the dispatcher.
- **Phase 19** — Phase 19's S3 driver is the only V1 driver
  implementing `Presigner`; this phase exercises the resolver against
  it in the integration test (and SKIPs against the fs / in-mem /
  sqlite / postgres drivers in the smoke).

## Risks / open questions

- **Risk: the renderer registry skeleton this phase ships could
  drift from the eventual 73n landing.** Mitigation: the skeleton
  ships with a stable dispatch table contract documented in
  `web/console/src/lib/chat/renderers/README.md` (one-pager); 73n
  extends, does not rewrite. The 73n phase plan owner is asked to
  confirm before commit.
- **Risk: extending `artifacts.list`'s filter shape after Phase 73
  ships could break wire compatibility.** Mitigation: every new
  field is optional and the deserialiser tolerates absence. The
  wire-shape conformance test in `internal/protocol/conformance/`
  covers the additive-only invariant.
- **Risk: the `MaxRequestBytes` Protocol-config field may not yet
  exist.** Inspection of `internal/config/config.go` shows
  `ProtocolConfig` does not yet carry it. This phase adds it with a
  documented default (4 MiB; aligned with the spec's "moderate-size
  upload" posture); operators tuning above need to update their
  config. The Phase 73 plan may have already booked the field — if so,
  this phase consumes the existing field; if not, this phase adds
  it.
- **Risk: PresignGet expiry behaviour on the FS / SQLite-blob /
  Postgres-blob drivers.** Resolved per Brief 05 §3 + D-022: those
  drivers do NOT implement `Presigner` and the handler returns
  `CodePresignUnsupported` loudly. The Console renders the typed
  error and falls back to a proxy Download link. The proxy Download
  path bypasses the resolver (it streams bytes through the runtime
  via `artifacts.get`); this is acceptable for V1 because dev /
  embedded deployments use FS and have low artifact volume. Production
  deployments use S3 and benefit from real presigned URLs.
- **Open question: should `Source` be a free-text field or a closed
  enum?** Resolved: closed enum (`tool` / `planner` / `user_upload`
  / `system`) per the spec §12 mockup-aligned refinements. Unknown
  values return `CodeInvalidArgument` (per §13 fail-loud). Free-text
  invites drift; the enum can be extended in a future RFC PR.
- **Open question: who owns the artifact `Tags []string` extension on
  `ArtifactRef`?** The `internal/artifacts/artifacts.go` `ArtifactRef`
  shape carries `Source map[string]any` but not `Tags`. Two options:
  (a) add `Tags []string` to `ArtifactRef` in this phase; (b) project
  tags only on the `ArtifactRow` Protocol-side type, sourcing from
  `Source["tags"].([]any)`. Recommendation: **(b)** — the Protocol
  row shape is independent of the storage shape, and existing
  artifacts (no Tags) deserialise cleanly. The decision is closed in
  this phase plan; a follow-up RFC could promote Tags to the storage
  shape if it warrants the depth.
- **Open question: integration-test presigner stub posture.** The
  in-mem driver has no `Presigner` impl. The integration test
  registers a test-only stub presigner that returns
  `http://test-presigner.invalid/<id>?expires=<unix>` so the resolver
  call site can be exercised end-to-end. **This stub lives in
  `*_test.go` files only** (test-only build tag is not strictly
  required because the symbol is package-private to the test file).
  The stub does NOT ship as a registered driver; it is constructed
  inline in the test. This satisfies §13's "test stubs as production
  defaults" amendment — the stub is never reachable from the
  production binary.

## Glossary additions

- **`artifacts.put`** — the Protocol method this phase introduces for
  the Console (and Playground) file-upload pipeline. Routes through
  `audit.Redactor` then `artifacts.ArtifactStore.PutBytes` with full
  identity validation. Identity is mandatory at the boundary; missing
  tenant/user/session returns `CodeIdentityRequired`. RFC §6.10,
  brief 11 §PG-2.
- **`artifacts.get_ref` (Protocol method)** — the read-side
  presigned-URL resolver on the Protocol surface. Invokes
  `artifacts.Presigner.PresignGet(ctx, scope, id, expiry)` via
  type-assertion on the underlying `ArtifactStore`. Drivers without
  `Presigner` return `CodePresignUnsupported` loudly — no silent
  fallback. The Console's Preview / Download / Share / bulk Download
  all go through this single resolver per D-022 / D-026. RFC §6.10.
- **`ArtifactRow`** — the Protocol-side row shape returned by
  `artifacts.list`. Wraps the canonical `ArtifactRef` with
  `Tags []string`, `Driver string`, and `CreatedAt time.Time` for
  catalog rendering. Distinct from `ArtifactRef` (the storage shape)
  to keep the Protocol's wire surface independent of storage internals.
- **`Canonical renderer registry`** — the shared dispatch table at
  `web/console/src/lib/chat/renderers/` that maps mime types to
  `.svelte` renderer components. First consumer: the Playground page
  (Phase 73n). Second consumer: the Artifacts preview pane (this
  phase). Bespoke per-mime renderers outside this registry are
  forbidden per Brief 12 §"shared chat / playground library" and
  §13.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
      (`internal/protocol/handlers/artifacts*.go` ≥ 80%)
- [ ] If multi-isolation paths changed: cross-session isolation test
      passes (covered by `TestArtifactsListHandler_RejectsCrossTenant_WithoutAdmin`
      + the integration test's cross-tenant rejection assertion).
- [ ] **If this phase builds a reusable artifact (handler): concurrent-reuse
      test passes** — `TestArtifactsHandler_ConcurrentReuse_NoCrossTalk`
      runs N=100 concurrent `artifacts.list` calls against a single
      shared handler + store under `-race`.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes
      a cross-subsystem seam: an integration test exists** —
      `test/integration/artifacts_page_test.go` wires real
      `ArtifactStore` (in-mem + sqlite + fs) + real Protocol transport
      + identity propagation + cross-tenant failure mode under `-race`.
- [ ] If new vocabulary: glossary updated (four entries — see
      §"Glossary additions").
- [ ] If a brief finding was departed from: N/A — no departure.
- [ ] Frontend gate: `npm run check` (`svelte-check --fail-on-warnings`)
      + `npm run lint` (stylelint rejects raw tokens + ESLint clean) +
      `npm run build` + Playwright spec passes
      (`web/console/tests/artifacts-page.spec.ts`).
- [ ] `make protocol-ts-gen-check` clean (the typed Protocol client
      regenerated to expose the three new methods).
