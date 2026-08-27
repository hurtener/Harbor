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

**Do NOT register built-ins here.** `RegisterTools` is for the tools *your module compiles*. Anything you list under `tools.built_in` in `harbor.yaml` (`clock.now`, `artifact_fetch`, the `skill_*` set, …) is registered **by the runtime** at boot, from config, with its backing stores wired in — registering it in `RegisterTools` too is the duplicate name above, and the boot dies (`duplicate tool name: clock.now`). The yaml entry IS the opt-in; no Go wiring accompanies it.

**The one built-in that looks like a custom tool: `skill_create_draft` (HA-62).** It is a shipped, ordinary runtime tool — you opt into it under `tools.built_in` and register it NEVER, just like the rest of the `skill_*` set. It is deliberately disabled by default (absent from every recommended default, alongside `skill_propose`) and it is NOT a domain tool: its closed input is bounded `{intent, feedback?}`, it runs the governed authoring path's safety-wrapped LLM adapter, and it writes exactly ONE immutable caller-scoped `SKILL.md` **draft** artifact through a narrow write-only seam. The result carries `artifact_ref` + `package_hash`, bounded review metadata, a fixed `provenance` marker, `state: "draft"`, and always `installed: false` — it drafts an immutable proposal with **zero skill-install/config/pack/membership/authority mutation**, and its ONE and only persistence is exactly that immutable caller-scoped ArtifactStore draft: no skill-store upsert, no membership/revision, no operator-pack proposal/publication, no capability registration, no tool exposure, and no approval/OAuth path; declared `required_tools` are metadata only, never grants. Installation of a draft is exclusively the separate import validate/commit workflow. Listing it on an LLM-less runtime fails the boot loud (registration requires the assembly's composed LLM client). If your agent needs that drafting behaviour, it is yaml — not a `RegisterFunc` you author here.

### Serving compiled tools with their declared policy (`sdk/server`)

When you serve your agent from your own binary (`harbor scaffold --with-server`, which reaches the Protocol through `sdk/server` — see [`scaffold-a-harbor-agent`](../scaffold-a-harbor-agent/SKILL.md)), pass this same `RegisterTools` as the server's registrar:

```go
h, err := server.Open(ctx, cfg, server.Options{
    RegisterCatalog: agent.RegisterTools,
    Framework: server.FrameworkIdentity{
        Version: "v1.28.0", // your pinned Harbor product version
        Commit:  "<immutable-harbor-source-commit>",
    },
})
```

`Framework` is release provenance, not host-binary metadata. When set, it
adds `framework_version` / `framework_commit` to `runtime.info`; the existing
`build_*` fields still identify your compiled agent binary. Supply the pair
together from your release configuration. Leave it zero only for a local build
whose immutable Harbor source commit is not known.

`RegisterCatalog` runs at the runtime's **pre-policy catalog seam** — the same point operator YAML tools register — *before* the catalog Builder applies each `tools.entries[]` declaration. So a compiled tool you register here receives the identical declared **approval gate / OAuth binding / reliability policy** an operator's YAML-declared tool gets. Declaring an approval gate for a compiled tool is therefore just a `tools.entries[]` block in `harbor.yaml`:

```yaml
tools:
  entries:
    - name: weather.get_current
      approval:
        policy: deny-all      # every invocation pauses for HITL approval
```

**The trap:** registering a tool any *other* way — after `server.Open` returns, via a post-assembly `Catalog.Register` — skips the wrapping band entirely, so the tool reaches the planner with **no** approval/OAuth/policy shell. Always register through `RegisterCatalog`; a declared-but-unregistered tool fails the boot loud (`ErrToolNotRegistered`), never a silent no-op.

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

A raw heavy payload (>=128KB by default — `artifacts.heavy_output_threshold_bytes`) must never reach the LLM context window. Harbor enforces this at the LLM edge: raw heavy content that is not already an `ArtifactStub` fires `ErrContextLeak` and emits a `llm.context_leak` event (RFC §6.5, D-026).

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

`artifact_fetch` takes `{ref: string, max_bytes?: int, offset?: int}` and returns `{ref, mime, size_bytes, content, offset, returned_bytes, total_size_bytes, truncated}`.

**`artifact_fetch` is a TEXT read, and it refuses rather than corrupting.** Its `content` is a JSON string, and JSON encoding rewrites every invalid UTF-8 byte to `U+FFFD` — so a binary window used to arrive silently corrupted at a length the same response contradicted (an 8-byte PNG header arriving as 10). The built-in now tests the actual window for UTF-8 admissibility and refuses one it cannot return intact. On a refusal it still returns `ref`, `mime`, `size_bytes` and `total_size_bytes` so the model knows what it asked about, leaves `content` empty, and zeroes `offset` / `returned_bytes` / `truncated` (a refusal claiming `truncated: true` would invite endless paging into the same wall). The error names the stored MIME and the failing byte offset, and points at the route that works.

