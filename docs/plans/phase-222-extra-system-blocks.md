# Phase 222 — `ExtraSystemBlocks` on the agent-config payload

## Current status after Phase 225

Phase 225 and D-387 preserve Phase 222's admin-only trust and ordering model while correcting byte fidelity: normalization validates `strings.TrimSpace(body) != ""` but stores and hashes every admitted nonblank body exactly as supplied, including leading and trailing whitespace. Historical wording below that says the normalizer "trims" blocks means validation-only blank detection; it does not authorize rewriting body bytes.

## Summary

The agent-config payload's prompt surface is two flat `*string`s — `AgentConfigPromptLayers{Base, User}` (`internal/protocol/types/agentconfig.go:64-73`) — with no name, no ordering and no attribution. N independent capability sources that each want to contribute one prompt block therefore collapse into one opaque string, and removing one contributor's text means re-deriving the whole composition from prose. This phase adds a new agent-config payload SECTION, `ExtraSystemBlocks`, carrying an ordered list of `{name, body}` blocks with a dedicated admin verb, a deterministic declared-order render into the operator-trusted additive position, and name-addressed removal. Absent section ⇒ byte-identical prompt.

## RFC anchor

- RFC §6.2 — Planner / ReAct prompt; the structured sections this composes into (unchanged taxonomy).
- RFC §6.16 — Agent Registry; the blocks are part of the agent's content-hashed config, so a block edit is a config revision with free diff + rollback.
- RFC §6.15 — Governance; the admin-verb desired-state pattern every agent-config section already follows.
- RFC §5.2 — what the Protocol exposes; one additive method and two additive wire types on the agent-config control surface.
- RFC §6.5 — LLM client layer and the context-window safety net; the bound on how much composed prompt text may reach a provider.

## Briefs informing this phase

- brief 13
- brief 11

## Brief findings incorporated

- **brief 13 §2.1 (twelve XML-tagged sections):** the reference prompt is a fixed, structured taxonomy — depth comes from what goes INTO the sections, not from minting new ones per contributor. This phase composes N blocks into the EXISTING `<additional_guidance>` position rather than emitting one tag per block; a caller-named tag would make the section taxonomy a function of config data.
- **brief 13 §2.2 (dynamic per-turn augmentation):** the reference design merges runtime-supplied guidance into the system prompt as additive content, with the merge order fixed by the builder rather than by the contributor. Harbor's `buildAdditionalGuidance` already merges `operator-baked → override-additive → per-turn repair` in a fixed order (`internal/planner/react/prompt.go:714-724`); the blocks take a fixed slot in that same sequence.
- **brief 13 §2.3 (memory framing — UNTRUSTED data):** provenance decides framing. The brief's rule is why `<user_instructions>` is escaped and framed subordinate (`prompt.go:726-763`) while `<additional_guidance>` is joined raw. This phase argues the blocks' trust level from their WRITE DOOR rather than assuming one, and makes "blocks never carry user-authored text" a binding obligation with a test on the lower-tier write path.
- **brief 11 (Console feature surface):** prompt configuration is an operator surface rendered from canonical events plus a registry snapshot, never from a Console-side shadow store (D-061). Blocks land in the config payload, so the existing `agent.config.revised` event, the `agent_config.get` read and the revision diff carry them with no new Console-side state.

## Findings I'm departing from (if any)

**None from a brief.** One departure from the UPSTREAM ASK's placement, agreed before this plan was written and recorded in D-367:

The upstream ask proposed `ExtraSystemBlocks` on the PER-RUN override bundle. **It belongs on the agent-config PAYLOAD instead** — durable, per-agent, composable. A per-run bundle is consumed at the next message and reconstructed by whoever assembles the request, so a per-run block list "inherits the same 'who reconstructs the rest' problem it is meant to solve": the second contributor still has to know, and re-send, the first's block. On the config payload the block list is durable state the registry owns, readable by name through `agent_config.get`, and mutable by a name-addressed read-modify-write that phase 221's expected-revision token makes safe against a concurrent second contributor.

## Goals

- Give the agent-config payload a named, ordered, attributed carrier for N independent prompt contributions, so adding or removing one contributor's block is a name-addressed operation rather than a re-derivation of prose.
- Fix the render position and trust level of those blocks by argument from the write door, and pin both with tests.
- Make the ordering deterministic by construction — declared order, no map, no sort — and make a re-ordering a visible, hashed, diffable config change.
- Keep the change strictly additive: an agent with no `extra_system_blocks` section produces a byte-identical system prompt.

## Non-goals

