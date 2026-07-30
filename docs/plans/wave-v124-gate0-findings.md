# Wave v1.24 — Gate-0 findings

Four adversarial reviews were run against the first draft of phases 212–217. Every claim
below was verified against the tree at `plan/v124-wave` (base: `dev-experimental`
@ `9918d718`). **The first-draft plans carry FAIL-level defects in all six phases and
must be rewritten, not patched.** This file records what was verified so the rewrite does
not re-derive it.

Two independent reviewers agreed on three findings (213's consumer undercount, 213's
phantom test files, 216's false dependency on 215). Agreement across independent
reviewers is noted where it occurred.

---

## Verified TRUE — facts the rewrite can rely on

### Artifacts / dispatch

- `internal/tools/builtin/artifact_fetch.go:170,:279` — `Content string` populated by
  `string(window)`. The binary-corruption defect is real.
- `internal/protocol/types/artifacts.go:351` — `ArtifactsGetResponse.Content` is
  `[]byte`. The correct twin exists.
- `internal/runtime/dispatch/dispatch.go:388-394` vs `:454-463` — `callTool` returns the
  invoke error up; `callParallel` treats a branch failure as that branch's result. The
  asymmetry is real.
- `internal/runtime/dispatch/dispatch.go:303,:353` — the run-scoped artifact resolver is
  seated once, on the dispatch ctx, closed over `(tenant, user, session)`.
- `internal/config/config.go:2044+` — the fetch ceilings do not derive from the heavy
  threshold. They are independent.

### Agent config / selection

- `internal/protocol/types/control.go:94` — `StartRequest` has no agent field.
- `internal/tasks/tasks.go:224` — `SpawnRequest` has none.
- `internal/runtime/serve/serve.go:416,:571,:637` — `devAgentConfigID` is the only
  production value reaching `opts.AgentConfigID`
  (`harbortest/devstack/devstack.go:640` is the test-kit twin).
- `internal/runtime/agentcfg/projection/projection.go:167,245,320,379,484,572,640,699,830,881,911`
  — every projection takes `agentID` as an ordinary parameter.
- `internal/tools/drivers/mcp/registry.go:550-552` — the owner tag is ENFORCED at the
  registry, not merely passed: a cross-owner `Deregister` answers not-found.
- `projection.go:938-962` + `composeUserLayer:966` — the four-tier composition ships in
  the documented order.
- `internal/runtime/agentcfg/protocol/user.go:37` — `userPayloadToDomain` can only build
  `PromptLayers{User:}`; `AgentConfigUserPayload` has no `Base`. The boundary is
  structural.
- `projection.go:171,:249,:324` — all three reconcile legs read `ConfigScopeAgent`.
- `internal/agentcfg/drivers/statestore/statestore.go:99-107` — `ConfigScopeUser` keys by
  `(tenant, real-user, agentID)`.

### MCP `_meta`

- `internal/tools/drivers/mcp/mcp.go:1388-1403` — flat merge, reserved check on the whole
  key.
- `mcp.go:1525-1543` — `injectMeta` nests, and takes a PRE-SPLIT `path []string`.
- `internal/config/validate.go:1780-1787` — only empty and whole-key-reserved are
  rejected today.
- `internal/config/validate.go:2527` — `IsReservedMCPMetaKey` is the shared predicate.

### Wave hygiene

- Dependency graph is acyclic; every cited predecessor phase exists.
- `D-357..D-362` are unique and unused (highest shipped: `D-356`, `decisions.md:10142`).
- Phase numbers 212–217 are unused in `docs/plans/` and `scripts/smoke/`.
- All nine drift-audit-required headings present on all six plans.
- §13 primitive-with-consumer: no violation in any phase.

---

## FALSE claims in the first draft — corrected

### Phase 212

**The "dead run" premise is false.** `internal/runtime/steering/runloop.go:947-957`
converts every `ExecuteDecision` error into `map[string]any{"error": …}` as the step's
observation and continues — the comment states *"The runloop does NOT abort the run on a
single tool error."* The id and reason are already carried
(`dispatch.go:366` → `ErrArtifactRefNotFound`). **The recovery the draft proposed
building is shipped.** The only real gap is a machine-distinguishable classification in
the observation shape.

**`ErrResolveFailed` would duplicate `dispatch.ErrArtifactRefNotFound`**
(`dispatch.go:64`), which is already the typed classification and already what the seated
resolver returns. Two sentinels for one fact is the §13 shape.

**Refusing binary collides with a shipped recovery path.**
`internal/llm/materialize.go:181-196` emits `StubFetch{Tool: "artifact_fetch", ID: …}`
for every auto-materialized over-threshold attachment — image/audio/file, binary by
construction. The comment records the incident that hint fixed (the Playground
"agent doesn't know what to do with my image" report) and names the lockstep test. A
blanket refusal reintroduces it. `internal/planner/react/prompt.go:447-455`
(`<heavy_results>`) promises "retrieve the full payload" with no binary caveat.

**Front-trimming without adjusting `Offset` is itself a silent-degradation bug.**
`artifact_fetch.go:116-119,:172-173` document `Offset` as the returned window's start and
tell the model to page with `offset + returned_bytes`. Trimming k head bytes while
echoing the requested offset drops k bytes with no signal and makes the next computed
offset short.

**Trimming introduces a paging livelock.** `effectiveMax` floors only at `<= 0`
(`:86-94`), so `max_bytes: 1` against a multi-byte rune trims to empty while
`truncated` stays true — the paging rule then yields the same offset forever.

**Coverage targets were set BELOW measured current** (`internal/tools/builtin` 91.5% vs
target 85%; `internal/tools/artifactref` 92.3% vs 90%), which sanctions a regression.

### Phase 213

**"Four consumers" is false — there are seven direct ones** (agreed by two independent
reviewers). Missing from the draft: `internal/tui/renderers/registry.go:138,:152`,
`internal/mcpconsole/apps.go:173`, `internal/mcpconsole/toolcontext.go:94`. The operator
field reaches further still (memory get/list, pause-list, flow catalog, tools annotator)
— **Protocol-visible surfaces where a 32→128 KiB move silently flips responses from
artifact-ref to inline in that band.** ROOT CAUSE: `internal/config/config.go:2035-2038`'s
own single-sourcing comment enumerates only three consumers and is STALE; the draft
copied the comment instead of grepping.

**The "designed pair" defence holds for only one of three leak classes.**
`internal/llm/materialize.go:48-100` walks only `PartImage`/`PartAudio`/`PartFile`
DataURLs. `findContextLeak` additionally guards `RoleTool` text and
`Messages[].ToolCalls[].Args` — neither is offloaded by materialization, so for those two
classes raising the threshold is a straight 4× weakening with no counterpart at the LLM
edge.

**The search-split rationale is arithmetically impossible.** `search.go:77`
`PreviewMaxRunes = 256` caps every preview AFTER the heavy check, so ten previews are
~10 KB regardless of the threshold. And `search.go:423-425` returns `("", true, nil)` —
an EMPTY preview plus a ref — so the threshold does ref-vs-inline selection, not
truncation. The draft's AC and smoke assertion describe a mechanism the code never
produces.

**Two phantom files, two unlisted breaking tests** (agreed by two reviewers).
`internal/llm/registry_test.go` and `internal/search/search_test.go` do not exist. What
will actually break: `internal/config/validate_core_test.go:58` and
`internal/tui/renderers/registry_test.go:46-59`.

**§18 violation:** `docs/skills/add-an-in-process-tool/SKILL.md:168` and
`docs/skills/define-the-agent-yaml/SKILL.md:172` hardcode the old value, plus
`docs/CONFIG.md:742-756`. None listed.

### Phase 214

**The D-347 departure answers a paraphrase, not the deferral.** It omits part 6
consequence 3 (`decisions.md:9602`): *"Handing stored-as-authored bytes to their own
author and handing them to a third party are different questions, and this entry declines
to answer the second."* That is precisely this phase's shape. It also answers only half
the grant-semantics blocker, leaving the §13 parallel-implementation clause
(`decisions.md:9591`) untouched. Compounding: `decisions.md:9594` settles that artifact
bytes are stored UNREDACTED, so what egresses is by-design-unredacted content.

**"Grants no reach the connection did not already have" is FALSE.** The draft measured
reach at the RUN; the field is written by a tenant ADMIN (`auth.ScopeAdmin` on every
`agent_config.*` write). An admin already shapes the prompt and tool set for the tenant's
runs, so the chain is: attach a server you control → map a param → declare eligible →
receive a user's artifact bytes. Today's approximation (instruct the model to paste
content) is bounded by `findContextLeak` AND leaves a trajectory trail; egress
substitution is 8 MiB and **dispatch-local by design, so it leaves none**. Since bytes are
stored as authored, an artifact may itself contain a credential — this lands on D-300's
invariant, not an analogy to it. **Byte-eligibility must be boot-declared, default-off,
never wire-writable.** The mapping may stay wire-writable on top.

**`MCPArtifactParams map[string][]string` reintroduces the phase-212 bug one layer out.**
`mcp.go:850-885` decodes into `map[string]any`; a Go `string` carrying non-UTF-8 is
rewritten to `U+FFFD` by `encoding/json`. The "byte-for-byte" AC is unsatisfiable under
the draft's own spec. **The wire encoding must be normative in the plan** — base64
`[]byte` or MCP's typed `blob` block.

**A fourth `desc.Invoke` site has no seated resolver.**
`internal/mcpconsole/apps.go:283` invokes the wrapped descriptor from a browser-driven
Protocol request with no run. A mapped tool there hits `ErrNoResolver`; "fixing" it by
seating a second resolver destroys the run-scoping the isolation argument rests on.

**A sixth sink was missed.** `mcp.go:953` → `:1046` persists `Input: args` (the raw
`json.RawMessage`) into a durable, Protocol-readable store replayed into a browser on
session reopen. `toolcontext.go:62-72` additionally hashes `args`. The plan must state
normatively that substitution mutates only the decoded arg map, never `args`.

**The AST fence cannot cover the claimed risk.** `scan.go:94` bounds where a function is
CALLED, not where its output TRAVELS; downstream of the encoder the value is a naked
`[]byte` with no carrier and no projection bound. `scan.go:137-140` resolves by import
path, so putting `egress.go` inside `artifactref` is a same-package blind spot.

**Unnamed:** retry amplification (`mcp.go:789-800` → `RunWithPolicy` re-reads the artifact
per attempt; at N≥128 × 8 MiB × retry budget that is multi-GiB transient), and D-025
mapping immutability. `internal/protocol/agentconfig.go` in the file list does not exist.

### Phase 215

**The proposed validation seam does not exist.** `agent_config.*` performs NO agent-registry
lookup: `internal/runtime/agentcfg/protocol/service.go:894-904` checks only that
`agent_id` is non-empty, and the Service's `registry` field is the CONFIG registry
(`agentcfg.Registry`), not the agent registry. That absence is why orphan configs exist.

**There is no cheap correct replacement.** `Registry.Get` → `loadRecord` loads at the full
quadruple (`registry_impl.go:637`); `List` is triple-scoped (`:294-295`). `ListTenant`
(`:335-372`) is the only tenant-scoped read, is admin-gated by its own godoc, and is a
full-store scan filtered in Go — the shape §6 rule 2 names. **Choosing this seam is
design work, not edge validation.**

**A caller-supplied `agent_id` makes the RFC 8693 actor token client-supplied.**
`runloop.go:1285` → `tools.WithInvokingAgent` → `InvokingAgentFrom` →
`tokenexchange.go:857` `form.Set("actor_token", agentID)`. `internal/config/config.go:1330`
states verbatim: *"The actor token is the runtime's VERIFIED acting principal — never a
client-supplied field."* Compounding: `:1335-1341` documents that the exchanged token is
cached WITHOUT `agent_id` in the key, so a run naming agent B can reuse a token minted
under agent A's assertion. The draft does not mention the credential plane.

**"Findings I'm departing from: None" is false.** D-309 rules the session→agent binding
absent because a session may run multiple agents; the producer pins it
(`internal/sessions/protocol/lister_projector.go:199`). The draft's `SessionRow.agent_id`
criterion reintroduces the single-valued binding D-309 refused. Persist on the TASK only,
or record the departure.

**The unregistered dev default is a blocker, not an open question.** `mux.go:381-383` —
the boot agent is "never registered as a fleet entity"; `registry_projector.go:50-55,246`
synthesises it as an `IsDefault` row, and under a new session `ListAgents` returns ONLY
that synthetic row. **So the one id a Console selector can offer is the one id
registry-membership validation would reject.**

**`agents.list` already ships** (`internal/protocol/methods/methods.go:872`) — the draft's
"has NOT been verified" open question was answerable and §16 expects it verified in-plan.

### Phase 216

**The dependency on 215 is false** (agreed by two independent reviewers).
`ConfigScopeUser` already keys by `(tenant, real-user, agentID)`; two users under the
hardcoded agent already hold distinct revisions — which is how the shipped durable-user
PROMPT tier works today (`projection.go:984-1002`). The per-user axis is the user, not the
agent. **216 is independently schedulable, so Stage 2 has no reason to exist for it.**

**There is no run-start ATTACH leg.** `ReconcileConnections` (`projection.go:167-200`)
detaches only; `runloop.go` contains zero `Attach` calls. D-287's as-built note records
that the attach leg "stays deferred" and that the live add verb is the only attach path —
and that verb is admin-gated (`addconnection.go:206-231`). **The phase's core AC requires
building a deferred leg the draft neither scopes nor sizes.**

**The isolation goal claims a property D-301 explicitly refuses.** D-301: *"The wave claims
NO hard cross-tenant isolation of runtime-added tool DISPATCH in a shared runtime… a
shared runtime therefore TRUSTS its co-tenant admins."* The catalog is
`byName map[string]ToolDescriptor` with unfiltered `Resolve(name)`
(`internal/tools/catalog.go:20,145-148`), so per-user invisibility is achievable as a
projected-VIEW property only. Extending write authority from admins to every end user
also moves D-301's trust boundary without a superseding decision.

**The central AC contradicts the non-goals.** "the three reconcile legs compose the user
tier" — but legs 2 and 3 are credential-plane and the descriptor forbids the fields they
read.

**The bare-name collision is not open, and cannot be resolved where the draft says.**
`attach.go:256-259` already refuses a same-name attach by a different owner with
`ErrConnectionNameOwnerConflict`. The shipped answer is first-writer-wins-with-loud-refusal.
Per-user namespacing in the projected catalog is unreachable when registration,
resolution and dispatch are all bare-name.

**The SSRF rule the draft cites does not exist.** `validateConnection`
(`addconnection.go:323-395`) performs no URL parse, no scheme check, no host check — an
admin `http://169.254.169.254/…` is accepted today. The private-range refusal lives only
on the credential plane (`discovery.go:712-723`, `tokenexchange.go:487-489`). **The phase
must CREATE connection-URL validation, not inherit it.**

### Phase 217

**"Per segment RATHER THAN whole key" LOOSENS the guard.** `IsReservedMCPMetaKey`
(`validate.go:2527-2534`) has two arms: an exact-match set AND
`strings.HasPrefix(k, "io.modelcontextprotocol/")`. Splitting `io.modelcontextprotocol/ui`
on `.` yields `["io", "modelcontextprotocol/ui"]` — neither segment carries the prefix, so
per-segment-only ADMITS a spec-reserved annotation refused today, breaking three shipped
tests. **The rule is per-segment AND whole-key.**

**The §10 blast-radius claim is false and the AC was already failed at authoring time.**
`internal/runtime/agentcfg/protocol/setrevision_connections_test.go:274` ships
`MetaAnnotations: {"vendor.tag": "blue"}` inside the canonical happy-path test, asserted at
`:294` to round-trip — the shipped surface deliberately treats a dotted annotation as
well-formed. (Semantic blast radius on operator CONFIG is still zero; five other
populating callers use flat keys.)

**The depth-cap assignment is an import cycle.** `maxInjectionMetaKeyDepth` lives in
`internal/runtime/agentcfg/protocol` (`wireinjectiondescriptor.go:62`), which imports
`internal/config`. The draft puts enforcement in `internal/config/validate.go`. The
constant must be hoisted, touching a file not in the list.

**A fourth validation door was missed.** `internal/tools/drivers/mcp/attach.go:432-439`
(`resolveOAuthBinding`, called at `:183` — the shared boot + runtime-set attach path) runs
its own whole-key annotation check.

**The named concurrency hazard is near-unreachable — the draft wrote an INERT GUARD.**
`p.cfg.MetaAnnotations` is `map[string]string` and cannot hold nested maps;
`buildIdentityMeta` allocates a fresh `meta` per call. The N≥128 `-race` test would pass
trivially and prove nothing. **The real hazards the draft missed:** (a) `mcp.go:1391` is
`for k, v := range annotations` — Go randomises map iteration, so `{"a":"1","a.b":"2"}`
(both legal under per-segment checking) yields different wire bytes per RPC; (b)
`injectMeta:1535` type-asserts `map[string]any` — building `mcpsdk.Meta` intermediates
instead silently REPLACES the map, wiping every sibling.

**Scalar/map collision is silent.** `injectMeta:1535-1538` overwrites a non-map
intermediate with no error and no log; annotations run first (`mcp.go:856`), injection
second, so a flat `vendor` annotation plus `injection.meta_key = vendor.api_key` silently
discards the annotation. §13 shape, on the path this phase widens.

**Nesting causes OVER-redaction, not a leak.** `internal/audit/rules.go:167-183` matches
the key first, replaces the whole value, and does not recurse. Annotation `token.env` is
unredacted today (last segment `env`); nested, node key `token` matches and **the entire
subtree collapses to `***`**, including an injection credential's sibling in the same
namespace. Redaction coverage is preserved; the defect is over-redaction.

**AC 5's rationale is wrong.** `rules.go:167` returns `ErrRedactionDepthExceeded` — the
audit fails loud, it does not emit unredacted. The cap is worth having; the stated failure
mode is not the one that exists.

---

## Wave-level defects

**The wave-end E2E is recorded nowhere binding.** `test/integration/wave_v124_test.go` is
named only in the coordination file, which is outside §2's authority chain and unread by
`scripts/drift-audit.sh` (it globs `docs/plans/phase-*.md` only). Precedent is unbroken —
`wave_v110` through `wave_v123_test.go` all exist. **It must be in phase 216's (or the
final Stage-2 phase's) file list and test plan.** The same durability gap applies to
Track C, which includes a §6 isolation security bug.

**214 and 216 both extend the connection descriptor and neither acknowledges the other.**
214 adds byte-eligibility (exfiltration-plane); 216 hands connection authorship to end
users. **Neither answers: can a user-scope connection be byte-eligible?** 216 does not dep
214 and does not mention it. This is a security question, not a merge-order question.

**Generated-file collision across the staging plan.** 214, 215 and 216 all regenerate
`wire-manifest.gen.json` and `docs/site/protocol/*.md`, which §13 and D-209/D-223 forbid
hand-editing — so a conflict cannot be hand-resolved; the loser must re-run the
generators. Not called out anywhere.

**Six file overlaps with no dependency edge**, four inside Stage 1's parallel set:
`dispatch_test.go` (212/213), `examples/harbor.yaml` (213/217), `config.go` (213/214),
`validate.go` (214/217), `mcp.go` (214/217), `addconnection.go` (214/217 — both adding a
new rule at the same attach door).

**214 omits dep 211**, which both siblings touching the same attach door declare.

**Three plans have `## Glossary additions` but omit `docs/glossary.md` from their file
lists**: 213, 215, 217. `docs/glossary.md:108` also names a nonexistent `eof` field on the
artifact read response, and `:673` says annotations merge "verbatim" — both go stale.

---

## What the rewrite must do

1. **212** shrinks: keep the UTF-8 correctness half, specify `Offset`/livelock semantics,
   reconcile with `materialize.go`'s fetch hint and `<heavy_results>`; drop the
   recovery half to a classification-only change reusing `ErrArtifactRefNotFound`.
2. **213** needs a per-consumer decision matrix over all seven direct consumers plus the
   Protocol-visible operator-field consumers, a leak-class-qualified risk section, and a
   rewritten (or dropped) search split.
3. **214** needs a boot-declared eligibility gate, a normative wire encoding, the
   `mcpconsole` invoke path, the sixth sink, an honest fence claim, and a departure
   section that answers D-347 part 6.
4. **215** needs a Gate-0 on the validation seam and a decision on the actor-token
   consequence before it can be sized.
5. **216** needs the attach leg scoped in or the phase narrowed, its 215 dep dropped, its
   isolation goal restated as a view property, and connection-URL validation created.
6. **217** needs per-segment AND whole-key, the hoisted depth constant, the fourth door,
   and its concurrency test repointed at determinism and map-type identity.
7. **The wave** needs the E2E in a phase plan, the 214/216 byte-eligibility question
   answered, and generated-file ownership assigned to one phase.
