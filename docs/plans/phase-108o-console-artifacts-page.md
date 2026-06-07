# Phase 108o — Console Artifacts page

## Summary

Phase 108o brings the Console **Artifacts** page to the Phase 108 page-polish
acceptance bar AND lands the admin mutation it had been deferring. Console
half: a carded, viewport-locked master-detail rebuild + a king-file refactor
into an `ArtifactsPageState` controller + pure `derive.ts`. Go half (the §13
primitive-with-consumer pairing, like D-184/D-186): the real admin-gated
`artifacts.delete` method, backed by the existing `ArtifactStore.Delete` (no
driver-seam change) — turning the page's prominent Delete affordances (row /
bulk / rail) from disabled placeholders into a live, audited admin feature.

## RFC anchor

- RFC §6.10 (Artifacts)
- RFC §7 (Console layer — the runtime-lens Protocol-client principle)

## Briefs informing this phase

- brief 11 (Console feature surface — §"Artifacts view", §PG-4 rich-output renderers)
- brief 12 (Console deployment and shared UI — §"The shared chat / playground library")

## Brief findings incorporated

- brief 11 §"Artifacts view": Delete is admin-only — a mutating verb requiring
  strictly more than the read scope. `artifacts.delete` gates on the verified
  `admin` claim (D-079) and emits an audit event; download stays read-only and
  identity-bound via the presigned URL.
- brief 11 §PG-4 / brief 12 §"The shared chat / playground library": the preview
  pane dispatches through the canonical renderer registry at
  `$lib/chat/renderers` — no bespoke per-mime renderer (CLAUDE.md §13). The
  rebuild keeps `ArtifactPreview` unchanged.
- D-026: heavy bytes never cross the wire — the catalog rows are metadata-only,
  preview/download route through the presigned URL, and the CSV export is
  metadata-only.

## Findings I'm departing from (if any)

`artifacts.usages` (the "Where used" cross-reference, page-artifacts §3/§5) and
the `Set retention` bulk action stay deferred: usages requires a state-store
join that is not a cheap pure-consumer read, and retention is an immutable-V1
carve-out (§10). Rather than render fabricated/empty surfaces, they are honest
findings (the rail surfaces preview + actions + metadata + tags only). Documented
per AGENTS.md §15.

## Goals

- Retheme to the carded (`.panel.card`), viewport-locked master-detail
  composition: filter card + records TABLE (sticky `<thead>`, internal scroll) +
  a stacked right rail (preview / actions / metadata / tags). Drop the per-page
  PageHeader.
- King-file refactor into `ArtifactsPageState` + pure `derive.ts` + the focused
  `ArtifactsTable` component.
- Land the real admin `artifacts.delete` (backed by `ArtifactStore.Delete`);
  wire the row / bulk / rail Delete consumers in the same wave (§13).
- Keep upload (`artifacts.put`), preview/download (`artifacts.get_ref`), the
  saved-view chips, and the metadata-only CSV export.

## Non-goals

- No `artifacts.usages` / `Set retention` / artifact mutation beyond delete.
- No driver-seam change — `artifacts.delete` composes the existing
  `ArtifactStore.Delete`; all V1 drivers back it with zero per-driver work.
- No bespoke per-mime renderer (canonical registry only).

## Acceptance criteria

- [x] Carded + viewport-locked: `scrollHeight == innerHeight`, no horizontal
      overflow at 1512×945 (verified live).
- [x] The per-page PageHeader is gone.
- [x] The catalog renders the real store (`artifacts.list`); selecting a row
      resolves a preview (`artifacts.get_ref`); upload uses `artifacts.put`.
- [x] `artifacts.delete` evicts an artifact (admin-gated, audited, idempotent);
      fails closed without `admin` (D-079) and on a missing identity (D-001).
      Row / bulk / rail Delete are all live (disabled-with-tooltip for non-admin).
- [x] All four `PageState` branches + the disconnected redirect preserved.
- [x] `make conformance` count 79→80; Go tests (surface + gating) under `-race`;
      `npm run check` 0/0, lint clean, the unit + e2e suites green.

## Files added or changed

