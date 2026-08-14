# Phase 244 — Draft-only personal-skill proposer tool (HA-62)

## Summary

Add `skill_create_draft` as an ordinary, disabled-by-default runtime tool that
turns bounded natural-language intent and optional feedback into a reviewable
caller-scoped `SKILL.md` artifact. It uses the governed authoring path's
safety-wrapped LLM adapter while sharing Phase 243's canonical semantic skill
DTO, validator, deterministic serializer, and `PackageHash`; it has
zero mutation authority — no
durable skill, configuration, membership, or publication authority.

## RFC anchor

- RFC §5.2
- RFC §5.5
- RFC §6.4
- RFC §6.5
- RFC §6.7
- RFC §6.10
- RFC §6.13
- RFC §6.15
- RFC §6.16

## Briefs informing this phase

- brief 03
- brief 04
- brief 07

## Brief findings incorporated

- brief 03 §4: tool catalog entries carry schemas, deadlines, approval and
  OAuth policy, and identity-scoped invocation rather than ad-hoc authority.
- brief 04 §4.7: generated `SKILL.md` must pass the same deterministic parser,
  normalizer, supporting-file rules, validator, and serializer as import.
- brief 04 §4.8: LLM-assisted proposal and persistence concerns are separable;
  this tool keeps its generated artifact draft-only and delegates installation
  of that artifact to the explicit import approval boundary.
- brief 07 §8 and §10: ordinary dispatch owns schema validation, policy,
  deadlines, cancellation, audit, redaction, and runtime-stamped call identity;
  model text cannot mint authority.

## Findings I'm departing from (if any)

- brief 04 §4.8 and the older RFC §6.7 generator paragraph describe the
  existing structured `skill_propose(persist=true)` surface, including its
  caller-selected scope and project default. D-423 does not change or deprecate
  that behavior, nor `agent_config.user.skills.upsert`. It narrows only the new
  natural-language tool: an artifact produced by `skill_create_draft` installs
  through Phase 243 validate/commit. The LLM adapter/prompt/decoder stays
  separate from structured `skill_propose`; both share canonical semantic DTO,
  validation, deterministic serialization, and `PackageHash` only.

## Goals

- Register `skill_create_draft` through the standard catalog and policy shell,
  disabled by default and explicitly enabled per agent configuration.
- Accept a bounded intent and optional bounded feedback without accepting
  tenant, user, session, scope, agent, persistence, publication, or capability
  authority from tool arguments.
- Reuse D-411's safety-wrapped LLM adapter pattern for the new intent-to-draft
  path. Keep its prompt/decoder separate from the existing structured
  `skill_propose`, which invokes no LLM. Share Phase 243's canonical semantic
  DTO, validator, deterministic serializer, supporting-file rules, and
  versioned `PackageHash`.
- Store the validated serialized draft as one immutable artifact under the
  invocation's verified `(tenant,user,session)` and return its reference plus
  bounded non-secret review metadata.
- Make installation of an artifact returned by this tool an explicit consumer
  action through Phase 243 validate/commit, preserving reviewed-byte approval.
  Existing direct upsert and structured `skill_propose(persist=true)` remain
  compatible and unchanged.

## Non-goals

- No `SkillStore.Upsert`, agent-config revision/membership write, operator-pack
  proposal/publication, scope promotion, capability activation, or tool grant.
- No `persist`, `save`, `publish`, `replace`, scope, owner, or effective-agent
  argument, even if the model emits one in free text or structured output.
- No reuse of the structured `skill_propose` handler as an LLM adapter: the new
  intent prompt/decoder is separate. No second semantic validator, serializer,
  `PackageHash`, artifact writer, or Phase 243 workflow-specific install path.
- No claim that creating the draft installed or enabled a skill.
- No remote URL ingestion, arbitrary local filesystem access, unbounded model
  output, or executable supporting-file generation.

## Acceptance criteria

- [ ] `skill_create_draft` is absent from the model-visible catalog by default.
      Enabling it uses the existing per-agent tool policy; policy, approval,
      governance, rate/cost limits, deadline, cancellation, redaction, and audit
      wrappers are identical to other ordinary tools.