The gate is **content-driven, not MIME-driven** — it inspects the bytes, not the stamp. In practice every binary type refuses (PDF, images, audio, archives), and so does a text artifact carrying one invalid byte mid-window, rather than dropping it silently. **To act on binary bytes, don't send the model through `artifact_fetch`** — declare a `tools.ArtifactRef` parameter on a tool (next section) and let the runtime hand that tool the bytes directly; or, from a Protocol client rather than from inside a run, call `artifacts.get`, whose `content` is `[]byte` (base64 on the wire) and is byte-exact for every MIME at every offset.

**Windowing.** `offset` is a **byte** index, not a line or row index, so the model pages a large file by reading at offset 0 and re-calling with `offset` advanced to the previous `offset + returned_bytes` while `truncated` is `true`. A window can begin and end mid-line; the model splits the text itself. (Row- or schema-addressed windowing is deliberately not offered — a stored MIME is not revisable on a content-addressed store, so keying read behaviour on one would turn a wrong stamp into a permanent refusal.)

Two windowing details follow from the text rule. The returned `offset` reports where the returned content **actually begins**, not what was requested: a window opening mid-rune has its leading continuation bytes trimmed and a truncated trailing rune dropped, and `offset` moves accordingly — so the paging rule above stays correct. And an effective `max_bytes` floors at 4 bytes (the longest UTF-8 rune), so a `max_bytes` of 1–3 is served at 4 rather than trimming to empty while `truncated` stayed true and the paging rule returned the same offset forever.

**Bounds are operator policy.** `max_bytes` defaults to `artifacts.fetch_default_max_bytes` (64 KiB) and is clamped to `artifacts.fetch_hard_max_bytes` (1 MiB); both are `harbor.yaml` keys you can tune — see [`docs/CONFIG.md`](../../CONFIG.md) → `artifacts`. A `max_bytes` above the ceiling is **served at the ceiling**, not refused, and the response says so through `truncated` / `total_size_bytes` / `returned_bytes`. The same knobs bound the `artifacts.get` Protocol method, so a model and a Console client reading one artifact never disagree about what "truncated" means. The ceiling bounds ONE fetch — it is not a budget over repeated fetches; that stays the governance layer's concern.

Cross-tenant reads are rejected by the artifact store — the meta-tool surfaces a soft "not found" error without exposing the bytes (the `internal/tools/builtin/artifact_fetch_test.go::TestArtifactFetch_CrossIdentity_RejectedByStore` test is the regression gate).

**The scope is the session, and that is the whole scope.** The store resolves a read on `(tenant, user, session)` plus the ref id; the producing task is recorded on the artifact as provenance and takes no part in resolution. So a later run in the SAME session can fetch an artifact an earlier run produced — which is what makes the `<session_artifacts>` block honest, since it lists every artifact in the session and tells the model it may fetch any of them. A different session, user or tenant still answers "not found" through the same soft-error path, with no way to tell "not yours" from "does not exist".

If your tool's results are typically small (well under the threshold), no action is needed — the materialiser only fires above the cap, and the planner sees the raw result inline as usual.

### Heavy INPUTS — take content by reference

The seam above governs what comes OUT of a tool. The mirror-image problem is a tool that needs to READ something large: a stored CSV, an uploaded PDF, a prior tool's materialised result. Routing that content through the model's context to get it into your argument struct is exactly the leak `artifact_fetch` exists to bound — and it is unnecessary, because an in-process tool can take the content by reference.

Declare a field of type `tools.ArtifactRef`:

```go
import "github.com/hurtener/Harbor/sdk/tools"

type SummarizeArgs struct {
    Doc      tools.ArtifactRef `json:"doc"`
    MaxWords int               `json:"max_words"`
}

func Summarize(ctx context.Context, in SummarizeArgs) (SummarizeResult, error) {
    body, err := in.Doc.Bytes()   // the stored bytes, never the model's
    if err != nil {
        return SummarizeResult{}, fmt.Errorf("summarize: %w", err)
    }
    ...
}
```

The reflection deriver renders `doc` to the model as a plain **string**, so the model writes `{"doc": "tool_ab12cd34ef56", "max_words": 200}` — an artifact id, exactly the id the runtime already quotes to it in a truncated tool result or the `<session_artifacts>` block. The runtime resolves the id at dispatch, under the run's own `(tenant, user, session)` scope, and hands your function the bytes.

