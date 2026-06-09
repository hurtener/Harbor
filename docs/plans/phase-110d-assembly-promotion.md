# Phase 110d — Assembly promotion (`internal/runtime/assemble`)

## Summary

The config→stack fan-out — ~700 ordered lines wiring stores → bus → llm → memory →
skills → tasks → catalog → coordinator → planner, with closers — exists in exactly two
places: `cmd/harbor/cmd_dev.go::bootDevStack` (`:396-771`, `package main`) and
`harbortest/devstack`'s `tryAssemble` (`devstack.go:467` — error-returning but
unexported; the exported `Assemble` demands `*testing.T`). The SDK friction audit
(`docs/notes/sdk-friction-audit.md` §2 P6–P8, §5) found the official external surface
is therefore "a test fixture wearing the assembly-entry-point's clothes", and the two
copies have already diverged (devstack's MCP attach drops `ToolPolicy` projection
silently). Phase 110d promotes the `tryAssemble` shape into an exported,
error-returning `assemble.Assemble(ctx, *config.Config, Options) (*Stack, error)` in a
non-test package (`internal/runtime/assemble`); `bootDevStack` and
`devstack.Assemble(t, ...)` both become thin wrappers — collapsing the last of the
D-094 mirror. It also promotes the remaining cmd-local assembly legs (an exported MCP
attach helper including the policy projection, `auth.BuildProviders` for the
OAuth/KEK/sealer/tokenstore chain, and an `events.OpenWith` variant so the durable
event driver can share the runtime's StateStore), and ships
`docs/recipes/embed-harbor-headless.md` as its acceptance-gated recipe — the recipe the
audit said "cannot honestly be written" until these promotions land. Part of the Wave B
re-homing program (D-193); this phase's decision is **D-197 (reserved; logged when the
phase ships)**.

## RFC anchor

- RFC §6.4 — tool catalog + transports (the MCP attach lifecycle + ToolPolicy
  projection and the OAuth provider assembly being promoted).
- RFC §6.13 — typed event bus (the durable driver's StateStore sharing via
  `events.OpenWith`).
- RFC §9 — persistence triad (the store factories the fan-out opens in order, with
  closers).
- RFC §10 — stack decisions / configuration (the `*config.Config` the assembly
  consumes end-to-end).

## Briefs informing this phase

- brief 06 — events, observability, devx (the dev surface must boot the runtime
  in-process and consume it, never re-implement it).
- brief 05 — state, tasks, artifacts, sessions, distributed (the subsystems the
  fan-out composes, in dependency order, with lifecycle).
- brief 01 — core runtime (one model, no legacy "before" mode — the
  single-assembly-path rule).

## Brief findings incorporated

- **brief 06 §5 "Tightly coupled Playground".** "Harbor avoids this by making
  `harbor dev` boot the runtime in-process and have the Console talk to it via the
  protocol — no playground-private endpoints." The corollary the audit surfaced: the
  *boot itself* must be a runtime capability, not a binary-private ritual. When the
  only assembly prior art is 1,100 lines of `package main` and a `*testing.T`-gated
  mirror, every embedder re-implements boot — the exact coupling failure the brief
  warns about, one layer down.
- **brief 01 §5 "An egress endpoint with two bolted-on modes is a trap … pick one
  model and ship it. There is no legacy 'before' mode."** Two hand-ordered copies of
  the fan-out ARE two modes of the same feature; they have already diverged (the MCP
  policy-projection drop). After 110d there is ONE assembly function and two thin
  wrappers — no second ordering to drift.
- **brief 05 §8 (cross-subsystem dependencies).** The state/tasks/artifacts/sessions
  band is explicitly dependency-ordered (stores before consumers; closers in reverse).
  The promoted `Assemble` makes that documented order an executable, tested contract
  (partial-failure → close-what-opened) instead of a comment convention duplicated in
  two files.

## Findings I'm departing from (if any)

None.

## Goals

