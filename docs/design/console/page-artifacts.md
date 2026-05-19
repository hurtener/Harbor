# Console page — Artifacts

**Slug:** `artifacts` &middot; **Sidebar cluster:** Resources &middot; **Route:** `/console/artifacts`
**Mockup:** `docs/rfc/assets/console-artifacts-page.png` (canonical, 2026-05-18)

## 1. Purpose

Artifacts is the browser for the runtime's artifact store — the binary content (images, audio, documents, JSON dumps, generated reports, file uploads from the Playground) Harbor's tool calls and planner decisions produce. Every heavy content shape in Harbor materialises as an `ArtifactRef` per D-022; the artifact store (Phases 17–19, `Shipped`) is the canonical home; the Protocol exposes artifacts by reference per RFC §5.2 ("Heavy bytes always go by `ArtifactRef`, never inline"). The page answers: "what artifacts exist in this session?", "preview this PDF / image / CSV", "where is this artifact used?", "download artifact X for offline analysis," "delete this artifact (admin-only)." The page is a catalog + preview surface, virtualised for high-cardinality artifact stores.

## 2. Where it sits in the IA

Artifacts sits last under the **Resources** cluster (Resources → Flows, Memory, MCP Connections, Artifacts). The operator reaches it from the sidebar, from a Session detail's Artifacts-tab "View all" link, from a Task detail's Output-tab artifact-preview card, from a Live Runtime "Recent Artifacts" sub-panel, or from the global search palette. Breadcrumb: `<runtime> / Artifacts` (list) and `<runtime> / Artifacts / <artifact-id>` (preview).

## 3. Functionality matrix

- **Catalog list — virtualised list of artifacts, default newest-first.** `[wave-13-extends]` `artifacts.list` Protocol method (Phase 73 acceptance; currently `Pending`).
- **Per-row metadata — filename / artifact-id, mime type, size (bytes), age (created-at), source-task id, content-hash, identity (truncated).** `[wave-13-extends]` `artifacts.list` payload, which projects `artifacts.ArtifactRef` (`Scope` / `ID` / `MIME` / `Size` / `Hash` / `CreatedAt` per `internal/artifacts/artifacts.go`).
- **Filters — mime type, size range, identity scope, session, source-task, time range.** `[wave-13-extends]` `artifacts.list` query payload.
- **Free-text search (filename / content-hash).** `[shipped]` Console-side index per Brief 11 §CC-4 — artifacts catalog is moderate-cardinality; large stores may need runtime-side via a `search.artifacts` Protocol method (Wave 13 to decide).
- **Per-artifact preview — per-mime-type renderer (image / PDF / audio / video / JSON tree / CSV table / Markdown / plain text / hex dump for binary).** `[wave-13-extends]` `artifacts.get_ref` Protocol method (Phase 73 acceptance) returning a presigned URL (or in-band bytes for small artifacts) per D-022. Heavy bytes never travel inline through the Protocol.
- **Per-artifact "Where used" — list of sessions / tasks that reference this artifact.** `[wave-13-extends]` `artifacts.usages` Protocol method (NEW) joining the artifact id against state-store references.
- **Download — fetch the artifact bytes via the presigned URL.** `[wave-13-extends]` Via `artifacts.get_ref` (`Presigner.PresignGet` capability per `internal/artifacts/artifacts.go` glossary entry — read-side only; bounded [1 min, 7 days]; identity mandatory).
- **Share — copy a time-bounded presigned URL.** `[wave-13-extends]` Same `artifacts.get_ref` mechanism with explicit expiry; the Console UI lets the operator pick the expiry window within the [1 min, 7 days] bound.
- **Delete — admin-only.** `[wave-13-extends]` `artifacts.delete` Protocol method (Phase 73 acceptance).
- **Per-mime renderer registry — markdown, image, audio, video, link, embedded resource, code-block highlight, JSON tree, CSV table.** `[shipped]` Reuses the same canonical renderer registry the chat / playground module uses per Brief 11 §PG-4 + Brief 12 §"The shared chat / playground library" (`web/console/src/lib/chat/renderers/`); no Protocol surface.
- **Artifact lineage — show the producing task + the parent session + the artifact's `Hash` for dedup detection.** `[wave-13-extends]` `artifacts.list` extended fields (source-task, hash) + client-side rendering.
- **MCP-Apps content shapes interop — artifacts that came in as MCP `ImageContent` / `AudioContent` / `EmbeddedResource` (per Brief 11 §PG-3) render with the same renderer registry.** `[shipped]` Renderer dispatch on canonical content shape.
- **Per-mime type cap on preview size — large binaries render as "Preview unavailable — Download to view" rather than streaming gigabytes through the Console.** `[shipped]` Local UI policy (consumes `Size` field).
- **No Priority field rendered.** `[deferred]` D-065 invariant preserved.
- **Saved filter chips.** `[shipped]` Console-local per D-061.
- **Cross-tenant artifact list (admin-only).** `[shipped]` Filter bar's tenant facet rendered only when JWT carries `auth.ScopeAdmin`.

