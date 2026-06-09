# Phase 84b / 84c / 84d — framework-consumer (headless SDK) lens review

> Status: **amendments applied** (2026-06-09) — every B/C/D edit below has been
> applied to the three plan files, the RFC §6.5 addendum, and the D-189 entry
> (all uncommitted in the same working tree). Kept as the rationale record.
> Original scope: evaluates the three
> pending plans (`docs/plans/phase-84b-multimodal-disposition-policy.md`,
> `phase-84c-provider-native-multimodal.md`, `phase-84d-embedder-semantic-retrieval.md`)
> against the consumer RFC §1 promises: a team that imports Harbor as a Go module,
> constructs the runtime in Go, calls the planner / `LLMClient` / stores directly,
> and never runs `harbor dev`, never serves the Protocol, never opens the Console.

## 0. The ground truth the evaluation stands on

**Where the run loop lives.** The thing that resolves `tasks.Task.InputArtifactIDs`
into `planner.InputArtifactView`s and drives the planner is `perTaskRunLoopDriver`
in **`cmd/harbor/cmd_dev_runloop.go`** (`resolveInputArtifacts` at
`cmd/harbor/cmd_dev_runloop.go:856`), mirrored verbatim per D-094 in
**`harbortest/devstack/devstack.go:1661`**. There is **no reusable run loop in
`internal/`**. A library consumer builds their own `RunContext` (the posture D-149
explicitly names: "library consumers building their own RunContext") and calls
`planner.MaterializeInputContent` (`internal/planner/multimodal.go:60` — a pure
function) themselves. Consequence: **any logic the 84-plans place in
`cmd_dev_runloop.go` is (a) invisible to the library consumer and (b) already
duplicated twice (cmd + devstack), about to be three times (every consumer's
hand-rolled loop).** This is the single most load-bearing fact for all three
verdicts.

**Which touchpoints are reusable vs. cmd-only**, per the plans' "Files added or
changed" sections:

| Touchpoint | Package | Reachable headless? |
|---|---|---|
| `AttachmentDisposition` enum + `InputArtifactView.Disposition` | `internal/planner` (planner.go:590) | ✅ consumer constructs the view |
| `materializeOne` policy consultation | `internal/planner/multimodal.go:81` | ✅ pure function |
| Per-MIME disposition map | `internal/config` (plan as written) | ⚠️ config-only as planned |
| Precedence resolution (hint > config > default) | `cmd/harbor/cmd_dev_runloop.go::resolveInputArtifacts` (plan 84b line 123) | ❌ cmd-only as planned |
| Protocol `disposition` hint | `internal/protocol/types` | n/a (Protocol path) |
| bifrost upload-in-`Complete` + `file_id` cache | `internal/llm/drivers/bifrost` | ✅ any `llm.Open` consumer |
| `ProviderFileID` part fields + `ErrContextLeak` exemption | `internal/llm/llm.go` | ✅ |
| `InputArtifactView.ProviderFileID` | `internal/planner` — but populated by whom? | ❌ see 84c finding C1 |
| `Embedder` interface + factory + bifrost driver | `internal/llm` (or `internal/embeddings`) | ✅ §4.4 seam |
| Semantic memory / skill retrieval modes | `internal/memory`, `internal/skills` | ✅ *if* injected via `Deps` (see 84d) |
| Embedding + retrieval-mode config | `internal/config` + `harbor.yaml` | ⚠️ must also be constructor-reachable |