- [ ] Invocation authority comes only from the verified run context and the
      runtime-resolved effective agent. Arguments containing tenant/user/session,
      agent, scope, persistence, replacement, publication, tool visibility,
      OAuth, or approval directives are rejected or treated solely as inert
      intent text and can never affect execution authority.
- [ ] Intent, feedback, model response, normalized body, file count, individual
      file size, and total serialized package bytes have named limits enforced
      before artifact persistence. Limit errors are typed and carry no raw model
      output or secret-bearing prompt text.
- [ ] The new tool uses the governed authoring path's safety-wrapped LLM
      adapter pattern. Its intent prompt and model-output decoder are distinct
      from structured `skill_propose`, which receives a caller-supplied
      `SkillDraft` and invokes no LLM. `skill_propose`, `skill_create_draft`,
      and Phase 243 share one canonical semantic skill DTO, validation,
      deterministic serialization, and versioned `PackageHash`; lockstep tests
      fail if any bypasses that shared core.
- [ ] Existing `skill_propose(persist=true)` and
      `agent_config.user.skills.upsert`, including caller-selected `ScopeUser`,
      remain byte- and behavior-compatible and are not deprecated. Only
      installing an artifact returned by `skill_create_draft` is exclusive to
      Phase 243's validate/commit workflow.
- [ ] Malformed, schema-invalid, unsafe, or policy-disallowed model output is
      never stored as a valid draft. Bounded repair may use the existing
      proposer policy; exhaustion/refusal fails loud and creates no artifact.
- [ ] A successful result writes exactly one immutable `SKILL.md` draft
      artifact under the invocation's caller scope and returns `ArtifactRef`,
      versioned `PackageHash`, normalized name/title/description summary,
      warnings, and an explicit `installed: false`/draft state. Raw package
      bytes do not enter canonical events, audit, task rows, or tool-result
      metadata beyond the existing bounded artifact-reference contract.
- [ ] No success, failure, retry, cancellation, replay, or response-loss path
      can call SkillStore mutation, user-skill membership/revision mutation,
      operator-pack proposal/publication, or capability registration. Spies at
      those seams are mandatory non-vacuity guards.
- [ ] Replaying the same runtime-stamped invocation converges to the same
      content-addressed artifact result without duplicate mutable state; a
      changed intent/feedback produces a distinct provenance/hash as
      appropriate.
- [ ] The returned draft validates through
      `agent_config.user.skills.import_validate`; its reviewed package hash and
      normalized representation are identical. Only a later explicit Phase 243
      commit may install that artifact, and current method/body-scope authority,
      signed reach/lifecycle, expected config hash, integrity, and configured
      ceilings are rechecked there.
- [ ] Two users and two sessions sharing the tool/LLM adapter cannot read,
      reuse, list, or commit one another's drafts. Unknown/cross-identity
      references fail as typed not-found/denied without existence disclosure.
- [ ] Model refusal, malformed structured output, validation failure, LLM
      timeout, cancellation during generation and artifact write, ArtifactStore
      failure, response loss, ordinary tool disablement/signed-reach or
      lifecycle denial, and session erasure have deterministic tests and no
      leaked goroutines or partial mutation.
- [ ] N>=128 concurrent invocations against one shared tool/LLM adapter across
      mixed triples and effective agents pass under `-race` with no data race,
      context bleed, cancellation cross-talk, goroutine leak, or cross-scope
      ref.
- [ ] Tool schema, builtin registration/config docs, operator skill, canonical
      result/event metadata, generated docs, and any Console lockstep
      projection land together without a Protocol version bump or new Protocol
      method unless implementation proves an ordinary tool cannot carry the
      settled contract.

## Files added or changed

- `internal/skills/generator/` compatibility pins plus shared semantic DTO /
  validation / deterministic serialization / `PackageHash` adapter only
- `internal/skills/authoring/` reusable safety-wrapped LLM adapter/prompt/
  decoder factored from governed authoring without carrying persistence
- `internal/runtime/agentcfg/protocol/` keeps the distinct D-411 operator-pack
  proposal ledger and the D-422 commit-phase token-derived ledger and consumes
  the factored LLM adapter where needed
