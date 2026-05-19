# Phase 72e — `pause.list` snapshot

## Summary

Adds the `pause.list` Protocol method — a paginated, identity-scope-filtered snapshot of currently-paused tasks / sessions, projected from the shipped Pause/Resume Coordinator (Phase 50) state. The page-overview spec calls for the intervention queue to read this snapshot at first render and reconcile against live `pause.requested` / `pause.resumed` deltas; until 72e ships, the only way to know whose runs are paused is to watch events fly past. `pause.list` closes that gap with one snapshot method. First consumer (same phase, §13 primitive-with-consumer) is the integration test in `test/integration/pause_list_test.go` — it creates two pauses under different tenants, calls `pause.list` from each identity, asserts only the in-scope pause is returned, and asserts cross-tenant calls without `admin` claim are rejected `403`. The Overview page's intervention queue (Phase 73a, Stage 2.3) is the UI consumer; that consumer lands in the next stage.

## RFC anchor

- RFC §5.2 (Protocol surface — task-control / observability rows)
- RFC §6.3 (Pause / Resume primitive)
- RFC §6.5 (Context-window safety net — heavy-content threshold)
- RFC §7 (Console layer)

## Briefs informing this phase

- brief 11 (Console feature surface — `LR-4 Interventions sub-panel`, `Overview view`)
- brief 12 (Console deployment + two-surface model — the typed Protocol client at the Console seam)
- brief 05 (State / tasks / artifacts / sessions — pagination posture for runtime-side high-cardinality reads)

## Brief findings incorporated

- **brief 11 §LR-4 (Interventions sub-panel):** "A live list of pending pause records for this session. Shows: type (Human Approval / OAuth-required / Tool approval), elapsed time paused, requester (agent or system), reason. 'View' → opens the intervention detail; 'Resume' → action button gated by JWT scope. Comes from Phase 50 (Pause/Resume Coordinator) + Phase 54 (Protocol task control surface)." `pause.list` is the missing read surface that bridges the Coordinator (write) and the Phase 54 action methods (`approve` / `reject` / `resume`). The wire shape mirrors what the panel needs: token, reason, payload (sanitised), paused-at, identity triple, run id — every row is independently actionable by a Phase 54 control call.
- **brief 11 §"Overview view" (via page-overview.md §3 / §5):** "Intervention queue — pending pauses across all sessions in scope, with operator action affordances. Requires a `pause.list` Protocol method that returns a snapshot of unresolved `pause.requested` records keyed by `(tenant, user, session, run, PauseToken)`." `pause.list` is the snapshot half of the page's intervention-queue contract; live deltas continue to flow through `events.subscribe` on the `pause.requested` / `pause.resumed` topics (Phase 50 + 72a).
- **brief 11 §CC-2 (Identity-aware UI):** "Cross-tenant rollups require the `admin` claim." `pause.list` defaults to the operator's own `(tenant, user)` slice; cross-tenant filtering requires the `auth.ScopeAdmin` claim (D-079) and is rejected `403` otherwise (mirrors the existing `events.subscribe` + Phase 72 cross-tenant posture).
- **brief 12 §"the two-surface model":** "the wire shape MUST be the same one third-party Console implementations consume." `PauseSnapshot` / `PauseFilter` live in `internal/protocol/types/` — the canonical single-source location (CLAUDE.md §8 + D-002). Third-party clients and the bundled Console consume the same wire shape; the typed Protocol client (D-093) regenerates from them.
- **brief 05 §"runtime-side high-cardinality reads":** "the runtime owns the index; the Protocol exposes a paginated method, never a client-side filter over a full dump." `pause.list` is paginated from the start (`Page` + `PageSize`); the Coordinator's process-local registry is the index. The pagination shape is consistent with Phase 73's broader inspection-surface set (`sessions.list`, `tasks.list` — same `Page` / `PageSize` / `PageCount` envelope) so a future Console-side pagination component can be shared across pages without per-method ceremony.