- **Not a second way to write the base prompt.** `PromptLayers.Base` is the SPINE — one value, positional, replaced wholesale by a session `SystemPromptOverride`. A block is never the spine. The selection rule is stated in godoc and in the glossary: **spine → `Base`; user-authored → `User`; per-capability additive attribution → `ExtraSystemBlocks`.**
- **Not a lower-tier write surface.** The session-user safe subset (`agent_config.session.set_user_prompt`, claim-free per `internal/protocol/methods/methods.go:1440-1458`) writes ONLY `PromptLayers.User` and must keep writing only that. Blocks are admin-only. A test asserts the session verb cannot reach the section.
- **Not a home for user-authored or model-authored text.** The section renders verbatim in an operator-trusted position. Recalled conversation content belongs in phase 219's UNTRUSTED-framed `MemoryBlocks` tiers; user instructions belong in `PromptLayers.User`, which is escaped through `escapeUntrustedSection` (`prompt.go:748-763`).
- **No per-block upsert/delete verbs.** The section takes the desired-state whole-section replace every sibling section takes, and phase 221's expected-revision precondition is what makes a two-contributor read-modify-write safe. Shipping BOTH a section replace and per-item verbs would be §13's two mechanisms for one concept. The skills section's `skills.upsert` / `skills.delete` pair is not a counter-precedent to copy blindly: it exists because a skill BODY is an independently-addressable artifact, whereas a block is one element of an ORDERED composition whose order is semantic — a per-item upsert has no well-defined insertion position.
- **No collision with phase 220.** Phase 220 owns `planner.LLMOverrides.ExtraInstructions` (the per-run additive string). This phase introduces its OWN carrier and must NOT flatten blocks into that field: two Stage-2 phases converging on one string would erase this phase's whole point (attribution) and 220's precedence decision at once. Stated in both plans.
- **No `ProtocolVersion` bump.** Additive method + additive wire types.
- **No new error code.** The write door refuses malformed input with the existing `invalid_request`.
- **No new prompt SECTION or XML tag.** The taxonomy is untouched (the 92e non-goal, carried forward).
- **No Console page change.** The typed client mirrors the method and types (§4.5 item 5); rendering a block editor is the consuming page's work.

## Acceptance criteria

- [x] `agentcfg.ConfigPayload` gains `ExtraSystemBlocks *ExtraSystemBlocks` and the wire payload gains `extra_system_blocks`, carrying `Blocks []NamedBlock{Name, Body string}`.
- [x] A new admin verb `agent_config.set_extra_system_blocks` records a revision REPLACING ONLY that section — prompt layers, skills, tool exposure, connections, OAuth providers, LLM params, hooks and naming are all carried forward (the bidirectional section-preservation invariant, pinned in all directions).
- [x] The verb is in `canonicalAgentConfigMethods` AND `canonicalAgentConfigAdminMethods` (`internal/protocol/methods/methods.go:1370-1472`), so it gates on the verified `auth.ScopeAdmin` claim. A non-admin caller is refused.
- [x] The session-user safe subset cannot write the section: `agent_config.session.set_user_prompt` still produces a payload whose only prompt content is `PromptLayers.User` (extending the assertion at `internal/runtime/agentcfg/protocol/user.go:21,41`).
- [x] **Ordering is the declared slice order.** The renderer emits blocks in payload order with no sort and no map iteration anywhere on the path. Rendering the same payload N times is byte-identical.
- [x] **`NormalizePayload` does NOT sort the block list** (`internal/agentcfg/agentcfg.go:633-670`) — unlike `Skills.Names` (`sortDedup`, line 647) and `OAuthProviders` (`normalizeOAuthProviders`, sorted by name, lines 826-863), whose orders are NOT semantic. A re-ordering of blocks therefore changes the `ContentHash` (`agentcfg.go:868-877`) and is a real new revision, visible in the diff.
- [x] Block names are unique within the section and match a restricted identifier charset; a duplicate name is refused at the write door with `invalid_request`, naming both offending positions.
- [x] **Render position and trust:** blocks render VERBATIM (unescaped) inside `<additional_guidance>`, after the binary's baked operator guidance and BEFORE the additive extra-instructions, each preceded by a plain-text `[name]` label. A block name never becomes an XML tag.
- [x] Blocks survive a session `SystemPromptOverride` — the same property `ExtraInstructions` has, and for the same structural reason (`buildSystemContent` is reached on both branches of `baseRequest`, `prompt.go:201-227`).
- [x] **Absent ⇒ byte-identical.** An existing stored revision with no `extra_system_blocks` key unmarshals to nil, contributes nothing, and produces a system prompt byte-equal to today's — pinned by a byte-equality test, not by inspection.
- [x] Run-start projection resolves the active revision's blocks into the run's override bundle through the SAME shared projection `cmd/harbor` and `harbortest/devstack` both reach (`internal/runtime/agentcfg/projection`), next-turn only; in-flight runs keep their immutable snapshot (D-025).
- [x] The revision diff reports the block delta by name (added / removed / body-changed) plus a reorder flag; `agent.config.revised` is emitted; rollback repoints.
- [x] Identity is scoped by the triple; `agent_id` is a key, never an isolation filter (§6); admin authority is derived server-side from the verified session, never the request body.
- [x] `make protocol-ts-gen` and `make protocol-docs-gen` regenerate to a clean diff; the Console typed client mirrors the method and both wire types by hand; `ProtocolVersion` does not move.
- [x] `scripts/smoke/phase-222.sh` passes against the preflight dev server with OK ≥ 12 and FAIL = 0.

