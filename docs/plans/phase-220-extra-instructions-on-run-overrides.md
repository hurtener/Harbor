# Phase 220 — `extra_instructions` on `RunOverrides`

## Summary

`planner.LLMOverrides.ExtraInstructions` already exists, is already additive, already renders verbatim into `<additional_guidance>`, and already survives a `SystemPromptOverride` — but it has exactly one producer, the admin-scoped governance tenant-override record, and it is not reachable over the Harbor Protocol at all. This phase adds ONE optional wire field, `extra_instructions`, to `RunOverrides`, carries it through the existing `runs.set_overrides` validate → store → consume → compose path, and pins the two-producer question with a decision rather than a default: the run-level value **composes below** the tenant value, it never replaces it. Zero new prompt semantics, zero new section, zero new error code, no `ProtocolVersion` bump.

## RFC anchor

- RFC §5.2 — what the Protocol exposes; `runs.set_overrides` is the task-control surface this widens by one field.
- RFC §6.2 — Planner interface / `RunContext`; `LLMOverrides` is the run-start-resolved bundle the field lands in.
- RFC §6.5 — LLM client layer and its context-window safety net; the section that bounds how much prompt text may reach a provider (see Risks — the bound is the TOKEN guard, not the byte guard).
- RFC §6.15 — Governance; the tenant-override record that is `ExtraInstructions`'s only producer today, and the layer this phase must not let a lower tier erase.

## Briefs informing this phase

- brief 13
- brief 02

## Brief findings incorporated

- **brief 13 §2.1–2.2 (structured sections + dynamic augmentation):** the reference design merges runtime-supplied guidance *into* the system prompt as an additive block, rather than re-authoring the prompt. Harbor's `<additional_guidance>` section is exactly that merge point (`internal/planner/react/prompt.go:714-724` — `buildAdditionalGuidance` joins operator guidance, the override's extra instructions, and the per-turn repair guidance with `"\n\n"`). This phase adds a producer to that existing merge; it does not add a section, a tag, or a rendering rule.
- **brief 13 §2.3 (memory framing — UNTRUSTED data):** the brief's whole point is that content of different provenance must be framed differently — memory gets an anti-injection preamble, operator guidance does not. Harbor implements exactly that split: `<user_instructions>` is escaped through `escapeUntrustedSection` and framed as subordinate (`prompt.go:726-763`), while `<additional_guidance>` is joined raw. This phase inherits the split rather than blurring it, and its Non-goals name the one thing that must never land here (recalled conversation memory).
- **brief 13 §6 (adopted as-is vs. modified):** the brief's guidance is to compose into Harbor's existing prompt taxonomy rather than import a second one. The smallest possible change — one optional pointer field on an existing wire struct — is that guidance taken literally.
- **brief 02 (planner + steering + HITL):** per-run control state belongs on `RunContext` / the run's override bundle, never on the compiled planner artifact (D-025). `ExtraInstructions` is read exclusively through `rc.LLMOverrides` (`prompt.go:766-773`), so this phase adds no mutable state to any shared artifact and inherits the concurrent-reuse contract unchanged.

## Findings I'm departing from (if any)

**None from a brief.** One departure from the UPSTREAM ASK's framing, recorded here and in D-365:

The upstream ask stated that Harbor offers "no additive sibling" to whole-spine prompt replacement, and asked for a new additive mechanism. **That claim is refuted by the code.** `planner.LLMOverrides.ExtraInstructions` (`internal/planner/planner.go:426`) is documented ADDITIVE (`planner.go:402-405`), renders verbatim into `<additional_guidance>` (`prompt.go:716`, `prompt.go:766-773`), and survives `SystemPromptOverride` — `buildSystemContent` receives `b.extraGuidance` and reads `rc.LLMOverrides` on BOTH branches of `baseRequest` (`prompt.go:201-227`), a property pinned today by `TestApplyLLMOverrides_SystemPromptOverrideReplaces` / `internal/planner/react/override_test.go:139-159` and `TestComposition_ExtraInstructionsStillAdditive` / `internal/planner/react/promptlayers_test.go:99-111`. The real gap is narrower and purely one of REACH: `RunOverrides` (`internal/protocol/types/runs.go:27-60`) exposes `session_id`, `reasoning_effort`, `temperature`, `max_tokens`, `system_prompt_override` and `model` — and no `extra_instructions` — so the only producer is `governance.set_tenant_overrides` (`internal/protocol/types/governance.go:114-118` → `internal/runtime/governance/protocol/service.go:185-187` → `internal/runtime/serve/runloop.go:660`). Building a NEW additive mechanism on top of the existing one would be §13's "two parallel implementations of the same conceptual feature". This phase therefore ships reach, not a mechanism.