## 4. Page anatomy

- **Sidebar** (shared).
- **Top bar** (shared).
- **Main canvas** (per-page, list mode):
  - Row 1 — filter bar + saved-filter chips + search box.
  - Row 2 — artifacts table (virtualised) with per-row mime-type icon + size + age + source-task link.
- **Main canvas** (per-page, preview mode):
  - Row 1 — artifact detail header (filename + mime type + size + age + content-hash + source-task link + Download / Share / Delete buttons).
  - Row 2 — preview viewport (per-mime renderer; full canvas).
- **Right rail** (per-page, preview mode): Metadata card (full artifact-ref shape) + "Where used" list (sessions / tasks).
- **Bottom dock** (per-page): empty.
- **Footer** (shared).

## 5. Components — data in / actions out

| Component | Data in (Protocol source) | User actions (out) | Tag |
|---|---|---|---|
| Artifacts table | `artifacts.list` (Phase 73 acceptance) | Click row → preview; sort (local UI state) | `[wave-13-extends]` |
| Filter bar | local UI state → `artifacts.list` query | Apply / Clear | `[wave-13-extends]` |
| Saved-filter chips | Console DB (local) | Save / Rename / Delete (local UI state only) | `[shipped]` |
| Search box | Console-side index OR `search.artifacts` (NEW) | Submit | `[shipped]` |
| Artifact detail header | `artifacts.get` (Phase 73 — `artifacts.get`) | Copy id / hash; click source-task → Tasks detail | `[wave-13-extends]` |
| Preview viewport | `artifacts.get_ref` (Phase 73 acceptance) returning presigned URL | Renderer-dispatched (Markdown / image / PDF / audio / video / JSON tree / CSV table / hex) | `[wave-13-extends]` |
| Download button | `artifacts.get_ref` (presigned URL) | Fetch + save | `[wave-13-extends]` |
| Share button | `artifacts.get_ref` with expiry | Copy URL to clipboard (local UI state) | `[wave-13-extends]` |
| Delete button (admin) | `artifacts.delete` (Phase 73 acceptance) | Submit (confirms first) | `[wave-13-extends]` |
| Metadata card (right rail) | `artifacts.get` extended fields | Copy fields (local) | `[wave-13-extends]` |
| "Where used" list | `artifacts.usages` (NEW) | Click → Sessions / Tasks detail | `[wave-13-extends]` |

## 6. Controls + actions

- **Toolbar:** filter bar + saved-filter chips + search box.
- **Row-action (list):** click → preview; right-click → Download / Share / Delete (admin).
- **Header-action (preview):** Download / Share / Delete (admin) / Copy id.
- **Renderer-action (preview):** per-renderer affordances (e.g. JSON tree expand / collapse; CSV table sort; image zoom; audio play/pause/scrub).
- **Keyboard shortcuts:** `g R` (Resources / Artifacts) — operator-rebindable per Brief 11 §CC-5; `j` / `k`; `Enter` open preview; `Esc` back; `d` Download; `s` Share.

