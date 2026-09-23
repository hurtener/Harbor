# RFC 002 — Portable session context

**Status:** Accepted for incremental implementation in PR #779; not released.

Phase 268 implements portable compaction and request budgeting. Phase 269 adds
cumulative session memory, dispatch journaling, checkpoint reuse, explicit
reconciliation, result recovery, applied steering and attachment continuity.
The September 23 amendment below replaces the separate opt-in retained window;
its owner consolidation and multi-window acceptance are tracked independently
of the earlier within-window results. The runtime now uses the cumulative
owner; final-tree and live acceptance remain pending.
Final diagnostics, conformance and release gates remain in progress. Neither phase
is declared RC-ready. The [implementation tracker](docs/notes/portable-context-tracker.md)
separates implemented behavior from pending release acceptance.

**Date:** 2026-09-18.

**Source baseline:** `hurtener/Harbor` main at `a99ebfd8595410e439558236f0cabade287c20ac`.

## Goal and scope

An agent must be able to read a resource, act on that evidence, finish a turn,
and continue the same session without losing its useful earlier context when
the detailed turn window rolls over. A constraint supplied only in turn 1 must
still govern an edit after several complete windows, without restatement or
external long-term-memory retrieval.
Compaction must keep working during long runs. A compatible model reached through
the pinned Bifrost SDK must be able to continue without restoring provider-owned
conversation state.

Harbor owns bounded session execution context. Long-term memory remains an
external service responsibility: no memory ingestion, semantic indexing,
preference extraction, consolidation, or cross-session recall is added here.
Existing trusted completion hooks and external integrations remain unchanged;
summarization does not invoke them or expose them as planner tools.

Keep this implementation small: evolve the existing trajectory, compactor,
request builder, StateStore, and ArtifactStore. No new service, second public
transcript, general context framework, resource database, or direct provider SDK.
No provider-native compaction, opaque compaction checkpoint, or semantic response
cache. Prompt caching is an optional transport optimization, never a requirement.

## Authority and source basis

This is an accepted scoped amendment to [RFC 001](RFC-001-Harbor.md), not a
replacement for Harbor's architecture. It specifies changes to RFC §6.2
(trajectory projection), RFC §6.5 (request budgets), and RFC §6.9 / RFC §6.11
(session continuation and persistence), and RFC §6.6 (one session-memory owner).
The old pair-only rolling-summary pipeline and separate retained-context
activation path are replaced together. No backward-compatibility layer for
those APIs or storage formats is required; no second summary or long-term-memory
implementation may remain beside the replacement.

Preserve RFC §4 isolation, RFC §6.3 pause authority, RFC §6.4 runtime-owned
dispatch, RFC §6.10 artifact routing, and RFC §7 Console-as-Protocol-client.
The consumer turn projection remains bounded and excludes raw arguments/results
([phase 246](docs/plans/phase-246-durable-conversation-turns.md)). Best-effort
observability is not a replay journal
([phase 266](docs/plans/phase-266-ordered-observability-event-path.md)).

Informing briefs and inherited findings:

- [Brief 02 §1](docs/research/02-planner-and-control.md): runtime mechanisms
  must remain separate from the swappable planner's reasoning policy.
- [Brief 04 §1](docs/research/04-memory-and-skills.md): session memory is
  declared-policy and identity-scoped; its old pair-summary loop is replaced by
  the execution-context compactor under D-477, not retained as another engine.
- [Brief 05 §1](docs/research/05-state-tasks-artifacts-sessions.md): reuse
  identity-scoped StateStore and ArtifactStore with backend conformance.
- [Brief 08, LLMClient mapping](docs/research/08-llm-client-validation.md):
  Bifrost stays the model-call substrate behind the existing LLM client.

The briefs are historical context, not current default-value or API evidence.
In particular, the old no-native-tool-calling description in brief 08 is already
superseded by RFC 001's D-167 amendment. Native tool calling remains supported;
this proposal excludes native *compaction*, not native tool declarations.

Source-verified reasons for the change at the baseline above:

