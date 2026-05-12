# Research Brief 10 — Code-mode as a Harbor primitive

**Date:** 2026-05-12
**Status:** research input proposing a new V1.1 / post-V1 phase. **Not yet on the master plan.** This brief argues the shape of "code-mode as a Harbor primitive over the existing tool catalog" and documents what bifrost's `core/schemas` exposes as the working reference implementation in Go. The phase plan + master-plan PR are downstream of this brief.
**Bifrost version inspected:** `github.com/maximhq/bifrost/core v1.5.8`. Type signatures quoted here will drift; re-`go doc` at implementation time.

## Why this brief exists

Brief 03 (§1, §6) and brief 07 (the "elegance principle") establish Harbor's tool-dispatch posture: the runtime owns dispatch; the LLM emits decisions; tools are uniform across in-process / HTTP / MCP / A2A. The default planner mode is **one tool call per LLM round-trip** — the LLM produces a single `(tool_name, args)` decision, the runtime invokes, the result feeds the next round.

For *tool-heavy* sessions (planner needs to call 5+ tools in a row, often with simple data-shuffling between them) the one-call-per-round pattern is expensive: N round-trips, N prompt re-renders, N latencies, N tool-catalog re-tokenizations.

**Code-mode** is an alternate planner mode where the LLM is asked to emit a small *program* (in a restricted language — typically a Python subset) that the runtime executes in a sandbox, with the runtime's tools exposed as callable bindings inside the sandbox. The program runs N tool calls in one local pass; only the final return value re-enters the LLM context. The savings are real on tool-heavy workloads (Anthropic and others have published 80%+ context savings on agentic loops). The cost is a sandbox runtime, a binding surface, and architectural decisions about how the sandbox composes with Harbor's existing primitives.

`bifrost`'s `core/schemas` package implements code-mode as an MCP-specific feature: an MCP client marked `IsCodeModeClient: true` exposes its tools through four meta-tools (`listToolFiles`, `readToolFile`, `getToolDocs`, `executeToolCode`) backed by an embedded Starlark interpreter. This brief documents that implementation as a reference *and* argues why Harbor's adoption — if it happens — should be a Harbor primitive over the full `ToolCatalog`, not a "route MCP through bifrost" subordination.

**The adoption decision is RFC-territory and not in this brief's scope.** What this brief settles is: (a) what bifrost actually exposes; (b) what shape a Harbor-native code-mode primitive should take; (c) which existing phases / contracts it touches.

## What bifrost provides (the reference)

### The four-meta-tool pattern

bifrost code-mode hides every per-tool function from the LLM and exposes exactly four meta-tools. From the public docs (verbatim):

1. **`listToolFiles`** — Discover available MCP servers.
2. **`readToolFile`** — Load Python stub signatures on-demand.
3. **`getToolDocs`** — Access detailed tool documentation.
4. **`executeToolCode`** — Run Python code with tool bindings in a sandbox.

The LLM's loop becomes: call `listToolFiles` to see what's there; `readToolFile` to fetch the typed stubs for one server's tools; `getToolDocs` for any tool whose contract is unclear; then `executeToolCode` with a small program. The full per-tool catalog never appears in the prompt unless the LLM explicitly fetches it.

### The wire constants

```go
// Quoted verbatim from github.com/maximhq/bifrost/core
func IsCodemodeTool(toolName string) bool

// And from core/schemas:
type CodeModeBindingLevel string
const (
    CodeModeBindingLevelServer CodeModeBindingLevel = "server"
    CodeModeBindingLevelTool   CodeModeBindingLevel = "tool"
)

// On MCPClientConfig:
type MCPClientConfig struct {
    // ... non-code-mode fields elided ...
    IsCodeModeClient     bool                 // Whether the client is a code mode client
    CodeModeBindingLevel CodeModeBindingLevel // How tools are exposed in VFS: "server" or "tool"
    // ... rest elided ...
}
```

`IsCodemodeTool(name)` is the runtime-side recognition seam — the LLM-edge dispatcher checks this to route the call to the sandbox layer.

`CodeModeBindingLevel` is the VFS-binding granularity:

- **`server`** — one Python module per MCP server. The LLM accesses tools as `youtube.search(...)`, `github.create_issue(...)`. Closures are coarse but the import surface is small.
- **`tool`** — one Python file per tool. The LLM accesses them as flat top-level names. Larger import surface but cleaner stub-per-tool model.

### The sandbox

bifrost embeds the Go-native Starlark interpreter (`go.starlark.net`). Harbor already has `go.starlark.net` as a transitive dependency through bifrost (`go.sum` line: `go.starlark.net v0.0.0-20260102030733-3fee463870c9`). Starlark is a Python-subset designed for sandboxed configuration / build languages (Bazel uses it). Hard constraints:

- No `import` (the interpreter loads modules via a host-provided loader; the host controls the surface).
- No file I/O.
- No network access.
- No classes, no async/await.
- No mutable global state (Starlark uses frozen modules + immutable values).
- List comprehensions, dict operations, basic control flow, function definitions — yes.
- Default 30-second execution timeout (bifrost-side).
- Synchronous execution only (one program, one Go goroutine).

The Go API surface (`go.starlark.net/starlark`) lets the host inject globals — `youtube`, `github`, etc. become objects whose methods are Go functions wrapping the underlying tool dispatch.

### The plugin-pipeline hook (the load-bearing detail)

```go
// On schemas.MCPConfig:
type MCPConfig struct {
    // ... non-code-mode fields elided ...

    // PluginPipelineProvider returns a plugin pipeline for running MCP plugin hooks.
    // Used when executeCode tool calls nested MCP tools to ensure plugins run for them.
    // The plugin pipeline should be released back to the pool using ReleasePluginPipeline.
    PluginPipelineProvider func() interface{} `json:"-"`

    // ReleasePluginPipeline releases a plugin pipeline back to the pool.
    ReleasePluginPipeline func(pipeline interface{}) `json:"-"`
}
```

This is the surface where bifrost lets the host inject per-nested-call hooks (PreMCPHook / PostMCPHook). When the LLM-emitted Starlark calls `youtube.search(...)` inside `executeToolCode`, bifrost dispatches the MCP call through the plugin pipeline so observability / governance / redaction still fire on the nested call.

The Harbor equivalent of this hook is the seam where `tools.RunWithPolicy` (D-024), audit redaction (§7), event emission (`tool.invoked`/`tool.completed`), cost accounting (Phase 36a), and identity propagation must run for every nested call.

### What bifrost code-mode does NOT cover

- **MCP-only.** No HTTP / in-process / A2A tool routing in the sandbox. Harbor's catalog spans four transports.
- **No pause/resume integration.** A nested call requiring HITL or OAuth inside the sandbox is undefined behaviour — bifrost's MCP layer surfaces `MCPUserOAuthRequiredError` from the outer call, but inside Starlark the binding either succeeds or raises a Starlark error. Long pauses don't compose.
- **No identity propagation primitives in the sandbox.** Bindings can close over `BifrostContext`, but the docs don't specify how Harbor's identity triple flows into Starlark globals or onto nested calls.
- **No artifact-store routing.** Heavy outputs from nested calls return as Starlark values inline. D-022 / D-026 mandatory routing isn't covered.
- **No event-bus emission for nested calls.** Whether bifrost emits a `mcp.tool_invoked`-equivalent for the *underlying* tool calls inside `executeToolCode` is unclear from the docs. From Harbor's perspective, an event taxonomy without nested-call visibility breaks topology projection (Phase 74).

## Why "Harbor primitive over the full catalog" is the right shape

If Harbor adopts code-mode, three options exist:

1. **Route MCP traffic through bifrost-MCP + bifrost code-mode.** Brief'd already: violates §13 (two parallel MCP implementations), bypasses Phase 28's identity/audit/policy seams, demotes Resources/Prompts. Rejected.
2. **Wrap bifrost's Starlark layer over Harbor's catalog.** Possible but awkward — bifrost's sandbox bindings expect `MCPClient`-shaped descriptors; Harbor's `ToolDescriptor` is a different model. Adapter layer ends up being most of the work anyway.
3. **Embed `go.starlark.net` directly, write Harbor-native bindings over `tools.ToolCatalog`.** The bindings call `tools.RunWithPolicy(ctx, descriptor, args)` for each nested invocation. The sandbox sees the full catalog: in-process Go tools, HTTP tools, MCP tools, A2A tools, Flow-as-Tool. The 80% of the work is binding generation + sandbox wrapper; the 20% saved by reusing bifrost's code is not enough to take the architectural hits.