## 7. Empty / loading / error / unauthorized states

| State | Trigger | What renders | Recovery action |
|---|---|---|---|
| Empty store | No artifacts in scope | Empty-state: "No artifacts yet — artifacts are produced by tool calls and planner decisions" + link to Live Runtime | Visit Live Runtime |
| Filtered empty | Filters yield zero | "No artifacts match these filters" + Clear | Clear |
| Initial loading | `artifacts.list` in flight | Skeleton rows | Auto |
| Preview oversize | Artifact size > preview cap | "Preview unavailable — Download to view" + Download button | Download |
| Preview unsupported mime | Mime type lacks a renderer | Hex dump fallback + Download | Download |
| Protocol error — `CodeNotFound` on preview | Artifact id missing (perhaps deleted concurrently) | "Artifact not found"; back link | Back |
| Protocol error — `CodeScopeMismatch` on Delete | Operator submitted without admin scope | Inline error | Request admin scope |
| Protocol error — `CodeIdentityRequired` on `artifacts.get_ref` | Identity tuple incomplete | Inline error: "Identity required" | Re-attach |
| Protocol error — `CodeAuthRejected` | JWT expired | Banner + re-auth | Re-enter passphrase |
| Presigned URL expired | Expiry window elapsed mid-preview | "Preview link expired — click to refresh" | Re-fetch `artifacts.get_ref` |

## 8. Multi-tenant / multi-runtime nuances

Artifacts is tenant-scoped: every `artifacts.list` / `artifacts.get_ref` carries the operator's `(tenant, user, session)` triple and the artifact store rejects cross-tenant access per the `ArtifactScope` shape (`internal/artifacts/artifacts.go::ArtifactScope`). With `admin`, the tenant facet appears in the filter bar and cross-tenant queries elevate the subscription (with `audit.admin_scope_used` emitted on the server). Multi-runtime mode swaps the store when the runtime switcher changes; artifact stores can be different drivers per runtime (in-mem / fs / sqlite / postgres / s3 per `internal/artifacts/drivers/`), all with conformance parity.

## 9. Identity scope claims required

- Default `(tenant, user, session)` triple — list / preview / download / share artifacts within own scope.
- `admin` (`auth.ScopeAdmin`) — cross-tenant list; required for Delete operations.
- `console:fleet` (`auth.ScopeConsoleFleet`) — post-V1 cross-runtime aggregator.
- **Delete is a mutating verb** requiring admin scope — strictly more than the read scope (download is read-only and identity-bound per the `Presigner.PresignGet` contract).

## 10. Out of V1 (deferred)

- **Write-side presigned URLs.** Glossary `PresignGet` entry: "Read-side only — write-side presigned URLs are an attack surface intentionally not exposed at V1." Post-V1 if and when a use case justifies the threat model.
- **Cross-runtime artifact aggregator.** D-091 — post-V1.
- **Per-artifact annotations (tag / note / star).** Console-local possible per D-061; surface depth deferred to keep V1 lean.
- **Server-side artifact transformation (rename, tag, attach metadata).** Out of V1 — artifacts are immutable in V1; mutation lands as a post-V1 use case.
- **Artifact-to-Evaluation conversion.** D-064 — Evaluations is post-V1.
- **Priority field rendered anywhere.** D-065 invariant preserved.

## 11. References