## Files added or changed

```text
internal/agentcfg/
├── agentcfg.go        # NamedBlock, ExtraSystemBlocks, the ConfigPayload
│                      #   section, the ORDER-PRESERVING normalizer, the
│                      #   ExtraSystemBlocksDiff and its Diff field
└── agentcfg_test.go   # the order-is-semantic hash property + the
                       #   normalizer's must-not-sort guard

internal/protocol/
├── types/agentconfig.go       # AgentConfigNamedBlock,
│                              #   AgentConfigExtraSystemBlocks, the payload
│                              #   field, the request/response, the diff
├── methods/methods.go         # MethodAgentConfigSetExtraSystemBlocks +
│                              #   both closed sets (canonical + admin)
├── singlesource/singlesource.go       # the method + the two wire types
├── transports/stream/agentconfig_handler.go  # the route + decode
└── conformance/conformance.go # the method matrix count

internal/runtime/agentcfg/
├── protocol/extrasystemblocks.go       # NEW — the admin verb: validate,
│                                       #   compose a revision, preserve
│                                       #   every sibling section, emit
├── protocol/extrasystemblocks_test.go  # NEW — the write door + the
│                                       #   all-direction preservation matrix
├── protocol/service.go                 # the two projection helpers
├── protocol/{llmparams,mcppolicy,skills,addconnection,removeconnection,
│             setoauthprovider,setdiscoveryorigins}.go
│                                       # carry the new section forward
└── projection/projection.go            # ActiveExtraSystemBlocks + the
                                        #   ApplyPromptLayers overlay

internal/planner/
├── planner.go                 # LLMOverrides.ExtraSystemBlocks + NamedBlock
└── react/prompt.go            # renderExtraSystemBlocks + its fixed slot in
                               #   buildAdditionalGuidance

cmd/
├── harbor-gen-protocol-docs/{methods,typeindex}.go
├── harbor-protocol-ts-lockstep/typeindex.go
└── harbor-protocol-ts-types/typeindex.go

web/console/src/lib/protocol/
├── agentconfig.ts             # the hand-mirrored wire types
├── client.ts                  # the method on the typed namespace
└── wire-manifest.gen.json     # REGENERATED (make protocol-ts-gen)

test/integration/agentcfg_extra_system_blocks_test.go  # NEW
scripts/smoke/phase-222.sh                             # the live gate
docs/site/protocol/{methods,types}.md                  # REGENERATED
docs/skills/use-the-harbor-protocol/SKILL.md           # §18 same-PR update
docs/{decisions.md, glossary.md}                       # D-367 + vocabulary
```

## Public API surface

```go
// internal/agentcfg
type NamedBlock struct {
    Name string `json:"name"`
    Body string `json:"body"`
}

// ExtraSystemBlocks is the ORDERED list of named, operator-authored
// additive prompt blocks. Order is SEMANTIC: it is the render order, so
// the canonical form preserves it and a re-ordering is a new revision.
type ExtraSystemBlocks struct {
    Blocks []NamedBlock `json:"blocks"`
}

type ConfigPayload struct {
    // ... existing sections ...
    ExtraSystemBlocks *ExtraSystemBlocks `json:"extra_system_blocks,omitempty"`
}

type ExtraSystemBlocksDiff struct {
    Added     []string // block names present only in the to-revision
    Removed   []string // block names present only in the from-revision
    Changed   []string // names whose body differs
    Reordered bool     // same name set, different order
}

// internal/protocol/methods
const MethodAgentConfigSetExtraSystemBlocks Method = "agent_config.set_extra_system_blocks"

// internal/protocol/types
type AgentConfigNamedBlock struct {
    Name string `json:"name"`
    Body string `json:"body"`
}

type AgentConfigExtraSystemBlocks struct {
    Blocks []AgentConfigNamedBlock `json:"blocks"`
}

type AgentConfigSetExtraSystemBlocksRequest struct {
    Identity          IdentityScope                `json:"identity"`
    AgentID           string                       `json:"agent_id"`
    ExtraSystemBlocks AgentConfigExtraSystemBlocks `json:"extra_system_blocks"`
    // + phase 221's expected-revision token, inherited on rebase.
}

// internal/planner
type NamedBlock struct {
    Name string
    Body string
}

type LLMOverrides struct {
    // ... existing fields ...

    // ExtraSystemBlocks are the durable, operator-authored named blocks
    // resolved from the agent's active config at run start. They render
    // VERBATIM, in declared order, into the operator-trusted additive
    // position, and they survive a SystemPromptOverride. Nil renders
    // nothing (no empty wrapper).
    ExtraSystemBlocks []NamedBlock
}

// internal/runtime/agentcfg/projection
func ActiveExtraSystemBlocks(ctx context.Context, reg agentcfg.Registry, agentID string,
    id identity.Quadruple) ([]agentcfg.NamedBlock, bool, error)
```

