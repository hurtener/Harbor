# Phase 212 — artifact read-path byte correctness + a classified reference-resolution observation

## Summary

`artifact_fetch` returns its window through a `Content string` field (`internal/tools/builtin/artifact_fetch.go:170`, populated at `:279` by `string(window)`). The Go conversion is lossless, but the observation is JSON-encoded on its way to the model, and `encoding/json` rewrites every invalid UTF-8 byte to `U+FFFD` — so a PDF, an image or a zip is delivered corrupted, at a *different length* than the `returned_bytes` the same response reports. This phase makes an inadmissible window refuse loudly and makes an admissible one exact, including the `Offset` arithmetic and the paging-termination property the current windowing does not hold. Separately — and much smaller than the first draft claimed — the runtime's tool-error recovery is already shipped (`internal/runtime/steering/runloop.go:947-957`); what is missing is a **machine-distinguishable class** on the resulting observation, so a planner can tell "the id you named does not resolve" from "the tool itself failed" and from "the runtime is misconfigured". That gap is closed by classifying on the sentinels that already exist, not by adding one.

## RFC anchor

- RFC §6.2 — the planner interface and `Trajectory`, whose `Step.Observation` / `Step.LLMObservation` pair is where the classification lands.
- RFC §6.4 — tool catalog and transports: the dispatch boundary where an artifact-reference parameter resolves and where a resolution failure is produced.
- RFC §6.5 — the LLM client layer's context-window safety net, whose recovery path `artifact_fetch` is.
- RFC §6.10 — Artifacts: the read side.

## Briefs informing this phase

- brief 05
- brief 07
- brief 13

## Brief findings incorporated

- **brief 05 §1 (l.29):** "A `NoOpArtifactStore` fallback that silently warns and truncates is an anti-pattern. **Harbor ships no such fallback.**" Read forward onto the READ side: a read that hands back replacement characters *is* a silent truncate-and-warn-nobody, performed at the last hop where it is least visible. The routing guarantee brief 05 makes mandatory on the write side is only worth what the read side returns.
- **brief 05 §"Anti-patterns" (l.301):** "A `NoOpArtifactStore` silent fallback that warns once and truncates is unsafe." The `U+FFFD` rewrite is worse than the shape the brief names, because it does not warn *once* — it does not warn at all, and it changes the byte count while reporting the old one.
- **brief 07 §"`llm_observation` vs `observation`" (l.104):** "Harbor maintains two views — the canonical `observation` for trajectory persistence and audit, and an LLM-facing `llm_observation` that may have been redacted." The classification must land on BOTH views, because the runloop assigns the same payload to each (`runloop.go:955-957`) and the audit view is the one a post-incident reader reconstructs a failed run from.
- **brief 07 §4 (l.189):** the `ObservationRenderer` "performs LLM-facing redaction (heavy outputs replaced with artifact refs)." That is the write half of the loop; `artifact_fetch` is the read half, and a read half that returns damaged bytes makes the ref an advertisement for something that cannot be collected.
- **brief 13 (l.134):** "Examples are the most token-efficient way to constrain `args` shape — a single … example is worth several lines of schema prose." Applied to the `<heavy_results>` reconciliation: the block gains the paging rule as a concrete two-call shape and the binary case as a concrete `mime`-discriminator sentence, rather than a paragraph of caveat prose.
- **brief 13 (l.18):** the reference prompt design "uses twelve XML-tagged sections … and a per-turn augmentation pass." `<heavy_results>` is one of those sections and its own godoc (`internal/planner/react/prompt.go:394`, `:438-446`) says "New meta-tools that act on stored references extend the bullet list in this section as they land." A *changed* meta-tool contract obliges the same edit, and it did not happen when `offset` shipped — see the departure below.

## Findings I'm departing from (if any)

None from the briefs. Two departures from the **first draft of this same phase**, both forced by facts verified against the tree and recorded here rather than silently dropped:

1. **The "dead run" premise is retracted.** The first draft (and the master-plan detail block at `docs/plans/README.md:355`) claim an unresolvable reference "terminates the dispatch step" and that "`ErrNoResolver` stays step-terminating." Both are false. `internal/runtime/steering/runloop.go:947-957` converts **every** `ExecuteDecision` error into `map[string]any{"error": execErr.Error()}` and assigns it to the step's `Observation` and `LLMObservation`; its own comment reads *"The runloop does NOT abort the run on a single tool error — that's the planner's call."* The step is then appended to the trajectory (`:1004-1010`) with no `Step.Error` and no `Step.Failure`, so `renderObservationForLLM` (`internal/planner/react/prompt.go:1809-1824`) renders it through `renderAny` and the model reads it on the next turn. The recovery the draft proposed building is shipped. **Only the classification is missing**, and this phase is re-scoped to that.
2. **No new sentinel.** `dispatch.ErrArtifactRefNotFound` (`internal/runtime/dispatch/dispatch.go:64`) already *is* the typed classification, and `withArtifactResolver` already returns it (`:366`). A second `ErrResolveFailed` would be two sentinels for one fact — the §13 "two parallel implementations of the same conceptual feature" shape. What a full-tree grep shows instead is more interesting: **`ErrArtifactRefNotFound` has exactly one reference in the whole repository outside its own declaration — its own producer at `:366`. There is no `errors.Is` on it anywhere, in production or in tests.** It is a classification nothing classifies on. This phase gives it its first consumer.

