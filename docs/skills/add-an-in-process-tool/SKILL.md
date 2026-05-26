---
name: add-an-in-process-tool
description: "Author a typed Go tool that the planner calls in-process. Use when the agent needs to do something Harbor's built-ins don't cover and you don't want an MCP server's process boundary — e.g. a private domain API, a typed CRUD wrapper, a deterministic computation."
license: Apache-2.0
metadata:
  framework: harbor
  surface: tools
  verbs: ""
---

# Add an in-process tool

Harbor's tool surface is transport-agnostic — the planner sees a uniform `Tool` interface regardless of where the tool runs (in-process Go, HTTP, MCP subprocess, A2A peer). In-process tools are the cheapest, lowest-latency option: they run in the same address space as the planner, get a typed Go contract, and avoid serialisation cost. Use them when you control the code and don't need a process boundary.

## 1. The typed-tool contract

A tool is a struct that implements `tools.Tool` — but you almost never write it by hand. Use `tools.NewTyped[Args, Result](spec, handler)` which gives you the type-safe wrapper:

```go
package weather

import (
    "context"

    "github.com/hurtener/Harbor/internal/runtime/runcontext"
    "github.com/hurtener/Harbor/internal/tools"
)

type Args struct {
    City string `json:"city" jsonschema:"description=City name (e.g. Madrid)"`
    Unit string `json:"unit,omitempty" jsonschema:"enum=c,enum=f,default=c"`
}

type Result struct {
    TemperatureC float64 `json:"temperature_c"`
    Description  string  `json:"description"`
}

func New() tools.Tool {
    return tools.NewTyped(
        tools.Spec{
            Name:        "weather.get_current",
            Description: "Return the current temperature + a short description for a city.",
            Cost:        tools.CostMedium, // fast/medium/slow — surfaces in the planner's tool selection heuristics
        },
        handle,
    )
}

func handle(ctx context.Context, rc *runcontext.RunContext, args Args) (Result, error) {
    // ... fetch from your domain API ...
    return Result{TemperatureC: 21.3, Description: "Partly cloudy"}, nil
}
```

Three things to notice:

1. **`ctx` is mandatory and first.** Use it for cancellation; pass it to every downstream I/O call. Never store it; never call `context.Background()` inside the handler (CLAUDE.md §5 "Context").
2. **`*runcontext.RunContext`** carries the identity triple (`tenant_id`, `user_id`, `session_id`), the `run_id`, the audit redactor, the artifact store, and the per-run logger. Read identity via `rc.Identity()`; emit events via `rc.Events()`; persist heavy outputs via `rc.Artifacts()`. NEVER pull identity from package-level state.
3. **`Args` / `Result` are real Go structs.** The `jsonschema` tags generate the planner-visible JSON schema; the planner sees a typed surface, not a free-form map. No `interface{}` smuggling.

## 2. Register the tool with the catalog

In your scaffolded `main.go`:

```go
import (
    "github.com/your-org/my-agent/tools/weather"
    // ... other imports
)

func main() {
    cat := tools.NewCatalog()
    cat.Register(weather.New())
    cat.Register(currency.New())
    // ... other tools

    rt, err := runtime.New(cfg, runtime.WithToolCatalog(cat))
    // ...
}
```

The catalog is the planner's tool index. `Register` validates the spec (unique name, valid schema, sensible cost) at boot — a duplicate name or a broken `jsonschema` tag fails LOUDLY at startup.

## 3. The concurrency contract — non-negotiable (D-025)

In-process tools are compiled artifacts: built once, called many times, **across many concurrent runs**. They MUST be safe for concurrent reuse:

- **No mutable fields on the tool struct that change post-construction.** A counter is fine if it's `atomic.Int64`; a `map[string]X` is a bug unless behind a mutex with documented invariants.
- **Per-run state lives in `ctx` / `rc`, never on the tool.** A `lastCity` field on the tool reading run A's args while run B's request lands is a context-bleed bug.
- **Cancelling run A's `ctx` MUST NOT affect run B.** Use `ctx` for cancellation, not a tool-level shared context.

Every tool that ships gets a concurrent-reuse test:

