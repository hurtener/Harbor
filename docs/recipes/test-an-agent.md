# Recipe: test an agent

The public `harbortest` package
(`github.com/hurtener/Harbor/harbortest`) is Harbor's flow-level test
kit. It is importable from outside the module, so end-users test their
agents with the same surface Harbor uses internally.

The full runnable version of this recipe is
[`examples/agents/echo/`](../../examples/agents/echo/).

## The surface

| Function | Purpose |
|----------|---------|
| `RunOnce` | Drive an `Agent` once under a deterministic identity quadruple; returns output + captured `EventLog`. |
| `AssertSequence` | Pin the observed event-type sequence. |
| `AssertNoLeaks` | The cross-session-isolation gate — fails on any event under a foreign `(tenant, user, session)` triple. |
| `SimulateFailure` | Inject N tool failures of a given error class via a `FaultInjector`. |
| `RecordedEvents` | Pull the events recorded for a specific run ID. |

## Steps

1. **Implement the `Agent` interface** on your code path:

   ```go
   import "github.com/hurtener/Harbor/harbortest"

   type EchoAgent struct{}

   // Compile-time assertion — turns interface drift into a build error.
   var _ harbortest.Agent = (*EchoAgent)(nil)

   func (a *EchoAgent) Run(ctx context.Context, input any) (any, error) {
       if err := ctx.Err(); err != nil {
           return nil, fmt.Errorf("echo agent: %w", err)
       }
       return input, nil
   }
   ```

   If your code path is a plain function rather than a method, adapt
   it with `harbortest.AgentFunc`.

2. **Drive it with `RunOnce`:**

   ```go
   func TestEchoAgent_RoundTrips(t *testing.T) {
       out, log, err := harbortest.RunOnce(context.Background(), &EchoAgent{}, "hello")
       if err != nil {
           t.Fatalf("RunOnce: %v", err)
       }
       if out != "hello" {
           t.Errorf("output = %v, want %q", out, "hello")
       }
       harbortest.AssertNoLeaks(t, log)
   }
   ```

   `RunOnce` assembles the identity quadruple, opens an in-memory
   event bus, subscribes, runs the agent, and closes the subscription
   before returning — no goroutine leaks past the call. It is safe to
   call from N concurrent goroutines (D-025).

3. **Always assert `AssertNoLeaks`.** Even for an event-free agent it
   stays green — and turns red the instant a real agent emits an
   event under a foreign triple. This is the multi-isolation gate.

4. **Test failure modes too.** A cancelled context, a forced tool
   failure (`SimulateFailure`) — not just the happy path
   (CLAUDE.md §17.3).

## Notes

- Run tests under the race detector: `go test -race ./...`. CI gates
  on it.
- For a fresh project, `harbor scaffold` generates a worked
  `harbortest`-driven test out of the box — see
  [Scaffold a new agent project](scaffold-an-agent.md).
- The `harbortest` godoc documents the full surface;
  `harbortest/agent_test.go` is the in-tree worked example.