## Goals

- A read of an artifact whose window is not admissible as text refuses loudly, naming the stored MIME and the byte offset where admissibility failed, and names the route those bytes *can* travel — rather than delivering `U+FFFD` soup at a length the response misreports.
- A read of an admissible window is byte-exact and self-consistent: `Content` is literally `blob[Offset : Offset+ReturnedBytes]`, so a model paging by the documented rule reassembles the artifact without gaps or duplicates.
- A paging loop over any artifact terminates. Today it provably does not for some legal `max_bytes`.
- A resolution failure is distinguishable, by a field rather than by string-matching an error message, from a tool's own error and from a runtime misconfiguration — on both the single-call and the parallel dispatch paths, and in both the canonical and the LLM-facing observation view.
- The shipped fetch hint that points a model at `artifact_fetch` for auto-materialized binary attachments keeps pointing somewhere useful. This is a hard constraint, not a nice-to-have; see "The reconciliation" below.
- The Protocol byte read and the tool byte read stop being described as one helper when they are deliberately two.

## Non-goals

- **No base64 of binary into the model's context.** A 1 MiB PDF is ~350–470k tokens of base64 the model can do nothing with. The route for binary is pass-by-reference — phase 210's shipped in-process arm (`internal/tools/artifactref.Ref`, public as `sdk/tools.ArtifactRef`) — and this phase *points at* that route rather than widening this one.
- **No change to `boundedWindow` or to `artifacts.get`.** `types.ArtifactsGetResponse.Content` is `[]byte` (`internal/protocol/types/artifacts.go:351`) and is base64 over the wire, so it is byte-exact by construction. Rune-trimming it would **corrupt** an operator's PDF read. The divergence is deliberate and this phase documents it on both sides; the behaviour of `internal/protocol/artifacts.go:741-753` does not move.
- **No change to the heavy-output threshold or to the two `artifacts.fetch_*_max_bytes` config keys.** Phase 213 owns those. This phase touches neither `internal/config/config.go` nor `docs/CONFIG.md`, deliberately, because both are on 213's file list and a shared-file edit here buys nothing (see Risks).
- **No change to `internal/llm/materialize.go`.** The fetch hint stays exactly as shipped. See "The reconciliation".
- **No change to who may read which artifact.** The scope construction at `artifact_fetch.go:222-234` and the run-scoped resolver at `dispatch.go:344-370` are untouched.
- **The class set is closed at two, on purpose.** `artifactref.ErrEmptyID`, `ErrUnresolved` and `ErrNotAddressable` are NOT classed. `ErrEmptyID` is an argument-shape failure already carried as `tools.ErrToolInvalidArgs` by `inproc.go:228` and is the schema's job to prevent; the other two are programming errors — a tool wired wrong, or a reference read before dispatch bound it — and neither a model nor an operator has a repair for them. Naming them here so the next author knows the set is closed by decision and where the seam to extend it is.
- **No new Protocol method, wire type, error code or event.** `ProtocolVersion` does not move; no `wire-manifest.gen.json` or `docs/site/protocol/*` regeneration, so this phase does not participate in the wave's generated-file contention.

## The reconciliation — the shipped fetch hint and `<heavy_results>`

This is the load-bearing design decision of the phase and it is stated before the acceptance criteria because three of them depend on it.

**The conflict, verified.** `internal/llm/materialize.go:181-196` stamps `Fetch: &StubFetch{Tool: "artifact_fetch", ID: ref.ID}` onto the `ArtifactStub` for **every** auto-materialized over-threshold attachment. `materializeRequest` (`:40-105`) walks exactly three part kinds — `PartImage`, `PartAudio`, `PartFile` — so every stub that carries this hint is binary by construction. The comment at `:183-193` records why the hint exists: the pre-84b value was `"artifact.fetch"`, a tool in no catalog, so the model "could not recover the bytes (the Playground 'agent doesn't know what to do with my image' report)", and `TestMaterialize_FetchHint_NamesRegisteredBuiltin` is the lockstep gate. Meanwhile `internal/planner/react/prompt.go:447-455` promises `artifact_fetch` will "retrieve the full payload of a stored result" with no binary caveat and — separately — with no mention of `offset` at all, five months after phase 209 shipped it.