Three properties follow, and they are what makes this worth reaching for:

- **The model never sees the content.** It authored an id and continues to see an id. The resolved value is dispatch-local: it does not enter the argument JSON, the trajectory, the observation the next prompt renders, any event payload, an audit payload, or a log.
- **There is no identity logic in your tool.** The reference resolves under the dispatching run's identity, so your tool reaches what its run reaches and nothing else. A ref from another tenant, user or session answers "not found" through the same soft path `artifact_fetch` uses.
- **It fails loudly.** An unresolvable ref is the step's error (the planner re-plans); reading a reference the argument never carried returns `tools.ErrArtifactRefUnresolved` rather than an empty slice you would measure as an empty artifact.

`ArtifactRef` works anywhere the deriver walks — a bare field, a slice, a map value, a nested struct. A worked example ships at `examples/tools/artifactstats/`.

This is the **in-process** arm: `ArtifactRef` is a Go type, so it belongs to in-process tools only.

#### The MCP arm — the same idea for a remote tool

A tool on the other side of a process boundary cannot be handed a Go value, but it CAN be handed the bytes. For an **MCP** connection you declare the mapping in config instead of in a Go type:

```yaml
tools:
  mcp_servers:
    - name: docstore
      transport_mode: streamable_http
      url: https://docs.internal.example.com/mcp
      artifact_byte_eligible: true          # the containment boundary
      artifact_params:
        ingest_document: [content]          # required parameter
        generate_image: [reference_image_base64_1?, reference_image_base64_2?]
```

The model still writes an artifact id. The runtime resolves it under the run's own `(tenant, user, session)` — the SAME resolver the in-process arm uses, so the reachable artifact set is identical — and writes the resolved bytes into the outbound tool-call body as standard base64. The remote server sees a document; the model never does.

Five things to know before reaching for it:

- **Byte-eligibility is required and is the boundary.** `artifact_params` without `artifact_byte_eligible: true` is refused at the door, not stored inert. Both are `http`-only; a `stdio` connection is refused (base64 bytes belong in an HTTP body, not a stdio frame).
- **The mapped parameter must exist in the server's own schema.** Harbor checks each one against the server's discovered `inputSchema` at attach — it must be declared, and declared string-typed. An absent or non-string parameter fails the attach loudly rather than the first call silently.
- **A mapped parameter is required unless its name ends in `?`.** The marker is stripped before Harbor checks the server schema, so `reference_image_base64_1?` addresses the bare `reference_image_base64_1` property. An optional slot that is absent, JSON `null`, or an empty / whitespace-only string is skipped; a non-empty value still follows the strict resolver path. Required empty ids are refused as `ErrEmptyArtifactID`.
- **Every substitution is recorded.** `mcp.artifact_egressed` carries the artifact id, server, tool, parameter, byte count and a `sha256:` digest — never the bytes — and it is emitted **fail-closed before the wire request**. If the record cannot be written, the call does not happen.
- **There is a ceiling and it refuses rather than truncates.** `tools.mcp_artifact_egress_max_bytes` (default 8 MiB) bounds one substituted value; an oversize artifact fails loud naming the artifact, its size and the ceiling. A half-delivered document is a corruption, not a bounded read.
- **Known limit: an MCP App's tool callback cannot use a byte-mapped parameter.** That path is browser-driven with no run behind it, so no resolver is seated and the call fails loud. Giving it its own resolver would create a second, differently-scoped definition of what the feature can reach.

Be deliberate about eligibility: it decides which *remote servers* may receive a user's artifact content, and artifacts are stored as authored (unredacted), so a byte-eligible connection can move whatever an artifact contains. It does not widen which artifacts a run can reach — only where they can go.

The HTTP and A2A transports remain unbuilt; the seam is visibly partial rather than silently so.

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

An http(s) MCP server can also bind a declared `tools.oauth_providers[]` entry by name — `oauth_provider: <name>` on the `mcp_servers[]` entry — so every identity-stamped per-call RPC injects a fresh per-identity `Authorization: Bearer` for the calling user (a token-fetch failure fails the call loud, never an unauthenticated fallback). A static `meta_annotations` map rides into the MCP `_meta` alongside the isolation triple for server-side attribution; each key is an annotation PATH, so a dotted key (`vendor.account_id`) nests under `_meta.vendor` rather than landing as a literal flat key. See `docs/CONFIG.md` › `tools.mcp_servers[].oauth_provider` for the full field surface and validation rules (reserved tokens are refused at the whole key AND at every segment, paths are depth-capped, and two declared paths may not collide).

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
