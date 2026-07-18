# Harbor v1.16.0 — Parallel Intent, Task Management, Cache Telemetry (phases 185–191)

> Coordination artifact for CLAUDE.md §17.7. The wave lifts the one-intent-per-
> response fossil from the planner edge (the `Batch` decision), pairs the new
> spawn power with observation/control brakes and an explicit cancel hierarchy,
> makes background work and turn failures conversationally honest, stops
> dropping provider cache accounting, and lands three consumer-ask surfaces
> (the default-agent row and the OAuth broker legs). Research base: briefs 16
> and 17 (mined + grounded in this planning PR).

## 1. Executive summary

Three tracks, seven phases, each independently reviewable:

**Track 1 — parallel intent + task management (185–188).**

1. **185 (D-322): the `Batch` decision + AC-21 supersession.** A fourth sealed
   shape (`Tools + Spawns + Join`); `_spawn_task` becomes batchable with
   catalog tools and other spawns; `_finish`/`_await_task` keep the standalone
   guard; the reserved-control prompt descriptions finally teach what the
   validator accepts.
2. **186 (D-323): the Batch executor.** One flat concurrent dispatch — tool
   branches through the existing `JoinSpec` path, spawns auto-grouped through
   the existing registry path; call-id-keyed observations replying in original
   call order; a loud `max_batch_spawns` breadth cap; cascade-by-default
   cancellation across the batch's group.
3. **187 (D-324): task-management meta-tools.** `_task_status` /
   `_cancel_task`, descendant-scoped; model-expressible
   `propagate_on_cancel: isolate` lands ONLY here (power with brake); the
   cancel hierarchy — operator over agent over cascade — tested end-to-end.
4. **188 (D-325): background wake + failure honesty.** Group resolution emits
   a notification-class conversational mirror on top of the untouched typed
   wake; the TUI/Console render background lifecycle lines and an explicit
   foreground turn-failure line (no more silent stop on a planner rejection).

**Track 2 — cache telemetry (189).**

1. **189 (D-326): cache-token capture.** Telemetry-only: the driver translator
   stops dropping `PromptTokensDetails` cache read/write tokens; `Usage` and
   the cost event mirror them; every hand-decoded consumer updates in the same
   PR. The request-side cache-intent surface ("Phase B") is a named mid-wave
   decision point, not a phase in this plan.

**Track 3 — consumer asks (190–191).**

1. **190 (D-327): the synthetic default-agent row** in `agents.list`
   (external ask HA-25) — the absence-representable class (D-311).
2. **191 (D-328): the OAuth broker legs** (external asks HA-26/27/28) —
   structured step-up visibility (report-not-act), the RFC 8707 resource
   indicator on the exchange + per-tool provider binding, the RFC 8693
   `actor_token` leg. Carries the wave-end E2E.

## 2. Version and decisions

- Product target: **v1.16.0**. Base: tagged **v1.15.0** through phase 184.
- The Harbor Protocol stays `0.1.0` — every wire change in the wave is
  additive (new event type, additive row/marker/fields), gated by the
  D-223/D-209 lockstep.
- Pre-assigned decisions:

| Phase | Decision | Subject |
|---|---|---|
| 185 | D-322 | `Batch` decision; standalone rule shrinks to `_finish`/`_await_task` |
| 186 | D-323 | Flat heterogeneous dispatch, auto-grouping, ordered observations, cascade default |
| 187 | D-324 | Descendant-scoped management meta-tools; `isolate` gated on them |
| 188 | D-325 | Notification-class wake mirror + turn-failure honesty |
| 189 | D-326 | Typed cache read/write tokens captured end-to-end |
| 190 | D-327 | `agents.list` synthetic default-agent row (`is_default`) |
| 191 | D-328 | OAuth broker legs: step-up data, resource-bound exchange, actor chain |

- The RFC §6.2 amendment (the `Batch`/`TaskStatus`/`CancelTask` shapes + the
  cancel-hierarchy invariant) is included in this planning PR.

## 3. Staging

### Stage 1 — phases 185, 189, 190 in parallel

Three tracks with no inter-dependencies: the decision shape + projector (pure
planner), the cache capture (pure LLM edge), the default-agent row (pure
Protocol read surface). Each is its own worktree agent.

### Stage 2 — phase 186

The executor consumes 185's shape — the same-wave consumer §13 requires.

### Stage 3 — phases 187 and 188 in parallel

187 needs 186's dispatch surface (the meta-tool decisions ride the same
executor seam). 188 needs 186's groups exercised by batches; it does NOT need
187 — the notification mirror is orthogonal to the management meta-tools.

### Stage 4 — phase 191

The OAuth legs land last and the phase bundles the wave-end E2E
(`test/integration/wave_v116_test.go`) across every shipped surface: a Batch
spawn+tools turn, a meta-tool cancel under the hierarchy invariant, a
background-wake notification reaching the conversation surface, captured
cache tokens on a real completion, the default-agent row on `agents.list`,
and one OAuth-leg failure mode — identity propagation and N≥10 concurrency
stress under `-race` throughout.

The operator confirms this staging before implementation dispatch (§17.7
step 2). Merges drain between stages; a cleanup `chore` PR may run between
stages if audit WARNs accumulate.

## 4. Mid-wave decision point — cache Phase B

After 189 ships and real cache-hit data exists, the operator decides whether
the request-side cache-intent surface (a `CachePolicy` field on
`CompleteRequest`, operator-declared per-model config lowering into the
gateway's existing cache wire vocabulary — brief 17 §5) enters this wave as a
phase-192 plan-amendment PR or waits for v1.17. The deferred
StabilityClass/canonicalization and cache-aware-compaction work stays out of
v1.16 regardless (brief 17 §5: the compaction gate is blocked on plumbing
that does not exist).

## 5. Binding invariants the wave must not weaken

- **Planner swappability:** `Batch`/`TaskStatus`/`CancelTask` are sealed
  decision shapes dispatched by the runtime; no planner-to-registry imports.
- **Cancel hierarchy:** operator reaches any task, always (isolate detaches
  from the parent's cascade, never from the operator; session-scoped cancel
  sweeps isolate too); agent reaches only its own descendants; cascade is the
  default. There is no uncancellable task.
- **Identity:** every new surface carries the triple; descendant scoping is
  in addition to — never instead of — identity scoping.
- **Report-not-act (OAuth legs):** the runtime surfaces challenges as data;
  custody, acquisition, refresh, and consent stay coordinator-side; D-300's
  credential-sink allow-lists compose and never weaken.
- **Fail loud:** breadth-cap and FailFast-disagreement rejections are
  whole-batch and named; a missed payload consumer is an acceptance-criteria
  failure, not a silent zero.
- **No client naming:** consumer asks are mirrored framework-framed only
  (external ids HA-25..HA-28 may be referenced; no consumer organization or
  product is ever named in committed text).

## 6. Documentation and adoption

- 185/187 update the planner-control prompt descriptions and the affected
  operator skills (§18 grep-by-surface).
- 188 updates `drive-the-harbor-tui` and the Console observe skill for the
  new conversational lines.
- 189 names its consumer list (TUI/Console/enricher) as acceptance criteria.
- 190/191 carry the full D-223/D-209 lockstep and the protocol skill updates.
- CHANGELOG `[Unreleased]` accumulates per phase; the release cut follows the
  v1.15 pattern.

## 7. Completion rule

After phase 191 merges, run the mandatory §17.5 read-only checkpoint audit
and land one `chore(checkpoint): wave-v116 audit fixes` PR before the next
wave's planning starts. v1.16 is not complete until every phase gate, the
wave E2E, coverage targets, and preflight pass.
