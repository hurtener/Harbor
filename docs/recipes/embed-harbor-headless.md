# Embed Harbor headless (no `harbor dev`, no Protocol server)

Run a Harbor agent runtime inside your own Go program — no CLI, no
HTTP listener, no Console. One call turns a validated config into a
running stack; you drive a goal through the planner/run-loop and read
the answer.

This recipe is acceptance-gated: its end-to-end path is executed by
`test/integration/phase110d_assemble_test.go`, so every snippet
references real exported symbols (Phase 110d, D-197).

> **Scope note.** This recipe's snippets use the `internal/` paths and
> therefore cover in-module embedding (a binary in this repo, a fork,
> or a vendored tree). The externally importable facade now exists —
> the curated `sdk/` alias tree (RFC §3.6, Phase 112a) re-exports every
> symbol this recipe touches (`sdk/config`, `sdk/assemble`,
> `sdk/drivers/prod`, …); Phase 112b flips this recipe's snippets to
> the public paths. `test/integration/phase112a_sdk_facade_test.go`
> already executes this recipe's path through `sdk/` imports only.

## The pieces

| Step | Symbol | Phase |
|------|--------|-------|
| Driver registrations | `_ "github.com/hurtener/Harbor/internal/drivers/prod"` | 110c (D-196) |
| Config baseline | `config.Defaults()` | 110c (D-196) |
| Headless validation | `cfg.ValidateCore()` | 110c (D-196) |
| The ONE fan-out | `assemble.Assemble(ctx, cfg, opts)` | 110d (D-197) |
| Tool dispatch | `Stack.Executor` (the promoted `dispatch.NewToolExecutor` concrete) | 110a (D-194) |
| The run loop | `Stack.RunLoop.Run(ctx, steering.RunSpec{...})` | 53 / 83i |
| The answer | `planner.AnswerEnvelope` | 110a (D-194) |

## 1. Imports

```go
import (
    "context"
    "fmt"
    "log/slog"

    // The production driver aggregator — the single sanctioned
    // blank-import home (§4.4). Without it every Open fails loud with
    // "unknown driver".
    _ "github.com/hurtener/Harbor/internal/drivers/prod"

    "github.com/hurtener/Harbor/internal/config"
    "github.com/hurtener/Harbor/internal/identity"
    "github.com/hurtener/Harbor/internal/planner"
    "github.com/hurtener/Harbor/internal/runtime/assemble"
    "github.com/hurtener/Harbor/internal/runtime/steering"
    "github.com/hurtener/Harbor/internal/tools"
)
```

## 2. Build and validate the config

`config.Defaults()` is the same baseline `config.Load` applies to a
`harbor.yaml`; a hand-built config therefore behaves exactly like a
loaded one. `ValidateCore()` runs every section validator except the
Protocol-server JWT ceremony a headless embedder never serves.

```go
cfg := config.Defaults()
cfg.LLM.Driver = "bifrost"
cfg.LLM.Provider = "openrouter"
cfg.LLM.Model = "anthropic/claude-sonnet-4"
cfg.LLM.APIKey = "env.OPENROUTER_API_KEY" // env-var indirection — never inline a key

if err := cfg.ValidateCore(); err != nil {
    return fmt.Errorf("config: %w", err)
}
```

Everything else — `state`, `events`, `artifacts`, `tasks`, `memory` —
defaults to the in-memory drivers. Point them at `sqlite` / `postgres`
DSNs for durability; select `events.driver: durable` and the assembly
shares the runtime's StateStore with the durable event log
automatically (`events.OpenWith`, Phase 110d).

## 3. Assemble the stack

One call composes the full dependency-ordered runtime — stores, bus,
LLM, memory, skills, tasks, tool catalog (builtins + OAuth providers +
approval gates + MCP attach), sessions, agent registry, pause
coordinator, planner, run loop — with reverse-order closers and
partial-failure cleanup.

```go
ctx := context.Background()
stack, err := assemble.Assemble(ctx, cfg, assemble.Options{
    Logger: slog.Default(),
})
if err != nil {
    if stack != nil {
        _ = stack.Close(ctx) // drain whatever opened before the failure
    }
    return fmt.Errorf("assemble: %w", err)
}
defer stack.Close(ctx)
```

Register your own in-process tools before assembling via
`assemble.Options.PreRegisterTools`, or after assembling via
`stack.Catalog.Register(...)`.

## 4. Run one goal

Identity is mandatory (§6): every run carries the
`(tenant, user, session)` triple plus a run ID. Drive the shared
`RunLoop` directly — the same loop `harbor dev` drives per spawned
task — with the assembled planner and executor.