- `internal/skills/tools/` draft tool registration, schema, and tests
- `internal/skills/` Phase 243 package validation/serialization surface
- `internal/tools/` only where standard builtin registration/config requires it
- `internal/runtime/serve/` standard builtin assembly and policy tests
- `internal/artifacts/` existing scoped writer integration tests; no new driver
  capability
- `test/integration/personal_skill_draft_test.go`
- `docs/CONFIG.md`, `examples/dev.yaml`, `docs/skills/`, generated tool docs,
  `docs/glossary.md`, `docs/decisions.md`, `docs/plans/README.md`,
  `RFC-001-Harbor.md`, and `CHANGELOG.md`
- `scripts/smoke/phase-244.sh`

## Public API surface

- Ordinary tool declaration `skill_create_draft` with bounded
  `{intent, feedback?}` input and `{artifact_ref, package_hash, summary,
  warnings, state}` output.
- Shared canonical semantic skill DTO, validation, deterministic serializer,
  and versioned `PackageHash` consumed by structured `skill_propose`, governed
  authoring, Phase 243 import, and this tool. LLM adapters/prompts/decoders and
  the D-411 operator-pack proposal ledger / D-422 commit-phase token-derived
  ledger remain separate.
- One additive per-agent configuration flag/policy entry using the existing
  tool enablement mechanism; default is disabled.

## Test plan

- **Unit:** input/result bounds; authority-field injection; semantic-core
  lockstep; separate LLM-adapter and structured-input tests; safe prompt/output
  decoding; repair/refusal; `PackageHash` goldens; compatibility pins for
  direct upsert and every `skill_propose` persist/scope branch; non-mutation
  spies; result redaction; replay/cancellation.
- **Integration:** ordinary catalog dispatch through policy/approval/governance
  to a scripted proposer and real ArtifactStore, then Phase 243 validate;
  include disabled policy, reach revocation, session erasure, and
  artifact-write failure with real drivers.
- **Conformance:** every ArtifactStore preserves the same draft bytes and
  metadata through the existing contract. Tool/Protocol integration run across
  registered drivers owns caller-scope authorization, cross-user/session
  denial, and typed method failures; store conformance does not claim auth. No
  new optional driver capability is introduced.
- **Concurrency / leak:** N>=128 mixed-identity invocations on one registered
  tool and proposer under `-race`, with independent cancellation barriers and
  a final goroutine baseline.
- **Fuzz:** model structured output, Markdown/frontmatter, feedback strings,
  authority-shaped arguments, and result decoding with bounded allocations.

## Smoke script additions

- Prove the tool is disabled by default, enable it through
  normal agent policy, create a draft, assert the returned ref is caller-scoped
  and no user skill/membership exists, then validate it through Phase 243.
- Assert authority-shaped input, cross-user access, refused/malformed output,
  and a direct persist attempt fail without mutation.

## Coverage target

- `internal/skills/generator`: 90%; `internal/skills/tools`: 95%; touched
  runtime/tool assembly: measured package baseline with 100% coverage of new
  authority and non-mutation branches; integration package: 85%.

## Dependencies

- Depends on Phases 26, 40, 41, 202, 205, 209, 232, 237, and 243.

## Risks / open questions

- The existing `skill_propose` persistence option is a compatibility surface.
  Shared semantic factoring must not change its project default, caller-
  selected `ScopeUser`, origin precedence, persistence, or receipt behavior;
  the implementation PR inventories callers and pins every branch first.
- The tool result must remain useful without embedding full Markdown in model
  context. The artifact ref plus bounded summary is the contract; consumers
  inspect bytes through existing authorized artifact reads.
- A model may mention powerful tools in prose or `required_tools`; canonical
  validation can preserve applicability metadata, but runtime composition must
  still filter it and never treat it as a grant.

## Glossary additions

- **Personal skill draft**
- **`skill_create_draft`**

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages >= stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] Concurrent-reuse test passes with N>=100 under `-race`, including no
      data races, context bleed, cancellation cross-talk, or goroutine leaks.
- [ ] Real-driver integration wires catalog policy, proposer, ArtifactStore,
      and Phase 243 validation with identity propagation and a failure mode.
- [ ] If new vocabulary: glossary updated
- [ ] The brief/RFC generator-persistence departure is recorded in D-423 and
      reconciled with the existing `skill_propose` contract before merge.
