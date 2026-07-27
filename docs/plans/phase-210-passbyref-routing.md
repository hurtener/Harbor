# Phase 210 — in-process pass-by-reference routing + the substitution invariant

## Summary

Ships the in-process arm of pass-by-reference tool routing: a tool declares an artifact-reference parameter in its typed input struct, the model supplies an artifact id, and the runtime resolves the ref at dispatch and hands the consumer the bytes. Bytes flow store → consumer; the model authors an id and never sees content. The phase's load-bearing half is the **substitution invariant** — a resolved artifact value never re-enters the model's context or the observable record — held by a production bound (one substitution call site, held there by a mechanical AST scan), a projection bound on the carrier type, and one widened arrival check at the LLM edge (`findContextLeak` gains `Messages[].ToolCalls[].Args`). Also corrects the stored-bytes contract: the audit redactor is an admission gate on `artifacts.put`, not a transform.

## RFC anchor

- RFC §6.4 — tool catalog and transports (the dispatch boundary the substitution ends at; the reflection-derived in-proc schema that makes a reference parameter a declared field type).
- RFC §6.5 — LLM client layer (the context-window safety net whose `findContextLeak` this widens by one field).
- RFC §6.10 — Artifacts (the store the reference resolves through, on the isolation-triple read key).

## Briefs informing this phase

- brief 03
- brief 05
- brief 07
- brief 15

## Brief findings incorporated

- **brief 03 §"Tool interface" (l.147):** "The registration helper uses generics + reflection to derive `ArgsSchema` and `OutSchema` from the `WeatherArgs` / `WeatherOut` types. No decorator equivalent is needed: type inference replaces it." This is exactly why a reference parameter is a **declared field type** (`artifactref.Ref`) rather than a hand-written schema convention a registrar would have to honour — the deriver already owns the model-facing rendering, so the deriver is where the reference becomes a plain string.
- **brief 03 §"Planners" (l.159):** "Planners receive a filtered `[]Tool` view at each step, never a `ToolDescriptor`. This guarantees the planner can be serialized (only schemas + names) and replayed." The planner sees schemas; a reference parameter must therefore be fully expressed IN the schema (a string the model authors), with the resolution invisible to the planner — which is what the ctx-seated resolver achieves and an argument rewrite would not.
- **brief 03 §"Result normalization" (l.178):** "Harbor enforces the layered approach: artifacts are mandatory, not opt-in." The output leg is mandatory routing; this phase gives the INPUT leg the same posture — a heavy input is passed by reference, not inlined, and the runtime fails loud rather than degrading when the reference does not resolve.
- **brief 05 §"Runtime → ArtifactStore" (l.248):** "the runtime injects a `ScopedArtifacts` facade per task that auto-stamps the identity triple on writes and scope-checks on reads. Tools call `Upload` / `Download` against the facade — they never see raw scopes." The reference parameter is the stronger form of the same finding: the tool sees neither a scope NOR a store handle, only bytes the runtime already resolved under the run's triple. No identity logic lives in any tool driver.
- **brief 05 §"ArtifactStore" (l.12):** "Returns compact `ArtifactRef`s that are safe to embed in LLM context." The reference id is the safe-to-embed half; the invariant is the statement that the UNSAFE half never travels back the other way.
- **brief 07 §"`llm_observation` vs `observation`" (l.104):** "Harbor maintains two views — the canonical `observation` for trajectory persistence and audit, and an LLM-facing `llm_observation`." Both views are surfaces the invariant names, and the integration test asserts the resolved value's absence from each independently rather than assuming the projection covers both.
- **brief 15 (native tool calling + deferred loading):** the cutover that made `ChatMessage.ToolCalls[].Args` a live replay field rather than a vestigial one — which is precisely why the arrival-side check has to walk it.

## Findings I'm departing from (if any)

None.

## Deviations from this plan, as shipped

