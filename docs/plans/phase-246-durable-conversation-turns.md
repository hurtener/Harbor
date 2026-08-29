# Phase 246 — Durable tail-paged conversation turns (HA-64)

## Summary

Add a dedicated, runtime-owned conversation read model —
`sessions.turns.list` (stable tail pages) and `sessions.turns.get` (one
`(session, task)` terminal reconciliation read) — the two named public
methods — so a Protocol consumer renders the
current chat from one durable projection instead of joining task/result/event/
App authority itself. Together with Phase 245's lifecycle read, a persisted
session reopens its latest turns in exactly two reads, independent of total
history.

**Production correction (v1.28.4).** The stock Runtime's root
`task.spawned` event deliberately has no envelope RunID; its per-task RunLoop
then emits planner/tool/App events under `RunID=TaskID`. The materializer must
derive only that exact already-spawned task/run binding, persist the rich
reasoning/activity/App components, and reproduce both the row and
`sessions.turns.get` bytes unchanged after durable restart. Explicit run
bindings and mismatches remain authoritative/fail-closed; raw reasoning, tool
arguments/results, App callback authority, and tool-context contents remain
absent from the turn row.

## RFC anchor

- RFC §5.2
- RFC §5.5
- RFC §6.2
- RFC §6.8
- RFC §6.9
- RFC §6.10
- RFC §6.13
- RFC §6.16
- RFC §7
- RFC §9

## Briefs informing this phase

- brief 05
- brief 06
- brief 07
- brief 11

## Brief findings incorporated

- brief 05 §4: tasks and sessions are durable identity-scoped records; the
  unified `TaskID` and lifecycle model are the projection's source authority.
- brief 06 §3/§4: the typed event bus is the canonical projection of runtime
  state; a projection derived from the durable log is the correct read model,
  and identity-triple filtering is server-enforced.
- brief 07 §5: ordered per-branch tool activity and per-step reasoning are
  first-class transcript content; raw tool arguments and results never enter a
  consumer payload (the `llm_observation` vs `observation` split).
- brief 11 §PG-1/§PG-4: a chat surface needs renderable per-message content —
  reasoning summaries, tool-call traces, usage, cost, and honest per-component
  availability — from Protocol data alone.

## Findings I'm departing from (if any)

- The current session-reopen path (the Console `history.ts` reducer over
  `state.history` windows, plus `tasks.get` and separately paged `events.list`
  joins per visible turn) is forensic reconstruction, not a consumer read
  model. D-425 does not deprecate `events.list` / `state.history` / `tasks.get`
  — they remain the raw drill-down surfaces — but it establishes a dedicated
  runtime-owned projection so a caller never reconstructs runtime state from
  forensic events. This is the same read-shape correction D-309 made for
  session counters, applied to conversation content.

## Goals

- Ship `sessions.turns.list` and `sessions.turns.get` as the two named public
  methods with the full contract below.
- Bounded Activity rides inline covering at least Harbor's configured per-turn
  tool-call budget; a separate named activity method is NOT a v1.28 method or
  acceptance — if the Protocol response ceiling forces the exact attachment
  contract, a future named fallback is recorded as a deferred follow-up.
- Build one dedicated, runtime-owned conversation-turn projection derived from
  Harbor's task, result, event, and App-context authority, incrementally
  materialized with idempotent sequence checkpoints, restart-survivable on
  durable drivers, and erased/fenced with its session.
- Guarantee indexed tail paging: work proportional to page size, a bounded
  constant number of storage operations, no full task enumeration, no
  request-path raw event scan, no per-row reads, and no skip/duplicate while a
  new turn starts.
- Make chat open two reads: one Phase 245 lifecycle read plus one turn-page
  read, with a gap-free page-to-live handoff.
- Ship a Protocol-only consumer in the same wave (the two-read chat-open
  acceptance and a minimal Console render path).

## Non-goals

- No generic projection framework, projection warehouse, or shadow transcript
  store; this is one dedicated projection with its own bounded schema.
- No operator analytics surface: the `operations` admin/fleet projection is
  deliberately NOT built here; no transcript content is exposed to admin/fleet
  observation, and no content-read/impersonation authority is created.
