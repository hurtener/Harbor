# Phase 158 — Session auto-naming: opt-in policy + terminal-boundary titling call

## Summary

Phase 157 gives sessions a title and a manual verb; this phase makes the runtime produce titles automatically — **opt-in, default off**. A new `naming` agent-config section (riding `set_revision` like the `hooks` section — no new Protocol verb) plus a yaml `runtime.naming` default declare the policy: whether to auto-name, after how many completed turns, whether/how often to re-name, a hard cap on total re-names, which model to use (default: the run's effective model), the naming-only reasoning posture, and the title length bound. The mechanism is a sibling of the run-completion hook at the run loop's terminal boundary: when a run completes and the resolved policy says a title is due, the runtime makes ONE bounded `Complete` call on the run's already-wrapped LLM client (governance/safety apply automatically via ctx identity) over a deterministically bounded transcript digest, then writes the result through an internal auto-path that can never overwrite a manual title. A naming failure never alters the run's outcome and is never silent (`session.naming_failed`, error class only).

## RFC anchor

- RFC §6.9
- RFC §6.17
- RFC §6.16
- RFC §6.5
- RFC §6.15

## Briefs informing this phase

- brief 02
- brief 03
- brief 05
- brief 06

## Brief findings incorporated

- brief 02 §5 item 2: steering drained "*inside* the planner loop... would force every alternate planner to replicate it. Harbor moves the inbox into the runtime" — auto-naming is the same call: RUNTIME mechanism on the run loop's terminal boundary; no planner concrete knows it exists, a swapped planner inherits it unchanged (the D-280 posture, reused).
- brief 02 §5 item 8: caps live "at runtime/protocol level, configurable, and applied before the event ever reaches a planner" — every naming bound (`after_turns`, `repeat_every`, `max_repetitions`, `max_title_len`, the transcript digest byte cap) is runtime-level configuration with validation, never a planner constant.
- brief 03 §5: "Two parallel LLM modes (the toggle smell)... Harbor picks one architecture and bakes the correction in" — the titling call rides the ONE wrapped LLM client chain (governance(retry(downgrade(corrections(safety(driver)))))); a second bespoke "naming client" or an unwrapped driver call is the rejected parallel implementation.
- brief 06 §5: free-form/user-derived strings stay off the observability surface — `session.naming_failed` carries a stable low-cardinality error class, never the transcript, the prompt, or the candidate title.

## Findings I'm departing from (if any)

- None.

## Goals

- **Opt-in, default off (binding).** With no `naming` section and no `runtime.naming` yaml block, the runtime's behavior is byte-identical to v1.11: no counters written, no LLM calls, no events. The zero-config golden path spends zero tokens on naming.
- A per-agent, versioned `naming` policy: `auto` (bool, default false), `after_turns` (int ≥ 1, default 1 — fire on the Nth completed run), `repeat_every` (int ≥ 0, default 0 = name once only), `max_repetitions` (int ≥ 1 when `repeat_every` > 0, default 5 — total auto-naming calls including the first; **no unlimited value exists**, so unbounded periodic re-naming is unrepresentable), `model` (string, "" = the run's effective model; else validated against `ModelProfiles`), `reasoning_mode` (`off` by default or `provider_default` to omit provider reasoning controls without inheriting the model profile), `max_title_len` (int, default 80, bounds [8, 200]).
- Resolution precedence mirrors the hooks section exactly: agent-config `naming` › yaml `runtime.naming` › off — resolved ONCE at run start (next-turn projection, D-234); an in-flight run keeps its snapshot.
- The titling call is governed: it carries the run's identity quadruple, flows through the wrapped client (a governance ceiling/rate block SKIPS naming with a classified event — the user's run is untouched), and its input is deterministically bounded far below the D-026 threshold.
- Manual-wins, structurally: the auto write path refuses when `TitleSource == manual`; re-naming (`repeat_every`) only ever refreshes `unset`/`auto` titles. Clearing a manual title re-arms auto-naming.
- Auto output is clamped (not rejected): the result of the internal LLM call is deterministically cut to `max_title_len` runes (single line, trimmed). Asymmetry with 157's reject-on-oversize is intentional and documented: the manual caller is an untrusted boundary (fail loud), the namer is trusted-internal post-processing its own model output.

## Non-goals

- No dedicated `agent_config.set_naming` verb — the section rides `set_revision` (the hooks precedent; a setter is additive later if a consumer asks). Consequence: no new Protocol method, wire impact is additive types only.
- No Console policy-editor UI (the agent-config panel's generic revision surface can already set the section; a dedicated naming panel is a named follow-up).
- No tenant-level kill switch beyond yaml+agent-config (settled with the operator: per-agent opt-in over yaml default is sufficient).
- No naming from the memory subsystem (default strategy `none` makes it a silent no-op — the transcript comes from the terminal boundary's live run state).
- No backfill naming of idle/closed sessions; no scheduled re-naming outside run completion.
- No title content on any event, log line, or audit payload.

## Acceptance criteria

- [ ] `agentcfg.NamingSection` on `ConfigPayload` (pointer-optional): normalization branch, `NamingDiff` + `DiffNaming` arm, accessor, wire type `AgentConfigNaming` + diff arm, the three generator `typeindex.go` registrations, `payloadToWire`/`payloadToDomain` branches, TS mirror + regenerated manifest + docs. NO new verb; `set_revision` round-trips it.
- [ ] `validateNaming` in the agentcfg protocol service (+ `ErrInvalidNaming` sentinel): bounds above; `repeat_every > 0` with `max_repetitions < 1` → rejected; `model` validated via the existing `validateModel` path; invoked from `set_revision` beside `validateLLMParams`/`validateHooks`.
- [ ] D-283 guard extended: the new section is carried forward by ALL existing section-scoped setters (`mcppolicy.go`, `addconnection.go`, `removeconnection.go`, `skills.go`, `promptlayers.go`, `llmparams.go`), `rcSeed` populates it, and the reflection guard passes — an omission fails `go test` naming the field.
- [ ] Yaml `runtime.naming.{auto,after_turns,repeat_every,max_repetitions,model,reasoning_mode,max_title_len}`: config schema + `validate.go` checks (same bounds plus the closed reasoning-mode enum) + example configs updated (§10); `projection.ActiveNamingPolicy` resolves agentcfg › yaml › off and is twinned in `cmd/harbor` + `harbortest/devstack` (D-094/§17.6). A present agent-config section is whole-section authoritative, so omitted `reasoning_mode` there defaults to `off` rather than inheriting yaml.
- [ ] Session record counters: `TurnCount`, `AutoNameCount`, `LastAutoNamedTurn` (additive JSON). `SessionRegistry.RecordCompletedTurn(ctx, id, ident) (int, error)` increments and returns `TurnCount`; it is called from the terminal boundary ONLY when a naming policy is active for the run (documented consequence: enabling naming mid-session counts turns from enablement — no per-run write amplification for the naming-off fleet).
- [ ] `SessionRegistry.SetTitleAuto(ctx, id, ident, title) error`: no-op-with-typed-refusal when `TitleSource == manual` (`ErrManualTitle`); clamps to the policy bound BEFORE the call (the trigger clamps; the registry re-validates length defensively); sets `TitleSource = auto`, bumps `AutoNameCount` + `LastAutoNamedTurn` in the SAME record save (never a torn two-write update); publishes `session.title_changed` (source=auto, content-free).
- [ ] Terminal-boundary trigger (sibling to the completion hook's deferred region in the run loop): after `(fin, err)` settle — eligibility: policy active AND `TitleSource != manual` AND (`AutoNameCount == 0` AND `TurnCount ≥ after_turns`, OR `repeat_every > 0` AND `AutoNameCount < max_repetitions` AND `TurnCount ≥ LastAutoNamedTurn + repeat_every`). Fires under `context.WithTimeout(context.WithoutCancel(runCtx), DefaultNamingTimeout=10s)` — identity values preserved, cancelled runs still name.
- [ ] The titling call: ONE `Complete` on the run's wrapped LLM client; `model` override applied via the same mechanism `ActiveLLMOverrides` uses when the policy names one; prompt = fixed template over a bounded transcript digest (first + last entries of the completion-boundary conversation, per-entry and total byte caps ≤ 4 KiB, existing title included on re-name) — a unit test pins the bound so D-026's `ErrContextLeak` is unreachable by construction.
- [ ] Failure semantics: a naming failure (LLM error, timeout, empty/multi-line-garbage output, registry refusal, panic — `recover()`-contained) NEVER alters the settled run outcome; emits canonical `session.naming_failed` (SafePayload: identity scope, session id, stable error class — `llm_error` / `timeout` / `empty_title` / `governance_blocked` / `manual_title` / `internal`) + one Warn log. A governance `PreCall` block is the `governance_blocked` class: the run is unaffected, naming is skipped loudly, never retried within the same completion.
- [ ] Opt-in proof: a config-free boot runs N runs and a test asserts zero naming LLM calls, zero counter writes, zero naming events (byte-identical session records vs. v1.11 shape modulo nothing).
- [ ] E2E (scripted-LLM, 83l pattern): enable via `set_revision` → next run completes → title lands with `source=auto`; manual `sessions.set_title` → subsequent runs never overwrite (`manual_title` skip class observed); clear manual → auto re-arms; `repeat_every=2, max_repetitions=3` honored across a run sequence (renames stop at the cap); governance-ceiling leg (exhausted ceiling → run succeeds, naming skipped with `governance_blocked`); identity asserted on every write.
- [ ] Concurrency: N≥10 concurrent sessions with naming on, distinct scripted titles — no cross-session title bleed, `-race` clean; goroutine baseline returns after the naming goroutines complete (bounded, joined via the detached ctx timeout).
- [ ] `scripts/smoke/phase-158.sh` OK ≥ 2, FAIL = 0.

## Files added or changed

- `internal/agentcfg/agentcfg.go` — `NamingSection`, payload field, normalization, diff arm, accessor.
- `internal/protocol/types/agentconfig.go` — `AgentConfigNaming` + diff wire arm; three `typeindex.go` files; `singlesource.go`.
- `internal/runtime/agentcfg/protocol/service.go` — `validateNaming`, `ErrInvalidNaming`, to-wire/to-domain branches; `rebuild_completeness_test.go` seed; the six setters' carry-forward blocks.
- `internal/config/config.go` + `validate.go` — `runtime.naming` block; `examples/` configs.
- `internal/runtime/agentcfg/projection/projection.go` — `ActiveNamingPolicy`.
- `internal/runtime/steering/` — `NamingSpec` on `RunSpec`, the terminal-boundary trigger (new `naming.go` beside `completion.go`), transcript digest builder reusing the completion transcript assembly, events (`session.naming_failed` lives with the trigger; `session.title_changed` is Phase 157's).
- `internal/sessions/sessions.go` + `registry.go` — counters, `RecordCompletedTurn`, `SetTitleAuto`, `ErrManualTitle`.
- `cmd/harbor/cmd_dev_runloop.go` + `harbortest/devstack/` — resolve + pin `NamingSpec` at run start (D-094 twins).
- `docs/site/protocol/types.md` / `events.md` (regenerated); `web/console/src/lib/protocol/agentconfig.ts` + `wire-manifest.gen.json`.
- `scripts/smoke/phase-158.sh`; `RFC-001-Harbor.md` §6.9 (amended in the plans PR); `docs/glossary.md`.

## Public API surface

- `agentcfg.NamingSection{Auto bool; AfterTurns, RepeatEvery, MaxRepetitions, MaxTitleLen int; Model, ReasoningMode string}` (+ diff arm).
- `sessions.SessionRegistry.RecordCompletedTurn`, `sessions.SessionRegistry.SetTitleAuto`, `sessions.ErrManualTitle`.
- Yaml: `runtime.naming.*`; wire: `AgentConfigNaming` (additive types only).

## Test plan

- **Unit:** eligibility truth table (first-name at `after_turns`; `repeat_every` cadence; cap stops exactly at `max_repetitions` counting the first call; manual halts; clear re-arms; mid-session enable counts from enablement); validation bounds incl. the `repeat_every`-without-cap rejection; clamp determinism (rune cut, single-line normalization); digest byte-bound pin; `SetTitleAuto` atomicity (counters + title one save) + `ErrManualTitle`; projection precedence (agentcfg › yaml › off).
- **Integration:** the E2E above (real devstack, scripted OpenAI-compatible LLM server, real in-mem drivers, identity propagation, TWO failure modes: governance-blocked + scripted LLM error → `session.naming_failed` with correct class, run outcome untouched).
- **Conformance:** N/A — no driver seam change.
- **Concurrency / leak:** the N≥10 concurrent-sessions bleed test + goroutine-baseline assertion above; the registry D-025 stress extended with concurrent `RecordCompletedTurn`/`SetTitleAuto`/`SetTitle` mixes on one instance under `-race`.

## Smoke script additions

- live-server: `set_revision` carrying a `naming` section round-trips through `agent_config.get` (section present in the payload + diff); an invalid section (`repeat_every: 2` without `max_repetitions`) → 400.
- unit-tests: the steering naming-trigger package tests.

## Coverage target

- `internal/runtime/steering`: 80%
- `internal/runtime/agentcfg/protocol`: 85%
- `internal/sessions`: 85%

## Dependencies

- 157 (title field + auto source constant + event), 150 (the terminal boundary + transcript assembly this siblings), 152 (the D-283 guard this extends), 92a (agent-config revisions), 118 (D-223), 83l (the scripted-LLM integration harness pattern).

## Risks / open questions

- Two runs of one session completing concurrently can both pass eligibility and both call the LLM (two naming calls, one winner) — accepted: writes serialize in the registry, the record is never torn, and the cost is one redundant cheap call in a race window; a cross-run naming mutex is deliberately NOT built (same posture as D-287's accepted reconcile window). Documented in the trigger's godoc + pinned by the concurrent test asserting a consistent final record.
- The titling call consumes the session identity's governance budget by design (it is work done on the tenant's behalf); operators pointing `model` at a cheap profile is the intended mitigation. Recorded in D-289.
- `RecordCompletedTurn` only counting policy-active runs trades global write amplification for "counts start at enablement" semantics — recorded in D-289; revisit only if a consumer needs absolute turn ordinals.

## Glossary additions

- "Session auto-naming" (docs/glossary.md, same PR).

## As-built notes (§4.3 deviations)

- **Eligibility read helper added.** The plan's Public API listed only
  `RecordCompletedTurn` + `SetTitleAuto`, but the eligibility gate needs the
  session's title provenance + counters. As built: `*sessions.Registry` gains a
  read-only `AutoNamingState(ctx, id, ident) (sessions.AutoNamingState, error)`
  (`{TitleSource, CurrentTitle, TurnCount, AutoNameCount, LastAutoNamedTurn}`);
  the steering trigger consumes the three methods through a narrow
  `steering.SessionTitler` interface. `RecordCompletedTurn` + `SetTitleAuto`
  join the `SessionRegistry` interface; `AutoNamingState` stays on the concrete
  (+ the steering interface) to bound interface churn.
- **Model resolution split (D-094 mirror).** `projection.ActiveNamingPolicy`
  returns the policy's `model`; each run-loop driver computes the effective
  model fallback (policy model → the run's `LLMOverrides.Model` → `""`), the
  same one-place-precedence deviation `ActiveRunCompletionHook` documents.
- **Normalization preserves section presence (adversarial-review M1 fix;
  supersedes the round-1 "normalization drop rule" note).** Round 1 dropped an
  "inert" naming section (auto false + all-zero) at normalize time, mirroring
  the hooks empty-tool posture, and justified it as "a bare `auto:false` is
  indistinguishable from unset at the Go `bool` level." That justification is
  RETRACTED: section PRESENCE is the distinguishable signal. The inert-drop
  made a bare `{auto: false}` opt-out revision silently vanish (200 OK, section
  normalized away, agent keeps auto-naming and spending over a yaml-on fleet
  default). As built: `NormalizePayload` preserves ANY non-nil naming section
  verbatim (model trimmed); the projection's section-present branch treats
  `Auto=false` as the explicit per-agent off that wins over yaml. Pinned by
  `TestNormalizePayload_Naming_PresenceIsPreserved`, the projection's
  `agentcfg_bare_auto_false_overrides_yaml_on` leg, and the E2E footgun
  regression `TestE2E_SessionAutoNaming_BareAutoFalseRevision_OverridesYamlOn`.
- **Terminal-boundary ordering: hook first, naming second (adversarial-review
  S1 fix).** The naming defer registers BEFORE the hook defer, so via LIFO the
  run-completion hook fires FIRST — its `CompletedAt`/`DurationMS` are stamped
  before the (up to 10s, synchronous) naming LLM call can inflate them, and
  transcript egress is never delayed behind naming. Pinned by
  `TestRun_TerminalOrdering_HookFiresBeforeNaming` (a slow naming completer
  advances a fake clock 7s; the hook payload's duration excludes it).
- **Failure-retry posture (documented per adversarial-review S3).** A naming
  failure does NOT consume the `max_repetitions` cap (only a successful
  `SetTitleAuto` bumps `AutoNameCount`), so a still-due title is retried at
  every subsequent completed run until one succeeds. Deliberate — a transient
  LLM outage must not permanently un-name a session — but on a naming-on fleet
  with a DOWN naming LLM every completed run pays one failing (≤ 10s)
  synchronous attempt + one `session.naming_failed`, indefinitely. Worst-case
  post-run latency envelope: hook timeout + naming timeout, serialized
  (default 10s + 10s). Documented in the `NamingSpec`/`fireNaming` godoc,
  `docs/CONFIG.md`, and the D-289 as-built note.
- **`max_repetitions` default 5 implemented at the policy layer
  (adversarial-review N2).** `NamingPolicy.WithDefaults` applies
  `MaxRepetitions = 5` when `RepeatEvery > 0` and the cap is unset, so the
  documented default is real for programmatically-built policies (embedders
  bypassing the yaml/wire edges); the wire and yaml validators still REQUIRE
  an explicit cap ≥ 1 whenever `repeat_every > 0`.
- **Synchronous trigger.** `fireNaming` runs synchronously inside `Run`'s
  deferred region (not a spawned goroutine), so the goroutine-baseline
  guarantee holds trivially; the accepted concurrent-completion race is between
  DISTINCT runs' terminal boundaries, serialized by the registry writes.
- **Naming-only reasoning compatibility (2026-08-15 correction).** The naming
  request defaults to reasoning effort off so a selected model profile's
  reasoning default cannot consume the fixed 64-token title allowance. Production
  evidence showed a low-effort reasoning model consuming all 64 tokens
  privately and returning empty visible content. The 4 KiB digest, 10s timeout,
  one-call shape, 64-token output ceiling, and 200-rune persisted-title clamp
  remain unchanged; reasoning is never used as title content. The closed
  naming-only `reasoning_mode` also permits `provider_default`, which omits
  provider reasoning controls without inheriting the planner profile for
  providers that reject such controls. A present durable naming section owns
  the whole section, so an omitted mode there resolves to the default `off`.
- **Governance E2E leg uses the rate-limit tier, not the budget ceiling.** The
  end-to-end governance-block leg
  (`TestE2E_SessionAutoNaming_GovernanceBlock_SkipsLoudly`) composes real
  identity-tier enforcement into the real wrapped chain and blocks the naming
  call's PreCall with a one-shot rate-limit bucket (capacity 1: the planner
  call drains it, the naming call underflows → `governance_blocked` on the
  bus, run untouched). A deterministic budget-CEILING breach would require
  synthetic cost accounting (the scripted provider reports no cost; the
  corrections usage-backfill knob only fires on all-zero usage) — the rate
  limiter exercises the same PreCall gate on the same chain with exact
  one-call semantics. The remaining plan-listed E2E legs all shipped:
  set_revision-enable over yaml-off
  (`TestE2E_SessionAutoNaming_SetRevisionEnable_OverridesYamlOff`) and the
  repeat cadence + cap across a real run sequence
  (`TestE2E_SessionAutoNaming_RepeatCadence_CapHonored`).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] Concurrent-reuse: registry + trigger stress under `-race` as above (N≥100 on the registry, N≥10 cross-session on the trigger)
- [ ] Integration test wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, runs under `-race`
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: N/A — none departed
