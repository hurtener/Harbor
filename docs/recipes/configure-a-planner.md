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

## Adding a new planner driver

Future planners (Plan-Execute, Workflow, Graph, Deterministic,
Supervisor, MultiAgent, HumanApproval per RFC §6.2) follow the §4.4
extensibility-seam pattern:

1. Implement the `Planner` interface under
   `internal/planner/<name>/`.
2. Self-register from the package's `init()`.
3. Add a blank import (`_ "github.com/hurtener/Harbor/internal/planner/<name>"`)
   in `cmd/harbor/main.go`.
4. Flip `planner.driver` in `harbor.yaml` to `<name>` to opt in.

The factory's error message lists the registered drivers, so a
misconfigured `driver:` name is obvious at boot.

## Notes

- Planner state is per-session — sharing a planner instance across
  sessions is a bug (CLAUDE.md §6 rule 7). The Runtime constructs
  per-session planner state for you; you do not wire this by hand.
- `internal/planner/deterministic` is Harbor's second `Planner`
  concrete — a scripted, LLM-free planner that drives ordered steps
  through the identical `Planner` interface. It anchors the planner
  conformance suite; V1 wires only `react` as a selectable
  `planner.driver` value.
