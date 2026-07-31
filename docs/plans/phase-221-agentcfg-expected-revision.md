# Phase 221 — an expected-revision token on the agent-config writes

## Summary

Every write onto the agent-config revision spine is unconditional last-writer-wins, and there is no conflict detection anywhere on the path — so two writers composing into one agent's config silently revert each other and both are told `200`. This phase adds ONE optional field, `expected_content_hash`, to all **sixteen** spine-writing request types, and ONE comparison inside the registry driver's existing read-modify-write. Present ⇒ a write whose base has moved is refused with a new machine-branchable `revision_conflict` code (HTTP 409). Absent ⇒ the path is byte-for-byte what it is today. The guarantee is stated with its bound: the compare-and-write is atomic **within one Runtime process** and nowhere else, because the persistence floor has no CAS primitive and this phase does not pretend otherwise.

## RFC anchor

- RFC §6.16 — the Agent Registry. `agent_id` is the registry key the config spine hangs off; it is registration identity, never an isolation principal, and this phase does not widen it.
- RFC §6.11 — StateStore. The persistence floor the agentcfg driver writes through, and the floor whose interface godoc states it does NOT enforce CAS — the fact that bounds this phase's guarantee.
- RFC §5.3 — Protocol versioning. A new error code and a new optional request field are additive; `ProtocolVersion` does not move.
- RFC §9 — the persistence triad and its conformance-parity rule. The precondition is pinned in the shared agentcfg conformance suite, so any future driver inherits it.
- RFC §7 — the Console as a Protocol client. The Console agent-config panel is the human editor whose read-to-write window is the longest one in the system.

## Briefs informing this phase

- brief 05 — state, tasks, artifacts, sessions. The persistence floor's idempotency contract, its explicit limits, and the concurrency-test bar.
- brief 09 — MCP OAuth. The identity-mandatory / fail-closed posture the new precondition must not weaken, and the single-flight precedent for coordinating N racers at agent granularity.
- brief 11 — Console feature surface. The Agents view: the operator-facing editor that races every programmatic writer.

## Brief findings incorporated