**A standing caveat the plans inherit but did not create.** All production code
lives under `internal/`, which the Go toolchain restricts to importers inside
`github.com/hurtener/Harbor`. The project knows this — it is the recorded reason
`harbortest/` lives at the top level (decisions.md, Phase 71 entry: "a test kit
that test-authors are meant to consume from their own modules CANNOT live under
`internal/`"). So today, "library consumer" strictly means *in-module* consumers
(cmd, harbortest, examples) plus external teams consuming via `harbortest`'s
exported surface. The three plans' "Public API surface" sections name
`internal/...` identifiers an **external** module cannot import at all. This
review adopts the pragmatic frame (reusable-`internal` vs. cmd-only), but flags:
if external-team embedding is a real V-next goal, Harbor eventually needs a
facade/export decision (an RFC-level call, like the Phase 71 one — not these
plans' job). Until then, every "Public API surface" promise in a phase plan is a
*module-internal* stability promise. Worth one line in each plan so the section
doesn't over-claim.

---

## 1. Phase 84b — disposition policy. Verdict: **GAP — right primitive, wrong home for the resolver. Fixable with plan edits; no re-scope.**

### What already holds

- The enum and `InputArtifactView.Disposition` land in `internal/planner` —
  exactly where a headless consumer lives. A consumer who builds their own views
  **sets `Disposition` directly**; that is a real programmatic seam and it
  answers "what if you have neither the Protocol hint nor agent.yaml": you don't
  need either layer, you *are* the top-precedence layer. The plan never says
  this; it should.
- The materializer stays a pure function consulting the resolved value
  (`multimodal.go:81` becomes the fallback map) — pure, reusable, good.
- The default-parity golden test is precisely the API-stability guard an
  embedding team needs across versions.
- The `ref` default + `Fetch.Tool` hint (populated at `multimodal.go:176` via
  `ToolCatalogView`) is already fully programmatic.

### Gaps and concrete edits

**B1 — The precedence resolver is planned into the cmd-only run loop.** Plan 84b
"Files changed" assigns "thread the hint into the view; **resolve precedence**"
to `cmd/harbor/cmd_dev_runloop.go::resolveInputArtifacts` (+ the devstack
mirror). That makes the *policy semantics* — the actual D-189 deliverable —
cmd-only and triple-duplicated.
*Edit:* add `internal/planner/disposition.go` with an exported pure resolver,
e.g. `ResolveDisposition(hint AttachmentDisposition, policy DispositionPolicy,
mime string) (AttachmentDisposition, DispositionLayer)` — returning which layer
won (the plan's own audit criterion "records which disposition fired and why
(which layer won)" needs that return value anyway). `cmd_dev_runloop.go` and
devstack become thin callers. The library consumer calls the same function or
skips it by setting `Disposition` directly.

**B2 — The per-MIME map is config-only.** The plan defines the map in
`internal/config` + `harbor.yaml` and nowhere programmatic.
*Edit:* define the map/policy **type** in `internal/planner`
(`DispositionPolicy`, a per-MIME map + default), and have `internal/config`
merely *decode into it*. Programmatic consumers construct the policy value
directly; the binary builds it from `harbor.yaml`. This is the user-suggested
"policy as programmatic primitive first, config/Protocol as thin adapters" and
it costs nothing — it's the same code, homed one package lower.

**B3 — "Logged warning" obligations on a pure function.** The plan's
fail-loud criteria (`tool:<name>` unknown → warn; `provider_native` pre-84c →
notice; "every materialization records which disposition fired") can't be
emitted by a pure function with no logger/bus.
*Edit:* state explicitly that the resolver/materializer *returns* the
degradation + winning-layer facts (typed result, not a log side-effect) and the
**caller** (run loop, devstack, or the consumer's loop) logs/emits. Otherwise an
implementor will thread a logger into the planner package or, worse, log only on
the cmd path.

**B4 — No library-consumer example.** The plan ships a Console composer selector
and a Protocol smoke; nothing for the headless path. `docs/recipes/` exists and
is the established home (`configure-a-planner.md`, `define-a-tool.md`).
*Edit:* add a recipe (e.g. `docs/recipes/control-attachment-disposition.md`)
showing: construct `InputArtifactView` with an explicit `Disposition`, and/or
build a `DispositionPolicy` + call the resolver — no Protocol, no `harbor.yaml`.

**B5 — RFC §6.5 addendum wording bakes the transports into the contract.** The
already-drafted addendum says precedence is "per-attachment (a Protocol hint) >
per-agent (`harbor.yaml`) > runtime default". As written, the *carriers* are the
contract.
*Edit (RFC §6.5 + D-189, one sentence each):* name the layers abstractly —
per-attachment caller hint (the Protocol input-artifact field is one carrier;
direct `InputArtifactView.Disposition` construction is the other) > per-agent
policy map (`harbor.yaml` is one carrier; programmatic `DispositionPolicy` is
the other) > runtime default. The semantics, not the transports, are the
contract.

**Identity (question 5):** clean. Disposition resolution is identity-free;
artifact reads already take the explicit scope
(`cmd_dev_runloop.go:868` builds `ArtifactScope` from the quadruple), and a
library consumer supplies identity via `identity.With` / `identity.WithRun`
(`internal/identity/identity.go:76,86`). No Protocol-only seam.

---

## 2. Phase 84c — provider-native mechanism. Verdict: **GAP — the load-bearing seam choice is right, but the plan carries a second, contradictory seam that only works on the dev/Protocol path. Needs a decisive edit (small re-scope of the observability half).**

### What already holds

- **The upload-inside-the-driver-during-`Complete` seam is the correct and
  sufficient SDK seam.** It lives in `internal/llm/drivers/bifrost`, behind
  `llm.Open(ctx, cfg, deps)` (`internal/llm/registry.go:426`) — reachable by a
  consumer who never touches the planner at all, let alone the Protocol.
  `LLMClient` stays one method (RFC §6.5). Keep this; it is the whole phase.
- The `ErrContextLeak` exemption edit lands in `internal/llm` — reusable.
- Identity for the cache is *available* without the Protocol: the LLM edge
  already mandates identity in ctx (`llm.HasIdentity`,
  `internal/llm/llm.go:692-705` — "the runtime fails closed on missing
  identity"), and library consumers stamp it with `identity.With`/`WithRun`.

### Gaps and concrete edits

**C1 — Two seams, half-specified each, and they contradict.** The plan
simultaneously specifies (a) the driver performs the upload *during `Complete`*
and (b) `planner.InputArtifactView.ProviderFileID` plus a smoke that asserts
"`tasks.get` shows a `ProviderFileID` on the input-artifact view". If the upload
happens inside `Complete`, the run loop **never learns the `file_id`** at
view-resolution time — (b) is only satisfiable if the run loop *pre-uploads*
(a second upload path, on the cmd-only run loop, invisible to library
consumers, and a §13 "two parallel implementations" smell).
*Edit (decisive):* **one seam — the driver.** Drop
`InputArtifactView.ProviderFileID` from the plan (Public API surface, files
list, and the smoke assertion). Re-spec observability as an **event**: the
driver emits e.g. `llm.provider_file.uploaded {artifact_ref, provider, file_id,
modality}` on the bus it already holds (`llm.Open` requires `Deps.Bus` —
`registry.go:431`). The Protocol/Console surface it from the event stream like
everything else; the smoke asserts the event, not a task field. This serves
both consumer classes identically and keeps the Console honest as a Protocol
client.

**C2 — The part-level flag is the real public API and it's missing from the
plan's API list.** "The bifrost driver … on a `provider_native`-flagged
over-threshold part" — flagged *how*? The plan's materializer change "sets the
upload-needed flag on the part" implies a field on
`ImagePart`/`AudioPart`/`FilePart`, but the Public API surface section lists
only `ProviderFileID`/`DocumentType`.
*Edit:* name the field explicitly (e.g. `Disposition` or `ProviderNative bool`
on the three part structs in `internal/llm/llm.go:320,330,340`), add it to the
Public API surface, and state the consequence that matters for this review: **a
consumer constructing `CompleteRequest` by hand can set the flag directly and
get provider-native handling with zero planner, zero config, zero Protocol.**
That sentence is the headless reachability guarantee for 84c.

**C3 — `file_id` lifecycle owned partly by a cancel hook the library consumer
doesn't have.** "Orphan cleanup via `FileDeleteRequest` on cancel/expiry" —
*whose* cancel? If the delete call is wired into the cmd run loop's cancel
path, a library consumer leaks provider-side files forever.
*Edit:* the **driver owns the full lifecycle**: identity-scoped cache keyed by
`(tenant,user,session,artifact-hash)` (as planned, reading identity from ctx —
state this explicitly, citing `identity.From`), TTL/LRU expiry with
delete-on-evict, and best-effort cleanup on client `Close`. The run loop's
cancel path may *additionally* trigger early cleanup, but it must be a thin
call into a driver-exported method, never the only path. Avoid a new optional
`Supports*` interface for this (§4.4 ceremony rule) — `Close`-time + TTL
cleanup needs no new interface method.

**C4 — Disposition reachability dependency.** `provider_native` "only fires
when the policy resolves it" — with 84b's B1/B2 edits, a library consumer can
resolve it (or set it on the part per C2). Without those edits, 84c is
config/Protocol-gated for headless consumers. The two plans' edits travel
together; same wave is right.

**C5 — No library-consumer example.** *Edit:* recipe
(`docs/recipes/provider-native-attachments.md` or fold into B4's recipe): `llm.Open`
plus `identity.With` plus a `CompleteRequest` with a flagged over-threshold image —
no `harbor dev` anywhere.

---

## 3. Phase 84d — Embedder + semantic retrieval. Verdict: **HOLDS, with pinning edits — structurally the most SDK-friendly of the three because it lands in the §4.4 seam; the risk is under-specified injection, not wrong placement.**

### What already holds

- `Embedder` as interface + driver + factory + registry is the same shape as
  `llm.Open(ctx, cfg ConfigSnapshot, deps Deps)` (`registry.go:426`) — by
  construction reachable programmatically; `harbor.yaml` is just one way to
  build the `ConfigSnapshot`. Blank-import at `cmd/harbor/main.go` affects only
  the binary; a library consumer blank-imports the driver in their own main —
  the standard, already-shipped pattern for every Harbor driver.
- The memory subsystem already has the **exact injection precedent the plan
  needs**: `memory.Open(ctx, cfg, deps)` with a fail-loud dependency guard —
  `internal/memory/registry.go:134-135`: "`Deps.Summarizer` is required for
  strategy `rolling_summary` (no stub fallback)". Semantic retrieval slots in
  as `Deps.Embedder` + a retrieval-mode value with the mirrored guard, and is
  then fully constructible in Go with no config file.
- Identity scoping of vectors rides the existing store scoping — no new
  Protocol-only seam.

### Gaps and concrete edits

**D1 — Pin the injection seams; don't leave "internal/memory/… — the
semantic-retrieval mode" as a hand-wave.** *Edit:* the plan names
(a) `memory.Deps.Embedder` + the registry guard mirroring `registry.go:134`
("`Deps.Embedder` is required when retrieval mode is `semantic` — no stub
fallback"); (b) the equivalent constructor injection for the skills directory /
`skill_search` path. This single edit is what makes both consumers reachable
headless; everything else follows.

**D2 — Declare the Embedder à la carte, in writing.** §13 requires in-wave
consumers (memory + skills — satisfied); nothing requires the primitive to be
*buried* behind them. A framework consumer doing their own retrieval (the `ref`
document path D-189 itself motivates: "a developer keeping a doc as a `ref` and
retrieving over it needs embeddings") should call `Embed` directly.
*Edit:* one sentence in Goals + Public API surface: "the `Embedder` is a
standalone, factory-constructible primitive; memory/skills are its first
consumers, not its gatekeepers." Plus a recipe
(`docs/recipes/embed-and-retrieve.md`): `embeddings.Open` + `Embed` + cosine
ranking over your own corpus — no memory subsystem, no config file, no
Protocol. This also future-proofs the seam against the temptation to re-add a
`document.search` tool later as a *consumer of the same primitive* rather than
a parallel implementation.

**D3 — Decide identity-at-the-Embed-edge, explicitly.** `Embed(ctx, []string)`
carries no identity in its signature — correct (vectors are derived data; the
*stores* scope by the triple). But Harbor's LLM edge fails closed on missing
identity (`llm.go:692-705`) for governance/cost attribution and audit. The plan
is silent on whether `Embed` does the same.
*Edit:* pin it — recommend **yes, mirror the LLM edge** (identity mandatory in
ctx, fail closed), since embedding calls are billable LLM-provider traffic the
governance subsystem will eventually meter. Crucially this stays
Protocol-free: `identity.With` is the library consumer's path, same as chat.

**D4 — Vector persistence × the persistence-triad rule.** "Vectors persist
alongside memory records in the existing state/sqlite/postgres stores" — §9
forbids a feature that "only works on Postgres". *Edit:* state that all three
drivers implement vector persistence and the **conformance suite**
(`internal/memory/conformancetest`) gains the semantic-retrieval cases. (The
plan's acceptance criteria imply it; the conformance-suite extension should be
named.)

**D5 — Package home.** The plan offers `internal/llm/ifaces` *or*
`internal/embeddings`. For the SDK lens prefer **`internal/embeddings`** (own
factory/registry, own `ConfigSnapshot`/`Deps`) — it keeps the à-la-carte
primitive (D2) free of the chat client's `Deps` (artifacts + bus) that an
embeddings-only consumer shouldn't need, and matches the D-189 RFC line
("a distinct capability from chat … its own `Embedder` interface"). Minor;
decide in the plan, not at implementation time.

---

## 4. The split itself (question 7)

**The 84b → 84c → 84d split survives the SDK lens — strengthened by it.**
"Disposition is policy, not mechanism" (D-189) is *more* true for a library
consumer than for the Playground: the consumer is the policy author of last
resort, and 84b-first is what makes 84c opt-in for them too. No reordering.
The one structural adjustment is the B1/B2 re-homing — the policy primitive
(enum + map type + resolver) must be the `internal/planner` core with
config/Protocol as thin adapters over it, rather than precedence logic living
in the cmd run loop. That is a plan edit, not a re-scope: same enum, same
precedence, same default, same tests — homed one package down. 84c's C1 is the
only genuine re-scope (drop the `InputArtifactView.ProviderFileID` half; event
observability instead), and it *shrinks* the phase. 84d is independent and
correctly decoupled from 84c (embeddings serve the `ref` path either way).

One cross-cutting addition for all three: each plan's "Files added or changed"
should include `harbortest/devstack/devstack.go` *only* as a thin-caller mirror
(84b already lists it; 84c/84d should confirm whether the mirror needs touching)
— and each should add a `docs/recipes/` entry, because today every example
surface in the three plans (Console selector, Protocol smoke, Playground
upload) is on the path the headless consumer never walks.

## 5. Bottom line — is it framework-ready?

- **84b: not yet, but one edit away.** The primitive is in the right package;
  the resolver and the policy-map type are planned into the wrong ones
  (cmd run loop / config). Re-home them (B1/B2), make degradation facts
  return-values not logs (B3), reword the RFC addendum's layer carriers (B5),
  add the recipe (B4) — then it's the cleanest of the three.
- **84c: not yet — internally inconsistent on the seam.** The driver-internal
  upload is the right and fully headless-reachable mechanism; the
  `InputArtifactView.ProviderFileID` + `tasks.get` half quietly assumes a
  run-loop pre-upload that would exist only in `cmd/harbor`. Cut it (C1), name
  the part-level flag as public API (C2), give the driver the whole `file_id`
  lifecycle (C3), and 84c serves a raw `LLMClient` consumer as well as it
  serves the Playground.
- **84d: yes, with pinning.** It lands in the seam pattern the codebase already
  proves out (llm/memory factories + `Deps` fail-loud guards). Pin
  `Deps.Embedder` injection (D1), declare à-la-carte use (D2), decide identity
  at the Embed edge (D3), name the conformance extension (D4), and pick
  `internal/embeddings` (D5).

And the standing note from §0: all "Public API surface" promises are
module-internal until Harbor makes an explicit facade/export decision for
external-team embedding — a one-line acknowledgment in each plan keeps the
sections honest; the decision itself is RFC-scale follow-up work, not part of
this wave.