## Goals

- Make the already-additive, already-composing `ExtraInstructions` reachable over the Harbor Protocol as one optional field on `RunOverrides`.
- Settle the two-producer question explicitly: define, document and TEST the join between the tenant-level producer and the new run-level producer.
- State plainly — in godoc, in the plan, and in `docs/decisions.md` — who may write verbatim operator-trusted prompt text as a consequence, without claiming an isolation property the design does not have.
- Keep the field's rendering position, trust framing and survives-a-replace behaviour byte-identical to what ships today for the tenant producer.

## Non-goals

- **NOT a home for recalled conversation memory, retrieved documents, or any user-authored text.** `<additional_guidance>` is rendered VERBATIM and unescaped (`prompt.go:717-724`); `<user_instructions>` and the memory tiers are the UNTRUSTED-framed positions (`prompt.go:726-763`; `planner.MemoryBlocks`, `internal/planner/planner.go:447-452`, whose godoc says "UNTRUSTED anti-prompt-injection framing"). Phase 219's `MemoryBlocks` tiers are where recalled conversation content belongs. A trusted position must not hold user-authored content, and a capability that wants to surface recalled text uses 219's tiers, not this field.
- **No new prompt section, tag, or rendering rule.** The field renders through `buildAdditionalGuidance` unchanged. The ReAct section taxonomy is untouched.
- **No named / attributed blocks.** Attribution of N independent contributors is phase 222's `ExtraSystemBlocks` on the agent-config payload — durable and per-agent, not per-run. This phase deliberately ships a single flat string, because that is what the existing field IS.
- **No new authorization tier and no new scope claim.** See Risks: the field inherits `runs.set_overrides`'s existing posture. Changing that posture is a separate decision about the METHOD, not about one field.
- **No change to `internal/runtime/agentcfg/projection`'s per-agent layer.** That layer carries sampling parameters ONLY and must keep carrying no prompt text — a property asserted today by `internal/runtime/agentcfg/projection/llmparams_test.go:52-54` and `internal/runtime/runs/protocol/overrides_test.go:131-136`. Those assertions stay green unchanged.
- **No collision with phase 222.** Phase 222 introduces its OWN carrier for named blocks; it must NOT flatten blocks into `LLMOverrides.ExtraInstructions`. This phase owns that field; 222 owns `ExtraSystemBlocks`. Stated in both plans so the two Stage-2 phases cannot converge on one field by accident.
- **No `ProtocolVersion` bump.** One additive optional field on an existing wire type.
- **No new error code.** The field validates through the existing `ErrInvalidRequest` → `invalid_request` path or not at all (see Acceptance criteria).
- **No Console page change.** The typed client mirrors the field (§4.5 item 5); wiring a Playground control to it is the consuming page's work.

## Acceptance criteria