**As built (§4.3).** The overlay rides the EXISTING `ApplyPromptLayers` seam
rather than gaining its own run-loop call site: that function is already the ONE
place both `cmd/harbor`'s run loop and the `harbortest/devstack` twin reach, so
hosting the blocks there adds zero new drift surface. The disclosed consequence
is one additional `Active` read per run, which shifted two failure-injection
indices in `internal/runtime/serve/runloop_failures_test.go` (the hook read 4 →
5, the naming read 5 → 6); both comments now name the new read. The validation
and the two payload projections live in
`internal/runtime/agentcfg/protocol/service.go` (already in the file list)
rather than in a new file.

## The three decisions this phase settles

### 1. Where the blocks render, and at what trust level

**Chosen: VERBATIM (unescaped), inside `<additional_guidance>`, after the binary's baked operator guidance and before the additive extra-instructions, each preceded by a plain-text `[name]` label.**

The trust level is argued from the write door, not assumed:

- The section is written by ONE verb, and that verb is in `canonicalAgentConfigAdminMethods` — the `auth.ScopeAdmin` set (`internal/protocol/methods/methods.go:1459-1472`). That is the SAME tier that writes `PromptLayers.Base`, via `agent_config.set_prompt_layers` (`methods.go:1465`).
- `PromptLayers.Base` is already rendered verbatim and is strictly MORE powerful: it is the spine (`prompt.go:212-215`, `buildSystemContent`'s `sections = []string{systemPrompt}` branch at `prompt.go:590-592`). Escaping a block while leaving `Base` unescaped would defend against a writer who can already replace the entire prompt — incoherent.
- Contrast the layer that IS escaped: `PromptLayers.User` has a CLAIM-FREE lower-tier write path (`agent_config.session.set_user_prompt`, `methods.go:1440-1447`; `internal/runtime/agentcfg/protocol/user.go:21,41`), which is exactly why `renderUserInstructions` runs it through `escapeUntrustedSection` (`prompt.go:748-763`). Blocks have no such lower-tier path, and this phase makes keeping it that way a tested invariant rather than an assumption.

The obligation this creates, stated rather than engineered away: **escaping is not the boundary here — the write door's authority tier is.** A capability that wants to surface user-authored or model-authored text must not put it in a block; it uses phase 219's UNTRUSTED-framed memory tiers or `PromptLayers.User`. That obligation is in the wire godoc, the glossary entry and D-367.

Why `<additional_guidance>` rather than a new section: the ReAct taxonomy is fixed by brief 13 §2.1 and by 92e's explicit non-goal, and the additive position already carries the property the ask needs — `buildAdditionalGuidance` is reached on BOTH branches of `baseRequest` (`prompt.go:201-227`), so blocks survive a session `SystemPromptOverride` for free.

Why a plain `[name]` label rather than a `<block name="…">` tag: **the attribution the ask needs is a DATA-MODEL property, not a prompt-syntax one.** The gap is that a contributor cannot find and replace its own contribution in the CONFIG; the read surface plus a unique name closes that. Minting a tag from config data would make the prompt's structural taxonomy a function of caller input, and would require the name charset to be load-bearing for structural safety rather than merely for legibility.

### 2. Deterministic ordering

**Chosen: declared slice order. No map, no sort, no explicit index.**

- `[]NamedBlock` is a slice, so the render order is fixed by the data. A `map[string]string` keyed by name would reintroduce exactly the map-iteration nondeterminism phase 217 removed elsewhere; no map appears anywhere on the write → normalize → hash → project → render path, and the smoke greps for its absence.
- An explicit integer index was rejected: it invites collisions and gaps, and it needs its own tie-break rule — which is a sort, which is the thing being avoided.
- **Order is SEMANTIC here, which makes the canonical form different from its two sibling sections.** `Skills.Names` is `sortDedup`'d (`agentcfg.go:647`) and `OAuthProviders` is sorted by name (`agentcfg.go:826-863`), both because "a re-ordering of a set does not change the hash" (`ContentHash`'s own godoc, `agentcfg.go:865-868`). For blocks, a re-ordering DOES change the prompt, so it must change the hash. The normalizer therefore preserves order, validates bodies as nonblank without rewriting their bytes, and de-duplicates names; a test asserts that swapping two blocks yields a different `ContentHash`, and a second test asserts the normalizer's output order equals its input order (the mutation "someone made blocks consistent with skills by adding a sort" turns both red).
- Uniqueness of names is what makes remove-by-name well defined; the write door refuses a duplicate, so the section is an ordered set rather than a bag.