- **The invariant landed with a THIRD mechanism the plan did not name: a projection bound on the CARRIER.** `artifactref.Ref` keeps the resolved bytes in an unexported field and projects itself as its id through `MarshalJSON`, `String` and `LogValue`, so a `Ref` reaching `json.Marshal`, `fmt` or `slog` emits an id BY CONSTRUCTION. It sits between the production bound and the LLM-edge check in durability, and it is what makes it safe for `dispatch` to hand `desc.Invoke` the model's own argument JSON unrewritten and bind on the DECODED value inside the policy shell instead — the alternative (resolve in dispatch, rewrite the args) would put the resolved value into the exact artefact the trajectory persists, the prompt builder replays and the lifecycle events summarise.
- **The carrier is re-exported through `sdk/tools`, which the plan did not require.** Without it the primitive would be unusable by the tool authors it exists for: the `add-an-in-process-tool` skill's registration examples compile from an external module against the `sdk/` facade. The forward is a VAR (`var NewArtifactRef = artifactref.NewRef`), not a func body, because the facade's no-behaviour gate permits func bodies only in its enumerated generic-forward list — a constraint the first draft tripped and preflight caught. The RESOLUTION side (`Substitute`, `WithResolver`, `Resolver`) is deliberately NOT re-exported: seating a resolver is the runtime's act at the dispatch boundary, and a tool that could do it would be reaching past the identity scope its run was given.
- **The resolver is seated ONCE at `ExecuteDecision` rather than per tool-invoking shape.** A later decision shape then inherits it instead of forgetting it, and the reach cannot widen sideways because every descendant path re-derives identity from the same triple.

## Goals

- A tool author can declare an artifact-reference parameter as a Go field type and read the resolved bytes, with no schema hand-authoring and no identity logic in the tool.
- The reference resolves under the DISPATCHING run's isolation triple, so a tool reaches exactly the artifacts its own run reaches.
- The resolved value is dispatch-local: absent from the trajectory, the interleaved observation, canonical event payloads, audit payloads and logs.
- The invariant is enforced mechanically on the production side (one call site) AND checked on the arrival side (the LLM edge), rather than asserted.
- Every failure path is loud: no resolver, an unresolvable ref, a store-less stack, and reading an unresolved reference all fail by name.
- The stored-bytes contract stops describing a transform the code does not perform.

## Non-goals

- **No third-party egress arm.** No loan or grant URLs, no grant tokens, no externally-reachable base-URL config, no change to `ServerConfig`. Deferred on three named blockers (D-354 part 5), not stubbed.
- **No change to the heavy-content threshold**, to the conversation-text exemption, to `ErrContextLeak`'s type, or to the `llm.context_leak` event. The edge check gains one field and nothing else.
- **No Protocol surface.** No method, wire type, error code, canonical event or capability; `ProtocolVersion` does not move; no TypeScript or generated-docs regeneration.
- **No ranged read on `ArtifactStore`** and no byte-offset windowing — separate phases.
- **No behaviour change to `artifacts.put`.** The stored-bytes part corrects documentation that describes a rewrite nobody performs.
- **No new builtin tool.** The consumer is a worked example plus the integration suite; adding a model-facing builtin would be a tool-surface decision this phase has no mandate for.

## Acceptance criteria

- [x] `artifactref.Ref` is a declared field type; `schema.Derive` renders it as `{"type":"string", "description": ...}`, and the derived schema validates the string form the model writes and rejects an object-shaped one.
- [x] `inproc.RegisterFunc` computes the reference flag ONCE at registration (immutable on the descriptor); a tool with no reference parameter invokes with nothing seated.
- [x] `dispatch.ExecuteDecision` seats a resolver closed over the run's `(tenant, user, session)`, resolving through `ArtifactStore.Get` on the reconciled read key.
- [x] A tool declaring a reference receives the stored bytes via `Ref.Bytes()`; the argument JSON handed to `Invoke` is NOT rewritten.
- [x] `Ref` projects its id — and never its content — through `MarshalJSON`, `String` and `LogValue`.
- [x] Four loud failures: no resolver seated, an empty reference id, an unresolvable reference (cross-tenant / cross-user / cross-session / nonexistent, all indistinguishable), and a stack with no artifact store wired.
- [x] `artifactref.Substitute` has exactly ONE production call site, and `ScanSubstitutionSites` fails when a second is added AND when the registered one is removed (stale registration).
- [x] `findContextLeak` walks `Messages[].ToolCalls[].Args` at the same `>=` threshold; an oversize args document fails with `ErrContextLeak` and emits `llm.context_leak` naming the exact site; an ordinary tool call still completes.
- [x] The resolved value appears in NONE of: the raw observation, the LLM observation, the serialised trajectory, any event payload or envelope published during the dispatch, any log record — with each arm guarded against vacuity.
- [x] `handlePut`'s godoc and the matching glossary entry describe an admission gate, not a transform.
- [x] `sdk/tools` re-exports the carrier (`ArtifactRef`, `NewArtifactRef`, `ErrArtifactRefUnresolved`) and NOT the resolution side.

## Files added or changed