- [ ] `RunOverrides` carries `ExtraInstructions *string` with JSON key `extra_instructions,omitempty`, and the field is godoc'd as ADDITIVE, verbatim-rendered, and operator-trusted — including the plain statement of who may write it.
- [ ] `runs/protocol.PendingOverride` carries the validated value; `Service.validate` copies it by value (no aliasing of the caller's string header).
- [ ] **The precedence decision, pinned by test: the run-level value COMPOSES with the tenant value, tenant first, joined by a blank line. It never replaces it, and there is no run-level way to clear it.** A request that sets `extra_instructions` while a tenant record also sets one produces `tenant + "\n\n" + run` in the resolved bundle.
- [ ] The join lives in `runsprotocol.ComposeLLMOverrides` — the ONE production composition that `cmd/harbor`'s run loop, the `harbortest/devstack` twin and the integration test all reach through `RunLoopDriver.resolveLLMOverrides` (`internal/runtime/serve/runloop.go:646-682`). No second copy.
- [ ] A present-but-empty (or whitespace-only) `extra_instructions` is ACCEPTED and contributes nothing — it is not an error, and specifically not a channel for clearing the tenant block. `overrideExtraInstructions` already trims (`prompt.go:766-773`); the composition must not emit a dangling `"\n\n"`.
- [ ] Every existing behaviour of the tenant producer is unchanged when the run-level field is absent: with `extra_instructions` unset, the resolved `LLMOverrides.ExtraInstructions` is byte-identical to today's, and the rendered system content is byte-identical.
- [ ] `<additional_guidance>` still renders the composed value VERBATIM (no escaping) and still survives a session `SystemPromptOverride` in the same request — asserted on the composed two-producer value, not only on a single-producer one.
- [ ] `events.RunOverridesSetPayload` gains `SetExtraInstructions bool`; the VALUE is never emitted (the payload's existing SafePayload rule, `internal/events/events.go:199-212`), and the flag appears in the `emitAudit` log attrs too.
- [ ] Identity stays mandatory and cross-session stays refused: a request with an incomplete triple is `ErrIdentityRequired` → 401, and a `session_id` outside the verified session is `ErrCrossSessionScope` → refused, both unchanged and both re-asserted with the new field set (a new field must not open a new door).
- [ ] `make protocol-ts-gen` regenerates `wire-manifest.gen.json` to a clean diff; `web/console/src/lib/protocol/runs.ts` mirrors the field by hand; `ALLOWED_OVERRIDE_KEYS` in `web/console/src/lib/protocol/tests/runs-set-overrides.test.ts:31-38` gains the key.
- [ ] `make protocol-docs-gen` regenerates `docs/site/protocol/types.md`; `docs/skills/use-the-harbor-protocol/SKILL.md` (surface `protocol`) is updated in the SAME PR per §18.
- [ ] `scripts/smoke/phase-220.sh` passes against the preflight dev server with OK ≥ 10 and FAIL = 0.

## Files added or changed

```text
internal/protocol/
└── types/runs.go                     # ExtraInstructions + the godoc trust statement

internal/runtime/runs/protocol/
├── overrides.go                      # PendingOverride field, validate copy,
│                                     #   ComposeLLMOverrides join (joinAdditiveGuidance),
│                                     #   emitAudit flag
└── extra_instructions_test.go        # NEW sibling of overrides_test.go (same
                                      #   protocol_test package, shared helpers):
                                      #   the join table + the no-clear property
                                      #   + the D-025 concurrent-reuse run

internal/events/
└── events.go                         # RunOverridesSetPayload.SetExtraInstructions

internal/planner/react/
└── extra_instructions_composed_test.go  # NEW: the COMPOSED value renders verbatim,
                                      #   survives SystemPromptOverride, absent-safe

internal/runtime/serve/
└── runloop_extra_instructions_test.go   # NEW: the join is reached through the run
                                      #   loop's own resolveLLMOverrides (unexported,
                                      #   so this must be an in-package test)

web/console/src/lib/protocol/
├── runs.ts                           # the hand-mirrored optional field
├── client.ts                         # the setOverrides godoc field list
├── tests/runs-set-overrides.test.ts  # ALLOWED_OVERRIDE_KEYS
└── wire-manifest.gen.json            # REGENERATED (make protocol-ts-gen)

test/integration/run_extra_instructions_test.go  # NEW — the two-producer seam
scripts/smoke/phase-220.sh                       # the live gate
docs/site/protocol/types.md                      # REGENERATED (make protocol-docs-gen)
docs/skills/use-the-harbor-protocol/SKILL.md     # §18 same-PR skill update
docs/{decisions.md, glossary.md}                 # D-365 + the vocabulary entry
```

## Public API surface

```go
// internal/protocol/types
type RunOverrides struct {
    // ... existing fields ...

    // ExtraInstructions, when non-nil, is an ADDITIVE block of guidance
    // for the next message. It is appended to — never a replacement of —
    // the agent's system prompt, and it survives a SystemPromptOverride
    // set in the same request.
    //
    // TRUST: the block renders VERBATIM and UNESCAPED into the
    // operator-trusted `<additional_guidance>` position. Whoever may call
    // `runs.set_overrides` for a session may therefore write text the
    // model reads as operator guidance. This is the same authority
    // `system_prompt_override` on this struct already carries.
    //
    // It composes BELOW any tenant-level extra instructions and can never
    // clear them.
    ExtraInstructions *string `json:"extra_instructions,omitempty"`
}

// internal/runtime/runs/protocol
type PendingOverride struct {
    // ... existing fields ...
    ExtraInstructions *string
}

// ComposeLLMOverrides keeps its signature. Its contract gains one
// sentence: ExtraInstructions is the one field that JOINS across layers
// (tenant then session, blank-line separated) instead of the per-field
// last-writer-wins every other field takes.
func ComposeLLMOverrides(session *PendingOverride, agent, tenant *planner.LLMOverrides) *planner.LLMOverrides

// internal/events
type RunOverridesSetPayload struct {
    // ... existing fields ...
    SetExtraInstructions bool
}
```

## The precedence decision (D-365)

Two producers now write one field. Three shapes were available; the plan picks one and pins it.

**Chosen: COMPOSE, tenant first, run-level appended below, joined by a blank line.**

1. **The field's declared semantic is ADDITIVE** (`internal/planner/planner.go:402-405`). A field that is additive with respect to the base prompt but *replacing* with respect to its sibling producer would carry two meanings — the §13 shape.
2. **Replacement would destroy a property the system holds today.** The tenant block is written by `governance.set_tenant_overrides`, an admin-scope-gated method (`internal/protocol/types/governance.go:135-140`). `runs.set_overrides` requires only a verified identity triple targeting the caller's own session (`internal/runtime/runs/protocol/overrides.go`, `ErrCrossSessionScope`) — no admin claim. And today the tenant's additive block is UNREMOVABLE by any session-level caller: a session `SystemPromptOverride` replaces the base spine but leaves `<additional_guidance>` intact (`prompt.go:201-227`). Per-field last-writer-wins would hand a non-admin caller a silent delete on an admin-set compliance block. Composition preserves the property; replacement would be a privilege inversion.
3. **Composition order is already the trust ordering.** `buildAdditionalGuidance` renders `operator-baked → override-additive → per-turn repair`, earlier meaning higher authority (`prompt.go:714-724`). Tenant-before-run continues that monotone descent, and reuses the identical `"\n\n"` join so the rendered shape is indistinguishable from a single block written by one author.
4. **Refusal was rejected.** Failing a run-level set because a tenant record happens to exist would make an invisible, admin-owned condition a hard block on a per-run contribution — precisely the "who reconstructs the rest" problem the upstream ask names, and a failure the caller cannot diagnose.

**Consequence to state, not hide:** the two contributions are NOT distinguishable to the model. Both sit in one trusted block. Per-source attribution is phase 222's job, on the durable per-agent surface where the contributors actually live.

## Trust — who may write operator-trusted prompt text

Stated plainly, because the design does not have an isolation property here and claiming one would be worse than the gap.

- `<additional_guidance>` is joined RAW: `buildAdditionalGuidance` (`prompt.go:717-724`) performs no escaping. Contrast `<user_instructions>`, which passes through `escapeUntrustedSection` and carries an explicit subordinate framing (`prompt.go:726-763`). The position is operator-trusted by construction.
- **Today**, the only producer of that additive text is the ADMIN-gated tenant-override record. **After this phase**, any caller who can reach `runs.set_overrides` for their own session — a verified identity triple, no admin claim — can also write into it.
- **This grants no authority class that surface does not already grant.** `system_prompt_override` is on the same struct, reachable by the same caller, and is strictly more powerful: it replaces the entire base spine, verbatim and unescaped (`prompt.go:206-210`, `buildSystemContent`'s `sections = []string{systemPrompt}` branch at `prompt.go:590-592`). A deployment that trusts a session caller with `system_prompt_override` already trusts them with strictly less.
- **Therefore no per-field scope gate.** Gating `extra_instructions` behind admin while leaving `system_prompt_override` ungated on the same method would be incoherent: the weaker capability harder to reach than the stronger one. **If a deployment wants operator-only prompt authorship, the gate belongs on the `runs.set_overrides` METHOD — covering both prompt-text fields — and that is a separate decision about an already-shipped surface, not this phase's to make.** It is carried as a named open risk below rather than silently resolved.
- **The binding bound this phase does enforce:** the field must be reachable ONLY through the same verified-identity door as its siblings. No route may accept `extra_instructions` from a request BODY identity that was not server-derived from the verified session (the `bodyscope` gate; `RunSetOverridesRequest` is registered on `SurfaceRuns` at `internal/protocol/bodyscope/coverage.go:135`). An integration test asserts the cross-tenant/cross-session body is refused with the new field set.

## Test plan

- **Unit** (`internal/runtime/runs/protocol/overrides_test.go`):
  - The four-cell join table — {tenant set, unset} × {run set, unset}: neither → nil; tenant only → tenant verbatim (today's behaviour, byte-identical); run only → run verbatim; both → `tenant + "\n\n" + run`, in that order.
  - The **no-clear property**: a run-level empty string, a whitespace-only string, and a run-level value alongside a tenant value each leave the tenant text present in the result. A mutation that turns the join into an assignment fails this sub-test.
  - No dangling separator: a whitespace-only run value produces the tenant text with no trailing `"\n\n"`.
  - `validate` copies by value: mutating the caller's `RunOverrides.ExtraInstructions` after the call does not change the stored `PendingOverride`.
  - The per-agent layer still carries NO prompt text — the existing assertions at `overrides_test.go:131-136` stay green unchanged (the guard that a new field did not leak into the agent arm).
  - The identity + cross-session refusals re-run with `extra_instructions` set, proving the new field opens no new door.
  - The audit flag is set exactly when the field is non-nil, and the VALUE never appears in the emitted payload (a substring assertion over the marshalled event).
- **Unit** (`internal/planner/react/promptlayers_test.go`): the COMPOSED two-producer value renders verbatim into `<additional_guidance>` with no escaping (assert both segments present, in order, and that a `<` in the text is NOT entity-escaped — the property that distinguishes this position from `<user_instructions>`); and the composed value still renders when `SystemPromptOverride` is set in the same bundle (extending `TestComposition_ExtraInstructionsStillAdditive`, `promptlayers_test.go:99-111`, from one producer to two).
- **Unit** (byte-identity): with `extra_instructions` absent, `buildSystemContent` output is byte-equal to the pre-change output for the same `RunContext`. This is the §10-shaped "absent ⇒ unchanged" pin.
- **Integration** (`test/integration/run_extra_instructions_test.go`, real drivers on every seam — real `events` inmem bus, real `audit` patterns redactor, real `agentcfg` registry, real tenant-override store, real `runs/protocol` Service and Store, real `RunLoopDriver.resolveLLMOverrides`, real ReAct prompt builder):
  - **The two-producer seam end to end**: an admin sets a tenant `extra_instructions`; a session caller sets a run-level one over `runs.set_overrides`; the run's resolved bundle carries both, in order, and the built system prompt contains both inside one `<additional_guidance>` block.
  - **Identity propagation**: the triple flows Protocol → Service → Store (keyed by `identity.Identity`) → `Consume` at run start → `RunContext`. A second tenant's session, running concurrently, sees ONLY its own tenant block and never the first's run-level text.
  - **Failure mode 1**: a request whose `overrides.session_id` names another session is refused `ErrCrossSessionScope` and NOTHING is stored (a follow-up `Peek` on both identities is empty).
  - **Failure mode 2**: a request with an incomplete identity triple is refused `ErrIdentityRequired` before the Store is touched.
  - **Failure mode 3**: the one-shot property survives the new field — a second run after the first consumes the slot sees the tenant block only, never a re-applied run-level block.
- **Conformance:** none new. `runs.set_overrides` is an existing method already covered by the `internal/protocol/conformance` method matrix; no new method, no new type, so the matrix count does not move.
- **Concurrency / leak:** `TestComposeLLMOverrides_ConcurrentReuse_NoCrossTalk` — N=128 goroutines against ONE shared `runs/protocol.Service` + one `Store`, each in its own tenant with its own distinguishable tenant block and run-level block, asserting every goroutine's composed result contains exactly its own two segments in the right order and no other goroutine's bytes. `ComposeLLMOverrides` is a pure function over its arguments; the test's value is proving the join allocates a fresh string per call and never mutates a caller's `*planner.LLMOverrides` in place (a mutation that appends into `tenant.ExtraInstructions` rather than building a new string would corrupt the shared tenant record across runs — this is the exact D-025 "mutable state on a compiled artifact" failure and the test is written to catch it). No new goroutines are started, so the package's existing leak baseline covers the leak guarantee.

### Mutation verification (binding)

Every guard is verified to fail — never to skip — when the thing it guards is removed. The four mutations to run before the PR, each named with the sub-test that must go red:

1. Turn the join into `out.ExtraInstructions = session.ExtraInstructions` → the join table's "both set" cell and the no-clear property both FAIL.
2. Swap the join order (run first) → the join table's "both set" cell FAILS on ordering.
3. Drop `escapeUntrustedSection`'s absence — i.e. route the composed value through the escaper → the verbatim-rendering sub-test FAILS.
4. Drop the `SetExtraInstructions` flag from `emitAudit` → the audit sub-test FAILS.

A guard that only stops SKIPPING is not a guard; each mutation above must produce a red test, and the PR description records the run.

## Smoke script additions

`scripts/smoke/phase-220.sh` (`PREFLIGHT_REQUIRES: live-server`). The live legs use the dev bearer and the `X-Harbor-{Tenant,User,Session}: dev` headers (the pattern `scripts/smoke/phase-217.sh:189-191` establishes).

1. **Static, single-source:** `internal/protocol/types/runs.go` declares `extra_instructions` on `RunOverrides` — this grep is the phase gate the live legs branch on.
2. **Static:** `internal/runtime/runs/protocol/overrides.go` carries `ExtraInstructions` on `PendingOverride` AND inside `ComposeLLMOverrides`'s session arm. FAILS if the field lands on the wire without reaching the composition (the inert-field shape).
3. **Static, the precedence guard:** `ComposeLLMOverrides`'s body contains a JOIN for this field and NOT a bare assignment — asserted by requiring the tenant value to be read inside the session arm. FAILS on mutation 1 above.
4. **Static:** `internal/events/events.go` declares `SetExtraInstructions`, and `overrides.go` sets it in `emitAudit`.
5. **Static, the trust statement:** the `RunOverrides.ExtraInstructions` godoc names both `<additional_guidance>` and the verbatim property. A field that ships without the trust statement is the documentation gap this phase exists to close.
6. **Static, the Console lockstep:** `runs.ts` mirrors the field and `ALLOWED_OVERRIDE_KEYS` in `tests/runs-set-overrides.test.ts` contains it (the D-223 hand-mirror obligation, checked here so a missed mirror fails preflight and not only `npm run lint`).
7. **Live:** an authenticated `POST /v1/runs/set_overrides` carrying `extra_instructions` returns 200. Route absent (404/405/501/000) → SKIP. **A 400 is a FAIL once guard 1 greps clean** — because the strict `DisallowUnknownFields()` decoder 400s an unknown key, so a 400 with the field declared means the wire type and the handler disagree.
8. **Live:** the same request with `extra_instructions` AND `system_prompt_override` set together returns 200 — the two are not mutually exclusive (the additive-survives-replace property, asserted at the wire door).
9. **Live:** a request with `extra_instructions` and NO identity headers returns 401 — the new field opens no new door.
10. **Live:** a request with `extra_instructions` whose `overrides.session_id` names a different session is refused (403) — cross-session scope holds with the new field set.
11. **Unit-test legs** under `-race`: the join table + no-clear + audit-flag + concurrent-reuse suite in `./internal/runtime/runs/protocol/`; the verbatim/survives-replace suite in `./internal/planner/react/`; the two-producer integration test in `./test/integration/`. Each leg OKs on a real pass, SKIPs only when the `-run` filter matches nothing (an older build), and FAILs on a genuine failure — the `run_filtered_tests` shape from `scripts/smoke/phase-217.sh:88-108`.

Done-definition: OK ≥ 10, FAIL = 0.

## Coverage target

Measured on the branch with `go test -count=1 -cover` before any change; each target is "hold at or above the measured baseline":

- `internal/runtime/runs/protocol`: **88%** (measured baseline 88.0%) — the package that gains the join, the validate arm and the audit flag; the new lines are all covered by the join table.
- `internal/planner/react`: **87%** (measured baseline 87.0%) — test-only change here; the target is a no-regression floor.
- `internal/events`: **93%** (measured baseline 93.4%) — one struct field; no new statements.
- `internal/protocol/types`: **62%** (measured baseline 62.6%) — a wire-struct field adds no statements; stated so a reviewer does not read the low number as a regression this phase caused.

## Dependencies

- **219** — the memory-blocks phase. It also edits `internal/protocol/types/runs.go` and it OWNS the regenerated wire manifest in Stage 1. This phase is Stage 2 and **rebases on 219**: `make protocol-ts-gen` and `make protocol-docs-gen` are run AFTER the rebase, once, so the committed `wire-manifest.gen.json` and `docs/site/protocol/types.md` carry both changes. Regenerating in parallel and merging the two generated diffs is the D-223 / D-209 failure mode this ordering exists to avoid.
- **73n** — the phase that shipped `runs.set_overrides`, its Store, its Service and the Playground consumer.
- **92b** — the tenant-override completion that made `ExtraInstructions` reachable at run start; the other half of the two-producer join.
- **92j** — the per-agent LLM-params layer, whose "sampling parameters only, never prompt text" invariant this phase must leave intact.

## Risks / open questions

- **`runs.set_overrides` is not admin-gated, and this phase does not change that.** After it lands, a verified session caller can write verbatim text into the operator-trusted `<additional_guidance>` position. The mitigation is that the same caller can already replace the whole system prompt through `system_prompt_override` on the same method, so no new authority CLASS is granted — but the surface area of trusted prompt authorship does widen from "admin only, via governance" to "any caller of this method". **Named, not resolved.** If Harbor wants operator-only prompt authorship, the gate belongs on the method (covering both prompt-text fields) and needs its own decision entry; adding it to one field would leave the stronger sibling ungated.
- **The bound on an oversized value is the TOKEN guard, not the byte guard — verified, and worth stating because the obvious assumption is wrong.** `findContextLeak` treats only `RoleTool` text as offloadable (`internal/llm/safety.go:360`, `offloadableText := m.Role == RoleTool`), so system-role text is byte-EXEMPT from `ErrContextLeak`. A huge `extra_instructions` is bounded by (a) `ProtocolConfig.MaxRequestBytes` at the wire door (default 4 MiB, `internal/config/config.go:1937-1944`) and (b) the LLM edge's token-budget guard, which fails loudly with `ErrContextWindowExceeded` (`internal/llm/safety.go:105-113`, `internal/llm/errors.go:41`). **No new per-field cap is added**, because `system_prompt_override` — unbounded on the same struct today — would make a cap on the weaker sibling pure ceremony. The two existing bounds are cited in godoc rather than implied away.
- **The two producers are indistinguishable in the rendered prompt.** Accepted for this phase; per-source attribution is exactly what phase 222 delivers, on the durable surface where the sources live. A reader who wants "which block came from where" reads the `runs.overrides_set` audit flag plus the tenant record, not the prompt.
- **No open RFC question gates this phase.**

## Glossary additions

- **additive guidance** — prompt text contributed to the agent's system prompt WITHOUT replacing any part of it, rendered verbatim into the operator-trusted `<additional_guidance>` section and surviving a `SystemPromptOverride`. Harbor has exactly one additive-guidance carrier, `planner.LLMOverrides.ExtraInstructions`, with two producers: the admin-scoped tenant-override record and (from this phase) the per-run `runs.set_overrides` field. The two COMPOSE, tenant first; the run-level producer can never clear the tenant's. Distinct from the **layered system prompt** (`Base` / `User`, the replaceable spine plus its escaped subordinate layer) and from the UNTRUSTED-framed memory tiers.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target, from a `go test -cover` run recorded in the PR description
- [ ] If multi-isolation paths changed: cross-session isolation test passes — the integration test's two-tenant concurrent arm
- [ ] **If this phase builds a reusable artifact: concurrent-reuse test passes** — `TestComposeLLMOverrides_ConcurrentReuse_NoCrossTalk`, N=128 against one shared `Service` + `Store` under `-race`, asserting no data races, no context bleed (each goroutine sees exactly its own two segments), no cancellation cross-talk, and no goroutine leak (no goroutines are started; the package baseline holds)
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists** — `test/integration/run_extra_instructions_test.go`, real drivers on every seam, identity propagation asserted, three failure modes covered, under `-race`
- [ ] The four mutation runs recorded in the PR description, each naming the sub-test that went red
- [ ] Rebased on 219 BEFORE running `make protocol-ts-gen` / `make protocol-docs-gen`; both regenerate to a clean diff
- [ ] §18: `docs/skills/use-the-harbor-protocol/SKILL.md` updated in the same PR
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (D-365)

## As shipped — deviations (§4.3)

Each is recorded in D-365 as well; they are repeated here so the plan is not read as the built shape.

1. **Unit tests land in a NEW sibling `internal/runtime/runs/protocol/extra_instructions_test.go`**, not appended to `overrides_test.go`. Same `protocol_test` package, same shared helpers.
2. **A driver-level test was ADDED beyond the plan** — `TestResolveLLMOverrides_ExtraInstructionsJoin_TenantThenSession` in `internal/runtime/serve`. The plan asserts the join is reached through `RunLoopDriver.resolveLLMOverrides`, but that method is unexported, so the integration test (like its phase-92b predecessor) calls `ComposeLLMOverrides` directly. An in-package test closes the claim mechanically instead of leaving it as prose.
3. **The react "absent ⇒ unchanged" test is `TestComposition_AbsentExtraInstructionsRendersNoGuidanceSection`**, not `..._IsByteIdentical`. "Byte-identical to the pre-change output" cannot be asserted inside one build. The property is pinned in two halves that can be: the react test asserts an absent field renders no `<additional_guidance>` section and produces a body byte-equal to a bundle without the dimension, and the join table asserts the composer returns the tenant's **exact pointer** when the session contributes nothing — stricter than equality.
4. **The smoke's phase-gate SKIP arm was DELETED.** Mutation-verified: with the arm in place, deleting the wire field produced `OK 0 / SKIP 1 / FAIL 0` and exit 0. The skeleton's raw `${HARBOR_DEV_TOKEN}` reads were replaced with `common.sh`'s `dev_bearer` (issue #624), and its `run_filtered_tests` helper (which SKIPs when `-run` matches nothing) with `assert_go_tests_pass` (which FAILs when a NAMED test does not run).
5. **A stale `§7` cross-reference in `docs/skills/use-the-harbor-protocol/SKILL.md` was corrected** under §17.6 — it pointed the `system_prompt_override` contrast at the topology-snapshot section.
6. **`mktemp` template portability gated in `scripts/drift-audit.sh`**, after this smoke's `mktemp -t phase220-gotest` failed the Linux preflight gate on `too few X's in template` — GNU rejects a template with fewer than three trailing `X`s, BSD does not. Same macOS/Linux divergence class as the existing `\t` / `\d` grep-escape guard. A sibling in `scripts/smoke/phase-184.sh` was fixed under §17.6. Verified against real GNU coreutils and mutation-verified four ways; see D-365 deviation 7.

## As shipped — coverage

Measured with `go test -count=1 -cover` on the branch.

| Package | Target (baseline) | As shipped | Verdict |
|---|---|---|---|
| `internal/runtime/runs/protocol` | 88% | **89.8%** | met, improved by the join table |
| `internal/planner/react` | 87% | **87.0%** | held (test-only change) |
| `internal/events` | 93% | **93.4%** | held (one struct field, no new statements) |
| `internal/protocol/types` | 62% | **62.6%** | held (a wire-struct field adds no statements) |