**A blanket refusal would make that instruction fail 100% of the time and reintroduce the incident.** So the refusal must not be a wall.

**The decision: the hint stays unchanged, the refusal is made informative, and the prompt language changes.** Three parts:

1. **`materialize.go` is not touched.** No suppression, no redirection to a different tool. The hint's job is *addressing* — "these bytes exist, here is the handle, here is the tool that answers about them" — not "these bytes are readable as text." Suppressing it returns to the pre-84b state the comment says was broken. Redirecting it would require a second tool that does not exist, which is a phase-214-shaped question about handing bytes to a consumer, not this phase's.
2. **An inadmissible read answers with facts, not only a refusal.** `Ref`, `MIME`, `SizeBytes` and `TotalSizeBytes` are populated; `Content` is empty; `Error` names the MIME, the failing byte offset, and the by-reference route. A model that follows the hint therefore learns *what it holds* and *what to do next* — which is strictly more than the hint delivers today, where it learns `��JFIF…`. The repo has already recorded that today's answer is worthless for exactly these MIMEs: D-190 (`docs/decisions.md:5054`) says over-threshold images "degraded to a text stub whose `artifact_fetch` returned raw bytes **the model still could not see**."
3. **`<heavy_results>` and the tool's own `WithDescription` gain the discriminator and the paging rule.** The `ArtifactStub` JSON shape (`internal/llm/llm.go:525-532`) already carries `mime` alongside `fetch`, so the model has the discriminator *before* it calls — the prompt just never told it what the field is for. No new stub field is added; `ArtifactStub.MarshalJSON` is an explicit "no extra fields" marshaller (`llm.go:542-550`) and widening it is a bigger contract change than this phase needs.

## Acceptance criteria

### Byte correctness

- [ ] `artifact_fetch` over an artifact whose window is not admissible as text returns a populated `Error` naming (a) the stored MIME and (b) the absolute byte offset in the artifact at which admissibility failed, with `Content` empty. The `U+FFFD` rewrite never reaches a caller.
- [ ] The refusal names the by-reference route explicitly — a tool declaring an artifact-reference parameter receives the bytes without routing them through the model's context — so the model's next decision has a destination and not only a wall.
- [ ] On a refusal, `Ref`, `MIME`, `SizeBytes` and `TotalSizeBytes` ARE populated and `Offset` / `ReturnedBytes` / `Truncated` are zero-valued. Pinned as an assertion, not left to fall out of a struct literal: a refusal that reported `truncated: true` would invite a model to page into the same wall forever.
- [ ] `artifact_fetch` over an admissible window returns `Content` byte-identical to the pre-phase build, asserted over a fixture set that includes ASCII, multi-byte runes, and a 4-byte rune (astral plane).
- [ ] A window whose TRAILING bytes are a truncated multi-byte rune is ADMITTED and trimmed to the last complete rune — the split is an artefact of windowing, not of the stored content.
- [ ] A window whose LEADING bytes are rune continuations (an `offset` landing mid-rune) is ADMITTED and trimmed from the front on the same rule.
- [ ] A window that is not valid UTF-8 *after* both trims is inadmissible. A single bad byte in the middle of otherwise-valid text refuses; it is not silently dropped.

### `Offset` truthfulness and paging termination

- [ ] **The reported `Offset` is where `Content` actually begins**, i.e. the requested offset advanced past any front-trimmed continuation bytes — not the requested offset echoed back. `ArtifactFetchOut.Offset`'s own godoc (`artifact_fetch.go:172-173`) already promises this ("the byte index the returned window starts at"); today `:280` echoes `args.Offset` (`:270`), which front-trimming would make false.
- [ ] **Reassembly invariant:** for every admissible response, `Content == string(blob[Offset : Offset+ReturnedBytes])` exactly. Property-asserted over the whole (offset × max_bytes) cross-product of a multi-byte fixture, not over a hand-picked table.
- [ ] **Strict-progress invariant:** for every admissible response with `Truncated == true`, `Offset + ReturnedBytes` is **strictly greater than the caller's requested offset**. This is the property that kills the livelock; it is stated as an invariant rather than as a patch so a later refactor cannot reintroduce the shape by a different route.
- [ ] `fetchBounds.effectiveMax` (`artifact_fetch.go:86-94`) floors its result at `utf8.UTFMax` (4). It currently floors only at `<= 0`, so `max_bytes: 1` against a multi-byte rune tail-trims to empty while `Truncated` stays true and the documented paging rule (`offset + returned_bytes`, `artifact_fetch.go:116-119`) yields the same offset forever. A boundary-aligned 4-byte window always contains at least one complete rune, which is what makes the strict-progress invariant provable rather than empirical.
- [ ] The floor applies to the OPERATOR-RESOLVED bound too, not only to a caller's `max_bytes`. A configured `fetch_default_max_bytes` of 1–3 is raised to 4 by the same path.
- [x] A paging loop that starts at 0 and follows the documented rule reassembles the artifact byte-for-byte and **terminates within `⌈total / max(1, max_bytes-3)⌉ + 1` iterations** for every `max_bytes` in `1..total+1`, asserted with a hard iteration cap that fails the test rather than hanging it. **AS-BUILT DEPARTURE (§4.3, D-357).** The plan as authored said `⌈total/4⌉ + 1`; that bound is arithmetically unreachable, because the floor guarantees a four-byte WINDOW, not four bytes of PROGRESS — the tail trim removes up to `utf8.UTFMax-1`, so an artifact alternating a 1-byte rune with a 4-byte one advances one byte on every other call. Measured against the real build: 100 such bytes at `max_bytes=4` take 40 iterations where `⌈total/4⌉ + 1` allows 26. The shipped cap is the provable one and `TestArtifactFetch_PagingBound_IsTheProvableOne` carries the adversarial fixture so the old constant cannot be quietly restored. The PROPERTY this criterion exists for — termination plus byte-exact reassembly, for every legal `max_bytes` — is unchanged and asserted.