Option 3 is what this brief argues for.

## Proposed Harbor primitive (design sketch)

### Naming

- **`code-mode planner adapter`** — a planner-adapter layer that the planner can opt into for a given step. Not a new planner concrete; the existing planners (ReAct, Plan-Execute, Workflow) decide *whether* to call code-mode for a step.
- **The four meta-tools** — Harbor names them (proposal): `code.catalog`, `code.signature`, `code.docs`, `code.run`. Shorter than bifrost's; namespaced under `code.` for clarity.
- **`internal/runtime/codemode/`** — the package home. Sandbox lifecycle, binding generation, the four-tool catalog entries.

### High-level shape (Go-flavored sketch)

```go
// package codemode

// Sandbox is the per-invocation Starlark runtime. Construct once per
// code.run call (each program gets its own sandbox; sandboxes are not
// reused — Starlark thread state is per-invocation).
type Sandbox struct {
    // unexported
}

// New constructs a Sandbox bound to the given catalog and identity.
// The identity triple is captured here and propagated to every
// nested tool call. The catalog snapshot is taken at construction
// time — subsequent catalog mutations don't affect the in-flight
// program (CLAUDE.md §5 "compiled artifacts are immutable").
func New(
    catalog tools.Catalog,
    identity identity.Quadruple,
    bus events.EventBus,
    policy tools.ToolPolicy,
    redactor audit.Redactor,
    artifacts artifacts.Store,
) *Sandbox

// Run executes the Starlark program. Returns either the program's
// returned value (a Harbor-typed value, not a Starlark value), or
// a typed error. Pause requests from nested calls surface as
// ErrPauseRequested carrying the original tool.auth_required /
// tool.approval_required payload — the runtime parks the run on
// the way out and the same code.run call resumes (with the
// captured sandbox state) once the pause completes.
func (s *Sandbox) Run(ctx context.Context, program string, timeout time.Duration) (codeResult, error)

// Sentinels
var (
    ErrPauseRequested     = errors.New("codemode: nested call requested pause")
    ErrTimeout            = errors.New("codemode: program exceeded timeout")
    ErrForbiddenBuiltin   = errors.New("codemode: program used a forbidden Starlark feature")
    ErrCatalogMiss        = errors.New("codemode: program referenced unknown tool")
    ErrSandboxArgsInvalid = errors.New("codemode: nested call args failed schema validation")
)
```

### The four meta-tools (registered as in-process tools)

Each is a regular `tools.ToolDescriptor` registered via `inproc.RegisterFunc` at startup. The descriptor's `Invoke` reads the current run's catalog snapshot from `ctx` and renders the response.

```go
// code.catalog — "what tools / servers are available?"
// Returns a typed listing: one entry per ToolSourceID + transport,
// plus a per-source tool count. NO per-tool schemas.
//   {"sources": [{"id": "github", "transport": "mcp", "tool_count": 27}, ...]}
//
// code.signature — "give me the Python-ish stub signatures for source X"
// Returns a Starlark-stub string that the LLM can paste / paraphrase
// into its program. The runtime generates the stubs from the catalog's
// JSON Schemas. Per BindingLevel (server | tool).
//   {"source": "github", "stubs": "def create_issue(repo: str, title: str, ..."}
//
// code.docs — "give me the full doc for tool Y"
// Returns the Tool's Description + Examples + SafetyNotes + the
// JSON-Schema rendering (so the LLM can recover from a schema miss).
//   {"name": "github.create_issue", "doc": "...", "schema": {...}, "examples": [...]}
//
// code.run — "execute this Starlark program"
// Args: { "program": str, "timeout_ms": int (default 30000) }
// Result: { "value": <typed-return>, "trace": [<per-nested-call summary>] }
//
// On nested-call pause:
//   raises ErrPauseRequested; the runtime catches it via the standard
//   tool-dispatch error path and parks the run. Resume reconstructs the
//   sandbox state and re-runs code.run with the captured continuation.
//   (See "Pause/resume composition" below for the implementation note.)
```