- **`internal/runtime/assemble` package** — the promoted fan-out:
  - `assemble.Assemble(ctx context.Context, cfg *config.Config, opts Options) (*Stack, error)`
    — error-returning, `testing`-free, promoted from `tryAssemble`'s shape
    (`devstack.go:467`) reconciled against `bootDevStack`'s ordering (where the two
    have drifted, **production's behaviour wins** and the difference is noted in
    D-197). Returns a partial `*Stack` on error so the caller's deferred `Close`
    drains whatever opened (today's contract, kept).
  - `assemble.Stack` exposes the composed subsystems (stores, bus, redactor, llm,
    memory, skills, tasks, catalog, sessions, pause coordinator, planner, registries)
    plus `Close(ctx) error` running the closer chain in reverse order; `assemble.Options`
    carries the injection points both existing callers need (logger, clock where
    applicable, the devstack's test conveniences expressed as options, not forks).
  - Server/listener concerns STAY in `cmd/harbor` (`devStack.serve`, mux, CORS,
    Console attach) — `Assemble` ends where the network surface begins.
- **MCP attach promoted next to the driver:** an exported helper (e.g.
  `mcpdrv.Attach(ctx, ms config.MCPServerConfig, cat tools.ToolCatalog, reg *Registry, bus events.EventBus, logger *slog.Logger, closers *[]func(context.Context) error) error`
  — final shape at implementor's discretion per §4.3) absorbing
  `cmd_dev.go::attachDevMCPServer` (`:2051`) **including** the config→`ToolPolicy`
  projection (`projectMCPToolPolicies` + `toolPolicyFromProjected`, `:2167-2198`).
  Devstack's copy **currently drops the policy projection silently** — converting it
  to the promoted helper fixes that drift in the same PR (§17.6 fix-both-sides).
- **OAuth provider assembly promoted:** `auth.BuildProviders(ctx, cfg config.ToolsConfig, deps Deps) (map[string]*toolapproval.ApprovalGate, map[string]toolauth.OAuthProvider, error)`
  (home: `internal/tools/auth`; absorbs `cmd_dev.go::applyToolCatalogWiring`'s
  KEK-resolve → sealer → token store → provider factory loop + gate construction,
  `:1637-1720`). Fail-loud posture unchanged (empty/wrong-length KEK, missing env,
  unknown driver all crash assembly with the offending field named).
- **`events.OpenWith` variant:** `events.OpenWith(ctx, cfg config.EventsConfig, r audit.Redactor, deps Deps) (EventBus, error)`
  with `Deps{State state.StateStore}` — so the durable driver
  (`drivers/durable/durable.go:119-189`, whose `New` already accepts a store) can
  share the runtime's StateStore through the factory path instead of cmd-only direct
  construction (`events/registry.go:18`'s `Factory` has no deps today). Existing
  `Open` keeps its signature and behaviour; drivers that ignore deps are unchanged.
- **Both callers become thin wrappers (§13 consumer, same phase):**
  `bootDevStack` = `Assemble` + the cmd-only legs (listener/mux/Console/dev-session);
  `devstack.Assemble(t, ...)` = `assemble.Assemble` + `t.Fatal` on error +
  test-fixture conveniences. `tryAssemble` and the duplicated fan-out bodies are
  deleted; the D-094 mirror is reduced to "thin caller", its source-of-truth comments
  retired.
- **The honest headless recipe:** `docs/recipes/embed-harbor-headless.md` —
  `config.Defaults()` → `ValidateCore` → `internal/drivers/prod` import →
  `assemble.Assemble` → run a task via the composed stack → `Close`. Acceptance-gated:
  the recipe's code path is exercised by an integration test, so it cannot ship as
  fiction (the audit's closing line for this seam).

## Non-goals

- **No external-module facade.** `internal/runtime/assemble` is module-internal; the
  recipe documents in-module embedding (cmd/harbortest/examples). Making `Assemble`
  externally importable is the audit's Wave D (RFC-level), explicitly out of scope.
- **No run-loop-driver redesign.** The per-task run-loop driver is wired by the
  assembly as-is; eager push wake-on-resolution and other steering-runloop follow-ups
  stay follow-ups.
- **No new subsystem wiring** beyond what cmd already does (the Wave C zero-consumer
  primitives — telemetry.New, governance enforcement, durable pauses — are NOT
  silently added here; the assembly reproduces today's production wiring exactly).
- No Protocol surface change; no config schema change.

## Acceptance criteria

- [ ] `assemble.Assemble(ctx, cfg, opts)` exists in `internal/runtime/assemble`
      (error-returning, no `testing` import — grep-asserted); ordering + closer chain
      + partial-failure semantics behaviour-match `bootDevStack` (golden boot test on
      an examples-shaped config; forced mid-assembly failure closes everything already
      opened, goroutine baseline restored).
- [ ] **§13 consumer in the same phase:** `bootDevStack` and `devstack.Assemble` are
      thin wrappers over `assemble.Assemble`; `tryAssemble` and both duplicated
      fan-out bodies are deleted — grep-asserted; the binary boots and all prior
      smokes pass (preflight green).
- [ ] The exported MCP attach helper lands next to the driver including the
      `ToolPolicy` projection; devstack's converted path now applies policy projection
      (regression test pins a policy-carrying `config.MCPServerConfig` →
      policy-resolved descriptors on the catalog — the silent drop is closed).
- [ ] `auth.BuildProviders` exported; cmd's `applyToolCatalogWiring` reduces to a thin
      call; fail-loud KEK/env/driver errors preserved (unit-tested).
- [ ] `events.OpenWith(ctx, cfg, redactor, Deps{State})` exists; a durable-driver
      config assembled via `Assemble` shares the runtime's StateStore (integration:
      events survive a bus reopen against the same store); plain `Open` untouched.
- [ ] `docs/recipes/embed-harbor-headless.md` ships and its end-to-end path
      (Defaults → ValidateCore → prod import → Assemble → one task → Close) is
      exercised by `test/integration/` — the recipe compiles against reality, not
      aspiration.
- [ ] Concurrent-use: N≥10 concurrent `Assemble`+`Close` cycles under `-race` (driver
      registries are shared process state) — no cross-stack bleed, no goroutine leak;
      plus N≥100 concurrent task-runs against ONE assembled stack (the stack is the
      compiled artifact; D-025).
- [ ] All prior phase smokes + integration tests pass against the converted binary.

## Files added or changed

- `internal/runtime/assemble/assemble.go` (+ `assemble_test.go`) — `Assemble`,
  `Stack`, `Options`, closer chain (new package under the §3 `internal/runtime/`
  ellipsis).
- `internal/tools/drivers/mcp/attach.go` (+ `_test.go`) — the attach helper + policy
  projection (file placement per the driver package's conventions).
- `internal/tools/auth/build_providers.go` (+ `_test.go`) — `BuildProviders`.
- `internal/events/registry.go` (+ tests) — `OpenWith` + `Deps`.
- `cmd/harbor/cmd_dev.go` — `bootDevStack` thin-wrapper conversion; `attachDevMCPServer`
  / `projectMCPToolPolicies` / `applyToolCatalogWiring` bodies deleted.
- `harbortest/devstack/devstack.go` — `tryAssemble` deleted; `Assemble(t, ...)` wraps
  `assemble.Assemble`; MCP policy-projection drift fixed by conversion.
- `docs/recipes/embed-harbor-headless.md` — the acceptance-gated recipe.
- `docs/recipes/README.md` — index entry.
- `test/integration/phase110d_assemble_test.go` — the recipe-path E2E + concurrency
  stress.
- `scripts/smoke/phase-110d.sh` — assertions below.
- `docs/glossary.md` — "assembly entry point (`assemble.Assemble`)".
- `docs/decisions.md` — D-197 (authored at ship time).

## Public API surface

- `assemble.Assemble(ctx context.Context, cfg *config.Config, opts Options) (*Stack, error)`
- `assemble.Stack` (composed subsystems + `Close(ctx) error`) + `assemble.Options`
- `mcpdrv.Attach(ctx, config.MCPServerConfig, tools.ToolCatalog, *Registry, events.EventBus, *slog.Logger, *[]func(context.Context) error) error`
  (shape refinable per §4.3)
- `auth.BuildProviders(ctx, config.ToolsConfig, Deps) (map[string]*toolapproval.ApprovalGate, map[string]toolauth.OAuthProvider, error)`
- `events.OpenWith(ctx, config.EventsConfig, audit.Redactor, Deps) (EventBus, error)`

> Scope note: "public" here is module-internal — `internal/` packages are not
> importable by external modules (the recorded reason `harbortest/` lives at the
> top level). This surface is stable for in-module consumers (cmd, harbortest,
> examples); external-team embedding needs a future facade/export RFC (the audit's
> Wave D — for which this phase is the named prerequisite: "you cannot facade what
> currently lives in a binary").

### SDK-consumer reachability

This is the capstone of the band's lens: after 110a–c a consumer can build every
subsystem — but the ~700-line dependency-ordered composition (with closers,
partial-failure cleanup, MCP attach, OAuth assembly, durable-bus/StateStore sharing)
still exists only as `package main` prose and a `*testing.T` fixture. After 110d,
`assemble.Assemble(ctx, cfg, opts)` is the one call that turns a validated config into
a running stack, and `embed-harbor-headless.md` documents it honestly because an
integration test executes it. The audit's external-surface verdict ("reachable
headless: no") flips to yes for in-module consumers; Wave D inherits a promotable
entry point instead of a binary.

## Test plan

- **Unit:** assembly ordering + closer-reversal; partial-failure cleanup per stage
  (table-driven forced failures); `BuildProviders` fail-loud matrix (KEK length, env
  missing, unknown driver); MCP policy projection golden (config → ToolPolicy,
  per-tool overrides, retry-class mapping); `OpenWith` deps plumbing (durable gets the
  store; inmem ignores it).
- **Integration:** `test/integration/phase110d_assemble_test.go` — the recipe path
  end-to-end on real drivers (inmem/sqlite): Defaults → ValidateCore → Assemble → run
  one task through the composed planner/runloop/executor → assert the answer envelope
  → Close; identity propagation asserted at the store + event layers; failure modes:
  (a) mid-assembly forced failure cleans up, (b) durable-events config without the
  shared store behaves per the documented loud-degradation contract. Devstack-vs-cmd
  parity: both wrappers produce stacks with identical wiring posture (probe-based
  assertions). `-race` throughout.
- **Conformance:** N/A — no new driver registry; `OpenWith` reuses the existing events
  conformance posture.
- **Concurrency / leak:** N≥10 concurrent Assemble/Close cycles (shared registries) +
  N≥100 concurrent runs against one assembled `Stack` under `-race`; goroutine
  baseline restored after `Close` (the long-lived-component leak rule).

## Smoke script additions

`scripts/smoke/phase-110d.sh` (static-only): assert
`internal/runtime/assemble/assemble.go` + `docs/recipes/embed-harbor-headless.md`
exist; grep-assert `assemble` does not import `testing`; grep-assert `devstack.go` no
longer defines `tryAssemble` and `cmd_dev.go` no longer defines `attachDevMCPServer` /
`applyToolCatalogWiring`; run
`go test ./internal/runtime/assemble/ ./internal/tools/auth/ ./internal/events/ -run 'Assemble|BuildProviders|OpenWith' -race -count=1`.
Skeleton ships with this plan (standard skip until the phase implements).

## Coverage target

- `internal/runtime/assemble`: 80% (assembly wiring; every error leg exercised by the
  forced-failure table).
- `internal/tools/auth` (new file): 90%; `internal/tools/drivers/mcp` (new file): 85%;
  `internal/events` additions: meets the package's existing target.

## Dependencies

- **110a** (the executor + envelope the assembly wires), **110b** (the runctx
  helpers and emitter constructors its run-loop wiring consumes), **110c** (every `FromConfig`
  projection, `Defaults`, `ValidateCore`, and the `internal/drivers/prod` aggregator
  the recipe imports). Stage 2 — dispatches only after Stage 1 (110a ∥ 110c) merges.
- 64/64a (bootDevStack + catalog wiring), 83g (MCP attach), 30/31 (OAuth + approval
  assembly), 57 (durable event log), 05 (events factory).

## Risks / open questions

- **Merge coordination (staging).** 110d runs in **Stage 2, in parallel with 110b**;
  both touch `cmd_dev.go` + `devstack.go`, and 110d's run-loop wiring consumes 110b's
  constructors — the coordinator merges 110b first or drains the overlap (the seams
  are adjacent but distinct: 110b owns the per-run closures, 110d owns the boot
  fan-out).
- **Reconciling two drifted orderings.** Where `tryAssemble` and `bootDevStack`
  disagree (the MCP policy drop is the known case; others may surface), production
  wins and D-197 records each reconciliation. Unknown drift is the phase's main
  discovery risk — budget for §17.6 fix-both-sides findings.
- **Options-surface creep.** `assemble.Options` must stay the union of what the two
  real callers need today, not a speculative embedder wishlist (§4.3 deviations
  allowed; additions need a consumer in the same PR per §13).
- **Recipe honesty.** The recipe is acceptance-gated by an integration test precisely
  because doc-only recipes drift; if the test cannot express a step, the step does not
  ship in the recipe.
- **`Factory` signature pressure.** `OpenWith` deliberately adds a parallel entry
  point rather than breaking the registered `Factory` signature; if a third deps-aware
  driver appears post-V1, revisiting the Factory shape is an RFC-level follow-up, not
  this phase.

## Glossary additions

- **Assembly entry point (`assemble.Assemble`)** — the exported, error-returning
  config→stack fan-out in `internal/runtime/assemble` that composes stores, bus, LLM,
  memory, skills, tasks, catalog, pause coordinator, and planner from a validated
  `*config.Config`, with reverse-order closers and partial-failure cleanup.
  `cmd/harbor`'s `bootDevStack` and `harbortest/devstack.Assemble` are its two thin
  wrappers (D-094 reduced to thin callers). Add to `docs/glossary.md`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — the
      integration E2E asserts identity propagation through the assembled stack.
- [ ] **Concurrent-reuse test passes (D-025)** — the assembled `Stack` is the compiled
      artifact; N≥100 concurrent runs against one stack + N≥10 Assemble/Close cycles
      under `-race`, goroutine baseline restored.
- [ ] **Integration test (§17):** the recipe-path E2E with real drivers, identity
      propagation, ≥2 failure modes, under `-race`.
- [ ] Glossary updated (assembly entry point)
- [ ] If a brief finding was departed from: N/A — none departed.