- No live-cursor redesign of `events.subscribe`: the page-to-live handoff is
  page-before-subscribe — the consumer folds the durable turn page and
  establishes bounded running/paused membership FIRST, then opens the
  EventSource with the page's `live_resume_seq` as the initial `resume_seq`;
  the server replays events strictly newer than that snapshot through the
  existing bounded replay source, and a browser reconnect `Last-Event-ID`
  takes precedence. Never a new streaming primitive.
- No overflow analytics: no unbounded overflow store, no per-request
  unbounded aggregation; the compact totals carry a declared cardinality cap
  plus an overflow bucket.
- No raw task/result/event/App payloads on the wire; no credentials, system
  prompt, provider stack, tool arguments, or tool results.
- No raising the 10,000-event bound, no reducing the client's initial turn
  count, no batching `tasks.get` while retaining event joins, no returning the
  whole trajectory, no reconstructing answers from completion chunks, and no
  summary cache.

## v1.29.4 production correction and release evidence

The v1.29.4 correction makes durable turn materialization advance through a
fully examined current source watermark after every returned canonical event.
Persisted bus-internal notices and fenced-session tails no longer repeat
global `state_records` prefix scans on an idle runtime. Concurrent final-fence
filtering preserves the page's pre-filter overflow proof, so a truncated page
cannot promote its checkpoint past a later canonical event. This stays within
D-425's existing bounded, durable, fail-closed convergence contract and adds
no Protocol wire surface or transcript authority.