### Stub generation

The `code.signature` tool generates Starlark stubs from the catalog's `ToolDescriptor.Tool.ArgsSchema` (JSON Schema). The stub is *not executable* — it's a typed signature surface for the LLM to read. The actual binding is Go-side: when the LLM writes `github.create_issue(repo="foo", title="bar")` inside `code.run`, Starlark dispatches to a Go function that the sandbox registered as the `github.create_issue` global. That Go function:

1. Reads `ctx` (carries identity + run scope).
2. Builds the args map from Starlark kwargs (Starlark → JSON-shaped Go map).
3. Validates against `ToolDescriptor.Tool.ArgsSchema`.
4. Invokes `tools.RunWithPolicy(ctx, descriptor, argsJSON)` — the standard Harbor tool-dispatch path.
5. On error, raises a Starlark error (most cases) or surfaces `ErrPauseRequested` (auth / approval needed).
6. On success, marshals the result back to a Starlark value, emits the `tool.completed` event, returns.

### Binding-level granularity

Match bifrost: `server` and `tool` modes. Default to `server` — simpler import surface, smaller LLM context budget for stubs. `tool` mode opt-in via config for catalogs with very few high-value tools.

## How the primitive composes with existing Harbor contracts

### D-024 (ToolPolicy reliability shell on every invocation)

Every binding inside the sandbox calls `tools.RunWithPolicy(ctx, descriptor, args)`. The policy fires per nested call, identically to direct LLM-emitted tool calls. **This is the single most important compositionality property** — the sandbox is a *caller* of the tool dispatch path, not a parallel implementation.

### D-025 (concurrent reuse)

The `Sandbox` is a per-invocation object — constructed once per `code.run` call. The shared `Catalog` is the concurrently-reused artifact. Sandbox construction takes a catalog snapshot; concurrent `code.run` calls each get their own sandbox + their own snapshot. No mutable state on the catalog from the sandbox layer.

### D-026 (LLM-edge context safety)

Sandbox return values that exceed the heavy-output threshold MUST be routed through the artifact store before being returned to the LLM via the `code.run` result. The binding-side post-processing (step 6 above) checks each nested return and either inlines (small) or materializes to `ArtifactRef` (heavy). The final program return value gets the same treatment.

### Identity (§6)

`identity.MustFrom(ctx)` is read at `Sandbox.New` time and captured. Every binding's nested-call ctx is a child of the original ctx — identity flows through. No package-level state; no Starlark-exposed identity (the LLM doesn't get to spoof tenant/user/session from inside the sandbox).

### Audit redaction (§7.6)

Every nested-call arg + result goes through `audit.Redactor` before being persisted or emitted. The redactor runs at the binding-wrapper layer, not inside Starlark. Tool args that contain secret-shaped values (bearer tokens, API keys) are redacted before the `tool.invoked` event is emitted; results follow the same path before the `tool.completed` event.

### Event taxonomy (RFC §6.13, Phase 5+)

Every nested call inside the sandbox emits the same `tool.invoked` / `tool.completed` events as direct calls — same payload shape, same identity scope. The `code.run` call itself emits a `codemode.program_executed` event with the trace of nested calls. Topology projection (Phase 74) sees the nested calls as children of the `code.run` call; the Console can render the tree.

### Unified pause/resume (RFC §3.3, Phase 50)

The hardest composition. When a nested binding inside the sandbox detects a pause requirement:

1. The binding raises a Starlark error.
2. Starlark unwinds the program.
3. The sandbox catches the unwind and inspects the error: if it's `ErrPauseRequested` carrying a pause payload, the sandbox returns that error from `Sandbox.Run`.
4. The `code.run` tool-descriptor sees the error, emits the original `tool.auth_required` / `tool.approval_required` event (with full provenance), and the runtime parks the run.
5. **On resume**: the run's `code.run` call is re-invoked. The Sandbox is reconstructed; the program re-executes. **But the previously-completed nested calls must short-circuit on re-execution.**

