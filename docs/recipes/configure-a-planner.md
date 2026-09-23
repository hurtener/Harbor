# Recipe: select and configure a planner

The planner owns reasoning policy; the Runtime owns mechanism (events,
tasks, tools, memory, artifacts, pause/resume). The contract is one
`Planner` interface and the concrete planner is swappable — selected
by the `planner` config block (D-103).

## Steps

1. **Choose the driver in `harbor.yaml`.** V1 ships the `react`
   driver — the LLM-driven ReAct reference planner (Phase 45, D-051):

   ```yaml
   planner:
     driver: react
     # max_steps overrides the driver-side circuit-breaker step cap.
     # Zero (the default) uses the driver's internal default —
     # react.DefaultMaxSteps (12) for the V1 reference driver.
     # max_steps: 12
   ```

2. **Tune the step cap.** `max_steps` is the per-run circuit breaker:
   raise it for longer trajectories, lower it to fail fast in tests.
   Leaving it at `0` inherits `react.DefaultMaxSteps`.

3. **The ReAct planner needs an LLM provider.** It calls the LLM
   client, so the `llm` block must name a real provider — see
   [Run the local dev loop](run-harbor-dev.md) for the
   provider/API-key wiring. A missing provider fails loudly at boot
   (CLAUDE.md §13 — no silent stub fallback).

## Working-input budget + trajectory compression

Long-running agents accumulate trajectory — every step's action and
observation contributes to context. `memory.budget_tokens` sets the soft
working-input target for the complete assembled request. The runtime can
repeatedly summarize eligible older exchanges while preserving the prior
checkpoint and recent results. Model output limits are separate.

```yaml
memory:
  strategy: rolling_summary
  budget_tokens: 8000
```

The contract:

- With `rolling_summary`, **zero derives the target from the effective model**,
  including its input capacity and output reservation. A positive target also
  enables within-run compaction for stateless agents.
- Compaction repeats as needed, never treating old tool evidence as new actions.
  Fresh results stay available to the next decision; an impossible final request
  fails admission rather than silently losing evidence.
- `planner.token_budget` was removed. The loader rejects it with migration
  guidance; there is no alias. The remaining cumulative-memory owner/activation
  migration is tracked in [PR #779's tracker](../notes/portable-context-tracker.md).
- Compression is observable: `trajectory.compressed` /
  `trajectory.compression_failed` ride the canonical event stream
  under the run's identity quadruple; a summariser failure fails the
  run loudly, never a silent fall-through to raw history.

### Headless (no config file)

Every piece is independently constructible — the YAML knob is a thin
carrier over the programmatic surface:

```go
summ, err := summarizer.NewTrajectorySummariser(llmClient,
    summarizer.WithTrajectoryModel("cheap-compactor-model"), // optional
)
if err != nil { /* handle */ }
runner := planner.NewCompressionRunner(summ)

spec := steering.RunSpec{
    Planner:     plnr,
    Compression: runner, // nil = compression off
    Base: planner.RunContext{
        Quadruple:  q,
        Goal:       goal,
        Trajectory: traj,
        Budget:     planner.Budget{TokenBudget: 8000}, // input target, NOT output tokens
    },
}
fin, err := runLoop.Run(ctx, spec)
```

`Budget.TokenBudget` is a per-run option on `RunContext`, never
planner state — the same `CompressionRunner` + `TrajectorySummariser`
pair is a shared compiled artifact safe across N concurrent runs
(D-025).

Zero uses model capacity for request-aware planners. A standalone planner with
no assembled-request preparation must supply a positive trajectory target.

## Adding a new planner driver

Future planners (Plan-Execute, Workflow, Graph, Deterministic,
Supervisor, MultiAgent, HumanApproval per RFC §6.2) follow the §4.4
extensibility-seam pattern:

1. Implement the `Planner` interface (in-tree concretes live under
   `internal/planner/<name>/`; an EXTERNAL module implements the
   interface via `sdk/planner` — the swappable-planner seam is
   deliberately public, D-205).
2. Self-register from the package's `init()` — in-tree via the
   internal registry; externally via `sdk/planner`'s `Register` /
   `MustRegister`.
3. Make the registration reachable: in-tree concretes add their blank
   import to the production aggregator (§4.4 / D-196); an external
   embedder blank-imports its own planner package in its binary.
4. Flip `planner.driver` in `harbor.yaml` to `<name>` to opt in.

The factory's error message lists the registered drivers, so a
misconfigured `driver:` name is obvious at boot.

## Notes

- Planner state is per-session — sharing a planner instance across
  sessions is a bug (CLAUDE.md §6 rule 7). The Runtime constructs
  per-session planner state for you; you do not wire this by hand.
- The deterministic planner (`sdk/planner/deterministic`) is Harbor's
  second `Planner` concrete — a scripted, LLM-free planner that drives
  ordered steps through the identical `Planner` interface. It anchors
  the planner conformance suite; V1 wires only `react` as a selectable
  `planner.driver` value.