### Classification

- [ ] An observation produced from a failed artifact-reference resolution carries a machine-readable class distinguishing it from a tool's own error. A planner distinguishes them by reading a field; string-matching an error message is not the mechanism.
- [ ] Two classes ship and both have a producer and a consumer in this phase: an unresolvable id (`dispatch.ErrArtifactRefNotFound`, model-recoverable) and an unavailable resolver (`dispatch.ErrArtifactStoreUnavailable` / `artifactref.ErrNoResolver`, operator misconfiguration and explicitly NOT model-recoverable — the class exists so a planner does not burn its step budget retrying).
- [ ] **No new sentinel is declared.** The classification is computed with `errors.Is` over the three sentinels that already exist. Their message strings do not change, so no shipped transcript or log grep is invalidated.
- [ ] The class survives the full wrap chain, which is `%w` at every hop and is asserted end-to-end rather than assumed: `withArtifactResolver` (`dispatch.go:366`) → `bindRef` (`artifactref.go:371`) → `Substitute` → `inproc.go:228` (double `%w`) → `callTool` (`dispatch.go:393`) → the runloop's `execErr`.
- [ ] The single-call path and the parallel path agree. `callTool`'s error becomes a classified step observation via the runloop; `branchObservations` (`dispatch.go:446-463`) stamps the same class onto that branch's `ParallelBranchObservation`. The `Batch` decision's tool half inherits it, because it already shares `branchObservations`.
- [ ] The class lands on BOTH `Step.Observation` and `Step.LLMObservation`. The runloop assigns one payload to both (`runloop.go:955-957`); the test asserts both slots independently rather than assuming they alias.
- [ ] The class is JSON-tagged and survives trajectory persistence and replay, and `renderAny` (`prompt.go:1848-1869`, a `json.Marshal` fallthrough) emits it into the rendered prompt text. Asserted against the rendered string, not against the struct.
- [ ] An unclassified error — a tool's own failure — is UNCHANGED: no class key, byte-identical observation to the pre-phase build. Pinned, because a widened payload on every tool error would be an unannounced prompt change on the hottest path in the runtime.

### The reconciliation

- [ ] `internal/llm/materialize.go` is unchanged and `TestMaterialize_FetchHint_NamesRegisteredBuiltin` still passes. The fetch hint is neither suppressed nor redirected for binary attachments.
- [ ] `<heavy_results>` (`prompt.go:447-455`) states: the `offset` parameter and the concrete two-call paging shape; that a non-text artifact refuses with a message naming its MIME; and that the stub's own `mime` field is the discriminator to read before calling. The stale "retrieve the full payload" phrasing, which has been silent about `offset` since phase 209 shipped it, is corrected in the same edit.
- [ ] `registerArtifactFetch`'s `WithDescription` (`artifact_fetch.go:107-125`) — the `<available_tools>` surface — carries the same three facts, so the two model-facing descriptions of one tool do not disagree.

### Twin divergence

- [ ] `internal/protocol/artifacts.go`'s `boundedWindow` is behaviourally UNCHANGED, and a test pins that `artifacts.get` over a binary artifact still returns exact bytes at every offset.
- [ ] `artifactWindow`'s godoc (`artifact_fetch.go:287-299`) — which today argues the two helpers must not disagree — is rewritten to name precisely **which** invariant is shared (the copy-not-alias one, which is the only one the existing paragraph actually argues for) and **which** is deliberately not (rune trimming and the 4-byte floor), with the reason. `boundedWindow`'s godoc gains the mirror sentence. A future reader who finds the divergence must find the argument for it in the same place.