The short-circuit mechanism is the sandbox-state-checkpoint design. Three implementation options:

- **Replay log** (recommended). Each completed nested call appends a `(call_id, args_hash, result)` record to a per-program log persisted in StateStore (Phase 7). On re-execution, the binding checks the log first: if `(args_hash)` matches, return the recorded result without re-invoking; if not, invoke fresh. The log is keyed by `(run_id, code_run_invocation_id)`. **Deterministic only if nested-call args are pure functions of program state.** The LLM-emitted program is usually deterministic given the same input, so this works for the common case.
- **Continuation snapshot.** Snapshot Starlark frame state on pause; resume from the frame. Theoretically cleaner; practically depends on Starlark's serialization story (it has none in the public Go SDK). Probably too expensive.
- **Just re-run the whole program**. Cheapest. Fine for short programs (10–20 nested calls). Fails on programs with non-idempotent nested calls. Probably the V1 behaviour; replay-log is a follow-up.

Phase 30 (tool-side OAuth) and Phase 31 (approval gates) compose into code-mode through this mechanism. **The pause primitive itself does not change** — code-mode uses it via the same `Coordinator` API as direct tool calls. What changes is the `code.run` descriptor handling: it catches `ErrPauseRequested` from the sandbox and routes it through the standard pause path.

### Cost accounting (Phase 36a)

Every nested call inside the sandbox emits its own `llm.cost.recorded` / tool-side cost event. Phase 36a's accumulator sees them as ordinary tool calls. The `code.run` call itself has no LLM cost beyond the LLM round-trip that emitted the program; nested calls accumulate as normal. Ceilings work transparently.

### Heavy outputs (D-022 / D-026)

Bindings post-process every nested return: byte arrays / `DataURL`s / large strings ≥ threshold are routed to `ArtifactStore` and replaced with `ArtifactRef`. The Starlark program sees the `ArtifactRef` (a small dict shape — `{"$ref": "..."}`); the LLM gets the reference in the final return value. **Critical**: the program can pass `ArtifactRef`s between nested calls without ever de-referencing them in the LLM context. This is one of the biggest *qualitative* wins of code-mode — manipulating heavy outputs entirely below the LLM line.

### Skill subsystem (Phase 37+)

Skills are not catalog tools but are addressable via planner tools. Their composition with code-mode is a follow-up question — most likely the skill tools (`skill.search`, `skill.get`) are themselves available inside the sandbox, but the *executed* skill is not a Starlark-callable. The phase plan should explicitly say.

## Starlark sandbox configuration (binding constraints)

Recommended starting policy. All settable via `codemode.Config`:

```go
type Config struct {
    // Hard deny list — Starlark builtins removed from the env.
    // Default: deny load(), print() (allow eprint via redirected stderr),
    //   no file I/O builtins (Starlark has none by default but we
    //   freeze the universe defensively).
    DeniedBuiltins []string

    // Default 30s. Hard kill via ctx cancellation propagated to the
    // Starlark thread (the interpreter checks ctx between statements).
    DefaultTimeout time.Duration

    // Default 100. Number of nested-tool calls per program. Beyond this,
    // the binding wrapper raises ErrCallBudgetExceeded.
    MaxCallsPerProgram int

    // Default 1MB. Maximum source size of the LLM-emitted program.
    MaxProgramBytes int

    // Default "server" (per bifrost). "tool" for catalogs with very few
    // high-value tools.
    BindingLevel CodeModeBindingLevel

    // Default "deny". If "allow", the binding wrapper does NOT block
    // calls whose result would exceed the heavy-output threshold; instead
    // it materializes them transparently. "deny" mode fails loudly.
    HeavyOutputPolicy string
}
```

The Starlark thread is given a `*starlark.Thread` with a `Print` function that redirects to the logger (debug-level) and a `Load` function that returns `errors.New("load not allowed")`. Thread.SetMaxExecutionSteps caps CPU.