## Findings I'm departing from (if any)

None.

## Goals

- Ship `pause.list` as a Protocol method that returns a paginated, identity-scope-filtered snapshot of currently-paused runs from the Pause/Resume Coordinator (Phase 50) state.
- Wire shape (`PauseSnapshot`, `PauseFilter`, `PauseListRequest`, `PauseListResponse`) lives in `internal/protocol/types/pause.go` — single source per CLAUDE.md §8 + D-002. The typed Protocol client regenerates from `CanonicalWireTypes` (D-093) without hand-editing.
- Identity-mandatory at the Protocol edge (CLAUDE.md §6 rule 9 + D-001): missing `tenant_id` / `user_id` / `session_id` → reject `401` with `CodeIdentityRequired` (Phase 54 / D-072 error code; never silently downgrade).
- Cross-tenant filtering (passing a tenant outside the operator's own) requires the `auth.ScopeAdmin` claim (D-079): a request without the claim is rejected `403` with `CodeScopeMismatch`. Identical posture to the `events.subscribe` cross-tenant filter (Phase 72a) — one scope-claim story, not two.
- Heavy-content bypass per D-026: a pause-record payload whose serialised size meets or exceeds the configured `HeavyOutputThresholdBytes` is materialised into an `artifacts.Ref` via the `ArtifactStore`, and the snapshot row ships the `ArtifactRef` (not the raw bytes). The runtime emits a `pause.payload_artifact_routed` audit observation when this triggers — never a silent truncation. This keeps `pause.list` snapshot responses from leaking heavy content into a Console or third-party client process at any size — the context-window safety-net principle applied to Protocol read snapshots, not just LLM prompts.
- Pagination shape (`Page`, `PageSize`, `PageCount`, `TotalRows`) consistent with Phase 73's broader inspection-surface (`sessions.list`, `tasks.list`) so the Console-side pagination component is shared, not per-method. Default `PageSize=50`, max `PageSize=200`, validated at the Protocol edge; a value out of range is rejected loud (`CodeInvalidRequest`, never silently clamped — silent clamp would defeat the per-row identity boundary the test asserts).
- `pause.list` is read-only against the Coordinator's exported `Status` surface (Phase 50). It does NOT mutate state, does NOT call `Resume`, does NOT clear checkpoints. Resume actions continue to flow through the existing Phase 54 `resume` / `approve` / `reject` Protocol methods (no parallel implementation per §13).
- The handler is a long-lived compiled artifact (one per Runtime process, shared across all `pause.list` calls). D-025 concurrent-reuse: N≥100 concurrent `pause.list` calls against a single shared handler under `-race`, asserting no data races, no context bleed, no cross-cancellation, no goroutine leak.
- Integration test (same phase) wires the real Coordinator (Phase 50) + real Protocol transport (Phase 60) + real auth middleware (Phase 61) + two-tenant identity scope; asserts the §13 primitive-with-consumer obligation is discharged in-PR.

## Non-goals

- **No new pause-list event topic.** Live deltas continue to flow through `events.subscribe` on the existing `pause.requested` / `pause.resumed` topics (Phase 50 / D-067). The Overview page reconciles a `pause.list` snapshot against those event deltas locally; minting a `pause.list_delta` topic would be a §13 "two parallel implementations of the same conceptual feature" smell.
- **No mutation surface in this phase.** Approve / Reject / Resume continue to invoke the Phase 54 control methods. `pause.list` is a read snapshot, not a control verb.
- **No cross-runtime pause aggregation.** D-091 — the multi-runtime fleet aggregator is Console-side (post-V1). 72e returns the pauses for the single Runtime instance it runs in.
- **No re-design of the pause-record envelope.** `PauseSnapshot` is a projection of the existing `pauseresume.Pause` + `pauseresume.Status` shapes; it does not change what the Coordinator persists.
- **No saved filters on the runtime side.** Saved-filter chips are Console-local per D-061; the runtime exposes the filter shape, not a "save / load filter" surface.
- **No subscription endpoint for `pause.list` itself.** A `pause.list` *subscription* (push-on-delta) duplicates the `events.subscribe` topics. The Console subscribes to `pause.requested` / `pause.resumed` and re-runs `pause.list` on focus, exactly the same posture as the rest of the snapshot-plus-deltas pages.

## Acceptance criteria

- [ ] `internal/protocol/methods/methods.go` declares `MethodPauseList Method = "pause.list"` alongside the ten shipped Phase 54 method names; the `canonicalMethods` map gains the entry.
- [ ] `internal/protocol/types/pause.go` defines `PauseSnapshot`, `PauseFilter`, `PauseListRequest`, `PauseListResponse` — single source per CLAUDE.md §8 + D-002. The typed Protocol client (D-093) regenerates without hand-editing.
- [ ] `internal/runtime/pauseresume/list.go` exposes a `List(ctx, ListFilter) (ListResponse, error)` method on the `Coordinator` interface, implemented by the existing process-local `coordinator`. The method reads the in-memory `pauses` registry (and falls back to checkpoint store enumeration when configured — same restart-survival path as `Status`).
- [ ] `internal/protocol/transports/stream/pause_list_handler.go` dispatches `pause.list` requests, performs identity-mandatory + scope-claim checks, calls `Coordinator.List`, and applies the D-026 heavy-content bypass on each row before responding.
- [ ] Identity-mandatory at the Protocol edge: a request with missing `tenant_id` / `user_id` / `session_id` → `401` with `CodeIdentityRequired`. NEVER silently downgrade (CLAUDE.md §6 rule 9 + §13 forbidden-practice ban on identity-downgrading knobs).
- [ ] Cross-tenant filter (a `PauseFilter.TenantIDs` containing a tenant outside the caller's own, OR `len(TenantIDs) > 1`) without `auth.ScopeAdmin` → `403` with `CodeScopeMismatch`. Mirrors the existing `events.subscribe` cross-tenant posture (D-079).
- [ ] Heavy-content bypass per D-026: a pause-record `Payload` whose `json.Marshal`-ed byte length is `>= cfg.ArtifactsConfig.HeavyOutputThresholdBytes` is routed through the `ArtifactStore`; the resulting `artifacts.Ref` is placed on `PauseSnapshot.PayloadRef`, and `PauseSnapshot.Payload` is left nil. The runtime emits a `pause.payload_artifact_routed` observation event so the bypass is loud, never silent.
- [ ] Pagination: `PageSize` defaults to 50, max 200; out-of-range `PageSize` (negative, 0, > 200) is rejected `400` with `CodeInvalidRequest` (never silently clamped). `Page` is 1-indexed (0 also rejected). The response carries `Page`, `PageSize`, `PageCount`, `TotalRows`.
- [ ] Filter axes supported: `Status` (`paused` / `resumed`), `TenantIDs []string`, `UserIDs []string`, `SessionIDs []string`, `RunIDs []string`, `Reasons []Reason`, `Since time.Time`, `Until time.Time`. Empty filter = caller's own identity scope, status=paused.
- [ ] Default `Status` filter is `paused` (the intervention-queue use case). An explicit `Status=resumed` is honoured but bounded by the in-memory registry's retention — resumed records persist only until they are cleared from the registry (post-resume, the Coordinator's destructive-on-resume contract per Phase 50 / coordinator.go applies); the response carries a `Truncated bool` flag when the resumed slice has aged out.
- [ ] D-025 concurrent-reuse test (`internal/runtime/pauseresume/list_concurrent_test.go`): N≥100 concurrent `Coordinator.List` calls against a single shared `coordinator` under `-race`, distinct per-goroutine identity quadruples — asserts no data races, no context bleed (each goroutine's filter is preserved end-to-end), no cross-cancellation (cancelling one call's ctx never affects another), baseline `runtime.NumGoroutine` restored after all calls return.
- [ ] Integration test (`test/integration/pause_list_test.go`) wires the real Coordinator (Phase 50) + real Protocol transport (Phase 60) + real auth middleware (Phase 61). Two-tenant scope: tenant A pauses one run, tenant B pauses another; `pause.list` from tenant A's identity returns only A's row; `pause.list` from tenant A with `TenantIDs=["B"]` and NO `admin` claim → `403`; `pause.list` from an admin identity with `TenantIDs=["A","B"]` returns both rows. Heavy-payload negative case: a pause whose payload exceeds `HeavyOutputThresholdBytes` materialises through the `ArtifactStore` and the snapshot row carries the `ArtifactRef`. Runs under `-race`.
- [ ] `scripts/smoke/phase-72e.sh` (`# PREFLIGHT_REQUIRES: live-server`): asserts (a) `pause.list` returns paginated rows; (b) cross-tenant filter without admin claim → `403`; (c) missing identity context → `401`; (d) malformed `PageSize` → `400`. Uses only the `common.sh` helpers per CLAUDE.md §4.2.
- [ ] `docs/plans/README.md` Phase 72 row's detail block notes the `pause.list` extension in the 72e sub-phase summary; `docs/glossary.md` gains `pause.list`, `PauseSnapshot`, `PauseFilter`.
- [ ] Coverage on `internal/runtime/pauseresume`: ≥ 90% (master plan target preserved; the existing 93.9% from Phase 50 must not regress).
- [ ] Coverage on `internal/protocol/types`: 90% (struct serialization).
- [ ] Coverage on `internal/protocol/transports/stream`: ≥ 80%.

## Files added or changed

```text
docs/plans/phase-72e-pause-list-snapshot.md           (new — this file)
docs/glossary.md                                      (+pause.list, +PauseSnapshot, +PauseFilter)
internal/protocol/methods/methods.go                  (+MethodPauseList constant + canonicalMethods entry)
internal/protocol/types/pause.go                      (new — PauseSnapshot, PauseFilter, PauseListRequest, PauseListResponse)
internal/protocol/transports/stream/pause_list_handler.go      (new — dispatch + identity + scope-claim + heavy-content bypass)
internal/protocol/transports/stream/pause_list_handler_test.go (new — unit: identity rejection, scope-claim rejection, heavy bypass)
internal/runtime/pauseresume/pauseresume.go           (extend Coordinator interface: +List(ctx, ListFilter) (ListResponse, error))
internal/runtime/pauseresume/list.go                  (new — List implementation: snapshot the registry + filter + paginate)
internal/runtime/pauseresume/list_test.go             (new — unit: filter combinations, pagination math, status-filter semantics)
internal/runtime/pauseresume/list_concurrent_test.go  (new — D-025: N≥100 concurrent List calls)
internal/runtime/pauseresume/events.go                (+EventTypePausePayloadArtifactRouted + PausePayloadArtifactRoutedPayload)
test/integration/pause_list_test.go                   (new — two-tenant + heavy-payload + admin-claim + under -race)
scripts/smoke/phase-72e.sh                            (new — live-server smoke)
```

## Public API surface

```go
// internal/protocol/types/pause.go
package types

import (
    "time"

    "github.com/hurtener/Harbor/internal/artifacts"
)

// PauseSnapshot is the wire projection of a single Coordinator pause
// record. Cross-package single source per CLAUDE.md §8 + D-002; the
// typed Protocol client (D-093) regenerates from this without
// hand-editing.
type PauseSnapshot struct {
    Token     string         `json:"token"`              // opaque pauseresume.Token (RFC §6.3)
    Reason    string         `json:"reason"`             // one of the four canonical PauseReasons
    State     string         `json:"state"`              // "paused" | "resumed"
    Identity  IdentityScope  `json:"identity"`           // (tenant, user, session [, run])
    PausedAt  time.Time      `json:"paused_at"`
    ResumedAt time.Time      `json:"resumed_at,omitempty"`
    // Payload is the sanitised pause payload INLINE when its serialised
    // size is below HeavyOutputThresholdBytes. Otherwise the runtime
    // routes it through the ArtifactStore and ships PayloadRef instead
    // (D-026 — context-window safety net applied to Protocol snapshots).
    Payload    map[string]any   `json:"payload,omitempty"`
    PayloadRef *artifacts.Ref   `json:"payload_ref,omitempty"`
}

// PauseFilter narrows the pause.list response. Empty filter = caller's
// own identity scope, status=paused.
type PauseFilter struct {
    Status     []string  `json:"status,omitempty"`      // "paused" | "resumed"; empty ⇒ ["paused"]
    TenantIDs  []string  `json:"tenant_ids,omitempty"`  // empty ⇒ caller's own; >1 OR foreign requires auth.ScopeAdmin
    UserIDs    []string  `json:"user_ids,omitempty"`
    SessionIDs []string  `json:"session_ids,omitempty"`
    RunIDs     []string  `json:"run_ids,omitempty"`
    Reasons    []string  `json:"reasons,omitempty"`     // one or more canonical PauseReasons
    Since      time.Time `json:"since,omitempty"`       // optional lower bound on PausedAt
    Until      time.Time `json:"until,omitempty"`       // optional upper bound on PausedAt
}

// PauseListRequest is the wire request.
type PauseListRequest struct {
    Identity IdentityScope `json:"identity"`
    Filter   PauseFilter   `json:"filter,omitempty"`
    Page     int           `json:"page,omitempty"`      // 1-indexed; 0 or negative is rejected
    PageSize int           `json:"page_size,omitempty"` // default 50, max 200; out of range is rejected
}

// PauseListResponse is the wire response.
type PauseListResponse struct {
    Snapshots []PauseSnapshot `json:"snapshots"`
    Page      int             `json:"page"`
    PageSize  int             `json:"page_size"`
    PageCount int             `json:"page_count"`
    TotalRows int             `json:"total_rows"`
    // Truncated is true when a status=resumed filter aged out beyond the
    // in-memory registry's retention. The Coordinator's resumed-records
    // retention is bounded by the destructive-on-resume contract
    // (coordinator.go) — a resumed Token is queryable only until the
    // Coordinator clears it.
    Truncated bool `json:"truncated,omitempty"`
}
```

```go
// internal/runtime/pauseresume/pauseresume.go (extension)
type Coordinator interface {
    Request(ctx context.Context, req PauseRequest) (Pause, error)
    Resume(ctx context.Context, token Token, decision Decision, payload map[string]any) error
    Status(ctx context.Context, token Token) (Status, error)
    // List returns a snapshot of pause records visible under the
    // caller's identity scope. Read-only; does NOT mutate the registry.
    // Identity-mandatory: a missing (tenant, user, session) triple
    // returns wrapped ErrIdentityRequired. Pagination is mandatory —
    // a 0 / negative / > 200 PageSize is wrapped ErrInvalidPage.
    List(ctx context.Context, req ListRequest) (ListResponse, error)
}

// ListRequest is the runtime-internal projection of types.PauseListRequest.
type ListRequest struct {
    Identity identity.Identity
    Filter   ListFilter
    Page     int
    PageSize int
    // AdminScoped is true when the caller carries auth.ScopeAdmin.
    // Set by the Protocol-edge handler; the Coordinator itself does
    // NOT read the scope from ctx (separation of concerns).
    AdminScoped bool
}

type ListFilter struct {
    States     []State
    TenantIDs  []string
    UserIDs    []string
    SessionIDs []string
    RunIDs     []string
    Reasons    []Reason
    Since      time.Time
    Until      time.Time
}

type ListResponse struct {
    Snapshots []Pause
    Statuses  []Status // parallel to Snapshots
    Page      int
    PageSize  int
    PageCount int
    TotalRows int
    Truncated bool
}
```

## Test plan

- **Unit:**
  - `list_test.go` — filter combinations: each axis tested in isolation + an "all axes" combination + empty filter (defaults to caller scope, status=paused); pagination math (PageSize=10 over 25 records yields PageCount=3); pagination edge cases (PageSize=0 / negative / > 200 → `ErrInvalidPage`); status filter semantics (paused vs resumed; the `Truncated` flag).
  - `pause_list_handler_test.go` — identity-mandatory rejection (missing triple → `CodeIdentityRequired`, `401`); scope-claim rejection (foreign tenant without `admin` → `CodeScopeMismatch`, `403`); malformed request (PageSize > 200 → `CodeInvalidRequest`, `400`); heavy-content bypass (a payload above threshold materialises through a fake `ArtifactStore` and the snapshot carries an `ArtifactRef`).
- **Integration:**
  - `test/integration/pause_list_test.go` — real `pauseresume.coordinator` (Phase 50) + real `protocol.ControlSurface` / `transports.NewMux` (Phase 54 / 60) + real `auth.Middleware` (Phase 61) over the testdata ES256 keypair + real in-memory `artifacts.Store`. Two-tenant scope: tenant A pauses run-a, tenant B pauses run-b. Scenarios: (1) `pause.list` from A's identity returns only A's row; (2) `pause.list` from A's identity with `TenantIDs=["B"]` and no `admin` claim → `403`; (3) `pause.list` from an admin identity with `TenantIDs=["A","B"]` returns both rows in deterministic (paused_at desc) order; (4) a pause whose `Payload` exceeds `HeavyOutputThresholdBytes` round-trips through the `ArtifactStore` — the row carries `PayloadRef`, the inline `Payload` is nil, and a `pause.payload_artifact_routed` event was emitted on the bus. Runs under `-race`. Failure modes covered per §17.3 #3: missing identity, foreign tenant without claim, malformed PageSize.
- **Conformance:**
  - `pause.list` is added to the Phase 62 Protocol conformance matrix (one happy-path scenario + one malformed-request scenario per the matrix-exhaustiveness check at the top of `RunSuite`). The conformance suite is the binding pass/fail definition of "the Protocol surface works at version 0.1.0" — a new method that lands without a conformance scenario fails the suite (D-080 lockstep).
- **Concurrency / leak:**
  - `list_concurrent_test.go` — N=128 (≥ the D-025 contract minimum of 100) concurrent `Coordinator.List` calls against one shared `coordinator` under `-race`. Distinct per-goroutine identity quadruples (a context bleed surfaces as a wrong triple on the returned snapshots), pre-cancelled ctx on a subset (no cross-cancellation), baseline `runtime.NumGoroutine` restored after all goroutines join (no leak). Asserts the four D-025 guarantees verbatim.

## Smoke script additions

`scripts/smoke/phase-72e.sh` (`# PREFLIGHT_REQUIRES: live-server`):

- `protocol_call 'pause/list' '{}'` → expect `200`; `assert_json_path '.snapshots | type' 'array'`; `assert_json_path '.page' '1'`; `assert_json_path '.page_size' '50'`.
- `protocol_call 'pause/list' '{"filter": {"tenant_ids": ["foreign-tenant"]}}'` (without `admin` claim) → `assert_status 403`.
- `protocol_call 'pause/list' '{}'` with no identity carrier → `assert_status 401`.
- `protocol_call 'pause/list' '{"page_size": 5000}'` → `assert_status 400` (PageSize > 200 is rejected, never silently clamped).
- Surface-existence probe: `skip_if_404 "$(api_url /protocol/pause/list)" 'phase 72e: pause.list route absent until Protocol layer ships'` so the smoke remains green on a Stage-1 partial build.

## Coverage target

- `internal/runtime/pauseresume`: ≥ 90% (master-plan Phase 50 target — must not regress from 93.9%; the new `list.go` is included in the package coverage envelope).
- `internal/protocol/types`: 90% (struct serialization for `PauseSnapshot`, `PauseFilter`, `PauseListRequest`, `PauseListResponse`).
- `internal/protocol/transports/stream`: ≥ 80% (the new `pause_list_handler.go` is included).

## Dependencies

**Same-wave (Wave 13, Stage 1):**

- Phase 72 (Console subscription protocol surface scope foundation — `Pending` in master plan; in-flight Wave 13).

**Already shipped (pre-Wave 13):**

- Phase 50 (Pause/Resume Coordinator — `Shipped`; supplies the coordinator state `pause.list` projects).
- Phase 51 (Pause-state serialise contract — `Shipped`; supplies the `format_version: 1` envelope shape that backs the `Status` fallback when the registry has aged out and the checkpoint store is configured).
- Phase 54 (Protocol task control surface — `Shipped`; supplies the `MethodResume` / `MethodApprove` / `MethodReject` actions the intervention queue invokes against rows `pause.list` returns).
- Phase 58 (Protocol single-source checker — `Shipped`; pins `MethodPauseList` in `methods.go` and `PauseSnapshot` in `types/pause.go` as the only declarations).
- Phase 60 (Protocol wire transport — `Shipped`; supplies the `transports.NewMux` HTTP/SSE binding).
- Phase 61 (Protocol auth — `Shipped`; supplies the `auth.Middleware` + `auth.ScopeAdmin` claim).
- Phase 62 (Protocol conformance suite — `Shipped`; the matrix-exhaustiveness check requires the `pause.list` scenarios to land in the same PR).
- Phase 17 / 18 / 19 (Artifact store + drivers — `Shipped`; supplies the `ArtifactStore` the heavy-content bypass routes through per D-026).

**Cross-wave coordination:**

- Phase 73 (Console state inspection surface — `Pending`; the snapshot wire shape consistency described in §"Goals" / §"Acceptance" mirrors `sessions.list` / `tasks.list` pagination. 72e is `Deps: 73` for *shape consistency*, not for behaviour — 73 ships in the same wave; pagination conventions land in whichever phase ships first and the second matches them).

## Risks / open questions

- **Resumed-record retention.** The Coordinator's resume path is destructive (per Phase 50 / `coordinator.go::Resume` — flips state to resumed in-memory, then deletes the checkpoint from the StateStore). A `pause.list` with `Status=resumed` therefore reflects only the resumed records that are still in the in-memory registry — a fresh Coordinator (post-restart) cannot see them. The `Truncated bool` flag on `PauseListResponse` is the load-bearing signal: operators inspecting historical resume activity should use `events.subscribe` on the `pause.resumed` topic (Phase 50 / 72a) instead of relying on `pause.list`. The integration test asserts `Truncated=true` whenever the resumed slice is empty AND a `Status=resumed` filter was requested.
- **Heavy-content bypass cost.** Routing a heavy `Payload` through the `ArtifactStore` per request is O(N) over the page's rows. For a typical intervention queue this is bounded by `PageSize` (default 50, max 200) — acceptable. Mitigation if a hot operator overrun emerges: a per-row Payload-size pre-check before serialising, so small payloads short-circuit the threshold path. Logged as a follow-up consideration; not blocking.
- **`pause.list` on a Coordinator without a checkpoint store.** A Coordinator constructed *without* a checkpoint store keeps every pause in the in-memory registry only — `pause.list` works fine, but a process restart wipes the snapshot. This matches the Phase 50 design ("pauses survive Runtime restart only when StateStore-backed checkpoint is configured"). The integration test covers BOTH configurations (with and without store) to pin the contract — restart-survival of `pause.list` rides on the same `WithCheckpointStore` option that already governs `Status` / `Resume`.
- **Filter axis explosion.** The seven filter axes (Status, TenantIDs, UserIDs, SessionIDs, RunIDs, Reasons, Since/Until) is the same axis count as the `events.subscribe` filter (Phase 72a). A future "saved filter" UI lands in Console DB (D-061 — Console-local) without re-litigating the runtime filter shape. Cross-cutting filter conventions across Console pages are documented in the Wave 13 decomposition doc.
- **Race with concurrent Resume.** Between snapshot capture and Console action, the user may have invoked Resume on the same Token. The Console-side flow MUST handle a 404 / `ErrAlreadyResumed` from the subsequent `resume` call gracefully — `pause.list` does not lock or reserve rows. This is the standard optimistic-snapshot pattern; the integration test does NOT assert this race (it's a Console concern), but the per-row Phase 54 control call (`approve` / `reject` / `resume`) is already idempotent against `ErrAlreadyResumed` (Phase 50 / coordinator.go).
- **Scope-claim handling consistency.** The 72e scope-claim posture (`admin` for cross-tenant) MUST match Phase 72a's `events.subscribe` cross-tenant posture (`events.crosstenant` scope per the Phase 72a plan). Wave 13 decomposition §4 row 72a names the scope claim `events.crosstenant` — D-079 lists only `admin` and `console:fleet` as canonical. The 72e plan goes with `auth.ScopeAdmin` (the D-079 canonical scope) because it is the already-shipped surface; if Phase 72a lands with a different name, the wave-end audit reconciles them. Risk surfaced for the Wave 13 coordinator-verify pass.

## Glossary additions

- **`pause.list`** — Protocol method returning a paginated, identity-scope-filtered snapshot of currently-paused runs from the `pauseresume.Coordinator` state. Read-only — does NOT mutate the registry. Cross-tenant filtering requires `auth.ScopeAdmin` (D-079). Heavy `Payload`s above the heavy-content threshold ship as `PayloadRef` per D-026. Added in Phase 72e.
- **PauseSnapshot** — wire projection of a single Coordinator pause record. Carries `Token`, `Reason`, `State`, `Identity`, timestamps, and the sanitised `Payload` OR an `ArtifactRef` when the payload exceeded the heavy-content threshold. Lives in `internal/protocol/types/pause.go` (single source — D-002).
- **PauseFilter** — narrows `pause.list` deliveries by `Status`, identity scope (`TenantIDs` / `UserIDs` / `SessionIDs` / `RunIDs`), `Reasons`, and `Since` / `Until` time window. Empty filter = caller's own identity, status=paused.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] `make protocol-ts-gen-check` passes (`web/console/src/lib/protocol.ts` regenerated from `CanonicalWireTypes` per D-093)
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (`pauseresume` ≥ 90%, `protocol/types` 90%, `protocol/transports/stream` ≥ 80%)
- [ ] If multi-isolation paths changed: cross-session isolation test passes (binding for this phase — the filter shape touches the isolation tuple; the integration test asserts cross-tenant rejection without `admin` claim)
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent `Coordinator.List` calls against a single shared coordinator under `-race` (D-025; `list_concurrent_test.go` pins N=128)
- [ ] **Integration test exists** — `test/integration/pause_list_test.go` wires real Coordinator + real Protocol transport + real auth middleware + two-tenant scope + heavy-payload negative + `-race` (§17)
- [ ] Phase 62 conformance matrix gains a `pause.list` happy-path AND malformed-request scenario in the same PR (D-080 lockstep — matrix exhaustiveness check fails the suite otherwise)
- [ ] Glossary updated (`pause.list`, `PauseSnapshot`, `PauseFilter`)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (None for this phase)
- [ ] **Coordinator-verify pass complete** before the PR is opened for operator review (wave-13-decomposition.md §12 item 3 — the binding coordinator-verify protocol)