```go
func TestWeatherTool_ConcurrentReuse_NoCrossTalk(t *testing.T) {
    tool := weather.New()
    const N = 100
    var wg sync.WaitGroup
    wg.Add(N)
    for i := 0; i < N; i++ {
        go func(i int) {
            defer wg.Done()
            rc := runcontexttest.New(t, identity.Triple{Tenant: "t", User: fmt.Sprintf("u%d", i), Session: "s"})
            args := []byte(fmt.Sprintf(`{"city":"City-%d"}`, i))
            res, err := tool.Invoke(t.Context(), rc, args)
            // ... assert per-i identity preserved in res, no cross-talk ...
        }(i)
    }
    wg.Wait()
}
```

Run with `go test -race`. The race detector + the per-run identity assertion is what makes the test load-bearing.

## 4. Heavy outputs — the artifact-stub seam

A tool that returns >32KB (the default `artifacts.heavy_output_threshold_bytes`) MUST NOT return the raw bytes in `Result`. Doing so leaks into the LLM context window — Harbor's LLM-edge guard fires `ErrContextLeak` and emits a `llm.context_leak` event (RFC §6.5).

Instead, persist the heavy payload via the artifact store and return an `ArtifactStub`:

```go
type Result struct {
    Summary  string             `json:"summary"`         // small; goes to the LLM
    Document tools.ArtifactStub `json:"document"`        // reference; the LLM sees a stub, not the bytes
}

func handle(ctx context.Context, rc *runcontext.RunContext, args Args) (Result, error) {
    raw, err := fetchHugeReport(ctx, args)
    if err != nil {
        return Result{}, err
    }
    stub, err := rc.Artifacts().Put(ctx, raw, "application/pdf")
    if err != nil {
        return Result{}, fmt.Errorf("persist artifact: %w", err)
    }
    return Result{
        Summary:  "12-page macro outlook for Q3 (full PDF in artifact)",
        Document: stub,
    }, nil
}
```

The Console's chat panel renders `ArtifactStub`s as clickable links; the planner sees `{ "ref": "art-abc123", "mime": "application/pdf", "size": 142853 }` and can decide to pull only the parts it needs via a subsequent tool call.

## 5. Errors — fail loudly

Tools wrap downstream errors with context:

```go
if err != nil {
    return Result{}, fmt.Errorf("weather.get_current: fetch %q: %w", args.City, err)
}
```

The wrapped chain shows up in the audit log + the Console's task panel. NEVER silently degrade — no `if err != nil { return Result{}, nil }` patterns (CLAUDE.md §13 "silent degradation"). The planner needs the error to decide whether to retry, replan, or surface to the user.

For domain-validation errors (the city doesn't exist; the unit is invalid), return a sentinel + wrap:

```go
var ErrUnknownCity = errors.New("unknown city")
// ...
return Result{}, fmt.Errorf("weather.get_current: %w", ErrUnknownCity)
```

The planner can `errors.Is(err, weather.ErrUnknownCity)` and choose a graceful fallback path.

## Common failure modes

- **`tools.NewCatalog().Register(...)` panics at boot with "duplicate tool name".** Two tools registered under the same `Spec.Name`. Names are the planner's only handle; keep them unique.
- **The `jsonschema` tag is rejected.** Probably a typo (`enum:c,f` instead of `enum=c,enum=f`) or an unsupported tag combination. The `jsonschema` library's docs at `github.com/invopop/jsonschema` are the canonical reference.
- **The concurrent-reuse test fails with the race detector tripping.** Almost always a mutable field on the tool struct. Audit for non-`atomic` counters, unprotected maps, package-level globals. See CLAUDE.md §5 "Concurrent reuse contract".
- **The planner doesn't pick the tool.** Either the description is too vague (write what the tool DOES, with concrete inputs the planner can pattern-match) or the planner's max_steps is too low to reach the relevant turn. Tune `planner.max_steps`.

## See also

- [`define-the-agent-yaml`](../define-the-agent-yaml/SKILL.md) — `tools.built_in` opts into harbor-shipped tools alongside your in-process catalog.
- [`drive-the-playground`](../drive-the-playground/SKILL.md) — exercise the tool against a real planner from the chat UI.
- [`observe-with-the-console`](../observe-with-the-console/SKILL.md) — the Task / Events / Tools pages show tool invocations live.
- Reference projects: `examples/tools/` in the Harbor repo (in-proc + HTTP + MCP + A2A examples).