Implementation PR #733 merged at
`90f5f8ce96f83f994462e33cdfeccc77c535ca7e`; hosted candidate run
`32620015889` completed successfully, including the live preflight,
PostgreSQL conformance, both Go platforms, Playwright, isolation, leak,
chaos, lint, docs, and examples. The immutable annotated `v1.29.4` tag
object is `d85ca3928171cbf5c72e890f7c4b622e4b2cf1ff` and peels to
`90f5f8ce96f83f994462e33cdfeccc77c535ca7e`; release workflow `32622414573`
succeeded, publishing [13 release assets](https://github.com/hurtener/Harbor/releases/tag/v1.29.4)
with verified aggregate `checksums.txt`, six sidecar checksums, and six
GitHub attestations. The native darwin/arm64 artifact reports Harbor v1.29.4,
Protocol 0.1.0, build `90f5f8ce96f83f994462e33cdfeccc77c535ca7e`; module
provenance records `Sum=h1:GNQ902D6ddXlYtiOmC+wGMN7LSbE7VQilFb5HggKUyU=`,
`GoModSum=h1:mlX6OoauN4FzVO6Bw2PZTvb3l1tf3y4WHYRzudiTkYg=`,
`Origin.Hash=90f5f8ce96f83f994462e33cdfeccc77c535ca7e`, and
`Origin.Ref=refs/tags/v1.29.4`. The post-tag scaffold pin and golden
fixtures are complete. Focused local
`go test ./cmd/harbor -run TestScaffold_Golden` and `make drift-audit`
passed; local `make preflight` was not run. No downstream runtime, fleet,
or database mutation is claimed.

## v1.29.5 production correction and release evidence

The v1.29.5 correction moves the turns materializer's lost-wake and
deferred-terminal-snapshot fallback from a two-second poll to a 30-second
poll. Durable event watches remain the fast path, and the bounded fallback
still catches up when the source watermark is ahead or a deferred terminal
snapshot needs reconciliation. An idle durable runtime consequently performs
two source-watermark reads per minute instead of thirty; the correction adds no
Protocol wire surface or transcript authority.

Implementation PR #735 merged at
`f0cd36b0c82f2332df575a5434b1a3e7a0d7a586`; hosted CI run `32628090701`
attempt 2 completed successfully on the exact reviewed implementation head
`280518aa36628ec602b668ea3b22fde1c082585f`, and documentation run
`32628090829` also completed successfully. The immutable annotated `v1.29.5`
tag object is `8aba749eadfc0919668bf0769796d26793181ba6` and peels to
`8540a26e70552d49acc8d7267f6c3c3a99cd9f5c`; release workflow `32635519880`
succeeded, publishing [13 release assets](https://github.com/hurtener/Harbor/releases/tag/v1.29.5)
with verified aggregate `checksums.txt`, six sidecar checksums, and six GitHub
attestations. The native darwin/arm64 artifact reports Harbor v1.29.5,
Protocol 0.1.0, build `8540a26e70552d49acc8d7267f6c3c3a99cd9f5c`; module
provenance records `Sum=h1:iD6KARsZ3yWkLoiQeHvdCoLpEtQ0T9F8deC169S6280=`,
`GoModSum=h1:mlX6OoauN4FzVO6Bw2PZTvb3l1tf3y4WHYRzudiTkYg=`,
`Origin.Hash=8540a26e70552d49acc8d7267f6c3c3a99cd9f5c`, and
`Origin.Ref=refs/tags/v1.29.5`. The post-tag scaffold pin and golden
fixtures are complete. Focused local `go test ./cmd/harbor -run
TestScaffold_Golden`, `make drift-audit`, `make markdownlint`, and `make docs`
passed; local `make preflight` was not run. No downstream runtime, fleet, or
database mutation is claimed.

## Acceptance criteria

- [ ] `sessions.turns.list` returns a stable tail page of root foreground
      conversation turns for the verified session; `sessions.turns.get`
      returns the same turn shape for one `(session, task)` and is the bounded
      terminal reconciliation read after live streaming. An equivalent
      namespaced method is acceptable only if it ships every semantic below.
- [ ] The request carries `session_id`, an opaque exclusive older-page cursor,
      `limit` (default 20, maximum 50), and the authorized projection. The
      response carries a lightweight session header, a snapshot/as-of
      identifier, the page turns in a declared stable order,
      `next_older_cursor`, `has_more`, an optional remaining-older count with
      `count_exact`, a live resume cursor, and page completeness/partial
      reasons. The client never invents sort authority.
- [ ] The opaque cursor is snapshot/keyset anchored with an immutable
      task/turn tie-breaker. Appending a new turn while paging older history
      produces neither duplicates nor omissions. Invalid, foreign,
      retention-expired, and snapshot-expired cursors have distinct typed
      outcomes. Backend work is proportional to page size, independent of total
      event/turn cardinality, with a bounded constant number of storage
      operations, no full task enumeration, no request-path raw event scan, and
      no per-row reads (asserted by instrumentation, not timing).
- [ ] One row is one root foreground user turn; background/child tasks are
      folded into that root turn's Activity or omitted by an explicit
      relationship rule, never surfaced as invented user messages. Each row
      carries immutable task/run id, stable turn ordering key,
      foreground/root task kind, effective agent id with an explicit
      defaulted/unknown indication, authoritative user query and input
      timestamp, and ordered input attachment metadata (immutable artifact id,
      consumer disposition, safe filename/MIME/size, reference availability).
      Attachment bytes stay a lazy authorized read; an attachment-only turn is
      still a real turn.
- [ ] Each turn carries a closed status enum (`pending`, `running`, `paused`,
      `complete`, `failed`, `cancelled`), start/update/finish timestamps,
      duration, and a bounded terminal reason (completion, failure,
      policy/step exhaustion, user cancellation, kill switch,
      context/runtime cancellation) with error class and a redacted
      consumer-safe message but no stack/provider dump.
- [ ] The answer is an explicit union: bounded inline result OR artifact
      reference (`artifact_id`, MIME, size), or `empty`, `evicted`,
      `unavailable`. A failed read never fabricates an empty answer; a
      referenced heavy answer may trigger one lazy authorized artifact-byte
      read when visible and stays visibly pending/unavailable until it
      resolves.
- [ ] For a running turn, bounded accumulated assistant-content and reasoning
      snapshots are tied to the same turn version and
      `last_applied_event_sequence` as the live-resume cursor, so deltas
      emitted before reopen are not lost.
- [ ] Ordered consumer-safe reasoning steps (or one canonical ordered reasoning
      value), each with a stable index and a shared monotonic Activity
      sequence; no raw planner event is exposed.
- [ ] Ordered tool activity entries carry invocation id, ordinal, planner step,
      the shared monotonic Activity sequence, attempt identity, parallel
      batch/group, qualified tool name, invoked/succeeded/failed/
      policy-exhausted status, start/finish/duration, attempt count, bounded
      error class and redacted summary — never arguments or results. Compact
      execution totals carry planner decisions, distinct invocations, attempts,
      serial batches, parallel batches, maximum fan-out, and per-tool
      success/failure/exhausted counts under a declared cardinality cap plus an
      overflow bucket.
- [ ] Model, prompt/output/total tokens, and cost are exposed with explicit
      availability/exactness. The Activity collection is required — counters
      alone cannot hydrate the existing Activity panel or explain parallel
      fan-out. The inline bound covers at least Harbor's configured per-turn
      tool-call budget. A separate named activity method is NOT a v1.28
      method or acceptance: if the Protocol response ceiling ever forces the
      exact attachment contract, this phase records a future named fallback
      as a deferred follow-up (with the same identity, snapshot, ordering,
      and no-arguments/no-results contract) rather than shipping it. Normal
      transcript render
      does not fetch any subresource; opening Activity may. There is no
      anonymous subresource and no silent truncation.
- [ ] Active intervention metadata needed to reopen a paused own-user turn is
      present, with a consumer-safe reason; the opaque action token/receipt is
      returned only when the verified caller also satisfies that pause's
      resume/approval/control tier; an exact-session owner lacking it receives
      non-actionable redacted status.
- [ ] Durable ordered MCP App references carry effective agent id, server id,
      resource URI, display mode, raw-HTML trust, and optional tool-call id /
      tool name. The replacement identity is exactly
      `(effective_agent_id, server_id, resource_uri)`: its first declaration
      fixes position, and a repeat replaces in place with the latest
      correlation metadata. App document bytes and captured tool input/result
      are deliberately excluded. The authoritative `mcp.app_available`
      declaration persists the App ref even when `tool_call_id` is absent; an
      absent id mounts the App without Data Delivery; a present id whose
      context exists delivers it; a present id whose context is missing,
      evicted, or cross-identity renders a stable unavailable placeholder. It
      never reruns the historical tool.
- [ ] Per-component exact/partial/unavailable state covers answer, reasoning,
      activity, Apps, and usage, plus projection version,
      `last_applied_event_sequence`, retention horizon, updated timestamp, and
      mutable-versus-terminal-sealed state. The persisted task lifecycle event
      is canonical for terminal status and its nonempty failure code. A
      task-record snapshot may fill a missing legacy event code, but a
      nonempty disagreement makes the snapshot's whole optional failure group
      unavailable and preserves the event-derived closed class. An optional
      historical terminal message that is invalid UTF-8, contains NUL/C0/DEL
      controls, or exceeds the projection bound is unavailable wholesale,
      never sanitized or truncated, and cannot stall later projection.
      When an already-sealed legacy row encounters a contradictory terminal
      lifecycle event at a strictly newer known sequence, the immutable first
      terminal row remains canonical while projection acknowledges the later
      event and advances its checkpoint; same-sequence conflicts, unknown
      ordering, and identity/task/run binding violations remain fail-closed.
- [ ] Durability and handoff: the projection is incrementally materialized with
      idempotent sequence checkpoints, reconciles after interruption, survives
      restart on durable drivers, and is erased/fenced with its session.
      In-memory restart loss remains explicit. In-flight rows carry a version
      and are mutable; terminal rows seal only after all required sources are
      applied — late task-record/answer convergence is BOUNDED (a bounded
      deferred-complete queue with a convergence budget and a lost-wake poll,
      never a blocking read) and DURABLE across an unchanged event watermark
      and restart (the durable unsealed row is re-read and re-converged);
      terminal completeness is never claimed from a one-time event read, and
      an unavailable/pending component stays in its explicit availability
      state, never hidden. `complete`, `partial`, `rebuilding`, `retention_gap`,
      `evicted`, and `unavailable` are distinguishable; a missing/stale
      projection never triggers an unbounded synchronous event rebuild during
      chat open. The snapshot-to-live handoff is page-before-subscribe: the
      list response's exclusive `live_resume_seq` becomes the initial
      `resume_seq` of the EventSource opened AFTER the durable page is folded
      and bounded running/paused membership is established; the server replays
      events strictly newer than that snapshot through the existing bounded
      replay source, and a browser reconnect `Last-Event-ID` takes precedence
      (one terminal event causes one `sessions.turns.get`
      terminal reconciliation; a page retry clears stale live membership but
      rebuilds it from freshly read authoritative running/pending/paused rows
      without duplicating bubbles/KPIs or re-admitting a terminal row, and a
      freshly read terminal durable row converges the existing bubble from
      that row).
- [ ] Two-read chat open: once the owning runtime is selected, a persisted
      session with more than 100,000 events, at least 10,000 turns, and one
      turn with more than 100 tool calls reopens its latest 20 turns —
      including the newest running or paused turn — with exactly one Phase 245
      lifecycle read plus one turn-page read. Cross-runtime resolution may make
      at most one lightweight lifecycle probe per entitled candidate runtime;
      the critical path performs zero raw history scans, zero full task
      enumerations, zero per-turn `tasks.get`, and zero per-turn `events.list`.
- [ ] The same renderable message skeletons, every inline answer, ordered
      Activity, usage, terminal cause, and ordered App refs are returned before
      and after durable-driver restart. Reopening a newest running or paused
      turn preserves its mutable version and streaming/cancellation/
      intervention state, then converges to the sealed terminal row. Older
      paging has no skip/duplicate while a new turn starts. Projector
      replay/restart is idempotent; retention gaps, evicted answer/context,
      partial ordered collections, and rebuilding states are honest and never
      become exact empty/zero values. Session erasure makes the projection
      unrecoverable.
- [ ] Same-user/session-reach, foreign-user, cross-tenant, erased-session,
      admin, and fleet cases run across every production durable driver,
      including N>=100 concurrent mixed identities under `-race`. Cross-identity
      denial is non-oracular. A paused owner without the required
      approval/control tier receives no action token and cannot resume; an
      otherwise identical authorized caller can. No widened read exposes
      transcript, reasoning, or App-correlation content; the `operations`
      projection is not built and admin/fleet observation implies no transcript
      access.
- [ ] Wire manifest, generated clients, capability/version discovery, protocol
      docs, operator skill, and Harbor's own chat surface (the Console) land
      with the methods. The current Console requires `sessions.turns.*` and
      surfaces `unknown_method` explicitly when that projection is absent;
      it does not fall back to raw `state.history` reconstruction, because
      completion chunks are non-durable. `state.history` remains available
      only to consumers that explicitly own that raw-event contract. Full
      D-223 lockstep + D-209 regen fire in the same PR.

## Files added or changed

- `internal/turns/` (or the natural subsystem home) — projection store,
  incremental applier, sequence checkpoints, tail-page cursor, turn and
  activity shape builders, conformance suite
- `internal/turns/drivers/{inmem,sqlite,postgres}/` — the §4.4 seam with an
  indexed triad (only where the projection persistence is driver-shaped)
- `internal/sessions/protocol/` — `sessions.turns.*` handlers, paging, and
  authority
- `internal/protocol/{types,methods,errors,bodyscope,singlesource,transports}/`
  — additive wire types/methods/errors without a `ProtocolVersion` bump
- `internal/protocol/client/` and `web/console/src/lib/protocol/` lockstep
  (hand-maintained mirror + `make protocol-ts-gen`)
- generated Protocol docs (`make protocol-docs-gen`) and the operator skill
- `web/console/src/lib/sessions/` — minimal chat-open consumer using the two
  reads (the current Console has no forensic `state.history` fallback)
- `test/integration/conversation_turns_test.go`
- `docs/glossary.md`, `docs/decisions.md`, `docs/plans/README.md`,
  `RFC-001-Harbor.md`, `docs/skills/`, and `CHANGELOG.md`
- `scripts/smoke/phase-246.sh`

## Public API surface

- `sessions.turns.list` — `{session_id, older_cursor?, limit, projection}` in;
  `{session_header, snapshot_id, turns[], order, next_older_cursor, has_more,
  remaining_older_count?, count_exact, live_resume_cursor,
  page_completeness, partial_reason?}` out.
- `sessions.turns.get` — `{session_id, task_id}` in; the same turn shape out.
- No third public method: if the Protocol response ceiling forces the exact
  attachment contract, a future named fallback is recorded as a deferred
  follow-up (not a v1.28 method or acceptance) with the same identity,
  snapshot, ordering, and no-arguments/no-results contract.
- `ConversationTurn` — the consumer shape above (query, answer union, ordered
  reasoning, ordered Activity, compact totals, usage, intervention metadata,
  ordered App refs, per-component availability, projection version,
  `last_applied_event_sequence`, retention horizon, sealed flag).
- Additive wire types only; `ProtocolVersion` stays `0.1.0`.

## Test plan

- **Unit:** turn shape builders and per-component availability; answer union
  (inline/ref/empty/evicted/unavailable — failed read never fabricates empty);
  cursor keyset/snapshot anchoring and tie-breaker; tail-page boundary
  (append-while-paging: no skip/duplicate); inline Activity bound covering at
  least the configured budget plus cardinality-cap/overflow-bucket totals
  (the deferred follow-up is recorded, not shipped);
  terminal-reason mapping; reasoning/
  Activity ordering and shared sequence; App-ref replacement identity;
  intervention-tier gating; sealed-vs-mutable versioning; projection
  checkpoint idempotency; erasure fence.
- **Integration:** real durable drivers; a >100,000-event / >=10,000-turn /
  100+-tool-call-turn fixture reopening its latest 20 turns in exactly one
  lifecycle read + one turn-page read; restart and reopen parity
  (renderable skeletons, answers, Activity, usage, terminal cause, App refs
  identical before/after); forced applier failure, retention gap, eviction,
  and rebuilding states; cross-identity non-oracular denial; the
  page-before-subscribe snapshot-to-live handoff with `events.subscribe`
  (fold the durable page, establish bounded membership, open the stream with
  `live_resume_seq` as the initial `resume_seq`; the server replays events
  strictly newer than the snapshot; reconnect `Last-Event-ID` takes
  precedence; one terminal event triggers one
  `sessions.turns.get`); N>=100 concurrent mixed identities under `-race`;
  erasure removes the projection and rebuild does not resurrect it.
- **Conformance:** every registered projection/state driver passes the turn
  projection conformance suite (indexed paging, checkpoint idempotency,
  erasure fence); Protocol integration across driver combinations owns
  authority and cross-principal assertions.
- **Concurrency / leak:** N>=100 mixed-identity list/get calls on one
  shared projection and handler set under `-race`, with cancellation barriers
  and a final goroutine baseline.
- **Fuzz:** cursor decoding, turn decoding, and page-boundary inputs with
  bounded allocations and no panics.

## Smoke script additions

- Open a durable session's chat in two reads (lifecycle +
  turn page) and assert the newest 20 turns render without a per-turn
  `tasks.get` / `events.list`; assert paging older history has no skip/duplicate
  while a new turn starts; assert inline Activity returns ordered entries
  with no arguments/results (a separate named activity method is not a v1.28
  method or acceptance; the deferred follow-up is recorded, not shipped);
  assert cross-identity turns are typed not-found.

## Coverage target

- Projection package (wherever it homes): 90%; touched drivers: 90%;
  `internal/sessions/protocol`: 90%; new Protocol authority paths: 100%;
  integration package: 85%; Console consumer: measured baseline with the
  two-read open path pinned.

## Dependencies

- Depends on Phases 130, 162, 204, 205, 232, 242, and 245.
- Phase 245 (HA-63) must land first or in the same wave (the two-read chat
  open depends on both).

## Risks / open questions

- The projection schema must choose a bounded row/child layout all durable
  drivers can index and page consistently; the keyset anchor needs an
  immutable per-turn tie-breaker that survives re-open and erasure fences.
- Incremental application and live streaming compose at the boundary; the
  page-before-subscribe snapshot-to-live handoff (fold the durable page and
  establish bounded membership BEFORE opening the stream, `live_resume_seq`
  as the initial `resume_seq`, server replay of events strictly newer than
  the snapshot, reconnect `Last-Event-ID` precedence) must be pinned against
  the shipped `events.subscribe` before implementation.
- Terminal sealing requires all source authorities (task result, events,
  App-context) to have converged; the seal rule and the rebuilding state must
  be explicit so a partially-applied turn is never presented as complete.
- The inline Activity bound rides the configured per-turn tool-call budget;
  if the Protocol response ceiling ever forces the exact attachment contract,
  the fallback is recorded as a deferred follow-up — not a v1.28 method or
  acceptance — and the inline bound is still tested with a real over-budget
  turn.

## Glossary additions

- **Conversation turn projection**
- **Turn-page cursor**
- **Live resume cursor**
- **Activity subpage**

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages >= stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] Concurrent-reuse test passes with N>=100 under `-race`, including no
      data races, context bleed, cancellation cross-talk, or goroutine leaks.
- [ ] Real-driver integration wires the >100,000-event fixture, the two-read
      chat open, identity propagation, and a failure mode under `-race`.
- [ ] If new vocabulary: glossary updated
- [ ] The projection/wire decisions and the consumer boundary are recorded in
      D-425 before implementation merges.