### 3. Interaction with `Base` / `User` — three positions, not two mechanisms for one concept

`Base`, `User` and blocks differ in all three of cardinality, trust and position, so this is not the §13 shape:

| | cardinality | write tier | framing | replaced by `SystemPromptOverride`? |
|---|---|---|---|---|
| `Base` | one | admin only | the spine, verbatim | yes — wholly |
| `User` | one | claim-free session path exists | subordinate, ESCAPED | yes — suppressed with the spine |
| blocks | N, ordered, named | admin only | additive, verbatim, labelled | no — survives |

Neither is expressible in the other: `Base` cannot be a block (it is the spine and it is replaceable); `User` cannot be a block (it is escaped and framed subordinate, because a lower tier may write it). The selection rule goes in godoc so a future contributor does not have to re-derive this table.

### §10 — additive, and absent means unchanged

The section is an optional pointer field. A stored revision written before this phase has no `extra_system_blocks` key, unmarshals to nil, normalizes out of the canonical form entirely (the same "an all-empty section drops out" rule `NormalizePayload` already applies to prompt layers at `agentcfg.go:640-644`), and therefore does not perturb an existing revision's `ContentHash`. `renderExtraSystemBlocks` returns `""` for a nil or empty list, so `buildAdditionalGuidance` joins nothing and the system content is byte-identical. **Pinned by a byte-equality test**, not asserted by inspection. No `harbor.yaml` key is added, so there is no config-schema migration.

## Test plan

- **Unit** (`internal/agentcfg/agentcfg_test.go`): the normalizer preserves input order (mutation: adding a sort turns it red); a re-ordering of the same blocks produces a DIFFERENT `ContentHash` while a re-ordering of `Skills.Names` still produces the same one (the two sibling behaviours asserted side by side, so the asymmetry is documented by test); empty names/bodies normalize out; an all-empty section drops out of the canonical form; the block diff reports added / removed / changed / reordered correctly, including the same-set-different-order case.
- **Unit** (`internal/runtime/agentcfg/protocol/extrasystemblocks_test.go`): the write door refuses a duplicate name, an empty name, and an out-of-charset name with `invalid_request`, naming the offender; a valid write records a revision; **the all-direction section-preservation matrix** — setting blocks preserves prompt layers, skills, tool exposure, connections, OAuth providers, LLM params, hooks and naming, AND each of those verbs preserves the blocks section (the bidirectional invariant 92d established; a new section that only one direction preserves is the classic drop); a non-admin caller is refused; the session-user safe-subset verb produces a payload with a nil blocks section.
- **Unit** (`internal/runtime/agentcfg/projection`): `ActiveExtraSystemBlocks` resolves the active revision's blocks in order; no active revision and no section both yield "not set" without error; the overlay composes onto an existing `*planner.LLMOverrides` without clobbering the prompt layers it already carries.
- **Unit** (`internal/planner/react/prompt_test.go`): blocks render in declared order with their `[name]` labels; a body containing `<` and `&` is NOT entity-escaped (the property that distinguishes this position from `<user_instructions>` — a mutation routing blocks through `escapeUntrustedSection` turns it red); a block name is never emitted as a tag; blocks render ABOVE the additive extra-instructions and BELOW the baked operator guidance; the section is omitted entirely when the list is nil or empty (no empty wrapper); **blocks still render when `SystemPromptOverride` is set**; and the **byte-identity pin** — with a nil block list, `buildSystemContent`'s output is byte-equal to the pre-change output for the same `RunContext`.
- **Unit** (determinism): the same payload rendered 1000 times produces byte-identical output. Cheap, and it is the guard that catches a map sneaking onto the path in a later refactor.
- **Integration** (`test/integration/agentcfg_extra_system_blocks_test.go`, real drivers on every seam — a real `agentcfg` registry over the real `statestore` driver, a real `events` inmem bus, a real `audit` patterns redactor, the real Protocol service over the real stream transport, the real projection, the real ReAct prompt builder):
  - **The N-contributor round trip:** contributor A writes block `alpha`; contributor B reads the section, appends `beta`, writes back; both blocks are present, in order, and A's body is byte-unchanged. Then B removes `alpha` by name and writes; `beta` survives with its body and position intact. This is the composability property the ask is about, asserted end to end.
  - **Run-start projection:** the active revision's blocks reach the built system prompt through the shared projection, in order, verbatim, and inside `<additional_guidance>`.
  - **Identity propagation:** the triple flows Protocol → service → registry → projection → `RunContext`. A second tenant's agent with the SAME `agent_id` sees only its own blocks — `agent_id` is a key, never an isolation filter (§6 clarifying note).
  - **Failure mode 1:** a non-admin caller is refused, and NO revision is written (the revision chain length is unchanged after the refusal).
  - **Failure mode 2:** a duplicate block name is refused at the write door and nothing is persisted.
  - **Failure mode 3:** a cross-tenant body identity is refused before the registry is consulted (the `bodyscope` posture), with the new request type registered on the agent-config surface.
  - **Failure mode 4 (the trust invariant):** the claim-free session verb cannot write the section — a `session.set_user_prompt` call leaves `ExtraSystemBlocks` untouched.