- Brief 11 §"Artifacts view", §PG-2 (file upload pipeline routes through artifact store), §PG-4 (rich output renderers — shared with this page).
- Brief 12 §"The shared chat / playground library" (`web/console/src/lib/chat/renderers/` — shared renderer registry).
- RFC-001-Harbor.md §5.2 (artifacts row — "Heavy bytes always go by `ArtifactRef`, never inline"), §6.10 (Artifacts), §7 (Console).
- Decisions: D-021 (Multimodality scope: inputs V1, outputs post-V1 tool wrappers), D-022 (`ArtifactRef` is canonical binary representation), D-026 (Context-window safety net — `ArtifactStub` everywhere), D-061 (Console DB local-only), D-065 (no session priority — invariant), D-066 (control claim).
- Phase plan: phase 17 (ArtifactStore iface + InMem + FS drivers — `Shipped`), phase 18 (SQLite + Postgres blob — `Shipped`), phase 19 (S3-style driver — `Shipped`), phase 73 (state inspection — `Pending`).
- Glossary terms used: `Console`, `Runtime lens`, `PresignGet`, `Scope claim`, `Fleet control / fleet observation`.

## 12. Mockup-aligned refinements (2026-05-18)

Reconciliation of `docs/rfc/assets/console-artifacts-page.png` against §3-§7.

### Refinements to §4 page anatomy

- **Sub-header strip.** Faceted filter chips left-to-right: `MIME type` ▾ (image / pdf / text / json / binary / `*`), `Source` ▾ (`tool` / `planner` / `user-upload` / `system`), `Size` ▾ (configurable thresholds), `Tenant` ▾, `Session` ▾, `Task` ▾, `Created` ▾ (window picker — default last 7 d), `More filters` ▾. Right side: `Upload artifact` (gated by `artifacts.write` scope claim — Console-local upload path that routes through `artifacts.put` per Brief 11 §PG-2), `Export ▾` (CSV manifest of filtered page only — never the artifact blobs themselves).
- **Saved-view chip row.** Color-coded saved-filter chips: `Saved views`, `Large > 10 MB`, `Stale > 7d`, `User uploads`, `Tool outputs`, `Pending deletion`. Console-local per D-061.
- **Main artifacts table (primary surface).** Columns in mockup order: checkbox / **Name / Filename** (filename + mime icon) / **MIME type** chip / **Created** (relative timestamp) / **Owner** (compressed identity triple — agent + session + user; with run-id chip when present) / **Size** / **Source** chip / **Tags** / **Driver** chip (`inmem` / `fs` / `sqlite` / `postgres` / `s3`) / row-action menu. Virtualised; pagination footer `Rows per page ▾ | Page N of M`.
- **Right rail — Selected artifact detail panel (full height when a row is selected).** Header: filename + mime icon + size + copyable artifact-id. Sub-sections in mockup order:
  - **Preview** — inline preview when mime is renderable (image / pdf via embed / text snippet / json viewer / audio waveform). Renders via the canonical renderer registry at `web/console/src/lib/chat/renderers/` per Brief 12 — never a bespoke per-mime renderer. For mimes the registry can't render, shows a `Preview unavailable` placeholder with `Download` link.
  - **Actions row** — `Download` (resolves `PresignGet` per D-022 / D-026 — Console never inlines blob bytes), `Save` (Console-local pin into a saved-views list), `Copy ref` (copies the `ArtifactRef` URI for use in other surfaces).
  - **Artifact Metadata** — full artifact-id, full identity quadruple, source (`planner` / `tool` / `user-upload` / `system`), creation timestamp, last-accessed timestamp, driver, storage URL (when applicable), checksum, retention policy (when set).
  - **Tags** — chip list of tags assigned by planner / tool emission; editing tags is deferred to post-V1 per §10.
- **Bulk-action toolbar.** Activates when ≥1 row is checked: `Download (zip)` (Console-local zip-stream over the resolved `PresignGet` URLs — no Protocol mutation), `Copy refs`. Mutation actions (`Delete`, `Set retention`) render as disabled-with-tooltip ("Deferred — Phase 73") per §10.
- **Footer.** `Connected to <runtime> | Protocol v<X.Y.Z> | Events Stream: ON|OFF | Console v<X.Y>`.

