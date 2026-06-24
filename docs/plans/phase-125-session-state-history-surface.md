# Phase 125 — Session state-history windowed event-replay surface

## Summary

Ship the Pending `state.history` Protocol method named in RFC §5.2's
State-snapshots row — but as the capability a generic Protocol client
actually needs to reopen a long conversation without melting: a **bounded,
tail-first windowed read of the session's durable event stream**. The
substrate is the gap-free event log, NOT memory: the Console already
reconstructs a reopened conversation by reducing the EVENT STREAM
(`web/console/src/lib/sessions/trajectory.ts` projects the
`events.subscribe` frames; the Playground hydration replays them), and the
memory subsystem holds nothing usable for this — the default `StrategyNone`
makes `AddTurn` a no-op, so sourcing a transcript from `MemoryStore` would
be an inert feature. Today the only replay primitive is **forward-only and
unbounded**: `stream.go::replayFromCursor` replays every event with
`Sequence > cursor` to the tail, and the SSE `id:` cursor is the raw
`Sequence` (`frame.go::encodeEvent`). Reopening a 5 000-event session
therefore streams all 5 000 events before the newest turn renders.

This phase adds the two missing reads on the same durable substrate:
(a) **discover the head/tail sequence** for a session, and (b) a **bounded
backward window** — "events for session S with `Sequence < before`, the
most-recent K" — returned oldest-first within the window with a
scroll-up cursor. Reduction (events → chat messages) stays **client-side**,
on the reducer the Console already owns. Per the §13
primitive-with-consumer rule, the first consumer lands in the same wave:
the open-source Console session-reopen hydration is rewired from its
current full-load reconstruction to **tail-first windowed + scroll-up-by-
cursor**, and a smoke client exercises the windowed round-trip including a
well-formed artifact reference that routes to the resolver. This is the
load fix for Harbor's own Console and the substrate every generic chat
client inherits.

This phase depends tightly on Phase 124 (the durable, gap-free event
stream): a windowed backward read is only meaningful against a log with no
holes. 124 lands the gap-free substrate; 125 reads it.

## RFC anchor

- RFC §5.2 — What the Protocol exposes (the State-snapshots row names
  `state.history` / `state.list_trajectories` as the surface this phase
  delivers the first of).
- RFC §6.10 — Heavy outputs travel by reference: a replayed event whose
  payload exceeded the heavy-output threshold carries an artifact
  reference, never inline bytes. RFC §6.5 (the context-window safety net /
  heavy-output threshold) is the sizing rule the offload already honors at
  emit time; the windowed read surfaces that reference as a routable ref.
- RFC §6.9 — Sessions and SessionManager (the `(tenant, user, session)`
  triple the window is keyed and scoped by; reopen-after-close reads a
  durable record, not a live resume).
- RFC §6.13 — the StateStore-backed durable event log the
  `events/drivers/durable` driver persists, keyed `(session, sequence)` —
  the gap-free substrate the windowed read scans.

## Briefs informing this phase

- brief 05

## Brief findings incorporated

- brief 05 §"state/tasks/artifacts/sessions" (the generic StateStore
  surface, settled as D-027): consuming surfaces land their typed reads at
  their own layer atop the durable log — this phase adds a windowed read
  capability to the event-bus seam + a Protocol projection handler, NOT a
  new persistence API inside `internal/state`. The durable log
  (`internal/events/drivers/durable`) already persists the per-session
  ordered sequence list; the window read scans it. The leaf stays a leaf.
- brief 05 §14 (the isolation triple): "A session contains many runs
  (foreground tasks). Identity is the triple `(tenant_id, user_id,
  session_id)`." — `state.history` is keyed and scoped by exactly this
  triple; a non-admin caller is hard-scoped to its own `(tenant, user)`
  and a cross-tenant read requires the verified `auth.ScopeAdmin` claim,
  mirroring `sessions.list` (`lister_projector.go:50-71` /
  `protocol.go:188-223`) and the SSE event stream's identity edge
  (`stream.go::resolveIdentity` / the `?admin=1` scope gate at
  `stream.go:218-224`).

## Findings I'm departing from (if any)

None. The phase narrows RFC §5.2's State-snapshots row to its first
method (`state.history`); `state.list_trajectories` and
`state.load_planner_checkpoint` are explicit non-goals (their own later
phases), which is scoping, not a departure. The earlier v16 draft of this
plan sourced the transcript from `MemoryStore`; that approach is dropped
(the inert-feature trap — `StrategyNone` makes `AddTurn` a no-op, so memory
holds no transcript), and the substrate is the durable event stream the
Console already reduces.

## Goals

- Register one new canonical Protocol method, `state.history`,
  single-sourced across `internal/protocol/methods` (the name) and
  `internal/protocol/types` (the wire shapes), reusing existing
  `internal/protocol/errors` codes — never a second definition site. Add
  the `IsStateMethod` O(1) predicate and a `CapStateSnapshots` capability
  advertised through the existing `VersionHandshake`.
- Add the windowed-read capability to the event-bus seam: an optional
  `events.HistoryReplayer` interface — sibling to the existing
  `events.Replayer` (forward-from-cursor) — with `Bounds` (discover
  head/tail sequence for a session) and `Window` (bounded backward read:
  `Sequence < before`, most-recent K, returned oldest-first). Both V1
  replay-capable drivers implement it: `events/drivers/durable` (scans the
  persisted per-session sequence list) and `events/drivers/inmem` (the
  best-effort ring).
- Return a **bounded page of flat wire events** (`StateEvent`), oldest-first
  within the window, plus the discovered `HeadSequence` / `TailSequence`
  and a `NextCursor` (the lowest sequence in the page — the scroll-up
  cursor). Reduction to chat messages stays client-side on the inherited
  reducer; the surface returns events, not pre-reduced turns.
- Carry heavy content **by reference, routably**: a replayed event whose
  payload was offloaded above the heavy-output threshold (RFC §6.5 / §6.10)
  surfaces a flat `StateArtifactRef` with a content-addressed `ID`
  (+ `SHA256` / `SizeBytes` / `MimeType` / `Filename`). The `ID` is
  well-formed and routes to the `artifacts.get_ref` resolver: on an
  S3-compat Presigner store the resolver returns a presigned URL; on the
  default CGo-free stores (inmem/fs — deliberately NON-Presigner) it returns
  the typed `CodePresignUnsupported` (HTTP 501), which still proves the id
  reached the resolver well-formed. This deliberately does NOT reuse
  `ArtifactRefSummary` (metadata-only, no `ID`/`SHA256` — unroutable); it
  follows the `SearchArtifactRef` / `MemoryArtifactRef` flat-ref precedent.