- [CompressionRunner](https://github.com/hurtener/Harbor/blob/a99ebfd8595410e439558236f0cabade287c20ac/internal/planner/compression.go)
  does not update a non-nil summary, while the
  [ReAct builder](https://github.com/hurtener/Harbor/blob/a99ebfd8595410e439558236f0cabade287c20ac/internal/planner/react/prompt.go)
  suppresses ordinary step replay when a summary exists. New results can be
  recorded yet absent from the next request.
- Successful [serve writeback](https://github.com/hurtener/Harbor/blob/a99ebfd8595410e439558236f0cabade287c20ac/internal/runtime/serve/runloop.go)
  and [RunOnce writeback](https://github.com/hurtener/Harbor/blob/a99ebfd8595410e439558236f0cabade287c20ac/internal/runtime/assemble/runonce.go)
  supply user/final-answer pairs, not the run's tool transcript.
- The trajectory-size estimate differs from the assembled request; the
  [request estimator](https://github.com/hurtener/Harbor/blob/a99ebfd8595410e439558236f0cabade287c20ac/internal/llm/tokens.go)
  omits native declarations and tool-call arguments. The
  [summarizer](https://github.com/hurtener/Harbor/blob/a99ebfd8595410e439558236f0cabade287c20ac/internal/llm/summarizer/trajectory.go)
  clips observation fragments before summarization.

These source findings are not a reproduced production incident or a performance
claim. The implementation must start with recording-provider regressions.

## Design

### 0. One session-memory configuration and implementation

`memory` is the sole public configuration entry point. The standard session
strategy is cumulative `rolling_summary`, with 20 recent detailed turns:

```yaml
memory:
  strategy: rolling_summary
  recent_turns: 20
```

`recent_turns` bounds recent detail, not the age of meaning in the checkpoint.
`memory.strategy: none` explicitly selects stateless execution. Consolidate
working-input compaction budgeting under `memory.budget_tokens`; zero selects
the effective model's safe assembled-request target, not unbounded storage or
disabled compaction. Model output limits, cumulative spend and run step limits
remain independent. `sessions` owns lifetime, deletion and retention policy,
not a second memory activation switch. Remove `sessions.retained_context_turns`,
`WithRetainedContext` and `planner.token_budget`, with their old assembly branches,
pair-summary loop and obsolete documentation. Removed keys fail validation with
actionable migration guidance instead of silently selecting a different mode.

Serving and embedding use the same memory owner, history projection and
compactor through the existing StateStore, ArtifactStore, dispatch journal and
governed Bifrost client. Update memory interfaces and their consumers together.
No new storage service, provider SDK, compatibility engine or public transcript.

Native session-memory semantic indexing and its `SearchTurns` SDK surface are
removed, not adapted into another session-history projection. Remove
`memory.retrieval`, `memory.retrieval_top_k`, `memory.retrieval_min_score` and
their environment overrides; explicitly reject their presence, including zero
values. Standalone embeddings, semantic skill retrieval and caller-supplied
external memory remain separate. Existing vector rows are not migrated or read;
authorized session erasure remains responsible for session data removal.

### 0.1 Cumulative committed coverage, not a cache of retained raw turns

The private session record holds one current checkpoint, a bounded recent tail,
validated exact evidence/attachment references, generation/coverage/retention
metadata, and bounded active admissions with existing recovery fences. It does
not accumulate a chain of old checkpoints or every historical source ID.

Each rollover consumes the previous checkpoint and newly eligible evidence:
`C1 = compact(batch1)`, `C2 = compact(C1 + batch2)`, and so on. Runtime-owned
committed generation and coverage establish which detail was represented even
after that raw detail is discarded. The current implementation's requirement
that every checkpoint source turn remain stored is superseded by D-477.

Freeze a fully settled eligible prefix, infer and validate outside storage locks
and the five-second persistence budget, then conditionally commit against the
unchanged generation, source prefix and erasure state. Preserve newer evidence;
discard covered detail only after a successful commit. A stale sibling cannot
overwrite a newer checkpoint or hide an earlier admitted turn that settles late.
Do not advance contiguous coverage across an unsettled admission. A run's admitted
view remains frozen; running evidence stays private until terminal publication
or explicit fenced recovery.

Trigger compaction from the complete assembled request budget or before the
detailed-history storage bound would be exceeded. Short conversations and one
large tool exchange must both work within bounds. Use the same compactor during
long runs and between turns, with the same authorization, accounting and
publication rules. A failed summary or write preserves committed state. When
safe progress cannot fit, return explicit capacity failure rather than evicting
unsummarized work. Fresh receipts/errors must reach the next decision request.

Compaction cleanup changes representation; deletion and retention expiry remove
information. Compaction/restart never renew source retention. Where removed or
expired information contributed to a checkpoint, rebuild from remaining
authorized evidence when possible; otherwise invalidate that checkpoint
explicitly. An opaque summary cannot certify selective forgetting. Exact
receipts, large versions, completeness flags and references remain runtime-owned
bounded evidence, not facts trusted solely to generated prose. Revalidate their
current lifetime and authority; unknown external effects remain unknown.

### 1. A checkpoint replaces a known prefix, never the future

Extend the existing trajectory summary with a version, generation, and an explicit
covered-through cursor tied to its source history. Use existing run/step/call
identities; do not invent another global sequencing system. Coverage metadata
belongs to the runtime, not to generated summary prose.

```text
History:       1 ........................................ 150
Checkpoint:    covers 1 ................ 100
Next request:  instructions + checkpoint + items 101 ...... 150
New result:    instructions + checkpoint + items 101 ...... 152
```

The next compaction combines the previous narrative with the newly eligible older
prefix, advances coverage, and keeps a recent tail. It must neither resummarize
all archived raw data on every step nor discard previously summarized constraints
because they were not repeated recently. Preserve current user input and steering
corrections; do not promote summary or tool-origin text into trusted instructions.

Cut only at complete exchange boundaries. Preserve native call/result pairing,
parallel-batch order, and fresh corrective errors. A newly settled result must be
represented in the next decision request before becoming eligible for ordinary
historical summarization. Retain the latest complete exchange and a bounded recent
tail; no dependency graph or model-written pinning protocol is required.

That exposure guarantee applies to the permitted model-facing result, not unlimited
raw bytes. An oversized exchange gets an explicit bounded view and an authorized
retrieval reference, or a typed capacity/unavailability outcome. Never silently
omit it, orphan a call, bypass a byte ceiling, or claim the model saw the full body.

Generate a candidate outside storage locks from a frozen prefix. Validate it,
then conditionally install it only if that source prefix is unchanged; preserve
any newer tail. Rollback, redirection, or deletion that invalidates the prefix
invalidates the candidate. Keep the old checkpoint/history on failure.

### 2. One request budget and one ordinary summarization path

Use `llm.EstimateRequestTokens` as the shared request-estimation path; extend it
rather than add a competing estimator. Count the assembled system/guidance,
active tool schemas, historical tool arguments, messages/results, checkpoint,
retrieved context, response schema, and supported multimodal contributions.
Do not count raw plus projected copies just because both exist in storage.

Separate the working-input target from cumulative run spend and step limits.
Clamp that target to the effective model's input/context limits with output
headroom and a conservative estimation margin. Respect provider accounting for
reasoning/output rather than double-counting it. A lower post-compaction target
must leave room for another bounded exchange. Rebuild and remeasure afterward.

Resolve capacity after effective task settings and route/model selection. Every
retry/fallback that changes the model must revalidate capacity before sending.
A smaller context window or unsupported modality must not silently drop evidence.
Local estimates are explicitly estimates; exact provider tokenization is not a
required dependency. Unknown model limits keep existing fail-closed validation.

Evolve `TrajectorySummariser` through the existing composed `LLMClient.Complete`
and pinned Bifrost driver. Default to the run's effective authorized route, with a
distinct maintenance-call/attempt identity. An explicitly configured summarization
model must have its own authorized route and input/output budget; never select a
cheaper external provider automatically. Preserve governance, grants, timeout,
retry, usage, and cost accounting on every attempt.

The compaction request has no executable tools. Ask for a small structured
narrative and validate it locally; native JSON-schema enforcement may assist via
the existing output-mode chain but is not required. Reject empty, malformed,
length-truncated, or over-budget summaries. Do not turn an arbitrary note into a
valid checkpoint. Semantic completeness is not guaranteed by schema validation.

Budget the summarizer's own input before sending it. Process the eligible prefix
in bounded chronological chunks when necessary, carrying forward the prior
narrative; every chunk is accounted for and total maintenance work is bounded.
Do not silently skip old steps or byte-clip exact result metadata. Keep large
bodies behind references. If bounded maintenance cannot produce a fitting
checkpoint, keep the prior state and stop with an explicit capacity error or the
existing pause mechanism. Continue unchanged only when the request still fits.

### 3. Retain execution evidence, not a second chat transcript

Persist bounded, model-eligible execution records under existing runtime
trajectory/session authority and the configured StateStore. Reuse existing IDs,
conditional writes, and artifact references. Extract only the small shared helper
needed by serve and RunOnce; no new registry or plugin system.

Record admitted input, completed assistant/tool-call messages, settled results,
errors, and terminal outcomes at their execution boundaries, not only on
`FinishGoal`. In durable mode, commit call intent before a side effect and its
settlement before the next dependent model request. Do not persist every token
delta, credentials, live handles, or raw internal reasoning by default. A failed
required write stops dependent work explicitly; telemetry acceptance is not proof
of persistence. Store blobs before publishing references; failed commits must
leave no dangling reference, and abandoned blobs follow bounded cleanup policy.

A bounded session index selects prior terminal root-turn context once at run
admission; the current run then uses that frozen prefix plus its own tail. Order
root turns by existing admission identity, not completion timing. Concurrent
siblings must not overwrite checkpoints or inject their in-flight steps into one
another. Recovery of an interrupted prefix requires explicit authorized
continuation after the earlier execution is fenced, not automatic sibling
inclusion. Child transcripts remain private to the child; parents receive
explicit outcomes through existing join handling. Isolation and current
authorization are checked on restoration/retrieval, not inferred from a remembered
tool name.

Keep reads proportional to the retained window, with indexed/keyset access and
bounded writes. Do not scan raw telemetry, enumerate the whole session on each
request, or rewrite an ever-growing trajectory after every tool call. Persisted
records and checkpoints need explicit byte/count/retention bounds on all drivers.

The next user turn restores checkpoint plus recent execution context through the
same projection used within a run. Do not also inject a second pair-only or
rolling summary of that same history. Replace the legacy memory interfaces and
callers in the same increment; no legacy-mode branch or duplicate projection.

Cold restart restores committed session context for an authorized subsequent
continuation. It does not relaunch a lost run or recreate process-local handles.
RFC §6.3's existing cold pause/resume unavailability remains unchanged. A save
that may have happened before its receipt was persisted has an unknown outcome;
reconcile through the owning tool/service or obtain intervention, never blindly
retry it. This plan makes no exactly-once side-effect claim.

### 4. Exact results remain recoverable without domain machinery

Preserve the permitted result envelope before summarization. Small envelopes stay
inline; larger ones use the existing ArtifactStore. Keep the observed resource
ID, version, range/completeness, and operation status only when actually supplied
in structured output. Do not guess fields from prose or require custom MCP schemas.
The retained result/reference is the generic baseline; no normalizer registry is
needed for the first implementation.

Summaries describe the work but do not mint references or certify saves. A Harbor
blob ID is not an external document ID, and an observed version is not necessarily
the current version. The resource-owning service enforces freshness and edit
preconditions. Save, render, and functional verification remain separate outcomes.

Use `artifact_fetch` for already surfaced references. Supply a minimal bounded,
identity-scoped lookup of retained execution-result references only where existing
surfaces cannot recover them; ship its first planner consumer and tests together.
Do not add a generic history-search service. Recovery reads receive the same fresh
result exposure guarantee and never rerun a historical write.

Apply existing content authorization/redaction before persistence and inference.
Retention/deletion must fence records, references, and derived checkpoints. An
expired reference is explicitly unavailable, not fabricated source. Removing
source content must invalidate/rebuild a checkpoint that could otherwise retain
it; ordinary logs/consumer turn rows never acquire raw execution payloads.

### 5. Tool continuity and cache stability stay modest

Carry a bounded set of discovered tool identities with retained session context.
Resolve them against the current authorized catalog, preserving canonical names
and refreshing changed definitions. Historical discovery is not execution
permission; removed/revoked tools remain unavailable. Never fuzzy-dispatch an
invented name. Repeated reads are diagnostics, not automatically suppressed calls.

Keep prompt prefixes stable within a checkpoint generation: deterministic tool
ordering, stable call IDs, append-only exchanges, and no per-step regeneration of
unchanged summaries. Append relevant state updates where the adapter supports
that shape; rebuild explicitly when policy or provider compatibility requires it.
Correctness and revocation take precedence over a cache hit.

Use cache controls only when the pinned Bifrost adapter supports them. Do not add
a provider client, silently upgrade Bifrost, or build a second capability catalog.
Absent optional cache support must not impair continuity; unsupported explicit
operator requirements are reported. The entire suite must pass with cache hints
off and with no provider-native compaction or remote conversation state.

## Delivery plan: three implementation slices

The following three reviewable increments amend the existing phases, without new
phase numbers or a new architecture. Earlier implementation checkpoints below
remain historical evidence, not completion of this cumulative contract.

| Slice | End-to-end outcome | Main existing areas |
|---|---|---|
| 1. Pin the cumulative contract | Amend both RFCs, decisions, plans and configuration docs; reproduce a turn-1 constraint disappearing from an actual request after window rollover | RFCs, `docs/`, recording-provider assembly regression |
| 2. Commit cumulative memory | Versioned checkpoint/coverage and atomic rollover; one compactor; wire served and embedded consumers together | `internal/memory/`, `internal/runtime/runctx/`, serve/assembly, planner, existing state/artifact seams |
| 3. Finish integration and acceptance | References, attachments, steering, recovery and content-free diagnostics; remove legacy paths; update SDK/examples; deterministic multi-window and real UI comparison | config, SDK, examples, Protocol/telemetry consumers and integration tests |

Slice 1 is contract and red regression only, not a runtime correctness claim.
Each implementation slice includes its production consumers, meaningful tests,
adversarial review and relevant documentation. Reuse existing diagnostics and
maintenance accounting rather than waiting for new observability infrastructure.
No new Console page or full prompt-logging facility is required for this delivery.

## Acceptance and validation

Use real Harbor assembly, ReAct, dispatch, stores, and the actual compressor.
A deterministic recording provider scripts model responses, then asserts both
the effective request and Bifrost serialization; it does not prove model skill.

| Scenario | Required evidence |
|---|---|
| Read, compact, read again, edit, error, retry | Each new result/error reaches the next request; exact complete pairs survive two or more compactions without duplicate replay. |
| Complete 14,660-byte synthetic receipt | `more:false`, resource/version metadata, and usable source survive the relevant decision boundary even when metadata follows the body. |
| Large/parallel/multimodal exchange | Whole call/result groups remain valid; offload/range recovery is honest; oversized protected input fails explicitly rather than looping or disappearing. |
| Summary failure or malicious tool text | Invalid/truncated/oversized output cannot replace the old checkpoint; tool-origin instructions cannot alter host coverage, authority, or references. |
| Next user turn and restart | Committed IDs/results restore without the final answer repeating them or an external memory service being available. Lost live handles keep existing typed failures. |
| Crash after save; failed state/artifact commit | An acknowledged outcome is not forgotten after commit; an ambiguous outcome is not auto-retried; no dangling reference or false successful durable seal. |
| Concurrent runs and deletion | Frozen-prefix and conditional-write checks prevent lost tails/cross-talk; deletion/revocation prevents stale reconstruction. |
| Tool refresh | Current names/schemas/permissions win; no revoked or guessed tool is invoked. |
| Provider/model switch, cache off | An authorized compatible Bifrost route continues from the portable checkpoint; changed limits/modalities are revalidated, including fallback attempts. |
| Long retained history | Instrumented storage work is bounded by the configured window, not total historical event count. |
| At least 100 turns and five checkpoint generations | A constraint introduced only in turn 1 remains in actual later decision requests; later corrections survive. Every maintenance request receives the previous checkpoint plus new eligible evidence. No Stowage, repeated instruction or canned summary may manufacture a missing constraint. |
| Multiple complete windows with failures and restarts | Repeat with failed writes/summarization, concurrent siblings, deletion during inference, expired references and model changes on in-memory, SQLite and PostgreSQL. Prove bounded stored bytes and request size, not just successful summary creation. |

Run the shared persistence conformance suite across in-memory, SQLite, and
Postgres where those configured drivers participate. Reusable components require
N>=100 concurrent calls against one shared instance under `-race`, including
isolation, cancellation, and leak checks. Cover corrupt/legacy checkpoints,
conditional-write conflicts, UTF-8 ranges, and retention boundaries.

Expose bounded content-free diagnostics through existing telemetry/Protocol
ownership: checkpoint generation/coverage, included ranges, section estimates,
offload/omission reasons, compaction attempts, route, and usage availability.
Do not confuse missing usage with measured zero. Include maintenance, failed
attempts, retries, child work, and tool time in outcome/cost reporting.

Real-model runs need a separately approved budget. Compare baseline and candidate
on identical synthetic documents, decks, and mockups with the same model, route,
tools, and checks. Verify diffs, prior-change preservation, render/interaction
behavior, human intervention, repeated reads, and total cost/latency. Report cold,
warm, post-compaction, and restored sessions separately; no cache-ranking or
percentage-improvement promise. Deterministic gates must pass without paid calls.
The UI comparison must cross multiple compaction boundaries while building and
editing, switching sessions, returning and restarting. Measure preserved,
distorted and omitted facts: recursive summaries are lossy even when schema-valid.

## Migration and completion

The September 23 owner-authorized migration makes cumulative `rolling_summary`
the standard session-memory path; explicit `memory.strategy: none` remains
stateless. Replace removed configuration/SDK surfaces, not an adapter layer around
them. Pin each active run's policy. Use fresh isolated sessions for the first new
format deployment; do not reinterpret old stored windows as cumulative coverage.

Old summaries without proven coverage must not be treated as covering all stored
steps. Rebuild/recompact from complete authorized source records when available;
otherwise surface partial/unavailable historical context without inventing it.
Existing final answers cannot recreate missing tool transcripts. Never deduplicate
unrelated turns solely because their user text matches.

Version persisted records and clearly reject incompatible data; supporting the
old private format is not required. Database migrations remain additive. Verify
that incompatible readers fail closed rather than guessing coverage. Retention
and reference lifetimes follow session deletion and operator policy; changing
storage mode is not a silent data migration. Old RC tags remain immutable.

Before runtime changes, carry the accepted scoped amendments into the canonical
RFC/decisions and allocate actual phase entries. Implementation PRs must pass the
applicable `make drift-audit`, `make check-mirror`, `make preflight`, lint, build,
and `-race` tests; new Protocol/tool surfaces require their smoke/SDK/docs consumer
in the same slice. A blocked gate is reported, never marked passed.

Completion means observed evidence reaches the right request, retained sessions
restore honestly, and compatible Bifrost routes work without native compaction or
external long-term memory. It does not mean identical model decisions, perfect
summary fidelity, or automatic recovery of arbitrary external side effects.

## Dispatch-boundary implementation checkpoint

D-466 adds required query, dispatch-intent and dispatch-settlement persistence
to the existing retained served/embedded mode. The StateStore writes a small
run head and a bounded action frame atomically, preserving preceding frames.
Terminal window publication seals the run journal; conditional cleanup follows.
Failure before dispatch blocks the action; failure after dispatch blocks the
next decision and cannot be converted into a tool retry.

This is an incremental durability checkpoint, not completion of slice 2:
interrupted-prefix continuation still requires explicit reconciliation and
execution fencing. See [the checkpoint note](docs/notes/retained-dispatch-durability.md)
and the [phase 269 plan](docs/plans/phase-269-retained-session-context.md).

## Historical native projection checkpoint

D-467 implements the historical native-exchange part of slice 2. Retained
records remain non-executable; the existing ReAct renderer projects their
native call/result groups with stable source-scoped IDs. Version-2 windows
preserve legacy data as inert context and reject unknown history explicitly.
See [the checkpoint note](docs/notes/retained-native-exchanges.md).
Cross-turn checkpoint reuse and the remaining phase 269 criteria are not implied.