```go
q := identity.Quadruple{
    Identity: identity.Identity{
        TenantID:  "acme",
        UserID:    "u-42",
        SessionID: "s-1",
    },
    RunID: "run-1",
}
goal := "Summarise the latest deployment status."
traj := &planner.Trajectory{Query: goal}

fin, err := stack.RunLoop.Run(ctx, steering.RunSpec{
    Planner: stack.Planner,
    Base: planner.RunContext{
        Quadruple:      q,
        Query:          goal,
        Goal:           goal,
        Trajectory:     traj,
        RepairCounters: &planner.RepairCounters{},
        Catalog: tools.NewPlannerView(stack.Catalog, tools.CatalogFilter{
            TenantID:  q.TenantID,
            UserID:    q.UserID,
            SessionID: q.SessionID,
        }),
    },
    ToolExecutor: stack.Executor,
    MaxSteps:     cfg.Planner.MaxSteps,
})
if err != nil {
    return fmt.Errorf("run: %w", err)
}
```

## 5. Read the answer

`planner.AnswerEnvelope` is the canonical answer shape the per-task
run-loop drivers marshal onto `tasks.TaskResult` — build the same
envelope from the terminal `Finish`:

```go
answer := ""
if s, ok := fin.Payload.(string); ok {
    answer = s
} else if m, ok := fin.Payload.(map[string]any); ok {
    if v, ok := m["answer"].(string); ok {
        answer = v
    }
}
envelope := planner.AnswerEnvelope{
    Answer:        answer,
    FinishReason:  string(fin.Reason),
    ToolCallsSeen: len(traj.Steps),
}
fmt.Println(envelope.Answer)
```

`fin.Reason == planner.FinishGoal` is the success case; any other
`FinishReason` maps to a task-error code via
`planner.TaskErrorCodeForFinish`.

## 6. Shut down

`stack.Close(ctx)` runs every subsystem's Close in reverse dependency
order and is idempotent. After Close returns, the stack's goroutines
(notification subscriber, session GC sweeper, metrics bridge) have
drained — the integration test asserts the goroutine baseline is
restored.

## Enforce governance headless

Two paths, depending on how many stacks your process runs:

- **One stack (the common case)**: just populate
  `cfg.Governance.DefaultTier` + `cfg.Governance.IdentityTiers` before
  calling `assemble.Assemble` — the assembly builds the enforcement
  subsystem (MaxTokens → rate limit → cost ceiling) and installs it via
  `governance.SetFactory` before `llm.Open` composes the wrapper chain.
  Configured tiers then reject with `governance.ErrBudgetExceeded` /
  `ErrRateLimited` / `ErrMaxTokensExceeded` and emit the matching
  `governance.*` events on `stack.Bus`. Empty tiers stay fully latent
  (D-044).
- **N stacks with different tier maps**: `SetFactory` is process-global
  (the second caller wins — see its godoc), so skip it and compose per
  stack instead:

```go
sub, err := governance.NewSubsystemFromConfig(
    governance.ConfigFromOperator(cfg.Governance), // or a hand-built governance.Config
    stack.State, stack.Bus)
if err != nil {
    return err // fail-loud: tiers without store/bus are a misconfig
}
if sub != nil { // nil = empty tiers = the sanctioned latent default
    client = governance.Wrap(client, sub) // governance stays outermost (D-043)
}
```

`NewSubsystemFromConfig` + `governance.Wrap` are the documented
multi-runtime path (Phase 111a, D-198): no process-global state, one
Subsystem per stack, accumulator state persisted in that stack's
StateStore.

## Variations

- **Observe events**: subscribe before running —
  `stack.Bus.Subscribe(ctx, events.Filter{Tenant: ..., User: ..., Session: ...})`.
- **Spawn through the task registry** instead of driving the RunLoop
  directly: `stack.Tasks.Spawn(...)` gives you the task FSM +
  `task.spawned` events; you then own the subscriber that calls
  `stack.RunLoop.Run` per task (the shape `cmd/harbor`'s per-task
  driver implements).
- **Skip what you don't need**: `assemble.Options.SkipCatalog` /
  `SkipSteering` / `SkipRunLoop` build partial stacks for harness-style
  embedding (the `harbortest/devstack` kit uses exactly these knobs).
- **Mock LLM for CI smoke**: blank-import
  `internal/llm/mock`, set `cfg.LLM.Driver = "mock"` — dev-only
  (D-089); never in production.