- **Conformance:** the agent-config driver conformance suite (`internal/agentcfg/conformance`) gains the new section in its payload round-trip, so all three drivers persist and return blocks in order. The Protocol method matrix covers the new method by construction (it iterates `methods.Methods()`).
- **Concurrency / leak:** `TestSetExtraSystemBlocks_ConcurrentReuse_NoCrossTalk` — N=128 concurrent writes + projections against ONE shared `Service` and ONE registry under `-race`, each goroutine in its own tenant with its own distinguishable block set, asserting every goroutine's projected blocks are exactly its own, in its own order. The per-agent write lock (`s.lockAgent`, used by every sibling verb) is exercised by a second arm in which 32 goroutines write the SAME agent: the final revision must be one of the 32 written payloads in full, never a splice of two. No goroutines are started by the service, so the package's existing leak baseline holds.

### Mutation verification (binding)

Each guard is verified to fail — never to skip — when the thing it guards is removed. The six mutations to run before the PR, each naming the sub-test that must go red:

1. Add a `sort` to the block normalizer → the order-preservation test AND the order-changes-the-hash test both FAIL.
2. Change the carrier to `map[string]string` → the determinism test FAILS (and the compile breaks first, which is the point of choosing a slice).
3. Route block bodies through `escapeUntrustedSection` → the verbatim-render test FAILS.
4. Drop the blocks arm from ANY sibling verb's section carry-forward → the corresponding cell of the all-direction preservation matrix FAILS.
5. Remove the method from `canonicalAgentConfigAdminMethods` → the non-admin-refused test FAILS.
6. Drop the duplicate-name refusal → the write-door test FAILS and the remove-by-name integration arm becomes ambiguous.

A guard that only stops SKIPPING is not a guard. The PR description records the run.

## Smoke script additions

`scripts/smoke/phase-222.sh` (`PREFLIGHT_REQUIRES: live-server`). Live legs use the dev bearer and the `X-Harbor-{Tenant,User,Session}: dev` headers, with a per-invocation agent id so the script is idempotent against a long-lived server (the pattern `scripts/smoke/phase-217.sh:189-192` establishes).