- Be **honest about retention gaps, never silently lossy**: when the
  durable log's retained head sits above the session's first sequence
  (retention trimmed older events), the response sets `Truncated: true` —
  the gap is surfaced, never a silent drop (CLAUDE.md §5 fail-loud / §13 no
  silent degradation). An un-projectable event fails loud with
  `CodeRuntimeError`.
- Be identity-mandatory and fail-closed at the wire edge: an incomplete
  triple is `CodeIdentityRequired` (401); `adminScoped` derives **solely**
  from `auth.HasScope(ctx, ScopeAdmin)` on the verified ctx (the request
  body carries NO elevation knob — D-219); a cross-identity or unknown
  `session_id` is `CodeNotFound` (404 — existence is never revealed across
  identities, mirroring `tasks.get`).
- Ship the first consumer in the same wave, wired through the real
  production + live-dev boot path: the open-source Console session-reopen
  hydration is rewired from its current full-load reconstruction to
  tail-first windowed + scroll-up-by-cursor, and the LIVE preflight dev
  server returns OK (not SKIP) for the windowed round-trip including a
  well-formed artifact ref that routes to the resolver.
- Decide and document the ProtocolVersion impact (additive surface
  addition — no version-string bump; capability advertised through the
  handshake). See "Risks / open questions".

## Non-goals

- `state.list_trajectories` and `state.load_planner_checkpoint` (the
  other two State-snapshots methods in RFC §5.2) — their own later
  phases. This phase ships `state.history` only.
- Any new persistence inside `internal/state` — the windowed read is a
  scan over the already-shipped durable event log
  (`internal/events/drivers/durable`, the per-session sequence list) plus
  the existing `artifacts.get_ref` resolver. No new `StateStore` method, no
  new migration.
- Server-side reduction of events into chat turns. The surface returns
  flat wire events; the events → messages reduction stays client-side on
  the reducer the Console already owns (`trajectory.ts` and the
  Playground's `chunk-stream` / `answer-envelope` / `wire-events`
  reducers). Moving reduction server-side would fork a second transcript
  model from the live stream's — explicitly avoided.
- Mutating verbs. `state.history` is read-only; there is no
  edit/append/delete counterpart at V1.
- Replacing the live event stream. `events.subscribe` (SSE) stays for the
  live tail; `state.history` is the bounded historical-page read. The
  forward-only `replayFromCursor` reconnect path stays for SSE reconnect;
  this phase adds the backward-window read alongside it, not in place of
  it.
- Substring / semantic search over event content — that is `search.events`.

## Acceptance criteria

- [ ] `methods.MethodStateHistory = "state.history"` is registered in
      `canonicalMethods` and a new `canonicalStateMethods` set;
      `IsStateMethod(MethodStateHistory)` returns `true` and
      `IsControlMethod` / `IsSearchMethod` / `IsSessionsMethod` /
      `IsStreamingEventsMethod` / `IsMemoryMethod` all return `false`. The
      name is defined in exactly one place (single-source lint stays a
      no-op).
- [ ] `internal/protocol/types/state.go` declares `StateHistoryRequest`
      (`Identity` / `SessionID` / `Before uint64` / `Limit int`),
      `StateEvent` (the flat exported wire-event projection — `Type` /
      `Sequence` / `OccurredAt` / `Tenant` / `User` / `Session` / `Run` /
      `Payload any` / `Extra` / `Artifacts []StateArtifactRef`),
      `StateArtifactRef` (the flat **routable** ref — `ID` /
      `MimeType` / `SizeBytes` / `Filename` / `SHA256`, mirroring
      `SearchArtifactRef`), `StateHistoryResponse` (`Events` /
      `HeadSequence` / `TailSequence` / `NextCursor uint64` / `HasMore` /
      `Truncated`), and the `DefaultStateHistoryLimit` (50) /
      `MaxStateHistoryLimit` (200) bounds. `StateArtifactRef` is a NEW flat
      type — `ArtifactRefSummary` (no `ID`/`SHA256`) is explicitly NOT
      reused.