- `internal/tools/artifactref/artifactref.go` — new: the `Ref` carrier, the `Resolver` seam + ctx accessors, `Substitute`, `TypeContainsRef`.
- `internal/tools/artifactref/scan.go` — new: `ScanSubstitutionSites` + `Violation` (the shipped minting-scan shape).
- `internal/tools/artifactref/{artifactref,scan,edges}_test.go` — new.
- `internal/tools/schema/schema.go` — derive `artifactref.Ref` as a string with a model-facing description.
- `internal/tools/schema/artifactref_test.go` — new.
- `internal/tools/drivers/inproc/inproc.go` — registration-time reference flag; the one `Substitute` call, after decode, inside the policy shell.
- `internal/tools/drivers/inproc/artifactref_test.go` — new (includes the N=128 concurrent-reuse run).
- `internal/runtime/dispatch/dispatch.go` — `withArtifactResolver`, seated once in `ExecuteDecision`; `ErrArtifactRefNotFound`, `ErrArtifactStoreUnavailable`.
- `internal/llm/safety.go` — `findContextLeak` walks `Messages[].ToolCalls[].Args`.
- `internal/llm/safety_toolargs_test.go`, `internal/llm/safety_toolargs_client_test.go` — new.
- `internal/protocol/artifacts.go` — stored-bytes contract godoc (three places).
- `sdk/tools/tools.go` — `ArtifactRef`, `NewArtifactRef`, `ErrArtifactRefUnresolved`.
- `examples/tools/artifactstats/artifactstats.go` + `_test.go` — new: the worked in-process consumer.
- `test/integration/artifact_passbyref_test.go` — new.
- `scripts/smoke/phase-210.sh` — real assertions.
- `docs/decisions.md` (D-354), `docs/glossary.md`, `docs/plans/README.md`, `docs/skills/add-an-in-process-tool/SKILL.md`, `docs/recipes/define-a-tool.md`.

## Public API surface

```go
// internal/tools/artifactref (public via sdk/tools for the carrier only)
type Ref struct{ /* unexported fields */ }
func NewRef(id string) Ref
func (r Ref) ID() string
func (r Ref) Supplied() bool
func (r Ref) Resolved() bool
func (r Ref) Bytes() ([]byte, error)          // ErrUnresolved when never resolved
func (r Ref) Size() int
func (r Ref) String() string                  // the id
func (r Ref) LogValue() slog.Value            // the id
func (r Ref) MarshalJSON() ([]byte, error)    // the id
func (r *Ref) UnmarshalJSON(b []byte) error

type Resolver interface {
    ResolveArtifact(ctx context.Context, id string) ([]byte, error)
}
type ResolverFunc func(ctx context.Context, id string) ([]byte, error)
func WithResolver(ctx context.Context, r Resolver) context.Context
func ResolverFrom(ctx context.Context) (Resolver, bool)

func Substitute(ctx context.Context, ptr any) error   // THE one call site
func TypeContainsRef(t reflect.Type) bool

func ScanSubstitutionSites(root string, allow map[string]string) ([]Violation, int, error)

// sdk/tools — the carrier only; the resolution side is deliberately
// absent, and every entry is an ALIAS or a VAR forward because the
// facade's no-behaviour gate allows no func bodies outside its
// enumerated generic-forward list.
type ArtifactRef = artifactref.Ref
var NewArtifactRef = artifactref.NewRef
var ErrArtifactRefUnresolved = artifactref.ErrUnresolved

// internal/runtime/dispatch
var ErrArtifactRefNotFound error
var ErrArtifactStoreUnavailable error
```

## Test plan

- **Unit:**
  - `internal/tools/artifactref` — decode as a plain string; a non-string reference rejected; JSON null leaves it unsupplied; `Bytes` fails loudly when unresolved; `Substitute` resolves and binds; the carrier's `MarshalJSON` / `String` / `fmt` / `slog` projections all emit the id against a planted marker while the bound value is proven present; no-resolver / empty-id / resolver-error / non-pointer-target refusals; an omitted optional reference is not an error; the walk across nested structs, pointers, slices, arrays and map values (with the map write-back); a `Ref` inside an `any` is unreachable and `TypeContainsRef` agrees; idempotency under a retried attempt; the depth bound; `Violation.String`; the scan's blank/dot-import cases, deterministic ordering, and parse-error surfacing.
  - `internal/tools/schema` — string rendering + description; nesting (bare field, slice items); required vs `omitempty`; compile + validate round trip.
  - `internal/tools/drivers/inproc` — schema shape at the descriptor; end-to-end resolve; the argument JSON asserted UNCHANGED and the result asserted marker-free; no-resolver and unresolvable failures classified as invalid args; a reference-free tool needs no resolver.
  - `internal/llm` — the widened arm above / below / AT the threshold; the parallel branch named by index; the conversation-text exemption untouched; through the mandatory client with `LeakSite` and `SizeBytes` asserted; an ordinary tool call still completes.
  - `examples/tools/artifactstats` — the reference parameter renders as a string; measurements returned and content not; unresolved / empty / non-UTF-8 cases.