1. **Static, the phase gate:** `internal/protocol/types/agentconfig.go` declares `AgentConfigExtraSystemBlocks` and the payload field; the live legs branch on this grep.
2. **Static, ordering by construction:** the carrier is a SLICE (`Blocks []`), and neither the wire type nor the domain type declares a `map` for blocks. FAILS on mutation 2.
3. **Static, the must-not-sort guard:** the blocks arm of `NormalizePayload` does NOT call `sortDedup` or `sort.` — the asymmetry with `Skills.Names` is deliberate and mechanically pinned. FAILS on mutation 1.
4. **Static, admin tier:** the method is in BOTH `canonicalAgentConfigMethods` and `canonicalAgentConfigAdminMethods`. FAILS on mutation 5 — and this is the guard that keeps the verbatim-render decision honest, since the whole trust argument rests on the write door's tier.
5. **Static, the trust obligation:** the wire godoc names the verbatim property AND states that blocks must not carry user-authored text.
6. **Static, the lower tier stays out:** `internal/runtime/agentcfg/protocol/user.go` still writes only `PromptLayers`, with no reference to the blocks section.
7. **Static, the render slot:** `internal/planner/react/prompt.go` renders blocks inside `buildAdditionalGuidance` and does NOT pass them through `escapeUntrustedSection`. FAILS on mutation 3.
8. **Static, single-source:** the method appears in `singlesource.go`, in all three generator type indexes, and in the Console typed client (the D-223 hand-mirror obligation).
9. **Live:** an authenticated `set_extra_system_blocks` write of two named blocks returns 200.
10. **Live:** `agent_config.get` returns the two blocks IN THE WRITTEN ORDER — the ordering decision asserted over the wire, not only in Go. A reversed order is a FAIL.
11. **Live:** a duplicate block name is refused 400 and the error names the offending name.
12. **Live:** an out-of-charset / empty block name is refused 400.
13. **Live, the section-preservation invariant:** after the block write, a `set_prompt_layers` write on the same agent leaves the blocks intact (read back and compare), and a subsequent block write leaves the prompt layers intact. Two directions, both asserted — mutation 4's live counterpart.
14. **Live:** a write with no identity headers is refused 401.
15. **Unit-test legs** under `-race`: the normalizer + hash asymmetry + diff suite in `./internal/agentcfg/`; the write door + preservation matrix + concurrent-reuse suite in `./internal/runtime/agentcfg/protocol/`; the projection suite in `./internal/runtime/agentcfg/projection/`; the render + verbatim + byte-identity + determinism suite in `./internal/planner/react/`; the integration seam in `./test/integration/`. Each leg OKs on a real pass, SKIPs only when the `-run` filter matches nothing, FAILs on a genuine failure.

Done-definition: OK ≥ 12, FAIL = 0.

## Coverage target

Measured on the branch with `go test -count=1 -cover` before any change; each target is "hold at or above the measured baseline":

- `internal/runtime/agentcfg/protocol`: **85%** (measured baseline 85.5%) — the package that gains the verb, the write-door validation and every sibling's carry-forward arm.
- `internal/agentcfg`: **77%** (measured baseline 77.1%) — the domain types, the order-preserving normalizer and the block diff.
- `internal/runtime/agentcfg/projection`: **82%** (measured baseline 82.1%) — the run-start resolution and the overlay.
- `internal/planner/react`: **87%** (measured baseline 87.0%) — the renderer and its fixed slot.
- `internal/protocol/types`: **62%** (measured baseline 62.6%) — wire structs add no statements; stated so a reviewer does not read the low number as a regression this phase caused.

### As shipped — measured after the change

Re-measured with `go test -count=1 -cover` on `dev-experimental` after the wave landed. Every target
is met. (The `internal/agentcfg` and `internal/runtime/agentcfg/protocol` figures are shared with
phase 221, which landed in the same wave and touches both packages; the numbers below are the
composite post-wave state, which is the state the gate actually measures.)

| Package | Target (baseline) | As shipped | Verdict |
|---|---:|---:|---|
| `internal/runtime/agentcfg/protocol` | 85% (85.5%) | **86.9%** | met — the write door, the validation table and the preservation matrix |
| `internal/agentcfg` | 77% (77.1%) | **80.3%** | met — the order-preserving normalizer, the hash asymmetry and the block diff |
| `internal/runtime/agentcfg/projection` | 82% (82.1%) | **82.3%** | met — the run-start resolution and the overlay |
| `internal/planner/react` | 87% (87.0%) | **87.0%** | held — the renderer and its fixed slot |
| `internal/protocol/types` | 62% (62.6%) | **62.6%** | held — wire structs add no statements |

### As shipped — deviations (§4.3)

Each is recorded in D-367 as well; they are repeated here so the plan is not read as the built shape.

1. **This section's verb is the SEVENTEENTH spine-writing door, and phase 221's exact-count guards
   caught it** — `scripts/smoke/phase-221.sh` reported three FAILs (17 found, 16 wanted) on the wire
   types, the json tags and the Console mirror. Bumping the counts alone would have been the wrong
   fix: 221's behavioural door table and its reflection twin are hand-enumerated, so they would have
   stayed green while the new door went undriven. Both were extended, so the new door is DRIVEN with
   a stale token and asserted to refuse with `ErrRevisionConflict`; the smoke's three counts, the two
   test length assertions, the two test names and the glossary's door count all move together.
   Mutation-verified both ways.
2. **The smoke's ordering fixture was inert as authored and was replaced.** Its two-block fixture was
   `[alpha, beta]` — already sorted — so the ordering assertion reported OK against a sorting mutant.
   The fixture is now deliberately reverse-alphabetical (`[zulu, alpha]`), which is what makes
   mutation 7 (add a sort to `payloadToWire`, rebuild the binary, re-run the live smoke) turn the
   live ordering leg red.

