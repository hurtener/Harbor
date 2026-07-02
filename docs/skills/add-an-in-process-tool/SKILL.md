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

A tool is a plain Go function over typed input/output structs — you almost never implement the `Tool` interface by hand. `inproc.RegisterFunc[Args, Result]` derives the planner-visible JSON Schemas from your structs by reflection and wraps the function in the `ToolPolicy` reliability shell (timeout + retry + validation). The import paths are the public `sdk/` facade (RFC §3.6) — the same paths `harbor scaffold` emits, compiling from an external module:

```go
package tools

import (
    "context"
    "fmt"
)

type WeatherArgs struct {
    City string `json:"city"`
    Unit string `json:"unit,omitempty"`
}

type WeatherResult struct {
    TemperatureC float64 `json:"temperature_c"`
    Description  string  `json:"description"`
}

func WeatherGetCurrent(ctx context.Context, in WeatherArgs) (WeatherResult, error) {
    if err := ctx.Err(); err != nil {
        return WeatherResult{}, fmt.Errorf("weather.get_current: %w", err)
    }
    // ... fetch from your domain API ...
    return WeatherResult{TemperatureC: 21.3, Description: "Partly cloudy"}, nil
}
```

Three things to notice:

1. **`ctx` is mandatory and first.** Use it for cancellation; pass it to every downstream I/O call. Never store it; never call `context.Background()` inside the handler (CLAUDE.md §5 "Context").
2. **Identity and the bus ride on `ctx`.** The runtime stamps the run's `(tenant, user, session)` quadruple and the event bus onto the invocation context — read them via `sdk/identity`'s `MustQuadrupleFrom(ctx)` and `sdk/events`' `MustFrom(ctx)` when the tool needs them. NEVER pull identity from package-level state.
3. **`Args` / `Result` are real Go structs.** The `json` tags drive the reflection-derived schema; the planner sees a typed surface, not a free-form map. No `interface{}` smuggling.

## 2. Register the tool with the catalog

In your scaffolded `agent.go` (the `RegisterTools` function `harbor scaffold` generates is exactly this shape):

```go
import (
    "github.com/hurtener/Harbor/sdk/tools"
    "github.com/hurtener/Harbor/sdk/tools/inproc"
)

func RegisterTools(cat tools.ToolCatalog) error {
    return inproc.RegisterFunc[WeatherArgs, WeatherResult](
        cat,
        "weather.get_current",
        WeatherGetCurrent,
        tools.WithDescription("Return the current temperature + a short description for a city."),
        tools.WithSideEffect(tools.SideEffectExternal),
        tools.WithCostHint("medium"), // surfaces in the planner's tool-selection heuristics
    )
}
```

The catalog is the planner's tool index. Registration validates at boot — a duplicate name or a schema-underivable type fails LOUDLY (`ErrToolDuplicateName` / `ErrSchemaBuild`), never silently.

### Always-loaded vs deferred — picking a `loading_mode` (Phase 107c)

After 107c the React planner runs on native provider tool-calling and the operator gets a per-tool knob: should this tool appear in the LLM's catalog EVERY turn (`always`) or stay hidden until the LLM searches for it (`deferred`)?

- **`always` (default)** — the tool's `{name, description, args_schema}` lands in `req.Tools[]` on every turn. Best for high-value, frequently-used tools (your domain APIs, the everyday operations the agent is built around).
- **`deferred`** — the tool is absent from `req.Tools[]` until the LLM finds it via the `tool_search` built-in meta-tool. Once discovered, the planner appends the name to `RunContext.DiscoveredTools` and the tool joins the NEXT turn's declaration. Best for large catalogs (50+ tools) where rendering every schema each turn blows the prompt budget — typically MCP-server-imported tools, niche utilities, and the long tail.

Opt in via `harbor.yaml`:

```yaml
tools:
  entries:
    - name: weather.get_current
      loading_mode: always       # the default — explicit here for clarity
    - name: niche.compute_orbital_elements
      loading_mode: deferred     # only loaded when tool_search surfaces it

  built_in:
    - tool_search                # the LLM's discovery surface for deferred tools
    - tool_get                   # full schema for one named tool
    - artifact_fetch             # recovery path for heavy outputs above the threshold
```