- **Integration:** `test/integration/artifact_passbyref_test.go` — real in-memory artifact store, real event bus over the real pattern redactor, real catalog with the lifecycle shell live, production `dispatch.ToolExecutor`, the shipped example consumer. Asserts the invariant across five surfaces with per-arm vacuity guards; identity propagation via cross-tenant / cross-user / cross-session refusals plus an owner-reads-it control; failure modes (unknown ref, store-less stack); N=48 two-tenant concurrency stress.
- **Conformance:** N/A — no new driver interface. The artifact read key's five-driver conformance landed with the read-key phase and is unchanged here.
- **Concurrency / leak:** N=128 against ONE shared `ToolDescriptor`, each goroutine seating its own resolver over content whose length is derived from its index (a bleed is a size mismatch, not a byte compare); a cancellation-isolation pair proving a cancelled invocation does not affect a sibling. The integration stress covers the cross-package boundary.

## Smoke script additions

`scripts/smoke/phase-210.sh` (classified `unit-tests`; the surface is a runtime seam, not an HTTP endpoint):

- The `internal/tools/artifactref` package and its scan file exist.
- `findContextLeak` walks `ToolCalls` — a source guard on the widened arm, so deleting it fails preflight, not only `go test`.
- The substitution scan test runs green against the real tree (the one-call-site bound).
- The carrier's three projections (`MarshalJSON`, `String`, `LogValue`) are present on `Ref`.
- The deriver's reference case is present in `internal/tools/schema`.
- `handlePut` no longer claims the redactor "may rewrite" the payload, and does claim an admission gate.
- The dispatch resolver seat is present and scoped to the triple (no `TaskID`).
- The LLM-edge widening tests, the artifactref package tests, the inproc reference tests and the integration suite each run and pass.

## Coverage target

- `internal/tools/artifactref`: 85% (achieved 92.0%).
- `internal/tools/schema`: 85% (achieved 90.0%).
- `internal/tools/drivers/inproc`: no regression (80.6%; the phase adds only covered statements).
- `internal/runtime/dispatch`: no regression (85.1%).
- `internal/llm`: no regression (79.1%).
- `examples/tools/artifactstats`: 85% (achieved 95.0%).

## Dependencies

- 208 — the reconciled artifact read key resolution uses.
- 26 — the tool catalog and the in-process driver.
- 32 — the LLM client the edge check lives on.

## Risks / open questions

- **The reflective bind costs a walk per invocation for tools that declare a reference.** Bounded by computing `TypeContainsRef` once at registration, so a reference-free tool pays nothing. A tool with a reference pays a walk over its own (small) argument struct — negligible against the store read it precedes.
- **The one-call-site bound is a scan, not a type-system guarantee.** A contributor can register a second site with a reason. That is the intent — the gate makes it a reviewed decision rather than an edit — but it is a review-quality dependency, and the allow-list's prose is what a reviewer actually judges.
- **The invariant covers the runtime's surfaces, not a tool's own choices.** A tool that puts its resolved content into its own RESULT has leaked it, and no runtime mechanism can prevent that — the same way a tool can already return anything it likes. The example consumer models the correct shape (measurements out, not content) and the operator skill states it.
- **A reference inside an `any`-typed field is unreachable.** Deliberate and consistent (a JSON decode cannot produce one there, and reflection cannot write through it), and `TypeContainsRef` agrees with the walk so there is no shape that is "declared but silently unbound". Pinned by a test.
- **The third-party arm's three blockers are design questions, not backlog.** Recorded in D-354 part 5 so the next author inherits them rather than re-deriving them.

## Glossary additions

- **Artifact-reference parameter** — extended with the shipped carrier (`artifactref.Ref`, public as `sdk/tools.ArtifactRef`) and the deliberate absence of the resolution side from the SDK facade.
- **Substitution invariant** — extended with the three shipped mechanisms in descending order of durability (production bound, carrier projection, arrival check) and the circularity argument for taking the third.
- **`artifacts.put` (Protocol method)** — corrected: the redactor is an admission gate whose rewrite is discarded; stored bytes are the author's.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. — N=128 against one shared `ToolDescriptor` in `internal/tools/drivers/inproc/artifactref_test.go`, plus the cancellation-isolation pair.
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. — `test/integration/artifact_passbyref_test.go`.
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry filed — N/A, no departures.
