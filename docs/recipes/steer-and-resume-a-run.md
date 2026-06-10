# Steer and resume a run

How to work with Harbor's unified pause/resume primitive
(`internal/runtime/pauseresume`, RFC §3.3 + §6.3) from a headless Go
program. HITL approval, tool-side OAuth, A2A `AUTH_REQUIRED`, and
operator PAUSE all converge on the ONE `Coordinator` — there are no
parallel pause implementations.

> Phase note: the steering-control half of this recipe (sending
> RESUME / APPROVE / REJECT / INJECT_CONTEXT controls into a live run)
> is owned by Phase 111b and lands with it. This page currently
> documents the **durability + expiry** half (Phase 111c, D-200).

## Durable pauses + the pause lifecycle (Phase 111c)

### Construct a durable Coordinator

Durability rides on the runtime's existing `state.StateStore` (the
§4.4 persistence seam — D-067; no parallel checkpoint driver). Hand
the store to the Coordinator and every pause record — including the
run's serialized trajectory — is checkpointed:

```go
store, err := state.Open(ctx, config.StateConfig{Driver: "sqlite", DSN: "/var/lib/harbor/state.sqlite"})
if err != nil { /* fail loud */ }

coord := pauseresume.New(
    pauseresume.WithBus(bus),                       // pause.requested / pause.resumed on the canonical stream
    pauseresume.WithCheckpointStore(store),         // pauses survive a Runtime restart
    pauseresume.WithMaxParkDuration(24*time.Hour),  // 0 = pauses never expire (the default)
)
```

The assembled runtime (`assemble.Assemble`) wires all three for you
from `harbor.yaml` — the snippet above is the headless-SDK shape.

### What survives a restart

A fresh Coordinator over the **same** store rehydrates a parked pause
on demand: `Status(token)` reports it paused, `Resume(token, ...)`
re-attaches tool-context handles, restores the serializable half, and
clears the checkpoint. Resume is **destructive**: a resumed pause's
checkpoint is deleted, and a later Coordinator sees
`ErrPauseNotFound` — a resumed pause is terminal, not history (use
`events.subscribe` on `pause.resumed` for history).

What fails loud (never silently degrades):

- **`trajectory.ErrUnserializable`** at `Request` time — a pause whose
  trajectory or payload carries a non-JSON-encodable leaf is rejected
  before anything is recorded. No half-persisted checkpoint.
- **`trajectory.ErrToolContextLost`** at `Resume` time — the handle
  registry is process-local at V1 (RFC §6.3); a resume that needs a
  handle the restarted process never re-registered fails loud rather
  than resuming with a nil tool context.

### Who reaps abandoned pauses

Without a ceiling, a pause nobody answers (or a run cancelled while
paused) parks forever. `WithMaxParkDuration` + the pause sweeper give
the lifecycle an end:

```go
go func() {
    // Blocks until ctx is cancelled. Cancel + join on shutdown.
    err := pauseresume.RunSweeper(ctx, coord,
        pauseresume.WithSweepInterval(time.Minute))
    if err != nil && !errors.Is(err, context.Canceled) { /* loud */ }
}()
```

Every sweep pass resumes each pause past `PausedAt + max_park_duration`
with the typed **`timeout`** Decision (D-096): the `pause.resumed`
event carries `decision: timeout`, the checkpoint is deleted, and the
waiting run terminates as a **constraints-conflict** — a deadline the
human missed is a constraint the planner cannot resolve. Never a
silent unpark-and-continue.

In the binary, both knobs come from `harbor.yaml` and the assembly
starts the sweeper for you:

```yaml
pauseresume:
  max_park_duration: 24h # 0 = never expire (default; no sweeper)
  sweep_interval: 1m     # must be <= max_park_duration when set
```

`RunSweeper` fails loud (`ErrSweeperMisconfigured`) against a
Coordinator with no max-park duration — a sweeper that silently reaps
nothing forever is the failure mode the error closes.