## Dependencies

- **221** — the expected-revision token on the agent-config write requests. It also edits `internal/protocol/types/agentconfig.go` and lands in Stage 1. This phase is Stage 2 and **rebases on 221**: the new request type carries 221's token from the start (a write request added after the token exists and does not take it is an immediate inconsistency), and `make protocol-ts-gen` / `make protocol-docs-gen` run once AFTER the rebase so the committed manifest and generated docs carry both changes. Regenerating in parallel and merging two generated diffs is the D-223 / D-209 failure mode this ordering avoids. The token is also what makes the whole-section desired-state replace safe for two concurrent contributors — the reason this phase ships one verb rather than per-item verbs.
- **92a** — the agent-config registry, its content-hashed revisions, its diff and its rollback.
- **92e** — the layered prompt section, whose trust boundary this phase reasons from and must not disturb.
- **92g** — the session-user safe subset, the lower tier that must stay unable to reach this section.
- **83a** — the structured ReAct prompt sections this composes into.
- **219 / 220** — sibling wave phases. 220 owns the per-run additive string; 219 owns the UNTRUSTED memory tiers. Neither is a carrier for blocks, and blocks are not a carrier for either.

## Risks / open questions

- **No cap on block count or body size, deliberately.** The bounds are (a) `ProtocolConfig.MaxRequestBytes` at the wire door (default 4 MiB, `internal/config/config.go:1937-1944`) and (b) the LLM edge's token-budget guard, which fails loudly with `ErrContextWindowExceeded` (`internal/llm/safety.go:105-113`, `internal/llm/errors.go:41`). **The byte leak check does NOT cover this content — verified, because the obvious assumption is wrong:** `findContextLeak` treats only `RoleTool` text as offloadable (`internal/llm/safety.go:360`), so system-role text is byte-exempt from `ErrContextLeak`. Inventing a per-section cap would be operator policy with no consumer asking for it, and `PromptLayers.Base` — unbounded on the same surface today — would make a cap on blocks pure asymmetry. Named as a risk, with the two real bounds cited in godoc rather than implied away.
- **The verbatim decision rests entirely on the write door's tier.** If a future phase adds a lower-tier write path to this section, the escaping question reopens immediately. The smoke's admin-tier guard (item 4) and the session-verb guard (item 6) are what make that reopening loud rather than silent; both are mutation-verified.
- **A block is attributed in the CONFIG, not in the model's reading of the prompt.** The `[name]` label helps a human debugging a transcript; it is not a security boundary and must not be described as one. Two blocks from two capabilities are, to the model, one contiguous run of trusted guidance — the same honest limitation phase 220 records for its two producers.
- **Read-modify-write is only as safe as 221's token.** A contributor that omits the expected-revision token gets last-write-wins and can silently drop a sibling's block. This phase's godoc directs contributors to send the token; enforcing its PRESENCE (rather than only honouring it when sent) is 221's decision to make, not this phase's to pre-empt.
- **No open RFC question gates this phase.**

## Glossary additions

- **extra system block** — one named, operator-authored unit of additive prompt text held in the agent-config payload's `ExtraSystemBlocks` section. Blocks are ORDERED (declared order is render order and is part of the content hash), name-unique, admin-written, rendered VERBATIM with a plain-text `[name]` label into the operator-trusted additive position, and they survive a session `SystemPromptOverride`. They exist so N independent capability sources can each contribute and later remove exactly their own text. Distinct from the **layered system prompt** (`Base`, the replaceable spine; `User`, the escaped subordinate layer) and from **additive guidance** (`ExtraInstructions`, the single flat per-run/per-tenant string).

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target, from a `go test -cover` run recorded in the PR description
- [x] If multi-isolation paths changed: cross-session isolation test passes — the integration test's same-`agent_id`-two-tenants arm
- [x] **If this phase builds a reusable artifact: concurrent-reuse test passes** — `TestSetExtraSystemBlocks_ConcurrentReuse_NoCrossTalk`, N=128 against one shared `Service` + registry under `-race`, asserting no data races, no context bleed (each tenant sees only its own blocks in its own order), no cancellation cross-talk, no goroutine leak
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists** — `test/integration/agentcfg_extra_system_blocks_test.go`, real drivers on every seam, identity propagation asserted, four failure modes covered, under `-race`
- [x] The six mutation runs recorded in the PR description, each naming the sub-test that went red
- [x] Rebased on 221 BEFORE running `make protocol-ts-gen` / `make protocol-docs-gen`; the new request carries 221's expected-revision token; both generators produce a clean diff
- [x] §18: `docs/skills/use-the-harbor-protocol/SKILL.md` updated in the same PR
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry filed (D-367)