- **brief 05 §"Idempotency"** (line 271): "`SaveEvent` keys on `EventID` (ULID provided by caller) and is a no-op on duplicate." This phase reads that finding for what it does **not** say: `EventID` idempotency answers "have I already applied THIS write?", never "is the slot still what I read?". `internal/state/state.go:68-70` states the same limit in the interface's own godoc — "The StateStore itself does NOT enforce CAS — it stores and returns the int." The precondition is therefore built where a compare and a write are already adjacent (the driver, under the service's write lock), and its bound is stated rather than inferred.
- **brief 05 §"Distributed contracts ship without backends"** (line 303): "in-process is accepted at V1 (deliberate)". This is the honest frame for the atomicity bound below. A process-local coordination primitive is the shape Harbor has accepted elsewhere for exactly this reason; what is NOT acceptable is claiming a cross-process property it does not have.
- **brief 05 §"Concurrency tests"** (line 314): "N concurrent sessions × M concurrent tasks each, asserting no cross-talk." Read here as: the test that matters is not "the check returns an error when I hand it a stale hash" but "two writers actually race one shared registry and exactly one is refused." Both are written; only the second would have caught a check placed on the wrong side of the read.
- **brief 09 §"What Harbor must add" item 4** ("Identity-mandatory enforcement … fail closed on missing components"). The token is a **precondition, never an authority**. It is compared strictly after the existing identity and scope gates, it can only ever cause a write to be refused, and no value of it widens what a caller may write. Pinned by a test that presents a valid token with an insufficient scope and asserts the scope refusal still wins.
- **brief 09 §"Findings summary"** (line 541): "Concurrent refresh storms on agent-bound tokens need single-flight protection — N sessions racing the same expired token … Harbor needs its own (test mandatory)." The shipped `Service.writeLocks` striped mutex is that shape applied to config writes; this phase leans on it, names it as the sole source of atomicity, and tests it — rather than adding a second coordination mechanism beside it.
- **brief 11 §"Findings summary"** (line 556, and line 558's ⚠): the Agents view is called out as the largest semantic gap and needs "additional Protocol work". The specific Protocol work this phase supplies is the one a UI cannot supply for itself: a human editing an agent in the Console holds a read open for seconds to minutes, and there is currently no way for the Runtime to tell them their save is built on a base that moved.

## Findings I'm departing from (if any)

None. The one place this phase looks like a departure is that it does not build the CAS brief 05's persistence discipline might imply — but brief 05 never claims the store offers one, and `internal/state/state.go:68-70` says plainly that it does not. Naming that bound is following the briefs, not departing from them.

## Goals

- A caller that read a config can write it back under a precondition that the config has not changed since, and be **refused loudly** rather than silently reverting someone else.
- A refusal is **machine-branchable** — a client can tell "your base is stale, re-read and retry" apart from "the server broke" without parsing a message.
- A caller that supplies no token gets **exactly today's behaviour**, byte for byte, on every one of the sixteen doors.
- The precondition is enforced at **one place**, not sixteen — and the shared conformance suite pins it, so a second agentcfg driver cannot ship without it.
- The guarantee is documented with its process bound in godoc, in the Protocol reference, and in the operator skill — never as an unqualified "conflict detection".

## Non-goals

- **A cross-process compare-and-swap.** The persistence floor has no conditional-write primitive. Adding one (`StateStore.SaveIf(ctx, r, expectedEventID)`) is a §9 three-driver interface change with its own conformance rows and its own phase. This phase does not fake one, and it does not claim the property that one would provide. See "Where atomicity comes from" below.
- **A `data` field on the Protocol `Error` envelope.** The wire error is `{code, message}` (`internal/protocol/errors/errors.go:210-222`) and `Message` is explicitly advisory — "never the thing a client branches on". So the conflict carries the code and nothing else; the client learns the current state by re-reading `agent_config.get`, which already returns both `revision_id` and `content_hash`. Widening the error envelope is a §8 change across every transport, the lockstep manifest and the generated docs — out of scope, and not needed for the retry loop.
- **A create-if-absent precondition.** "There is no active revision and I expect none" has no expressible value in a hash-typed token (empty means unconditional), and inventing a second field (`expect_absent`) to arbitrate a first-write race would be a second mechanism for a much narrower window. A token supplied when no active revision exists is a conflict; stated, tested, and left there.
- **Making the internal reconciliation writers conditional.** The OAuth-resume attach re-drive (`internal/runtime/agentcfg/protocol/addconnection.go:620`) and the compensating revert-to-empty on a failed audit emit (`setoauthprovider.go:188`) have no caller and no base to compare against. They stay unconditional; the residual is stated in "Risks".
- **Any change to `agent_config.set_llm_provider` or the `agent_config.session.*` family.** Verified: `set_llm_provider` never calls `registry.SetRevision` — it drives `llmProviderInstaller` + an audit emit (`setllmprovider.go:114-133`) — and the five session verbs write the ephemeral session overlay, not the durable spine. Neither is a spine door, so neither takes the token.
- Any `ProtocolVersion` bump, any new method, any new event, any migration.

## Acceptance criteria

- [x] `expected_content_hash` is an optional string on all **sixteen** spine-writing request types (twelve admin + four user-tier), enumerated in "Every door found". A seventeenth spine writer added later without the field fails the smoke's door-count guard.
- [x] With the field **empty**, every one of the sixteen paths produces byte-identical behaviour to the pre-phase build: same revision minted, same content hash, same parent pointer, same emitted event, same response. Pinned by a golden test that drives each door and compares the persisted records against a recorded pre-phase golden.
- [x] With the field **set and matching** the active revision's content hash, the write proceeds exactly as the unconditional write would.
- [x] With the field **set and NOT matching**, the write is refused with `protoerrors.CodeRevisionConflict`, mapped to HTTP 409, and **nothing is persisted** — no revision record, no active-pointer move, no `agent.config.revised` event.
- [x] With the field set and **no active revision exists**, the write is refused with the same code.
- [x] The precondition is evaluated **before** the shipped idempotent-re-set short-circuit, so a stale token is never converted into a success by a payload that happens to equal the current content. A caller retrying a write that already landed gets an honest conflict, not a misleading `200`.
- [x] The comparison lives in the registry driver's `SetRevision` / `Rollback` read-modify-write — the same read that already loads the active revision (`internal/agentcfg/drivers/statestore/statestore.go:236`), so the precondition is compared against the **latest** value before the write, with no extra store round-trip.
- [x] The precondition is threaded through the `agentcfg.Registry` interface as **one** options struct on the existing methods, not as a parallel `SetRevisionIf` method (§13 — no two mechanisms for one concept).
- [x] `internal/agentcfg/conformance` gains rows for match / mismatch / absent-active / empty-token-is-unconditional, so any future driver inherits the contract.
- [x] The token is a precondition and never an authority: a request carrying a valid token but an insufficient scope is refused by the **scope** gate, and a request carrying a token with an incomplete identity triple is refused by the **identity** gate. Both pinned.
- [x] Two writers actually race one shared registry under `-race` and **exactly one** is refused; the winner's content is intact and the loser persisted nothing.
- [x] `CodeRevisionConflict` is registered in `canonicalCodes`, mapped to 409 in the control transport's status binding, present in the Protocol conformance code matrix, and carried into the regenerated `docs/site/protocol/errors.md` and the lockstep wire manifest.
- [x] Every godoc, the generated Protocol reference row, and the `use-the-harbor-protocol` skill state the **single-process bound** alongside the guarantee. No text anywhere claims unqualified conflict detection.
- [x] `ProtocolVersion` is unchanged.
- [x] `scripts/smoke/phase-221.sh` shows `OK ≥ 12`, `FAIL = 0` against a live preflight build, and each guard turns an `OK` into a `FAIL` (never a SKIP) when mutated.

## Which token, and why

The read side already hands the caller both candidates on every revision: `AgentConfigRevisionView` carries `RevisionID` and `ContentHash` (`internal/protocol/types/agentconfig.go:422-440`). Implementation cost is identical — the driver holds both values in hand at the comparison point. The choice is purely semantic, and it is **`expected_content_hash`**.

1. **The guarded quantity is a value, so the token is a value.** Every spine write is a desired-state replace computed from a read of the config's **content** — `set_revision` replaces the whole payload; each section verb replaces its own section and carries the siblings forward (`promptlayers.go:54-68`, `skills.go:169-182`). The precondition a writer actually needs is "the content I computed against is still in effect". Content hash states that exactly. Revision id states a strictly stronger predicate whose extra strength is not a safety property.

2. **`rollback` is a shipped first-class verb that moves the pointer without necessarily changing the content.** `agent_config.rollback` repoints the active pointer to an existing revision (`statestore.go:365-398`); rolling back to the content a writer already read leaves the content identical and the revision id different. A revision-id token would refuse that write. Turning "the operator restored exactly what you read" into a conflict is a false positive **on the project's own recovery path**.

3. **It composes with the shipped idempotent re-set instead of duplicating it.** The re-set no-op is already defined by content equality against the active revision (`statestore.go:243`), over one canonical form (`agentcfg.NormalizePayload`, `agentcfg.go:633`) and one hash function (`agentcfg.ContentHash`, `agentcfg.go:870`). Making the precondition content-equality puts both on ONE comparand. A revision-id token would introduce a second notion of "unchanged" living beside the content one on the same write path — two answers to one question, which is the §13 shape.

4. **The ABA objection is real, bounded, and resolves in content hash's favour.** `R1(H1) → R2(H2) → R3(H1)` is reachable (a revert-by-re-set mints a new revision, because the idempotence check compares against the *active* revision only). A writer holding `H1` from `R1` will be accepted at `R3`. That is **not** a lost update: its base state and the current state are the same bytes, so the write it produces is exactly the write it would have produced at `R1`, and `H2` was already discarded by whoever wrote `R3`. Lost-update prevention is about not silently discarding a change; at `R3` there is no change left to discard.

5. **Accepting either token is rejected.** Two fields answering one question means four combinations to specify — neither, one, the other, both-and-disagreeing — and a client that can silently pick the weaker one. That is the §13 two-parallel-implementations shape. One field.

**The cost this choice incurs, and how it is paid.** A conflict cannot be handed straight to `agent_config.diff`, which takes revision ids (`AgentConfigDiffRequest.FromRevision` / `.ToRevision`). It is paid, not waved away: the client held the **revision id** from its own `agent_config.get` (the read returns both), and on a conflict it re-reads `agent_config.get` to obtain the current revision id — so `diff(from: what-I-read, to: what-it-is-now)` is available in full, at the cost of one extra round-trip on the rare conflict path. This is also why the non-goal above declines to widen the error envelope: the diff path does not need it.

## Where atomicity comes from — and where it does not

**It comes from exactly one thing: `Service`'s per-owner striped write lock.** `writeLocks [256]sync.Mutex` (`internal/runtime/agentcfg/protocol/service.go:293`, `:299`), acquired by `lockAgent` / `lockOwner` (`:304`, `:318-327`), FNV-1a-striped per `(scope, tenant, user, agentID)` so the same owner always resolves to the same shard. Every one of the sixteen doors takes it as its first act after identity validation and holds it across its whole read-modify-write, including the registry call. A check placed in the driver therefore runs **inside** that lock, and the compare-and-write is atomic against every other spine writer **in the same process**.

**It does not come from the store, and the store says so itself.** `internal/state/state.go:68-70`: *"`Version` is a hint for optimistic-concurrency at the typed-wrapper layer … The StateStore itself does NOT enforce CAS — it stores and returns the int."* The agentcfg driver writes the revision record and then the active pointer as two ordinary `Save` calls (`statestore.go:259-264`); `saveActive` mints a fresh `state.NewEventID()` every time (`:516`), so the active-pointer slot is unconditionally overwritten (`Save`'s own contract: *"If a record already exists at (Identity, Kind) but with a different EventID, Save overwrites it"*, `state.go:96-99`).

**So the guarantee is bounded, and the plain statement of the bound is:**

> The precondition is exact within a single Runtime process. Two Runtime processes sharing one Postgres or SQLite StateStore can still lose an update, and this phase does not claim otherwise.

**Two constructions were evaluated and rejected on the record**, so a later reader does not re-derive them:

- **Derive the active pointer's `EventID` from the expected content hash**, exploiting `Save`'s *"Same EventID + different Bytes: ErrIdempotencyConflict"* (`state.go:94`) — which both SQL drivers evaluate inside ONE transaction (`postgres.go:163-197`, REPEATABLE READ; `sqlite.go:339-373`, tx + a unique index on `event_id`), so the conflict would be genuinely store-enforced. **Unsound.** It only collides between two writers sharing the *same* expectation. A writer whose expectation is stale computes a *different* derived `EventID`, takes the different-EventID overwrite path, and clobbers the winner — which is precisely the lost update being prevented.
- **A successor-slot chain**: write each new revision's claim into `Kind: agentcfg.head.<parentRevID>` with an `EventID` derived from the parent, making each node's successor slot claimable exactly once. **Sound but unaffordable.** It turns `Active` — called on every run start through the projection — from one `Load` into a chain walk, and re-caching the head reintroduces the unconditional overwrite it was built to avoid.

**The real fix is named, not hinted:** a conditional-write primitive on the `StateStore` interface (`SaveIf(ctx, r StateRecord, expectedEventID EventID) error`), implemented across the in-mem / SQLite / Postgres triad with conformance rows. That is a §9 interface change and its own phase; this phase's driver-side check is written so it becomes the caller of that primitive with a one-line change when it lands. Recorded as a follow-up in "Risks / open questions".

**What the bounded guarantee is still worth, stated honestly.** The window a token closes is the **read-to-write** window — seconds to minutes for a human in the Console, and however long an agent takes to compose a config. The window it cannot close cross-process is the driver's own read-modify-write, on the order of microseconds. Single-process (every `harbor dev`, every single-instance deployment — the shape brief 05 line 303 records as deliberately accepted at V1) the guard is exact. Multi-process it is a large reduction in loss probability and not an elimination, and the godoc says so in those words.

## Every door found

Verified by grepping every `registry.SetRevision` / `registry.Rollback` call site outside tests and the conformance suite. **Sixteen** methods write the durable revision spine and all sixteen take the token.

Agent scope (`ConfigScopeAgent`) — twelve:

| Method | Handler | Spine write |
|---|---|---|
| `agent_config.set_revision` | `service.go:752` | `:819` |
| `agent_config.rollback` | `service.go:872` | `:884` |
| `agent_config.skills.upsert` | `skills.go:142` (`recordSkillsMembership`) | `:183` |
| `agent_config.skills.delete` | `skills.go:142` (same helper) | `:183` |
| `agent_config.set_tool_exposure` | `mcppolicy.go:50` | `:79` |
| `agent_config.set_prompt_layers` | `promptlayers.go:46` | `:70` |
| `agent_config.set_llm_params` | `llmparams.go:51` | `:78` |
| `agent_config.add_mcp_connection` | `addconnection.go:518` | `:562` |
| `agent_config.remove_mcp_connection` | `removeconnection.go:87` | `:130` |
| `agent_config.set_mcp_discovery_origins` | `setdiscoveryorigins.go:159` | `:200` |
| `agent_config.set_oauth_provider` | `setoauthprovider.go:138` | `:157` |
| `agent_config.remove_oauth_provider` | `removeoauthprovider.go:63` | `:88` |

User scope (`ConfigScopeUser`) — four:

| Method | Handler | Spine write |
|---|---|---|
| `agent_config.user.set_revision` | `user.go:80` | `:89` |
| `agent_config.user.rollback` | `user.go:145` | `:154` |
| `agent_config.user.skills.upsert` | `userskills.go:182` (`recordUserSkillsMembership`) | `:215` |
| `agent_config.user.skills.delete` | `userskills.go:182` (same helper) | `:215` |

**Not doors, and the reason for each** (each verified, not assumed):

- `agent_config.set_llm_provider` — takes the same write lock (`setllmprovider.go:112`) but never touches the registry: it drives `llmProviderInstaller.InstallLLMProvider` + an audit emit through `adminwrite.Apply` (`:114-133`). No revision, no base, no token.
- `agent_config.session.*` (five verbs, `session.go:44/70/94/124/165`) — the ephemeral session overlay, not the durable spine.
- The OAuth-resume attach re-drive (`addconnection.go:620`) and the compensating revert-to-empty (`setoauthprovider.go:188`) — internal reconciliation writers with no caller. Unconditional by design; the residual is in "Risks".

**What actually breaks today, precisely** (the defect surface is narrower than "all sixteen lose updates", and saying so is part of not over-claiming):

- `set_revision` / `user.set_revision` replace the **whole** payload with exactly what the caller sent. Any concurrent edit to any section, through any door, between the caller's read and its write is silently reverted. This is the large one.
- Two writers to the **same** section revert each other — including a section verb racing a `set_revision`.
- `rollback` / `user.rollback` move the pointer unconditionally and can discard a write that landed after the caller decided to roll back.
- The list-merging verbs (`skills.upsert`, `set_oauth_provider`, `add_mcp_connection`, …) re-read and merge, so a concurrent add of a *different* member survives — they are exposed to the whole-payload and same-section cases above, not to each other.

## Files added or changed

```text
internal/protocol/types/
  agentconfig.go                          # ExpectedContentHash on the 16 spine
                                          #   request types (optional, omitempty)
internal/protocol/errors/
  errors.go                               # CodeRevisionConflict + canonicalCodes
internal/protocol/transports/control/
  status.go                               # CodeRevisionConflict -> 409
internal/protocol/conformance/
  conformance.go                          # status-map row + exhaustiveness entry
internal/agentcfg/
  agentcfg.go                             # SetOptions{ExpectedContentHash};
                                          #   Registry.SetRevision / .Rollback take it;
                                          #   ErrRevisionConflict sentinel
internal/agentcfg/drivers/statestore/
  statestore.go                           # the precondition, inside the existing
                                          #   loadActiveRevision read; ordered BEFORE
                                          #   the idempotent short-circuit
  statestore_conditional_test.go          # NEW — match / mismatch / absent-active /
                                          #   nothing-persisted-on-refusal / ordering
internal/agentcfg/conformance/
  conformance.go                          # NEW rows — every driver inherits them
internal/runtime/agentcfg/protocol/
  service.go, promptlayers.go, llmparams.go, mcppolicy.go,
  skills.go, addconnection.go, removeconnection.go,
  setdiscoveryorigins.go, setoauthprovider.go,
  removeoauthprovider.go, user.go, userskills.go
                                          # thread the token; map ErrRevisionConflict
                                          #   -> CodeRevisionConflict
  conditional_write_test.go               # NEW — all 16 doors, unconditional golden,
                                          #   precondition-not-authority
  concurrency_test.go                     # EXTENDED — two racing writers, one refused
test/integration/
  phase221_agentcfg_conditional_write_test.go   # NEW
scripts/smoke/phase-221.sh                # NEW
docs/plans/phase-221-agentcfg-expected-revision.md  # NEW
docs/decisions.md                         # D-366
docs/glossary.md                          # expected-revision token; conditional config write
docs/skills/use-the-harbor-protocol/SKILL.md        # §18 same-PR surface update
docs/site/protocol/errors.md, types.md    # D-209 REGENERATED (make protocol-docs-gen)
web/console/src/lib/protocol/wire-manifest.gen.json # D-223 REGENERATED (make protocol-ts-gen)
web/console/src/lib/protocol/*.ts         # hand-mirrored field + code
```

**Regeneration ordering — this phase rebases, it does not regenerate in parallel.** Phase 219 owns `wire-manifest.gen.json` in Stage 1. This phase's wire change (sixteen optional fields + one error code) is mirrored and regenerated **after** rebasing onto 219's landed manifest, in one `make protocol-ts-gen && make protocol-docs-gen` run. Two Stage-1 branches regenerating the same committed artifact in parallel produce a conflict on a file that must never be hand-edited.

## Public API surface

```go
// internal/agentcfg

// SetOptions carries the optional preconditions a spine write may declare.
// The ZERO value is the unconditional write — byte-for-byte the behaviour
// that shipped before this option existed.
type SetOptions struct {
    // ExpectedContentHash, when non-empty, requires the agent's ACTIVE
    // revision to carry exactly this content hash at write time. A mismatch
    // (or no active revision at all) fails with ErrRevisionConflict and
    // persists nothing.
    //
    // The comparison is atomic against other writers IN THIS PROCESS, via
    // the agent-config service's per-owner write lock. It is NOT atomic
    // across Runtime processes sharing one StateStore: the StateStore has no
    // compare-and-swap primitive (see its own interface godoc). Two Runtimes
    // sharing one store can still lose an update.
    ExpectedContentHash string
}

// ErrRevisionConflict — a conditional write whose declared expectation no
// longer holds. The caller re-reads the active revision and retries.
var ErrRevisionConflict = errors.New("agentcfg: revision conflict")

type Registry interface {
    SetRevision(ctx context.Context, id identity.Quadruple, agentID string,
        scope ConfigScope, payload ConfigPayload, opts SetOptions) (Revision, error)
    Rollback(ctx context.Context, id identity.Quadruple, agentID, revisionID string,
        scope ConfigScope, opts SetOptions) (Revision, error)
    // Active / Get / ListRevisions / Diff / Close — unchanged.
}

// internal/protocol/errors

// CodeRevisionConflict — agent-config surface: a write declared an
// expected_content_hash and the agent's active revision no longer carries it
// (or there is no active revision). The request was well-formed and
// authorised; the target's current state forbids the operation. A dedicated
// machine-branchable code — NOT CodeInvalidRequest and NOT CodeRuntimeError —
// so a client can distinguish "re-read and retry" from a malformed request or
// a server fault. Maps to HTTP 409, the same posture as CodeSessionRunning /
// CodeSessionErased. Nothing is persisted on a refusal.
const CodeRevisionConflict Code = "revision_conflict"
```

An options struct on the existing methods, rather than a second `SetRevisionIf` method, is the §13 call: one write path that takes a precondition, not two write paths that differ by whether they check one. The interface has exactly one implementation plus the conformance suite, so the signature change is cheap and mechanical.

The evaluation order inside the driver is binding and is the one non-obvious part:

```text
active, hasActive := loadActiveRevision(...)      // the read that already happens
if opts.ExpectedContentHash != "" {               // 1. precondition FIRST
    if !hasActive || active.ContentHash != opts.ExpectedContentHash {
        return ErrRevisionConflict                //    nothing written
    }
}
if hasActive && active.ContentHash == newHash {   // 2. shipped idempotent re-set
    return active, nil                            //    unchanged
}
...                                               // 3. mint + save + emit, unchanged
```

Precondition-before-short-circuit is deliberate. The alternative order would let a stale token be converted into a `200` whenever the caller's payload happens to equal the *current* content — a success that misleads the caller into believing its base was still valid, which is the §5 silent-degradation shape. A conflict costs one re-read on a rare path and never lies.

## Test plan

- **Unit:**
  - `TestSetRevision_ConditionalWrite_MatchingHashProceeds` / `..._MismatchRefused` / `..._NoActiveRevisionRefused` — the three precondition outcomes, `errors.Is(err, agentcfg.ErrRevisionConflict)`.
  - `TestSetRevision_ConditionalWrite_RefusalPersistsNothing` — after a refusal: `ListRevisions` length unchanged, `Active` unchanged, and a subscribed real in-memory bus received **no** `agent.config.revised`.
  - `TestSetRevision_ConditionalWrite_PreconditionPrecedesIdempotentReset` — the ordering pin: a stale token plus a payload equal to the *current* content is a conflict, not a success. This test fails if the two blocks are transposed, which is the only way to catch that transposition.
  - `TestSetRevision_EmptyTokenIsUnconditional` — the §10 golden: each of the sixteen doors driven with an empty token against a shared fixture, comparing the persisted revision record, content hash, parent pointer, and emitted event to a recorded pre-phase golden.
  - `TestRollback_ConditionalWrite_*` — the same three outcomes on the pointer-move door.
  - `TestConditionalWrite_TokenIsPreconditionNotAuthority` — a valid token with an insufficient scope is refused by the scope gate; a valid token with an incomplete identity triple is refused by `ErrIdentityRequired`. The token never rescues either.
  - `TestConditionalWrite_AllSixteenDoorsAcceptTheToken` — table-driven over the sixteen methods; a new spine writer without the field fails here as well as in the smoke.
  - `TestErrorCode_RevisionConflict_RegisteredAndMappedTo409` — `IsValidCode`, `Codes()` membership, and the control-transport status binding.
- **Integration:** `test/integration/phase221_agentcfg_conditional_write_test.go` — the real `agentcfg` statestore driver over **real** StateStore drivers (in-memory **and** a file-backed SQLite via `t.TempDir()`, so the precondition is exercised against a real SQL read-modify-write, not just a map), a real in-memory `EventBus`, a real `audit.Redactor`, the real `agentcfg` protocol `Service`, and the real REST control transport over an `httptest.Server`. Covers: a Console-shaped read → edit → conditional write round-trip returning 409 with `{"code":"revision_conflict"}` after an interleaved second writer; identity propagation through every layer (two tenants and two users, each conditional write scoped to its own slot, no cross-talk); the empty-token path returning 200 with the same body shape as the pre-phase build; **failure mode** — a closed bus on the winning write, asserting the revision still landed and the conflict path is unaffected by the emit failure; and the `agent_config.get` → `agent_config.diff` recovery loop a conflicted client actually runs.
- **Conformance:** `internal/agentcfg/conformance` gains four rows run against the statestore driver (and every future driver): matching token proceeds; mismatching token fails `ErrRevisionConflict` with nothing persisted; token with no active revision fails; empty token is unconditional. Placed in the shared suite rather than the driver's own tests specifically so a second driver cannot ship the interface without the precondition (§9 parity).
- **Concurrency / leak:**
  - `TestSetRevision_ConcurrentConditionalWriters_ExactlyOneWins` — the test the upstream ask demands, and the one a mismatched-token unit test cannot substitute for: N=128 goroutines all read the same active revision, all submit a conditional write carrying that same hash, against ONE shared registry under `-race`. Asserts exactly **one** success, 127 `ErrRevisionConflict`, the active revision equals the winner's payload, `ListRevisions` grew by exactly one, and exactly one `agent.config.revised` was published.
  - `TestSetRevision_ConcurrentUnconditionalWriters_AllSucceed` — the §10 twin: the same N=128 with empty tokens still behaves as today (all succeed, last writer wins), so the feature did not tighten the unconditional path.
  - `TestService_ConcurrentReuse_MixedConditionalAndUnconditional` — D-025: N=128 across two tenants and two agents against one shared `Service` + one shared registry, asserting no cross-owner bleed, no cancellation cross-talk (cancelling one writer's ctx leaves the others intact), and `runtime.NumGoroutine()` back to baseline after teardown.
  - The cross-process bound is asserted **as a bound, not as a guarantee**: `TestConditionalWrite_CrossProcessBoundIsDocumented` drives two independently-constructed `Service` values over one shared SQLite store and asserts the lost update **still occurs**, with a top-of-test comment naming this as the documented residual and linking the `StateStore.SaveIf` follow-up. A property the mechanism lacks is pinned as absent rather than left to be discovered.

## Smoke script additions

`scripts/smoke/phase-221.sh` (`live-server`):

- **Static**: `expected_content_hash` is declared on all sixteen spine request types — asserted with an EXACT count, so *removing one* fails rather than silently passing a "≥1" presence check; `SetOptions` and `ErrRevisionConflict` exist in `internal/agentcfg`; the driver's precondition block appears **before** the idempotent-re-set comparison in `statestore.go` (a line-number assertion, because a transposition is the one mutation that leaves every grep-for-presence guard green); `CodeRevisionConflict` is in `canonicalCodes` and bound to `http.StatusConflict` in `status.go`; the conformance suite carries the four rows; `ProtocolVersion` is unchanged; and the single-process bound appears verbatim in the `SetOptions` godoc (a guard on the *honesty* of the claim, not only on its existence).
- **Live**: probe `POST /v1/agent_config/set_revision` with an EMPTY body to classify the route — a mounted route answers a typed `invalid_request`, an unmounted one answers `unknown_method` — following the phase-211 lesson that classifying on bare status converts a real answer into a SKIP. Then, on a mounted route: a conditional write carrying a deliberately-bogus `expected_content_hash` answers a typed `revision_conflict` with HTTP 409 (and specifically **not** `invalid_request` — a server that ignored the unknown field would answer differently); an unauthenticated conditional write is still refused by the identity/admin gate, proving the token did not become an authority. The live block degrades to SKIP on its own when the agent-config service is not mounted, so the static and unit legs still run standalone.
- **Unit-test legs**: four `go test -race -run` invocations — the driver's conditional-write tests, the conformance rows, the concurrent exactly-one-wins race, and the protocol-service door table. Each FAILS on a genuine failure and SKIPs only when the filter matches no tests at all.
- **Mutation verification is part of the phase, not a claim about it.** Each of these must be executed and each must turn a specific `OK` into a `FAIL` with SKIP unchanged: (1) delete the precondition block; (2) transpose the precondition and the idempotent short-circuit; (3) drop the field from one of the sixteen request types; (4) return `CodeInvalidRequest` instead of `CodeRevisionConflict`; (5) remove the 409 status binding; (6) delete one conformance row; (7) persist the revision record before the precondition check (proving the nothing-persisted assertion is load-bearing); (8) delete the process-bound sentence from the godoc. The PR records the observed OK/FAIL counts for each — a mutation that leaves the smoke green means the guard was decorative.
- Done-definition: OK ≥ 12, FAIL = 0 against the live preflight build.

## Coverage target

Baselines below were measured on this branch with `go test -cover` before any change; the target is the floor the PR must not fall below, and the PR records the re-measured figure.

- `internal/agentcfg/drivers/statestore`: **85%** — baseline measured 80.1%. The precondition lands here and its branches are directly tested, so the number should rise.
- `internal/agentcfg`: **80%** — baseline measured 77.1%. `SetOptions` + the sentinel are small additions; the rise comes from the conformance rows exercising the seam.
- `internal/runtime/agentcfg/protocol`: **85%** — baseline measured 85.5%; hold it. Sixteen doors each gain one field-threading line, all covered by the door table.
- `internal/protocol/errors`: **100%** — baseline measured 100%; a new constant plus a map entry must not drop it.
- `internal/protocol/transports/control`: **74%** — baseline measured 73.9%; the new status arm is covered by the code-mapping test.
- `internal/protocol/conformance`: **82%** — baseline measured 81.8%; hold it.
- `internal/protocol/types`: no target — the change is sixteen struct fields and their godoc, which contain no statements, so the package's measured 62.6% cannot move. That figure sits below what other plans state for the package; the shortfall predates this phase and is not addressed by it.

### As shipped — measured after the change

Re-measured with `go test -count=1 -cover` on `dev-experimental` after the wave landed. Two targets
are NOT met and are recorded as permanent deviations (§4.3) rather than restated as met. PR #623's
body disclosed both; this table is the plan-file record §4.2 item 11 requires.

| Package | Baseline | Target | As shipped | Verdict |
|---|---:|---:|---:|---|
| `internal/agentcfg/drivers/statestore` | 80.1% | 85% | **80.4%** | improved toward, **SHORT** |
| `internal/agentcfg` | 77.1% | 80% | **80.3%** | met |
| `internal/runtime/agentcfg/protocol` | 85.5% | 85% | **86.9%** | met |
| `internal/protocol/errors` | 100% | 100% | **100%** | met |
| `internal/protocol/transports/control` | 73.9% | 74% | **74.2%** | met |
| `internal/protocol/conformance` | 81.8% | 82% | **81.8%** | flat, **SHORT** |
| `internal/protocol/types` | 62.6% | none | **62.6%** | no target |

**`internal/agentcfg/drivers/statestore` — the 85% target rested on a wrong prediction about where
the uncovered statements are.** The precondition itself landed well covered: `SetRevision` measures
88.6% and every conditional branch is driven by the driver tests, the four conformance rows and the
exactly-one-wins race. The package's residual is elsewhere and this phase does not touch it — the
`StateStore` error arms in `emitRevised` (50.0%), `emitReverted` (50.0%), `loadActiveRevision`
(66.7%), `saveRevision` (66.7%), `loadRevision` (72.7%) and `saveActive` (71.4%), each an
`if err != nil` return on a `Save` / `Load` that neither the in-mem nor the SQLite driver fails in a
test. Adding ~4.6pp would mean building a fault-injecting `StateStore` fake purely to walk those
returns — coverage padding on the §17.3 "no mocks at the seam" boundary. Recorded as short.

**`internal/protocol/conformance` — the 82% "hold it" target was above its own stated baseline.**
The line reads "**82%** — baseline measured 81.8%; hold it", which is internally contradictory: a
floor 0.2pp above the baseline is a raise, not a hold. The package held exactly at 81.8%; the
phase's contribution there is one row in the code matrix, which adds no statements. The target was
misstated at authoring time and the honest reading is "hold at 81.8%", which was achieved.

### As shipped — deviation: the door count moved to SEVENTEEN one phase later

This plan enumerates and asserts **sixteen** spine-writing doors, and that was correct when it
shipped. Phase 222's `agent_config.set_extra_system_blocks` is the seventeenth, and this phase's
exact-count guards caught it exactly as designed — `scripts/smoke/phase-221.sh` reported three FAILs
(17 found, 16 wanted) on the wire types, the json tags and the Console mirror. Phase 222 extended
the behavioural table and its reflection twin rather than bumping the counts alone, so the new door
is DRIVEN with a stale token and asserted to refuse with `ErrRevisionConflict` like its siblings.
See D-367. The sixteen-door text above is left as the record of what this phase shipped; the live
count is maintained by the guards, not by this file.

## Dependencies

- 92a — the registry, the revision spine, the content hash, and the idempotent re-set this phase makes conditional.
- 92e — `set_prompt_layers`, the second door named in the upstream ask.
- 126a — the user-scope tier and `ConfigScope`, which supplies four of the sixteen doors.
- 219 — **ordering, not a code dependency**: 219 owns `wire-manifest.gen.json` in Stage 1, so this phase rebases onto it before running `make protocol-ts-gen`.

## Risks / open questions

- **The guarantee is single-process and the phase must never be described as more.** Stated in the `SetOptions` godoc, the generated `errors.md` row, the operator skill, and D-366; asserted as an absent property by `TestConditionalWrite_CrossProcessBoundIsDocumented`. The follow-up that would close it is named concretely: `StateStore.SaveIf(ctx, r, expectedEventID)` across the §9 triad with conformance rows. Filed as a tracking issue linked from D-366, not left as "someone should".
- **An unconditional writer can still clobber a conditional one, by design.** The token constrains the writer that supplies it, not the ones that do not. A caller wanting exclusivity needs every writer of that agent's config to participate — which is an operator/deployment convention, not something the runtime can enforce without making the token mandatory and breaking §10. Stated in the godoc so it is not discovered later as a surprise.
- **The internal reconciliation writers are unconditional and can supersede a conditional write.** The OAuth-resume attach re-drive (`addconnection.go:620`) records the connection descriptor when a paused attach completes, with no caller and no base. It runs under the same per-owner lock, so it never interleaves mid-write, but a conditional caller can still find its write superseded by a resume that landed between its read and its write. Correct behaviour — the resume is recording something that actually happened — and named here rather than hidden.
- **`ListRevisions`' maintenance-scoped scan is unchanged and still reads every record carrying the kind prefix** (`statestore.go:305-314` carries its own cost note). A client that responds to a conflict by listing revisions rather than calling `agent_config.get` will feel it. The recovery loop this phase documents uses `get` for exactly that reason; the scan cost is a pre-existing follow-up this phase neither worsens nor fixes.
- **The first-write race is unarbitrated.** Two callers creating an agent's very first revision cannot express "I expect none". Bounded deliberately (see Non-goals); the shape a future phase would use is a separate `expect_absent` boolean, rejected here as a second mechanism for a narrower window.
- **Sixteen request types is a wide mechanical edit, and the count is the risk.** A seventeenth spine writer added by a later phase without the field silently reopens the gap. Two guards: the exact-count smoke assertion and the `TestConditionalWrite_AllSixteenDoorsAcceptTheToken` table both fail on a door added without the field — the count is asserted, not documented.
- **`set_llm_provider` sits in the admin write family, takes the same lock, and is NOT a spine door.** Verified at `setllmprovider.go:114-133`. It is called out here because it is the one verb a reviewer would reasonably expect in the table, and its absence is a finding rather than an omission.

## Glossary additions

- **Expected-revision token** — the optional `expected_content_hash` an agent-config write may declare, requiring the active revision to still carry that content at write time.
- **Conditional config write** — a spine write carrying an expected-revision token; refused with `revision_conflict` when the precondition no longer holds.

Both land in `docs/glossary.md` in this PR.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target — **NOT met on two packages, deliberately left unticked (§4.3).** `internal/agentcfg/drivers/statestore` shipped at 80.4% against 85%, and `internal/protocol/conformance` held at 81.8% against a misstated 82% "hold" target. Both are recorded with their measured figures and their reasons in "As shipped — measured after the change" above. Neither is claimed as met.
- [x] If multi-isolation paths changed: cross-session isolation test passes — the integration test drives two tenants and two users through the conditional path and asserts no cross-slot effect
- [x] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** The `agentcfg.Registry` driver and the agent-config `Service` are both reusable artifacts; `TestService_ConcurrentReuse_MixedConditionalAndUnconditional` and `TestSetRevision_ConcurrentConditionalWriters_ExactlyOneWins` each run N=128 against one shared instance.
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** `test/integration/phase221_agentcfg_conditional_write_test.go`.
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry filed (none departed; D-366 files the decision itself)
- [x] D-223 lockstep: `make protocol-ts-gen-check` clean after rebasing onto phase 219's manifest
- [x] D-209: `make protocol-docs-gen-check` clean; `docs/site/protocol/errors.md` + `types.md` regenerated, never hand-edited
- [x] §18: `docs/skills/use-the-harbor-protocol/SKILL.md` updated in this PR with the field, the code, and the process bound
- [x] Every mutation in "Smoke script additions" executed, and the observed OK/FAIL delta recorded in the PR