### Hygiene

- [x] **AS-BUILT ADDITION (§17.6):** a THIRD stale `eof` reference was found and fixed in the same pass — the *ranged read* entry (`docs/glossary.md`, the "Ranged read (artifacts)" term) names the same nonexistent field in its conformance-contract sentence. Fixing all three also lets the smoke guard be a whole-file absence check on `` `eof` `` rather than a line-scoped one that would rot the first time an entry moves.
- [ ] `docs/glossary.md:108` and `:110` stop naming an `eof` field on the artifact read response. It does not exist: D-353 (phase 209's recorded departure) ships `truncated` and explicitly refuses `eof` as "two signals for one fact", and `ArtifactFetchOut` (`artifact_fetch.go:166-191`) and `ArtifactsGetResponse` (`types/artifacts.go`) both carry `truncated` only.
- [ ] `docs/glossary.md:126`'s `artifact_fetch` entry, which still describes the signature as `{ref, mime, size_bytes, content, truncated}`, is reconciled with what ships (`offset`, `returned_bytes`, `total_size_bytes`) plus this phase's admissibility rule.
- [ ] `docs/plans/README.md:355`'s Phase 212 detail block is rewritten to match this plan and its `Status` flips to `Shipped` in the same PR (§4.2 item 11). The shipped block currently asserts the retracted "terminates the dispatch step" premise verbatim; leaving it is exactly the stale-master-plan drift signal §4.2 names.
- [ ] Mutation-verified: reverting each guard turns a smoke `OK` into a `FAIL`, never into a `SKIP`.

## Files added or changed

**AS-BUILT deviations from this list are marked inline.** Each is a §4.3 deviation recorded in D-357.

```text
internal/tools/builtin/
├── artifact_fetch.go        # the admissibility gate; rune trimming at both ends;
│                            #   the truthful Offset; the utf8.UTFMax floor in
│                            #   effectiveMax; the refusal shape; the rewritten
│                            #   artifactWindow godoc; the WithDescription edit
├── artifact_fetch_readpath_test.go  # AS-BUILT: a SIBLING file rather than an
│                            #   append to artifact_fetch_test.go. Same package,
│                            #   same helpers; a separate file keeps this phase's
│                            #   additions off a file a sibling in-flight phase
│                            #   also edits. Carries the fixture matrix, the two
│                            #   invariants as property tests, the terminating
│                            #   paging loop and the N=128 concurrent-reuse run.
└── artifact_fetch_test.go   # one pre-existing offset-window case updated:
                             #   max_bytes 3 is now served at the rune floor

internal/planner/
├── observation_class.go     # NEW — the closed class vocabulary + the
│                            #   ClassifiedError interface. Placed here by the
│                            #   argument parallel_observation.go:11-17 already
│                            #   makes for its own type: BOTH sides of the
│                            #   round-trip consume it and both packages
│                            #   already import internal/planner, so no new
│                            #   cross-package edge is created. dispatch cannot
│                            #   own it — steering cannot import dispatch,
│                            #   because dispatch imports steering
│                            #   (dispatch.go:326, steering.ErrDecisionShapeUnsupported).
├── observation_class_test.go # NEW
└── parallel_observation.go  # ParallelBranchObservation gains ErrorClass

internal/runtime/dispatch/
├── dispatch.go              # classify at the two error sites: callTool's
│                            #   invoke-error wrap (:393) and branchObservations'
│                            #   branch-error entry (:454-463)
└── observation_class_test.go # AS-BUILT: a NEW file rather than additions to
                             #   dispatch_test.go, which the risks section names
                             #   as this phase's one file overlap with 213. A
                             #   separate file removes the overlap entirely.
                             #   Carries the classification on both paths and the
                             #   unclassified-error no-change pin.

internal/runtime/steering/
├── runloop.go               # the errPayload at :955 gains the class key when
│                            #   errors.As matches; unchanged otherwise
└── runloop_classification_test.go  # NEW, external test package (steering_test)
                             #   so it may import dispatch without a cycle

internal/planner/react/
├── prompt.go                # <heavy_results> — offset + paging shape + the
│                            #   binary caveat + the mime discriminator
└── prompt_test.go           # the rendered-block assertions

internal/protocol/
├── artifacts.go             # boundedWindow godoc ONLY — the mirror sentence
│                            #   naming the deliberate divergence. No behaviour.
└── artifacts_get_binary_test.go  # AS-BUILT: a NEW sibling rather than additions
                             #   to artifacts_get_test.go. The binary-bytes-exact
                             #   pin on the twin, at every offset and bound, plus
                             #   a one-byte-bound paging reassembly proving the
                             #   byte read acquired no rune floor.

test/integration/artifact_readpath_test.go   # NEW
scripts/smoke/phase-212.sh                   # replaces the skeleton
docs/decisions.md                            # D-357
docs/glossary.md                             # :108 + :110 eof; :126 the
                                             #   artifact_fetch entry; the new
                                             #   "admissible window" term
docs/plans/README.md                         # the Phase 212 detail block + Status
```

Files deliberately NOT touched, each for a stated reason: `internal/llm/materialize.go` (the reconciliation), `internal/config/config.go` + `docs/CONFIG.md` (phase 213 owns them and this phase's floor is a tool property, not a config-key semantics change), `internal/protocol/types/artifacts.go` (already correct), `docs/skills/**` (§18 check performed: `grep -rn artifact docs/skills/` returns only `scaffold-a-harbor-agent/SKILL.md:101`, which names `artifact_fetch` as an opt-in built-in and describes neither its response shape nor its read semantics — no skill documents a surface this phase changes).

## Public API surface

```go
// internal/planner

// ObservationClass is the machine-readable kind of a failed step's
// observation. Absent means "the tool's own error" — the shape every
// tool failure has always had, and the one this vocabulary does not
// disturb.
type ObservationClass string

const (
    // ObservationClassArtifactRefNotFound: the model named an artifact id
    // that does not resolve under the run's isolation triple. Recoverable
    // BY THE MODEL on its next turn — the <session_artifacts> manifest
    // already lists what is reachable.
    ObservationClassArtifactRefNotFound ObservationClass = "artifact_ref_not_found"

    // ObservationClassArtifactResolverUnavailable: no artifact store was
    // wired, or no resolver was seated. Operator misconfiguration. NOT
    // recoverable by the model, and named so a planner does not spend its
    // step budget discovering that.
    ObservationClassArtifactResolverUnavailable ObservationClass = "artifact_resolver_unavailable"
)

// ClassifiedError is an error that names the observation class it should
// be rendered under. The runtime's dispatch layer attaches it; the run
// loop reads it via errors.As. Deliberately NOT a new sentinel: the
// implementations classify with errors.Is over the sentinels that
// already exist.
type ClassifiedError interface {
    error
    ObservationClass() ObservationClass
}

// ObservationClassKey is the map key the run loop writes the class under
// on a step's error observation, alongside the existing "error" key.
const ObservationClassKey = "error_class"

type ParallelBranchObservation struct {
    // ... existing fields ...

    // ErrorClass names the kind of failure Error describes. Empty for a
    // tool's own error.
    ErrorClass ObservationClass `json:"error_class,omitempty"`
}
```

No change to `artifactref.Ref`, `Substitute`, `Resolver`, `WithResolver`, `ArtifactFetchArgs`, or any exported sentinel. `ArtifactFetchOut` gains no field — the refusal reuses the shipped `Error` soft-error channel (`artifact_fetch.go:189-190`).

## Test plan

- **Unit (`internal/tools/builtin/artifact_fetch_test.go`):** the admissibility fixture matrix — pure ASCII; 2-, 3- and 4-byte runes; a rune split at the window head; a rune split at the window tail; a rune split at both; a PNG magic-number prefix; a zip local-file header; a PDF header; one bad byte inside otherwise-valid text; an empty artifact; an offset past the end. The two invariants as **property tests over the full (offset × max_bytes) cross-product** of a mixed-width fixture, not a hand-picked table — the reassembly invariant (`Content == blob[Offset:Offset+ReturnedBytes]`) and the strict-progress invariant. The terminating paging loop with a hard iteration cap. `effectiveMax`'s floor table, including the operator-resolved-bound leg and the interaction with the existing clamp-down-to-ceiling behaviour. The refusal's populated/zero-valued field split. A golden test that an admissible response is byte-identical to the pre-phase build for the fixtures that were already admissible.
- **Unit (`internal/planner/observation_class_test.go`):** the classifier over each sentinel, over a `nil` error, over an unrelated error, and over a chain three wraps deep; and the JSON round-trip of `ParallelBranchObservation` with and without the class.
- **Unit (`internal/runtime/dispatch/dispatch_test.go`):** the classification on both paths against a real `inproc`-registered tool declaring an `artifactref.Ref` parameter and a real in-memory store — the wrap chain is the thing under test, so a fake resolver returning the sentinel directly would prove nothing. Plus the no-change pin: a tool whose own body errors produces a byte-identical observation to the pre-phase build.
- **Unit (`internal/runtime/steering/runloop_classification_test.go`, package `steering_test`):** the error observation carries the class key on both `Step.Observation` and `Step.LLMObservation`, and does not carry it for an ordinary tool error. External test package so it can import `internal/runtime/dispatch` — `dispatch` imports `steering`, so an in-package test importing `dispatch` would be a cycle.
- **Unit (`internal/planner/react/prompt_test.go`):** `<heavy_results>` contains the paging rule, `offset`, the binary caveat and the `mime` discriminator; and the rendered step observation for a classified failure contains the class string (the `renderAny` leg).
- **Integration (`test/integration/artifact_readpath_test.go`):** real `inmem` AND `sqlite` artifact stores behind a real dispatch executor and a real run loop. (a) Store a PDF fixture, `artifact_fetch` it, assert refusal-not-corruption and that the refusal names the MIME. (b) Store UTF-8 with multi-byte runes, page it to completion through the documented rule, assert byte-exact reassembly. (c) Dispatch a tool with an unresolvable `Ref`, assert the run reaches the NEXT planner turn carrying the classified observation. (d) The same via `CallParallel`, asserting the branch entry's `ErrorClass`. **Identity propagation** asserted on every leg; a cross-tenant read still answers the shipped indistinguishable not-found. **Failure modes (≥1 required, three shipped):** a store that errors mid-read; a stack with no artifact store wired (the resolver-unavailable class); a tool whose own body errors (the unclassified control).
- **Conformance:** N/A — no interface gains a method, so the five-driver `ArtifactStore` conformance suite is untouched.
- **Concurrency / leak (D-025):** `artifact_fetch` and the dispatch executor are both compiled artifacts. N=128 concurrent invocations against ONE registered tool instance and ONE shared store, mixing binary and text artifacts across two tenants and interleaving admissible reads with refusals and with unresolvable-ref dispatches, under `-race`: no data race, no cross-tenant content bleed, no *class* bleed (a goroutine's refusal must not be attributed to a sibling's success — the bound-bleed lesson phase 209 recorded at its own N=128 run), no cancellation cross-talk, and `runtime.NumGoroutine()` back to baseline after teardown.

## Smoke script additions

`scripts/smoke/phase-212.sh`, classified `unit-tests` (phase 210's precedent): the surfaces this phase changes are a builtin tool and a runtime seam, and there is no `tools.invoke` Protocol method — `internal/protocol/methods/methods.go` has none — so there is nothing to curl. What the gate is for here is that the guards exist in source and that the behavioural suites pass, so deleting one fails preflight rather than only a package test a reviewer might not run.

1. `assert_grep_present` on `unicode/utf8` in `internal/tools/builtin/artifact_fetch.go` — the admissibility gate's import.
2. `assert_grep_present` on `utf8.UTFMax` in the same file — the floor that kills the livelock.
3. `assert_grep_absent` on `Content:        string(window)` — the unguarded conversion is gone.
4. `assert_grep_present` on the class constants in `internal/planner/observation_class.go` and on `errors.Is(` against `ErrArtifactRefNotFound` in `internal/runtime/dispatch/dispatch.go` — the sentinel finally has a consumer.
5. `assert_grep_present` on `ObservationClassKey` in `internal/runtime/steering/runloop.go`.
6. `assert_grep_absent` on `eof` in the two reconciled `docs/glossary.md` entries.
7. `assert_grep_present` on `offset` inside the `<heavy_results>` block in `internal/planner/react/prompt.go` — the §18-adjacent model-facing surface this phase repairs.
8. `go test -race` gates, each recorded as its own OK/FAIL with the output printed on failure: `TestArtifactFetch` on `./internal/tools/builtin/`; the classification suites on `./internal/planner/`, `./internal/runtime/dispatch/` and `./internal/runtime/steering/`; the twin's byte-exactness pin on `./internal/protocol/`; and `TestE2E_ArtifactReadPath` on `./test/integration/`.

Every static guard is verified to turn OK into FAIL when the thing it names is removed (the phase-210 discipline), never into a SKIP.

## Coverage target

Measured on the base tree with `go test -cover` before authoring; every target is **at or above** the current figure, because a target below current sanctions a regression.

- `internal/tools/builtin`: **92%** (measured 91.5%)
- `internal/planner`: **95%** (measured 94.8%)
- `internal/runtime/dispatch`: **86%** (measured 85.1%)
- `internal/runtime/steering`: **87%** (measured 86.5%)
- `internal/planner/react`: **87%** (measured 86.7%)
- `internal/tools/artifactref`: **92.3%** (measured 92.3%; no production change — the sentinels stay in `dispatch`, so this is a no-regression floor)
- `internal/protocol`: **77.9%** (measured 77.9%; godoc + one test added, no production change)

## Dependencies

- **210** — the in-process pass-by-reference arm. It shipped `artifactref.Ref`, the run-scoped resolver at `dispatch.go:303`/`:344-370`, and the two sentinels this phase gives their first consumer. It is also the route the binary refusal points at, so without it the refusal would name nothing.
- **209** — `artifacts.get` and byte-offset windowing. It shipped `offset` / `returned_bytes` / `total_size_bytes` / `truncated` on `artifact_fetch`, the paging rule this phase makes terminating, and `boundedWindow`, the twin this phase documents the divergence from.

## Risks / open questions

- **A binary artifact currently "works" in the sense that it returns something.** Making it refuse is a behaviour change for any caller tolerating corruption. Taken deliberately: the returned content was never usable (D-190 says so about images in the repo's own words, `docs/decisions.md:5054`), the byte count it reported was wrong (an 8-byte PNG header JSON-encodes to 10 delivered bytes — measured, not reasoned), and §13 names silent degradation as the forbidden shape. The refusal carries the alternative route so a caller has a destination.
- **The UTF-8 gate could refuse valid text that happens to be windowed badly.** This is the main correctness risk, and it is why both-end trimming is an acceptance criterion rather than an implementation note: without it, paging a UTF-8 CSV at a fixed `max_bytes` would intermittently refuse. The full (offset × max_bytes) cross-product property test exists to pin exactly this, and is deliberately a property rather than a table because a table would have missed the livelock.
- **The 4-byte floor means a `max_bytes` of 1–3 is served at 4.** This is *more* than asked, where every other bound in the read path clamps *down*. It is not silent — `returned_bytes` reports the truth and the tool description states the floor — and the alternative postures are worse: returning empty with `truncated: false` is a lie (bytes do remain), and refusing a legal request is a wall. The degenerate case is not one a paging model reaches; it is reachable only by a model or an operator naming a sub-rune bound directly.
- **The twin divergence will look like a bug to a future reader.** `artifactWindow` and `boundedWindow` are byte-identical today and `artifactWindow`'s godoc argues against them disagreeing. Mitigated by requiring the argument to live in both godocs, but the residual risk is real: a later contributor may "restore consistency" by propagating rune trimming into the Protocol path, which would short-read an operator's PDF. Stated here rather than assumed away; the `artifacts.get` binary-bytes-exact test is the trip-wire.
- **A classified observation gives a model a retry loop it can spin in.** Bounded by the existing per-run step cap (`ErrMaxStepsExceeded`, `runloop.go:1015`) and by the governance layer's cost ceilings. This phase adds no new bound and claims none (D-351's bar). The resolver-unavailable class exists partly to shorten that loop by telling the planner the failure is not its to repair.
- **Shared-file contention with the rest of the wave, named per the Gate-0 finding.** `internal/runtime/dispatch/dispatch_test.go` is also on phase 213's list. This phase's additions are new top-level test functions in that file, so the conflict is append-only and hand-resolvable. `internal/config/config.go`, `docs/CONFIG.md` and `examples/*.yaml` are deliberately NOT touched here to keep the 213 overlap at one file. No generated artifact (`wire-manifest.gen.json`, `docs/site/protocol/*.md`) is touched, so this phase is outside the wave's generated-file ownership question entirely.
- **No open RFC question gates this phase.**

## Glossary additions

- **Admissible window** — the byte range `artifact_fetch` will return through its string-typed `content` field: the requested window trimmed to whole UTF-8 runes at both ends, with the reported `offset` advanced to where the trimmed content actually begins. A window that is not valid UTF-8 after trimming is *inadmissible* and is refused with its MIME and failing offset rather than delivered with `U+FFFD` substituted for the bytes that did not survive JSON encoding. Deliberately a property of the model-facing string channel only: `artifacts.get` returns `[]byte` and is byte-exact for every MIME, so the two window helpers share the copy-not-alias invariant and deliberately not the rune discipline. RFC §6.10, D-357.

Two existing entries are reconciled rather than added: **artifact fetch ceiling** (`:108`) and **byte-offset artifact window** (`:110`) stop naming an `eof` field that D-353 refused and no response carries; the **`artifact_fetch`** entry (`:126`) picks up `offset` / `returned_bytes` / `total_size_bytes` and the admissibility rule.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (and ≥ the measured pre-phase figure, which is what the targets are set from)
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **Concurrent-reuse test passes** — N=128 concurrent invocations against a single shared `artifact_fetch` registration and a single shared store under `-race`, asserting no data races, no content or class bleed across tenants, no cancellation cross-talk, no goroutine leak
- [ ] **Integration test exists** — `test/integration/artifact_readpath_test.go`, two real artifact drivers on the seam, identity propagation asserted, three failure modes covered, under `-race`
- [ ] If new vocabulary: glossary updated (and the two stale `eof` entries reconciled)
- [ ] `docs/plans/README.md`'s Phase 212 detail block rewritten and its Status flipped (§4.2 item 11)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (D-357)