### Components the mockup adds that the spec did not enumerate

| Component | Data in | User actions | Tag |
|---|---|---|---|
| Faceted filter chips (MIME / Source / Size / Tenant / Session / Task / Created / More filters) | `artifacts.list` filter params | Toggle facet | `[wave-13-extends]` (`artifacts.list` filter shape) |
| Saved-view chips (`Large > 10 MB`, `Stale > 7d`, `User uploads`, `Tool outputs`, `Pending deletion`) | Console-local saved views (D-061) | Apply / pin / unpin | `[Console-local]` (D-061; pure client-side derivation against `artifacts.list` rows) |
| Upload artifact button | Local file → `artifacts.put` | Open file picker | `[wave-13-extends]` (`artifacts.put` Protocol method — required for Brief 11 §PG-2 upload pipeline) |
| Export ▾ (CSV manifest of filtered page) | Already-loaded page rows | Client-side export of metadata only (never blobs) | `[Console-local]` (D-061; no Protocol mutation) |
| Preview pane (canonical renderer registry) | `PresignGet` URL per D-022 | Inline render via registry; fallback to `Download` for unsupported mimes | `[wave-13-extends]` (`artifacts.get` / `PresignGet` method) |
| Download button (resolves `PresignGet`) | Selected artifact-id | Trigger browser download | `[wave-13-extends]` (`PresignGet` Protocol method) |
| Save button (Console-local pin) | Selected artifact-id | Add to Console-local saved-views list | `[Console-local]` (D-061) |
| Copy ref button | Selected artifact ref URI | Copy to clipboard | `[Console-local]` (D-061) |
| Bulk Download (zip) | Selected artifact-ids | Client-side zip-stream over resolved `PresignGet` URLs | `[Console-local]` (D-061; routes each blob through `PresignGet`) |
| Bulk Delete / Set retention (disabled-with-tooltip) | Selected artifact-ids | None at V1 (deferred per §10) | `[deferred-post-V1]` (artifact mutation surface — Phase 73) |
| Tags column + chip rendering | `artifacts.list` row metadata | None (edit deferred per §10) | `[wave-13-extends]` (`artifacts.list` row shape extension) |

### No mockup violations of binding carve-outs

- **D-021 (Multimodality — inputs V1, outputs post-V1 tool wrappers).** Upload-artifact path lands a V1 input. The Preview pane renders outputs from planners/tools that already emit `ArtifactRef` — no Console-side generation of multimodal output.
- **D-022 / D-026 (`ArtifactRef` is canonical; `ArtifactStub` everywhere on heavy content).** Preview, Download, and bulk Download all resolve through `PresignGet`; the Console **never** inlines blob bytes in row metadata, the preview pane, or Export CSVs. Export ships metadata only.
- **D-061 (Console DB local-only).** Saved-view chips, the Save (pin) action, sort preferences, and the bulk-zip stream are Console-local. The mockup never persists a Protocol-mutating shadow of artifacts — every row round-trips through `artifacts.list` + `PresignGet`.
- **D-065 (no session-level priority).** No priority field appears on rows or in the right rail.
- **D-066 (control-scope claims).** Upload requires `artifacts.write`; mutation surfaces (Delete / Set retention) are deferred per §10 and rendered disabled-with-tooltip; observation requires only the read scope; cross-tenant inspection gates the `Tenant ▾` facet by scope per D-079.
- **D-091 (`harbor console` deployment).** Footer carries Protocol + Console versions and the connected-runtime label.
- **§13 forbidden practices.** No hand-rolled per-mime renderer — the canonical renderer registry at `web/console/src/lib/chat/renderers/` is the only path (Brief 12); no parallel implementation of artifact mutation (deferred surfaces are explicitly disabled); no inline blob bytes anywhere — `PresignGet` is the only download path (closes D-026 leak shape).