The two-turn rule is structural: turn N the LLM calls `tool_search`, turn N+1 the planner has appended the discovered tool to `Tools[]` and the LLM can call it. Same-turn race (search + call in one response) is naturally guarded by the AC-19 serialisation fallback — only the head of N>1 ToolCalls dispatches per turn.

Operators who don't care about prompt-budget pressure leave every tool at the default `always` and never see the difference. Operators with sprawling catalogs flip the long tail to `deferred` and the LLM finds them on demand.

## 3. The concurrency contract — non-negotiable (D-025)

In-process tools are compiled artifacts: built once, called many times, **across many concurrent runs**. They MUST be safe for concurrent reuse:

- **No package-level mutable state behind the handler.** A counter is fine if it's `atomic.Int64`; a `map[string]X` is a bug unless behind a mutex with documented invariants.
- **Per-run state lives in `ctx` and the arguments, never in the handler's closure.** A `lastCity` variable the handler reads while run B's request lands is a context-bleed bug.
- **Cancelling run A's `ctx` MUST NOT affect run B.** Use `ctx` for cancellation, not a shared context.

Every tool that ships gets a concurrent-reuse test:

```go
func TestWeatherTool_ConcurrentReuse_NoCrossTalk(t *testing.T) {
    cat := tools.NewCatalog()
    if err := RegisterTools(cat); err != nil {
        t.Fatal(err)
    }
    desc, ok := cat.Resolve("weather.get_current")
    if !ok {
        t.Fatal("weather.get_current not registered")
    }
    const N = 100
    var wg sync.WaitGroup
    wg.Add(N)
    for i := 0; i < N; i++ {
        go func(i int) {
            defer wg.Done()
            args := []byte(fmt.Sprintf(`{"city":"City-%d"}`, i))
            res, err := desc.Invoke(context.Background(), args)
            // ... assert the per-i city round-trips in res, no cross-talk ...
            _ = res
            _ = err
        }(i)
    }
    wg.Wait()
}
```

Run with `go test -race`. The race detector + the per-run identity assertion is what makes the test load-bearing.

**Your tool can be invoked in parallel WITHIN a single turn (Phase 107d).** The LLM may call several tools at once; with `planner.parallel_tool_calls: true` (the default) the runtime dispatches those branches concurrently against the *same* shared catalog. The concurrent-reuse contract above is exactly what makes this safe — two branches of one turn are no different from two separate runs. Set `planner.parallel_tool_calls: false` to fall back to one-tool-call-per-step if you need strictly serial dispatch.

## 4. Heavy outputs — the artifact-stub seam

A raw heavy payload (>32KB by default — `artifacts.heavy_output_threshold_bytes`) must never reach the LLM context window. Harbor enforces this at the LLM edge: raw heavy content that is not already an `ArtifactStub` fires `ErrContextLeak` and emits a `llm.context_leak` event (RFC §6.5, D-026).

For tool results, you don't wire this by hand — the runtime's executor materialises any above-threshold result to the artifact store automatically and hands the LLM a stub-shaped observation instead. Your tool just returns its typed value; design the `Result` struct so the LLM-relevant part is small (a summary, a count, a key finding) even when the underlying payload is large.

The Console's chat panel renders artifact stubs as clickable links; the planner sees `{ "ref": "art-abc123", "mime": "application/pdf", "size": 142853 }` and can pull only the parts it needs via a subsequent `artifact_fetch` call.

### What the LLM sees when a tool result exceeds the threshold

Tool results above the threshold are materialised to the artifact store automatically by the runtime; the LLM-facing observation becomes the head bytes (a short preview) plus a positional footer that names the `artifact_fetch` built-in and the ref. The full bytes stay in the artifact store under the run's `(tenant, user, session)` scope. Operators who want the LLM to be able to pull the full payload on demand should opt the `artifact_fetch` built-in into their agent yaml:

```yaml
tools:
  built_in:
    - clock.now
    - text.echo
    - artifact_fetch   # always-loaded; lets the LLM recover full payloads above the threshold
```

`artifact_fetch` takes `{ref: string, max_bytes?: int}` (default 64 KiB, hard cap 1 MiB) and returns `{ref, mime, size_bytes, content, truncated}`. Cross-tenant reads are rejected by the artifact store — the meta-tool surfaces a soft "not found" error without exposing the bytes (the `internal/tools/builtin/artifact_fetch_test.go::TestArtifactFetch_CrossIdentity_RejectedByStore` test is the regression gate).