- `internal/protocol/types/artifacts.go` — `ArtifactsDelete{Request,Response}`.
- `internal/protocol/methods/methods.go` — `MethodArtifactsDelete` + predicate/registry.
- `internal/protocol/artifacts.go` — `artifacts.deleted` audit event + `handleDelete` (admin-gated).
- `internal/protocol/transports/control/artifacts_handler.go` — decode the delete request.
- `internal/protocol/singlesource/singlesource.go` + `conformance/conformance.go` — counts.
- `internal/protocol/artifacts_delete_test.go` — Go surface tests.
- `web/console/src/routes/(console)/artifacts/+page.svelte` — rebuilt.
- `web/console/src/lib/artifacts/{state.svelte.ts,derive.ts}` — controller + projections.
- `web/console/src/lib/components/artifacts/ArtifactsTable.svelte` — NEW.
- `web/console/src/lib/protocol/{artifacts.ts,client.ts}` — wire type + namespace method.
- `web/console/src/lib/artifacts/tests/derive.test.ts` + `web/console/tests/artifacts-page.spec.ts`.
- `scripts/smoke/phase-108o.sh` — live-server + static guard.
- `docs/plans/phase-108o-console-artifacts-page.md` / `docs/decisions.md` (D-187) / `docs/design/console/page-artifacts.md` (§13).

## Public API surface

One new Protocol method (single-sourced in `internal/protocol/methods` +
`internal/protocol/types`):

- `artifacts.delete(scope, id)` → `ArtifactsDeleteResponse{deleted, protocol_version}` (admin scope, D-079; idempotent).

## Test plan

- **Unit:** `internal/protocol/artifacts_delete_test.go` (admin evicts + emits
  the `artifacts.deleted` audit event + the store no longer holds it; non-admin
  → `scope_mismatch`; idempotent on an absent id; missing-identity / empty-id
  failure modes). Console `web/console/src/lib/artifacts/tests/derive.test.ts`
  (fmtSize / sourceKind / displayStatus / relativeTime / previewFamily).
- **Integration:** the existing `artifacts_concurrent_test.go` covers the
  surface's D-025 reuse; the new delete dispatch shares that compiled artifact.
  The Console↔method seam is covered by the live smoke + the Playwright spec.
- **Conformance:** the `methods.Methods()` exhaustiveness + count (79→80).
- **Concurrency / leak:** N/A new — `handleDelete` is stateless over the
  D-025-safe `ArtifactsSurface`.

## Smoke script additions

`scripts/smoke/phase-108o.sh` (live-server): the `artifacts.delete` route
mounted; no-bearer → 401; with bearer routes + gates (200/403). Plus the static
Console guard (PageHeader gone, carded vocabulary, the controller + derive.ts,
the ArtifactsTable, the real row/bulk/rail Delete wiring, the client method, the
Save-view N7 contract, no hand-rolled fetch).

## Coverage target

- `internal/protocol`: the `handleDelete` happy + gating + failure paths are unit-covered.
- `web/console/src/lib/artifacts`: `derive.ts` unit-covered; the controller four-state via the existing seam + the e2e spec.

## Dependencies

- Phase 17–19 (the `ArtifactStore` interface + drivers the delete method composes).
- Phase 73l / D-120 (the `artifacts.{list,put,get_ref}` surface this rebuilds).
- Phase 108b (chrome) + Phase 108k / D-183 / 108n / D-186 (the carded
  master-detail pattern + controller refactor it mirrors).
- Phase 105 (the disconnected redirect).

## Risks / open questions

- `artifacts.usages` / retention stay deferred (see departures) — surfaced as
  honest findings, not stubs.
- Delete is idempotent: deleting an absent id returns `deleted=false` with no
  error (no `CodeNotFound`), matching the `ArtifactStore.Delete` contract — and
  the audit event fires only on an actual eviction.

## Glossary additions

None — the artifacts vocabulary (`PresignGet`, `ArtifactRef`) already lives in
`docs/glossary.md`; `artifacts.delete` extends the shipped `artifacts.*` family
without new domain terms.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes — `artifacts.delete` is identity-mandatory (validated) + admin-gated; covered by the surface tests' identity/scope modes.
- [x] **If this phase builds a reusable artifact:** N/A — `handleDelete` is stateless over the existing D-025-safe `ArtifactsSurface`.
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam:** the surface tests wire the real `ArtifactStore` + identity + admin end-to-end under `-race`; the Console↔method seam is covered by the live smoke + Playwright.
- [x] If new vocabulary: glossary updated — N/A.
- [x] If a brief finding was departed from: justified above + decisions.md entry filed — yes (usages/retention, D-187).