## Files / surface this would touch (forecast)

If this lands as a phase, the changes are roughly:

- `internal/runtime/codemode/codemode.go` — `Sandbox`, `New`, `Run`, sentinels.
- `internal/runtime/codemode/binding.go` — per-`ToolDescriptor` binding wrapper that bridges Starlark → `tools.RunWithPolicy`.
- `internal/runtime/codemode/stubgen.go` — JSON-Schema → Starlark stub renderer.
- `internal/runtime/codemode/tools.go` — the four meta-tool descriptors (`code.catalog`, `code.signature`, `code.docs`, `code.run`).
- `internal/runtime/codemode/replay.go` — replay-log (StateStore-backed).
- `internal/runtime/codemode/events.go` — `codemode.program_executed` event registration.
- `internal/runtime/codemode/codemode_test.go` — unit + integration + concurrent-reuse + pause/resume tests.
- `internal/config/config.go` — `CodemodeConfig` block.
- `internal/runtime/flow/...` — possibly a flow-as-code-mode hook; the `Flow` Tool is already in the catalog and would compose naturally.
- New event type: `codemode.program_executed` (typed payload: program hash, nested-call count, total cost, total latency, error class).
- New event type (optional): `codemode.call_budget_exceeded`.
- New decisions log entry: `D-NNN — code-mode is a Harbor primitive over the full tool catalog, not a transport-specific feature.`
- New glossary entries: `code-mode`, `Sandbox`, `code.run`, the four meta-tools.

## Phase placement options

Three sane options for where this lands on the master plan:

1. **As a V1 phase** between Phases 47 (parallel-call execution) and 48 (deterministic planner). Risk: V1 is already long; adding a phase delays cut.
2. **As a V1.1 / immediate-post-V1 phase**, alongside the existing post-V1 cluster (Phases 83+). Risk: ships after V1 cut; users on V1 get one-tool-per-round only.
3. **As an experimental phase** behind a feature flag in V1, with the production gate (`code.run` registered in the default catalog) flipped post-V1 once the replay-log and pause-resume composition are battle-tested.

This brief doesn't pick one. Recommendation: **option 3** if the tool-heavy planner cases show up early; **option 2** otherwise. The argument for option 1 would have to be "we're seeing N round-trips per session and code-mode would 3x the throughput today" — that's empirical, not architectural.

## Cross-impact on existing phases

- **Phase 26 (tool catalog).** No change to the catalog's interface. Code-mode bindings are pure consumers. The catalog's `ToolDescriptor.Tool.ArgsSchema` and `Description` and `Examples` are the substrate; if they're high-quality, stub generation is high-quality.
- **Phase 26a (Flow-as-Tool).** Flows are tools. They become callable from inside the sandbox automatically. No extra work.
- **Phase 27 (HTTP) / 28 (MCP) / 29 (A2A).** No change. Their `ToolDescriptor`s flow through code-mode bindings identically to in-process tools.
- **Phase 30 (tool-side OAuth — brief 09).** The `ErrUserAuthRequired` shape is what bindings re-raise as `ErrPauseRequested`. The same callback flow that resumes a direct tool call resumes a code-mode program. **Composition is automatic if Phase 30's primitive is built right.**
- **Phase 31 (approval gates).** Same composition as Phase 30.
- **Phase 36a (cost accumulator).** Nested calls inside the sandbox emit the same cost events as direct calls. Ceilings work transparently.
- **Phase 42–49 (planner family).** Code-mode is a planner-adapter feature, not a planner concrete. A planner opts in per-step. The `Planner.Next` decision can return either `(tool_name, args)` for a single call or `("code.run", {program: "..."})` for a code-mode step. **Planner interface unchanged.**
- **Phase 50 (pause/resume).** Coordinator unchanged. `code.run` participates via its descriptor.
- **Phase 55 / 56 (telemetry).** Code-mode adds one new event type (`codemode.program_executed`) and uses existing tool-call events for nested calls. OTel spans nest naturally — `code.run` is a parent span, nested calls are children.
- **Phase 72–74 (Console surface).** The Console renders the four meta-tools like any tool. The `codemode.program_executed` event is a topology pivot — the Console shows the program as a parent node with nested tool calls as children. Adds one rendering case; otherwise zero protocol changes.
- **Phase 76 (cross-tenant isolation conformance).** Adds: "code-mode programs cannot access tools outside their identity's catalog scope." The catalog snapshot at `Sandbox.New` already enforces this, but the harness must prove it.
- **Phase 77 (goroutine-leak harness).** Adds: "code-mode programs do not leak goroutines on cancel / timeout / pause." Standard.