If your tool's results are typically small (well under the threshold), no action is needed — the materialiser only fires above the cap, and the planner sees the raw result inline as usual.

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

## 6. Tuning retry / timeout for MCP tools — `policy:` and `tool_policies:` (Phase 26b)

In-process tools set their reliability shell programmatically with `tools.WithPolicy(...)` at registration. Tools imported from an **MCP server** have no Go call site you own, so Harbor exposes the same `tools.ToolPolicy` as operator YAML on each `tools.mcp_servers[]` entry:

```yaml
tools:
  mcp_servers:
    - name: youtube
      transport_mode: streamable_http
      url: https://example.com/mcp/youtube
      # Per-server default applied to EVERY tool this server registers.
      policy:
        max_attempts: 3            # TOTAL attempts incl. the first (NOT retries)
        timeout_ms: 10000          # per-attempt deadline
        retry_on: [transient, timeout, 5xx]
      # Per-tool overrides keyed by the MCP server-side tool name
      # (`get_metadata`, NOT the `youtube_get_metadata` Harbor name).
      tool_policies:
        get_metadata:
          max_attempts: 1          # one attempt, no retry
          timeout_ms: 60000        # a slow call gets one long deadline
        search:
          max_attempts: 6          # a flaky call gets more retries
```

Two semantics that trip people up:

- **`max_attempts` is the TOTAL attempt count, including the first** — not the retry count. `max_attempts: 1` means a single attempt with no retry; the package default is `4` (one call + three retries). It projects internally to `tools.ToolPolicy.MaxRetries = max_attempts - 1`.
- **Per-FIELD fall-through.** A field you omit inherits the package default for *that field only* — it does not reset the whole policy. A `policy:` block that sets only `timeout_ms: 5000` still keeps the default 4 attempts. This mirrors `tools.ToolPolicy`'s own zero-value resolution, so a partial policy is never surprising. Omit the entire `policy:` / `tool_policies:` blocks to keep today's behaviour (30 s per-attempt deadline, 4 total attempts).

An http(s) MCP server can also bind a declared `tools.oauth_providers[]` entry by name — `oauth_provider: <name>` on the `mcp_servers[]` entry — so every identity-stamped per-call RPC injects a fresh per-identity `Authorization: Bearer` for the calling user (a token-fetch failure fails the call loud, never an unauthenticated fallback). A static `meta_annotations` map rides into the MCP `_meta` alongside the isolation triple for server-side attribution. See `docs/CONFIG.md` › `tools.mcp_servers[].oauth_provider` for the full field surface and validation rules.

A tool named in `tool_policies` uses its override; tools absent from the map fall back to `policy` (or, if `policy` is omitted too, the package default). `retry_on` values must be one of `transient` / `timeout` / `5xx` / `permanent`; an unknown class fails config validation at boot. Resources and prompts a server exposes always use the per-server `policy` (the per-tool override is for tools). The whole block is restart-required.

## Common failure modes

- **`RegisterFunc` fails at boot with `ErrToolDuplicateName`.** Two tools registered under the same name. Names are the planner's only handle; keep them unique.
- **`RegisterFunc` fails with `ErrSchemaBuild` / `ErrUnsupportedType`.** Your `Args` / `Result` struct carries a field the reflection-based schema deriver cannot express (a channel, a func, an `any`-typed map). Stick to JSON-representable primitives, slices, and nested structs.
- **The concurrent-reuse test fails with the race detector tripping.** Almost always shared mutable state behind the handler. Audit for non-`atomic` counters, unprotected maps, package-level globals. See CLAUDE.md §5 "Concurrent reuse contract".
- **The planner doesn't pick the tool.** Either the description is too vague (write what the tool DOES, with concrete inputs the planner can pattern-match) or the planner's max_steps is too low to reach the relevant turn. Tune `planner.max_steps`.

## See also

- [`define-the-agent-yaml`](../define-the-agent-yaml/SKILL.md) — `tools.built_in` opts into harbor-shipped tools alongside your in-process catalog.
- [`drive-the-playground`](../drive-the-playground/SKILL.md) — exercise the tool against a real planner from the chat UI.
- [`observe-with-the-console`](../observe-with-the-console/SKILL.md) — the Task / Events / Tools pages show tool invocations live.
- Reference projects: `examples/tools/` in the Harbor repo (in-proc + HTTP + MCP + A2A examples).