- [ ] `types.CapStateSnapshots = "state_snapshots"` is added to
      `canonicalCapabilities`; `CurrentHandshake().Accepts(CapStateSnapshots)`
      is `true` on a runtime that wires the surface, and the
      `ProtocolVersion` string is UNCHANGED at `0.1.0` (additive
      minor-class addition — `version.go` `Version.Minor`: "bumped on a
      backward-compatible surface addition (a new method, a new
      capability, a new optional wire field)"; justified below).
- [ ] `internal/events` gains the optional `HistoryReplayer` capability
      interface (`Bounds` + `Window`) + an `ErrNoHistory` sentinel, sibling
      to `Replayer`. `events/drivers/durable` implements it over the
      persisted per-session sequence list (gap-free per Phase 124);
      `events/drivers/inmem` implements it over the ring. Both
      implementations are identity-scoped exactly like `Subscribe`/`Replay`
      (empty-triple non-admin filters → `ErrIdentityScopeRequired`), and
      `Window` returns at most `limit` events with `Sequence < before`
      (`before == 0` ⇒ from the tail), oldest-first within the window.
- [ ] `internal/protocol/transports/stream/state_history_handler.go`
      exposes `POST /v1/state/history`: resolve identity at the edge,
      derive `adminScoped` SOLELY from `auth.HasScope(r.Context(),
      auth.ScopeAdmin)` (no request elevation knob — D-219), assert the bus
      implements `events.HistoryReplayer`, dispatch `Bounds` + `Window`,
      project events → `StateEvent` with routable `StateArtifactRef`
      (enriched best-effort from the artifact store), encode. An incomplete
      triple → `CodeIdentityRequired` (401); a cross-identity/unknown
      session → `CodeNotFound` (404 — never `CodeScopeMismatch`, no
      existence leak); a bad `limit`/`before` → `CodeInvalidRequest` (400);
      a bus without the window capability → `CodeRuntimeError` (surfaced
      loud, never a silent empty page). The route is OPTIONAL in `NewMux`
      (a nil/disabled option does not mount it, so `skip_if_404` keeps
      preflight green on a partial build).
- [ ] A turn-by-turn page carries a well-formed, routable ref end-to-end: a
      replayed event whose payload was offloaded above the heavy-output
      threshold carries `StateEvent.Artifacts[i].ID` non-empty, and that
      `ID` ROUTES to the `artifacts.get_ref` resolver — the resolver returns
      EITHER HTTP 200 + `presigned_url` (an S3-compat Presigner store) OR the
      typed `CodePresignUnsupported` (HTTP 501 — the default CGo-free
      inmem/fs stores, which are deliberately NON-Presigner per
      `internal/artifacts/presigner_test.go`; the 501 proves the id reached
      the resolver well-formed). The actual 200-resolves assertion is gated
      behind a `HARBOR_LIVE_*` S3 env (CLAUDE.md §17.8). Optionally the test
      confirms the artifact's existence via `artifacts.list`. No inline heavy
      bytes ever travel through the surface. The Console mirrors the existing
      fallback-Download degradation when the resolver returns 501.
- [ ] The windowed read is wired through BOTH real boot paths:
      `cmd/harbor/cmd_dev.go::bootDevStack` (the live `harbor dev` server)
      and `harbortest/devstack.Assemble` get a `transports.WithStateHistory`
      wiring against the durable bus + artifact store — NOT just `httptest`.
      (The §17.6 dual-site lesson: a fix that lands only on the devstack
      seam and misses `cmd/harbor` silently ships an inert production
      surface.)
- [ ] The hand-maintained TS client gains `state.history`'s wire types in
      `web/console/src/lib/protocol/state.ts` and `make protocol-ts-gen`
      regenerates `web/console/src/lib/protocol/wire-manifest.gen.json`;
      `make protocol-ts-gen-check` (D-223 lockstep) passes. The Console
      session-reopen hydration (`hydratePastTurns` in the Playground
      `web/console/src/routes/(console)/playground/[session_id]/+page.svelte`,
      which today full-loads via `tasks.list` + N×`tasks.get`) is rewired to
      call `state.history` tail-first through the typed `HarborClient` (no
      hand-rolled `fetch`) and scroll-up by `NextCursor`; the
      `trajectory.ts` header comment is reworded to cite the now-shipped
      surface (the live-stream reducer it hosts is unchanged).
- [ ] `make protocol-docs-gen` regenerates the published Protocol
      reference (`docs/site/protocol/methods.md`) with the `state.history`
      row; `make protocol-docs-gen-check` (Phase 113a lockstep) passes. The
      `use-the-harbor-protocol` SKILL (+ its `docs/site` stub) gains a
      `state.history` mention (§18 — a new Protocol method is a documented
      operator surface, `surface: protocol`).
- [ ] An integration test wires the REAL durable `EventBus` (over an inmem
      `StateStore`) + REAL `ArtifactStore` behind the REAL
      `StateHistoryHandler` over `httptest`, seeds an ordered event set + a
      heavy offloaded payload, asserts the tail-first windowed page +
      scroll-up by `NextCursor` reaches the head, asserts a returned
      `StateArtifactRef.ID` ROUTES to `artifacts.get_ref` (accepting 200 on
      a Presigner store OR the typed `CodePresignUnsupported`/501 on the
      default inmem/fs store — the well-formed-id contract; the actual
      200-resolves leg gated behind `HARBOR_LIVE_*` S3 env per §17.8),
      proves identity propagation, covers ≥1 failure mode (missing identity
      401 + cross-tenant 404), and runs an N≥10 concurrency stress under
      `-race`.
- [ ] A D-025 concurrent-reuse test: N≥100 concurrent `state.history` calls
      against one shared `StateHistoryHandler` + bus under `-race`,
      asserting no data races, no context bleed across identities, no
      cross-cancellation, no goroutine leak (baseline-restored).
- [ ] `scripts/smoke/phase-125.sh` exercises the live `POST /v1/state/history`
      tail-first windowed round-trip (incl. a routable artifact ref whose
      `.id` ROUTES to `artifacts.get_ref` — accepting 200+`presigned_url`
      OR the typed `CodePresignUnsupported`/501 on the default inmem/fs
      store), the identity-mandatory 401, the cross-tenant 404 (pinned to
      404 exactly — a 403 would green-light an existence leak), and the
      single-source / no-Console-import static guards; FAIL = 0; OK > 0 once
      the surface lands (a SKIP-that-should-be-OK on the live server is a
      bug — §17.6).

## Files added or changed

```text
internal/protocol/methods/methods.go        # + MethodStateHistory, canonicalStateMethods, IsStateMethod
internal/protocol/methods/methods_test.go    # + predicate + registration coverage
internal/protocol/types/state.go             # NEW — state.history wire types (StateEvent/StateArtifactRef/...)
internal/protocol/types/state_test.go        # NEW
internal/protocol/types/version.go           # + CapStateSnapshots (no ProtocolVersion bump)
internal/protocol/types/version_test.go      # + capability coverage
internal/events/events.go                    # + HistoryReplayer interface + ErrNoHistory sentinel
internal/events/events_test.go               # + interface assertions
internal/events/drivers/durable/durable.go   # + Bounds/Window over the persisted per-session sequence list
internal/events/drivers/durable/durable_test.go
internal/events/drivers/inmem/inmem.go       # + Bounds/Window over the ring
internal/events/drivers/inmem/inmem_test.go
internal/protocol/transports/stream/state_history_handler.go      # NEW — POST /v1/state/history
internal/protocol/transports/stream/state_history_handler_test.go # NEW
internal/protocol/transports/transports.go   # + WithStateHistory option + StateHistoryRoutePattern mount
internal/protocol/transports/transports_test.go
internal/protocol/transports/concurrent_test.go  # + N>=100 D-025 reuse for the new handler
internal/protocol/conformance/conformance.go # + state.history in the method-registration set
cmd/harbor/cmd_dev.go                        # + transports.WithStateHistory(bus, artStore) in bootDevStack
harbortest/devstack/devstack.go              # + transports.WithStateHistory wiring in Assemble
cmd/harbor-gen-protocol-docs/methods.go      # + state.history docs join row (lockstep)
docs/site/protocol/methods.md                # regenerated (make protocol-docs-gen)
docs/skills/use-the-harbor-protocol/SKILL.md # + state.history mention (§18)
docs/site/protocol/<skill-stub>              # the SKILL site stub, if the §18 change adds a navigable entry
web/console/src/lib/protocol/state.ts        # NEW — hand-mirrored wire types
web/console/src/lib/protocol/wire-manifest.gen.json  # regenerated (make protocol-ts-gen)
web/console/src/lib/protocol/client.ts       # + state.history typed client method
web/console/src/lib/sessions/history.ts      # NEW — tail-first windowed hydration helper (scroll-up by cursor)
web/console/src/lib/sessions/trajectory.ts   # header comment reworded (surface no longer Pending)
web/console/src/routes/(console)/playground/[session_id]/+page.svelte  # rewire hydratePastTurns to state.history
test/integration/phase125_state_history_test.go     # NEW — real-driver E2E + ref-routes + concurrency stress
scripts/smoke/phase-125.sh                   # NEW
docs/plans/phase-125-session-state-history-surface.md  # this plan
docs/decisions.md                            # D-254
docs/glossary.md                             # windowed event-replay, state-history surface, scroll-up cursor
docs/plans/README.md                         # Phase 125 row Pending -> (ships) Shipped
README.md                                    # Status table Phase 125 + State-snapshots surface pointer
```

No new top-level directory (AGENTS.md §3 unchanged). The State-snapshots
surface lives in the existing `internal/protocol` + `internal/events`
trees; the §4.4 seam is the `events.HistoryReplayer` optional capability
(durable + inmem drivers), sitting beside the existing `events.Replayer`
precedent.

## Public API surface

```go
// internal/protocol/methods
const MethodStateHistory Method = "state.history"
func IsStateMethod(m Method) bool

// internal/protocol/types
const (
    DefaultStateHistoryLimit = 50
    MaxStateHistoryLimit     = 200
)
type StateHistoryRequest struct {
    Identity  IdentityScope `json:"identity"`
    SessionID string        `json:"session_id"`
    // Before is the exclusive upper bound: only events with
    // Sequence < Before are returned. Zero means "from the tail"
    // (the newest retained events).
    Before uint64 `json:"before,omitempty"`
    // Limit is the window size K (clamped to MaxStateHistoryLimit;
    // zero ⇒ DefaultStateHistoryLimit).
    Limit int `json:"limit,omitempty"`
}
// StateArtifactRef is the flat, routable artifact reference a replayed
// heavy-payload event carries — routed via artifacts.get_ref (which
// returns a presigned URL on an S3-compat store, or the typed
// presign_unsupported on the default inmem/fs stores). Mirrors
// SearchArtifactRef / MemoryArtifactRef; deliberately NOT
// ArtifactRefSummary (which has no ID/SHA256).
type StateArtifactRef struct {
    ID        string `json:"id"`
    MimeType  string `json:"mime_type,omitempty"`
    SizeBytes int64  `json:"size_bytes,omitempty"`
    Filename  string `json:"filename,omitempty"`
    SHA256    string `json:"sha256,omitempty"`
}
// StateEvent is the flat, single-sourced exported wire projection of one
// durable event — the same field set the SSE wireEvent projection carries,
// plus routable artifact refs. The client reduces a page of these into
// chat messages.
type StateEvent struct {
    Type       string             `json:"type"`
    Sequence   uint64             `json:"sequence"`
    OccurredAt time.Time          `json:"occurred_at"`
    Tenant     string             `json:"tenant"`
    User       string             `json:"user"`
    Session    string             `json:"session"`
    Run        string             `json:"run,omitempty"`
    Payload    any                `json:"payload,omitempty"`
    Extra      map[string]string  `json:"extra,omitempty"`
    Artifacts  []StateArtifactRef `json:"artifacts,omitempty"`
}
type StateHistoryResponse struct {
    Events       []StateEvent `json:"events"`
    HeadSequence uint64       `json:"head_sequence"`
    TailSequence uint64       `json:"tail_sequence"`
    // NextCursor is the lowest Sequence in this page — the value to pass
    // back as Before to scroll one window older. Zero when the head is
    // reached (no older events).
    NextCursor uint64 `json:"next_cursor"`
    HasMore    bool   `json:"has_more"`
    // Truncated is true when the durable log's retained head sits above
    // the session's first sequence (retention trimmed older events) — the
    // honest gap signal, never a silent drop.
    Truncated bool `json:"truncated,omitempty"`
}
const CapStateSnapshots Capability = "state_snapshots"

// internal/events
type HistoryReplayer interface {
    // Bounds returns the lowest (head) and highest (tail) retained
    // sequence for the filter's session, or ErrNoHistory when the session
    // has no events. Identity-scoped exactly like Subscribe/Replay.
    Bounds(ctx context.Context, f Filter) (head, tail uint64, err error)
    // Window returns at most limit events whose Sequence < before
    // (before==0 ⇒ from the tail), most-recent selected, returned
    // oldest-first within the window, matching f.
    Window(ctx context.Context, before uint64, limit int, f Filter) ([]Event, error)
}
var ErrNoHistory = errors.New("events: session has no retained event history")

// internal/protocol/transports/stream
const StateHistoryRoutePattern = "POST /v1/state/history"
func NewStateHistoryHandler(bus events.EventBus, arts artifacts.ArtifactStore, opts ...StateHistoryOption) (*StateHistoryHandler, error)

// internal/protocol/transports
func WithStateHistory(bus events.EventBus, arts artifacts.ArtifactStore) Option
```

## Test plan

- **Unit:** `methods` — `IsStateMethod` true for `state.history`, false
  for every other predicate; registration present in `canonicalMethods`.
  `types` — limit-bound clamping, JSON round-trip of all four shapes,
  `StateArtifactRef` carries `ID`/`SHA256` (the routable-ref contract).
  `events` (durable + inmem) — `Bounds` returns head/tail; `Window`
  returns the most-recent K with `Sequence < before`, oldest-first;
  `before==0` returns the tail window; an empty session returns
  `ErrNoHistory`; empty-triple non-admin filter → `ErrIdentityScopeRequired`.
  `state_history_handler` — identity-edge rejection (401), `adminScoped`
  derived from ctx scope only, cross-identity/unknown session (404, NOT
  403-with-existence-leak), bad limit/before (400), bus-without-capability
  (`CodeRuntimeError`), success shape + routable-ref projection; the
  `errors.Code → HTTP status` mapping.
- **Integration:** `test/integration/phase125_state_history_test.go` —
  REAL durable `EventBus` (over inmem `StateStore`) + REAL `ArtifactStore`
  behind the REAL `StateHistoryHandler` over `httptest.Server`; publish an
  ordered event set including one heavy offloaded payload; assert the
  tail-first window page, scroll-up by `NextCursor` until the head is
  reached, that a returned `StateArtifactRef.ID` ROUTES to
  `artifacts.get_ref` (accepting the typed `CodePresignUnsupported`/501 on
  the default inmem/fs store as proof the id reached the resolver
  well-formed; the actual 200+`presigned_url` leg gated behind a
  `HARBOR_LIVE_*` S3 env per §17.8), optionally confirm existence via
  `artifacts.list`, identity propagation through the edge, the
  missing-identity 401 + cross-tenant 404 failure modes, under `-race` with
  an N≥10 concurrency stress.
- **Conformance:** the `internal/protocol/conformance` method-registration
  suite gains `state.history`; the `singlesource` checker already covers
  the new `state.go` + the handler (no method string / error code / wire
  type redefined). The durable + inmem drivers run the existing events
  conformance suite plus the new `HistoryReplayer` assertions (both V1
  replay-capable drivers implement the capability).
- **Concurrency / leak:** `internal/protocol/transports/concurrent_test.go`
  — D-025 N≥100 concurrent `state.history` calls against one shared
  handler + bus under `-race`, distinct identities, asserting no cross-identity
  bleed and `runtime.NumGoroutine()` baseline restoration.

## Smoke script additions

- `scripts/smoke/phase-125.sh` (PREFLIGHT_REQUIRES: live-server):
  1. `assert_post_status 200 "$(api_url /v1/state/history)" '<seeded body, Before=0>'` —
     a tail-first windowed round-trip; `skip_if_404` keeps preflight green
     on a partial build (route unmounted).
  2. `assert_json_path` over the response: `.events | length > 0`, the
     last event `.events[-1].sequence` equals `.tail_sequence` (tail-first
     window), and `.head_sequence` ≤ `.next_cursor` ≤ `.tail_sequence`.
  3. Ref routes: pluck `.events[].artifacts[0].id` (the seeded heavy turn),
     assert it is non-empty, then POST that id to
     `$(api_url /v1/artifacts/get_ref)` and assert the resolver ROUTES the
     id — accept EITHER `200` (+ a `presigned_url`, an S3-compat Presigner
     store) OR `501`/`presign_unsupported` (the default CGo-free inmem/fs
     store — proves the id reached the resolver well-formed). The actual
     `200`-resolves assertion runs only when a `HARBOR_LIVE_*` S3 env is set
     (CLAUDE.md §17.8); otherwise the 501 is the green path. Optionally
     confirm the artifact exists via `artifacts.list`.
  4. Identity-mandatory: `assert_post_status 401 "$(api_url /v1/state/history)" '{}' "state.history rejects identity-less body"`.
  5. Cross-tenant gate: `assert_post_status 404 …` (pinned to 404 EXACTLY —
     a 403 would green-light an existence leak; D-219) for a body whose
     `identity.tenant` differs from the verified tenant without the admin
     scope — existence is not leaked.
  6. Static guards (always run, never skip): `assert_grep_count 1` for
     `MethodStateHistory Method = "state.history"` under
     `internal/protocol/methods`; `assert_grep_absent` for the literal
     `"state.history"` anywhere under `internal/` outside `methods.go`;
     `assert_grep_absent` for a Console import inside
     `internal/protocol/transports/stream`.
- New `scripts/smoke/common.sh` helper (if not already present):
  `assert_json_path_resolves <url> <jsonpath> <get_ref_url>` — extracts a
  ref id at `<jsonpath>` from the last response, POSTs it to
  `<get_ref_url>`, and asserts the id ROUTES (HTTP 200 on a Presigner
  store, or 501/`presign_unsupported` on the default inmem/fs store — both
  prove the id reached the resolver well-formed; the strict 200 assertion is
  gated behind `HARBOR_LIVE_*` S3 env). One-line docstring per §4.2.3.

## Coverage target

- `internal/protocol/types`: ≥ 85%
- `internal/protocol/methods`: ≥ 85%
- `internal/events` (+ durable + inmem driver deltas): ≥ 85% on touched files
- `internal/protocol/transports/stream`: ≥ 85% (the new handler)

## Dependencies

- **Phase 124 (tight dep)** — the durable, gap-free event stream. A
  bounded backward window is only correct against a hole-free log; 124
  lands the gap-free substrate, 125 reads it.
- Phase 72 / 72a — the SSE event transport (`stream.go`) + `events.Replayer`
  the windowed read sits beside (`replayFromCursor`, `frame.go` `id:`
  cursor), and the durable event-log driver (RFC §6.13) the window scans.
- Phase 72f / 73 — the Console Sessions/Playground surface + the
  `auth.HasScope` cross-tenant scope contract (`lister_projector.go` /
  `protocol.go`) the new handler mirrors.
- Phase 58 — `internal/protocol` single-source layout + the `singlesource`
  checker that gates the new `state.go` and the handler.
- Phase 60 — the SSE+REST wire transport + the `stream` handler posture
  (`resolveIdentity` / `auth.HasScope` edge) the new handler mirrors.
- Phase 113a — `cmd/harbor-gen-protocol-docs` + `protocol-docs-gen-check`
  (the new method's generated docs row).
- Phase 118 — the `make protocol-ts-gen-check` lockstep gate (D-223) the
  hand-mirrored TS wire types must satisfy.
- D-026 — the by-reference event payload shape (`artifact_ref { id, mime,
  size }`) the windowed read surfaces as a routable `StateArtifactRef`.

## Risks / open questions

- **ProtocolVersion impact — DECIDED: no bump.** Adding a canonical
  method + a capability is a backward-compatible surface addition. The taxonomy is
  pinned in `internal/protocol/types/version.go` (`Version.Minor`: "bumped
  on a backward-compatible surface addition (a new method, a new
  capability, a new optional wire field)"). The pinned `ProtocolVersion`
  string stays `0.1.0` while V1 is in flight — matching the `version.go`
  contract ("the streaming-events / state-snapshot / topology / artifacts /
  … surfaces extend it in later phases without bumping the major while V1
  is in flight") and the precedent of `CapEventsSubscribe` /
  `CapRuntimePosture` / `CapTopologySnapshot`, all added at `0.1.0`. A
  client negotiates the new surface via
  `VersionHandshake.Accepts(CapStateSnapshots)`, never by string-comparing
  the version. RFC §5.3 governs the rule that **bumping the pinned string
  is an RFC change** — which this phase explicitly does NOT do.
- **Substrate gap-freedom is Phase 124's contract.** A backward window read
  that lands on a hole returns a misleading page. This phase depends tightly
  on 124; the durable driver's `Window` reads the persisted per-session
  sequence list (already gap-free by construction per the durable driver's
  torn-write discipline), and fails loud (`CodeRuntimeError`) if the head
  lists a sequence whose entry record is missing — never serving a gap.
- **Retention vs completeness.** The durable log may be trimmed by a
  retention policy; the window read is honest about it via the `Truncated`
  flag (retained head above the session's first sequence) rather than
  silently presenting a trimmed window as complete. A future
  `state.list_trajectories` (the StateStore-backed append-only execution
  log) can deliver a deeper trace without a wire break — the response
  fields already carry the head/tail/cursor signal.
- **Heavy-payload ref extraction walks the redacted shape.** The handler
  surfaces `StateArtifactRef`s by reading the offloaded-payload
  `artifact_ref { id, mime, size }` shape the runtime already stamps
  (D-026). Because the durable event log persists events POST-redaction, the
  ref-extractor MUST walk the durable `RedactedMap` shape (the redacted
  payload the durable driver stored) to pull the artifact id/sha AFTER
  redaction — NOT the raw pre-redaction payload, which the windowed read
  never sees. It then enriches `SHA256`/`Filename`/`SizeBytes` best-effort
  from the artifact store. The exact stamping field path is verified at
  authoring against `internal/llm/safety.go` (the heavy-output edge), the
  durable driver's redacted-payload persistence, and the JS
  `isEventArtifactRef` narrower; the routable-ref contract (`.id` routes to
  `artifacts.get_ref`) is the acceptance gate regardless of the field path.
- **Cross-identity existence leak.** An unknown OR cross-identity
  `session_id` MUST return `CodeNotFound`, never a distinguishable
  `CodeScopeMismatch` for "exists but not yours" — mirror `tasks.get`. The
  smoke pins this to 404 EXACTLY (a 403 would green-light the leak; D-219).
  `adminScoped` is derived solely from `auth.HasScope(ctx, ScopeAdmin)` on
  the verified ctx (D-219); the request body carries no elevation knob.
  Verified by the integration test's cross-tenant case.
- **Ref dereference on the default stores.** The default CGo-free dev stores
  (inmem/fs) are deliberately NON-Presigner
  (`internal/artifacts/presigner_test.go`; `cmd/harbor/cmd_dev.go` defaults
  the dev store to inmem), so `artifacts.get_ref` returns the typed
  `CodePresignUnsupported` (HTTP 501), not a presigned URL — asserting a
  hard 200 in preflight would be infeasible. The gate is therefore "the id
  ROUTES to the resolver well-formed": accept 200+`presigned_url` (S3-compat
  Presigner store) OR 501/`presign_unsupported` (inmem/fs). The actual
  200-resolves assertion is gated behind a `HARBOR_LIVE_*` S3 env (CLAUDE.md
  §17.8). The Console mirrors its existing fallback-Download degradation when
  the resolver returns 501.
- Full §16 brief pass (brief 05 + RFC §5.2/§6.5/§6.9/§6.10/§6.13) when
  dispatched; the `use-the-harbor-protocol` skill gains a `state.history`
  mention in the same PR (§18 — a new Protocol method is a documented
  operator surface, `surface: protocol`).

## Glossary additions

- **windowed event-replay** — the `state.history` read shape: a bounded,
  tail-first backward page of a session's durable event stream (events with
  `Sequence < before`, the most-recent K, returned oldest-first), plus the
  discovered head/tail sequence and a scroll-up cursor. The substrate a
  generic Protocol client reduces into a reopened conversation.
- **state-history surface** — the `state.history` Protocol method: a
  by-id, identity-scoped, read-only windowed read of a session's durable
  event log, heavy content carried by routable `StateArtifactRef`.
- **scroll-up cursor** — `StateHistoryResponse.NextCursor`: the lowest
  sequence in a returned window, passed back as `Before` to fetch one
  window older. Zero when the retained head is reached.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes (incl. the live `state.history` round-trip
      returning OK, not SKIP)
- [ ] `make check-mirror` passes (no AGENTS.md/CLAUDE.md edits)
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test
      passes (the handler + bus window read are identity-scoped; concurrency
      test asserts no cross-identity bleed)
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent `state.history`
      calls against one shared handler + bus under `-race` (D-025).
- [ ] **Integration test exists** — `test/integration/phase125_state_history_test.go`
      wires the real durable bus + `ArtifactStore` end-to-end, asserts a
      returned ref id ROUTES to `artifacts.get_ref` (accepting the typed
      `CodePresignUnsupported`/501 on the default inmem/fs store as proof the
      id reached the resolver well-formed; the 200-resolves leg gated behind
      `HARBOR_LIVE_*` S3 env per §17.8), asserts identity propagation, covers
      ≥1 failure mode, runs under `-race`.
- [ ] **Live boot path wired** — `cmd/harbor/cmd_dev.go::bootDevStack` AND
      `harbortest/devstack.Assemble` both call `transports.WithStateHistory`
      (the §17.6 dual-site check — production not just devstack).
- [ ] `make protocol-ts-gen-check` passes (D-223 lockstep — TS wire types
      mirrored, manifest regenerated)
- [ ] `make protocol-docs-gen-check` passes (Phase 113a lockstep —
      `methods.md` regenerated); `use-the-harbor-protocol` SKILL updated (§18)
- [ ] If new vocabulary: glossary updated (3 terms above)
- [ ] If a brief finding was departed from: justified above + decisions.md
      entry filed — the MemoryStore-sourcing departure is recorded in D-254

---

## Implementation handoff

This appendix makes the plan turnkey: the exact index-row, decisions
entry, smoke assertions, and master-plan detail block an implementing
agent lands.

### (a) `docs/plans/README.md` index row (append in phase order)

```text
|125 | Session state-history windowed event-replay surface (ship the Pending RFC §5.2 `state.history` method — a by-id, identity-scoped, read-only TAIL-FIRST windowed read of the durable event stream: discover head/tail + a bounded backward page with a scroll-up cursor, heavy content by ROUTABLE artifact ref; client-side reduction; rewires the open-source Console session-reopen hydration off its full-load reconstruction; `CapStateSnapshots`, no ProtocolVersion bump; tight dep on 124; D-254) | internal/protocol + internal/events + web/console | §5.2, §6.5, §6.9, §6.10, §6.13 | 124, 72, 72a, 73, 58, 60, 113a, 118 | 85% | Pending (V1.6) |
```

### (b) `docs/decisions.md` entry (markdownlint-clean — blank lines around the heading and the `---` rules)

```markdown
---

## D-254 — `state.history` ships the State-snapshots surface as a tail-first windowed read of the durable event stream (not memory); additive (no ProtocolVersion bump)

**Date:** 2026-06-24

**Status:** Accepted

**Context.** RFC §5.2's State-snapshots row names `state.history` /
`state.list_trajectories` / `state.load_planner_checkpoint`, but no
`state.*` Protocol method existed. A generic Protocol client (third-party
console, IDE client, the SDK) — and Harbor's own open-source Console —
reopens a conversation by reducing the EVENT STREAM
(`web/console/src/lib/sessions/trajectory.ts` projects the
`events.subscribe` frames; the Playground `hydratePastTurns` replays prior
turns). The only replay primitive shipped was forward-only and unbounded:
`internal/protocol/transports/stream/stream.go::replayFromCursor` replays
every event with `Sequence > cursor` to the tail, and the SSE `id:` cursor
is the raw `Sequence` (`frame.go::encodeEvent`). Reopening a long session
therefore streams the entire history before the newest turn renders. An
earlier draft proposed sourcing a transcript from `MemoryStore`; that is an
inert-feature trap — the default `StrategyNone` makes `AddTurn` a no-op, so
memory holds no transcript. The durable, gap-free event log (Phase 124) is
the real substrate.

**Decision.**

1. **Ship `state.history` only — the first of the three State-snapshots
   methods — as a windowed event-replay read.** Single-sourced: the name in
   `internal/protocol/methods` (`MethodStateHistory` +
   `canonicalStateMethods` + `IsStateMethod`), the wire shapes in
   `internal/protocol/types/state.go` (`StateHistoryRequest` / `StateEvent`
   / `StateArtifactRef` / `StateHistoryResponse`), reusing existing
   `internal/protocol/errors` codes. `state.list_trajectories` and
   `state.load_planner_checkpoint` are deferred to their own phases.

2. **The substrate is the durable event stream, not memory.** Two reads are
   added to the event-bus seam as an optional `events.HistoryReplayer`
   capability (sibling to `events.Replayer`): `Bounds` (discover the
   session's head/tail sequence) and `Window` (a bounded backward read —
   events with `Sequence < before`, the most-recent K, returned
   oldest-first). Both V1 replay-capable drivers implement it
   (`events/drivers/durable` over the persisted per-session sequence list;
   `events/drivers/inmem` over the ring). MemoryStore sourcing is dropped
   (the `StrategyNone` inert-feature trap).

3. **Reduction stays client-side; the surface returns events.**
   `state.history` returns a page of flat `StateEvent` (the same field set
   the SSE `wireEvent` carries), plus `HeadSequence` / `TailSequence` / a
   `NextCursor` scroll-up cursor / `HasMore` / `Truncated`. The events →
   chat-messages reduction stays on the reducer the Console already owns
   (`trajectory.ts` + the Playground reducers) — no second server-side
   transcript model is forked from the live stream's.

4. **Heavy content by ROUTABLE reference.** A replayed event whose payload
   was offloaded above the heavy-output threshold (RFC §6.5 / §6.10)
   surfaces a flat `StateArtifactRef` with a content-addressed `ID` (+
   `SHA256` / `SizeBytes` / `MimeType` / `Filename`). The ref-extractor
   walks the durable `RedactedMap` shape (events are persisted
   post-redaction) to pull the id/sha after redaction. The `ID` routes to
   `artifacts.get_ref`: on an S3-compat Presigner store the resolver returns
   a presigned URL; on the default CGo-free inmem/fs stores (deliberately
   NON-Presigner per `internal/artifacts/presigner_test.go`) it returns the
   typed `CodePresignUnsupported` (HTTP 501) — which still proves the id is
   well-formed and reached the resolver. The Console mirrors its existing
   fallback-Download degradation on 501. This deliberately does NOT reuse
   `ArtifactRefSummary` (metadata-only, no `ID`/`SHA256` — unroutable); it
   follows the `SearchArtifactRef` / `MemoryArtifactRef` precedent. No
   inline heavy bytes ever travel through the surface.

5. **Identity-mandatory, fail-closed.** A non-admin caller is hard-scoped to
   its own `(tenant, user, session)`; `adminScoped` is derived SOLELY from
   `auth.HasScope(ctx, ScopeAdmin)` on the verified ctx — the request body
   carries no elevation knob (D-219). An unknown or cross-identity session
   is `CodeNotFound` (404 — existence is never revealed across identities;
   the smoke pins this to 404 exactly, never 403), mirroring `tasks.get` /
   `sessions.list`. Honest about retention gaps via `Truncated`; an
   un-projectable event fails loud with `CodeRuntimeError` (CLAUDE.md §5 /
   §13).

6. **First consumer lands in the same wave (§13), through the real boot
   path.** The open-source Console session-reopen hydration
   (`hydratePastTurns` in the Playground `+page.svelte`, today a full-load
   via `tasks.list` + N×`tasks.get`) is rewired to call `state.history`
   tail-first through the typed `HarborClient` and scroll up by
   `NextCursor`. The surface is wired through BOTH
   `cmd/harbor/cmd_dev.go::bootDevStack` and `harbortest/devstack.Assemble`
   (the §17.6 dual-site lesson — not just `httptest`), and the LIVE
   preflight server returns OK for the windowed round-trip including a
   routable artifact ref.

7. **Additive — no ProtocolVersion bump.** A new method + the
   `CapStateSnapshots` capability is a backward-compatible surface addition
   (`internal/protocol/types/version.go` `Version.Minor` taxonomy). The
   pinned `ProtocolVersion` stays `0.1.0` while V1 is in flight (the
   version.go contract + the `CapEventsSubscribe` / `CapRuntimePosture` /
   `CapTopologySnapshot` precedent); clients negotiate via
   `VersionHandshake.Accepts(CapStateSnapshots)`. Bumping the pinned string
   is an RFC change (RFC §5.3) and is NOT done here.

**§4.3 deviations.** Departs from the earlier v16 draft's MemoryStore
sourcing (the `StrategyNone` inert-feature trap) — the substrate is the
durable event stream. Deliberate scoping to one of the three State-snapshots
methods. No RFC drift — RFC §5.2 already names the method.

**Cross-references.** RFC §5.2 (the State-snapshots row), §6.5 / §6.10 (the
heavy-output threshold + by-reference rule), §6.9 (the session triple),
§6.13 (the durable event log). `internal/protocol/types/version.go`
(additive-vs-breaking taxonomy). D-026 (the by-reference event payload
shape), D-219 (verified-ctx scope authority), D-223 (the TS lockstep gate),
D-209 (the generated Protocol-docs lockstep), D-027 (consumer-side typed
reads atop the durable substrate). brief 05. Tight dep: Phase 124 (the
gap-free durable stream). Plan:
`docs/plans/phase-125-session-state-history-surface.md`.
```

### (c) `scripts/smoke/phase-125.sh` assertions to add

- `PREFLIGHT_REQUIRES: live-server` header; source `scripts/smoke/common.sh`;
  `skip` cleanly when `curl` / `jq` are absent.
- Live tail-first round-trip: `assert_post_status 200 "$(api_url /v1/state/history)" "$BODY" "state.history tail-first window"` where `$BODY` carries the dev identity + a seeded `session_id` + `"before": 0` (skips on 404 when the route is unmounted on a partial build).
- Window shape: `assert_json_path '.events | length > 0' …`; assert `.events[-1].sequence == .tail_sequence` (tail-first) and `.head_sequence <= .next_cursor` (scroll-up cursor sane).
- Ref routes (the load-bearing gate): extract `.events[].artifacts[0].id` for the seeded heavy turn, assert non-empty, then POST that id to `$(api_url /v1/artifacts/get_ref)` and assert it ROUTES — accept `200` (+ `presigned_url`, an S3-compat Presigner store) OR `501`/`presign_unsupported` (the default CGo-free inmem/fs store; proves the id reached the resolver well-formed). The strict `200`-resolves assertion runs only under a `HARBOR_LIVE_*` S3 env (§17.8). The new `assert_json_path_resolves` helper wraps this; one-line docstring in `common.sh`. Optionally confirm existence via `artifacts.list`.
- Identity-mandatory: `assert_post_status 401 "$(api_url /v1/state/history)" '{}' "state.history rejects identity-less body"`.
- Cross-tenant gate: `assert_post_status 404 …` (pinned to 404 EXACTLY — a 403 would green-light an existence leak; D-219) for a body whose `identity.tenant` differs from the verified tenant without the admin scope (existence not leaked).
- Static guards (always run, never skip): `assert_grep_count 1` for `MethodStateHistory Method = "state.history"` under `internal/protocol/methods`; `assert_grep_absent` for the literal `"state.history"` anywhere under `internal/` outside `methods.go`; `assert_grep_absent` for a Console import inside `internal/protocol/transports/stream`.

### (d) `docs/plans/README.md` per-phase detail-block stub

```markdown
### Phase 125 — Session state-history windowed event-replay surface

**Subsystem:** internal/protocol + internal/events + web/console
**RFC:** §5.2 (State snapshots), §6.5/§6.10 (heavy-output by reference),
§6.9 (Sessions), §6.13 (durable event log).
**Deps:** 124 (tight — the gap-free durable stream), 72/72a (SSE + durable
event-log driver + `events.Replayer`), 73 (Sessions/Playground surface +
`auth.HasScope`), 58 (single-source), 60 (wire transport), 113a
(protocol-docs gen), 118 (TS lockstep gate).
**Decision:** D-254.

Ships the Pending `state.history` method from RFC §5.2's State-snapshots
row as a TAIL-FIRST WINDOWED read of the durable event stream (NOT memory —
the default `StrategyNone` makes `AddTurn` a no-op, so memory holds no
transcript). Adds the `events.HistoryReplayer` seam (`Bounds` discover
head/tail + `Window` bounded backward read) implemented by the durable +
inmem drivers, and a `state.history` Protocol handler that returns a page of
flat `StateEvent` (heavy content by ROUTABLE `StateArtifactRef`, not
`ArtifactRefSummary`) plus a scroll-up cursor. Reduction stays client-side.
Single-sourced across `methods` / `types`; advertised via a new
`CapStateSnapshots` capability with NO ProtocolVersion bump (additive
minor-class per the `version.go` taxonomy). The §13 first consumer lands in
the same wave through the real boot path: the open-source Console
session-reopen hydration is rewired off its full-load `tasks.list` +
N×`tasks.get` reconstruction to tail-first windowed + scroll-up, wired
through both `cmd/harbor dev` and `harbortest/devstack.Assemble`, with the
LIVE preflight returning OK for the windowed round-trip incl. a routable
artifact ref (resolving to a presigned URL on S3, or the typed
`CodePresignUnsupported`/501 on the default inmem/fs store).

**Risks:** substrate gap-freedom (Phase 124's contract; window reads the
gap-free persisted sequence list, fails loud on a missing entry); retention
vs completeness (surfaced via `Truncated`, not hidden); cross-identity
existence leak (return `CodeNotFound` — pinned to 404 exactly, never 403 or
`CodeScopeMismatch`; admin authority from verified ctx only — D-219); ref
dereference on the default CGo-free stores (inmem/fs are NON-Presigner, so
`artifacts.get_ref` returns the typed 501 — the gate asserts the id ROUTES
well-formed, with the 200-resolves leg gated behind `HARBOR_LIVE_*` S3 env
per §17.8). Status: Pending (V1.6).
```