## Risks / open questions

1. **Starlark expressivity vs. LLM affinity.** Starlark is Python-ish but not Python. Models trained on Python may emit Python-only constructs (list slicing tricks, comprehension scoping, `yield`, `async`, decorators). The binding wrapper must produce *helpful* errors when the program uses something Starlark can't run, so the LLM can self-correct. **Open question**: does the runtime offer a built-in "retry with feedback" loop (Phase 36-style) for code-mode parse/exec errors? Recommended yes.
2. **Sandbox-escape surface.** Starlark is sandboxed by design but the Go bindings are not — a buggy binding can leak ctx, mutate shared state, or call out to the network. **Mitigation**: every binding goes through `tools.RunWithPolicy` and the same audit + identity gates as a direct call. No "fast path" for in-process bindings.
3. **Pause/resume semantics on non-idempotent nested calls.** The replay-log strategy assumes nested-call results are deterministic from the program perspective. Tools with side effects (`github.create_issue`) that are called twice on replay create two issues. **Mitigation**: tools tagged `SideEffectWrite | SideEffectExternal` (existing in `internal/tools/tools.go`) require an idempotency key in code-mode; the binding wrapper generates one from `(run_id, code_run_invocation_id, call_index_in_program)`. Servers that don't honour idempotency keys: the catalog should mark the descriptor `CodemodeForbidden = true` and the binding refuses to expose it inside the sandbox.
4. **Cost of stub generation.** If the catalog has 500 tools, the stub for a 27-tool MCP server is large. **Mitigation**: per-server stubs (default binding level). LLM only fetches the server's stub when it intends to use that server.
5. **Catalog drift mid-program.** A tool removed from the catalog mid-program is fine (sandbox uses the snapshot) but resuming a paused program against an updated catalog can hit catalog misses. **Mitigation**: replay-log records `descriptor_hash`; resume rejects with `ErrCatalogDrift` if the descriptor changed.
6. **LLM emitting unsafe Starlark.** The interpreter is sandboxed; the binding-wrapper validates args; outputs go through redaction. The remaining attack surface is timing/resource (`while True: pass`, allocate huge lists). **Mitigation**: `Thread.SetMaxExecutionSteps` caps CPU; `MaxCallsPerProgram` caps tool I/O; explicit deadline propagated from ctx.
7. **Determinism for the replay log.** Programs that depend on time / randomness break the replay. **Mitigation**: deny `time.now()`-equivalent globals in the sandbox; if the LLM wants randomness, it requests it as a tool call.
8. **Bifrost drift.** The `CodeModeBindingLevel` and `IsCodemodeTool` constants are bifrost-versioned. If we lift the *names*, we drift with bifrost. **Mitigation**: don't lift the names. Use Harbor-native names (`code.catalog` etc.); reference bifrost in comments only.

## What this brief does NOT do

- It does not decide whether code-mode is adopted. That's an RFC PR with the operator's input.
- It does not propose a specific master-plan slot. Three options are sketched; the choice is downstream.
- It does not enumerate per-tool stub generation rules. The phase plan author should write that as part of the per-phase spec.
- It does not pre-commit to the replay-log shape. Three options are sketched (replay log, continuation snapshot, just-re-run); the implementor picks based on a quick experiment.
- It does not propose a planner-API change. Planners stay swappable; code-mode is a tool in the catalog they may emit a call to.

## Findings summary

- ✓ bifrost code-mode is fully Go-native and uses `go.starlark.net` — already in Harbor's `go.sum` transitively. Embedding the interpreter directly is cheap.
- ✓ The four-meta-tool pattern is a clean LLM-facing surface that hides the per-tool catalog from the prompt and reduces context cost on tool-heavy sessions.
- ✓ Composes cleanly with every Harbor primitive *if built over the catalog*: D-024 reliability shell, D-025 concurrent reuse, D-026 LLM-edge safety, identity (§6), audit redaction (§7), event taxonomy (§6.13), unified pause/resume (§3.3), cost accumulator (Phase 36a), heavy-output routing (D-022).
- ⚠ Pause/resume composition requires either a replay-log, a Starlark continuation snapshot, or a "just re-run the whole program" policy. V1: just re-run + tag side-effecting tools as needing idempotency keys. Replay-log is a follow-up.
- ⚠ Stub generation quality is bounded by `ArgsSchema` + `Description` + `Examples` quality. Catalogs with poor descriptions don't get good code-mode UX. Forcing function for higher-quality descriptors.
- ⚠ Non-determinism (time, randomness, side effects on retry) needs explicit handling. Deny clock / RNG; require idempotency keys on side-effecting tools.
- ✗ Lifting bifrost's MCP code-mode wholesale would couple the LLM driver and tool catalog and break Harbor's transport-uniform tool model. Brief'd in the previous conversation; not adoptable.

## Source artifacts referenced

- bifrost packages: `github.com/maximhq/bifrost/core` (top-level — `IsCodemodeTool`), `github.com/maximhq/bifrost/core/schemas` (`CodeModeBindingLevel`, `MCPClientConfig.IsCodeModeClient`, `MCPConfig.PluginPipelineProvider`).
- Bifrost docs: <https://docs.getbifrost.ai/mcp/code-mode>.
- Starlark Go: `go.starlark.net` (in Harbor's `go.sum` transitively).
- Harbor tool catalog: `internal/tools/tools.go` — `Tool`, `ToolDescriptor`, `TransportKind`, `SideEffect`, `LoadingMode`.
- Harbor phase plans referenced: 26, 26a, 27, 28, 29, 30 (brief 09), 31, 36a, 50, 55, 72–74, 76, 77.
- RFC anchors: §3.3 (unified pause/resume), §3.4 (fail-loudly), §6.1 (engine), §6.4 (tool catalog), §6.13 (event bus).
- Decisions referenced: D-021 (multimodal), D-022 (heavy-output routing), D-024 (`ToolPolicy` everywhere), D-025 (concurrent reuse), D-026 (LLM-edge safety net).
- Brief 03 §1, §6 (tools/integrations/LLM). Brief 07 (the elegance principle).
- CLAUDE.md anchors: §5 (concurrent reuse contract), §6 (multi-isolation), §7 (security), §13 (forbidden practices — two parallel implementations).

## Re-discussion checklist

When this brief is brought back for the actual phase-plan PR:

- [ ] Decide V1 / V1.1 / post-V1 placement and the gating criterion (operator pain point vs. headroom feature).
- [ ] Settle the V1 pause/resume composition: just-re-run (simplest) vs. replay-log (cleanest) vs. continuation snapshot (rejected unless Starlark adds public serialization).
- [ ] Settle binding-level default (`server` recommended; `tool` opt-in).
- [ ] Decide whether Phase 36 (retry with feedback) applies to code-mode parse/exec errors (recommended yes).
- [ ] Decide idempotency-key policy for side-effecting tools inside the sandbox.
- [ ] Decide naming: `code.catalog` / `code.signature` / `code.docs` / `code.run` vs. an alternative.
- [ ] Confirm `go.starlark.net` is the embed target (vs. e.g. a hand-rolled subset).
- [ ] Confirm the sandbox runs `Thread.SetMaxExecutionSteps` + ctx-deadline propagation as the two CPU caps.
- [ ] Confirm the four meta-tools register as in-process tools via the existing inproc driver (no special-case in the catalog).
- [ ] Confirm `Flow`-as-tool composes (Flow descriptors are addressable inside the sandbox).
- [ ] Write the master-plan-PR entry, the RFC stub (if it touches RFC), and the new decisions-log entry.
