# Research Brief 03 — Tools, Integrations, and LLM Client

**Status:** research input for RFC and phase plans. Internal context derived from a non-Go reference implementation; not a public-facing artifact.

**Scope:** the unified tool abstraction that lets the planner address MCP servers, A2A peers, HTTP endpoints, and in-process Go functions through a single contract; tool-side OAuth/HITL plumbing; and the LLM client layer (provider abstraction, structured-output strategies, streaming, retry, cost). Out of scope (other briefs own these): DAG runtime, planner internals, memory/skills, state store/artifacts/tasks/sessions, CLI, test kit.

---

## 1. Subsystem Overview — the Unified Tool Abstraction

The planner reasons about exactly one concept: a **`Tool`** with a name, a JSON-Schema-shaped argument model, an output schema, and a description. It does not know — and must not have to know — whether the tool is:

- an **in-process Go function** registered via the catalog API,
- a **method on an MCP server** discovered over stdio / HTTP / streamable-HTTP / SSE / WebSocket,
- a **remote A2A skill** discovered via an Agent Card and called via `message/send` or `message/stream`,
- a **plain HTTP endpoint** described by an external manifest.

This unification is the single largest leverage point in the runtime. The reference implementation already moves in this direction (see `~/Repos/Penguiflow/penguiflow/penguiflow/catalog.py` for the in-process catalog and `~/Repos/Penguiflow/penguiflow/penguiflow/tools/node.py` for the MCP/UTCP integration), but Harbor will go further: in the reference, MCP/A2A/HTTP each have their own framing and their unification is implementation-level (each backend produces `NodeSpec` records, but they reach the planner via different code paths). Harbor moves the unification to the **type level** — every `Tool` is *the same struct* regardless of source, and the dispatch is one switch in one place.

Three load-bearing properties follow:

1. **Source pluggability.** Adding a new transport (gRPC, WebSocket-JSON-RPC, broker-mediated) is a `ToolProvider` driver; nothing else changes.
2. **Visibility scoping.** The catalog filter is keyed on the `(tenant, user, session)` triple from `harbor_isolation.md`. The reference filters by `tenant` only; Harbor filters on the full triple plus the policy-driven `auth_scopes` and `tags`.
3. **Provenance uniformity.** A tool call's audit record looks the same whether the call hit a localhost Go function or a remote agent across the network. The planner, the event bus, and the artifact store care about `(tool_name, args, result, source_id, transport, latency, cost)` — never about transport-specific framing.

---

## 2. Key Data Shapes (Go-flavored sketches)

These names are Harbor's. They are not 1:1 translations of the source.

```go
// Tool is the unified planner-addressable unit.
type Tool struct {
    Name        string            // unique within a session catalog (provider may namespace)
    Description string
    ArgsSchema  json.RawMessage   // JSON Schema (object)
    OutSchema   json.RawMessage   // JSON Schema (object); required for typed routing
    SideEffects SideEffect        // pure | read | write | external | stateful
    Tags        []string
    AuthScopes  []string
    CostHint    string            // free-form cheap/normal/expensive
    LatencyHint time.Duration
    SafetyNotes string
    Loading     LoadingMode       // Always | Deferred
    Examples    []ToolExample
    Source      ToolSourceID      // which provider produced this entry
    Transport   TransportKind     // InProcess | MCP | A2A | HTTP
}

type ToolExample struct {
    Args        map[string]any  // must JSON-marshal
    Description string
    Tags        []string
}

// ToolDescriptor is the *callable* binding produced by a provider.
// The planner sees Tool; the dispatcher uses ToolDescriptor.
type ToolDescriptor struct {
    Tool       Tool
    Invoke     func(ctx context.Context, args json.RawMessage, rc *RunContext) (ToolResult, error)
    Validate   func(args json.RawMessage) error  // pre-call argument validation
}

type ToolCatalog interface {
    Register(d ToolDescriptor) error
    Resolve(name string) (ToolDescriptor, bool)
    List(filter CatalogFilter) []Tool   // returns Tool views, never Descriptors
}

// CatalogFilter is what enforces (tenant, user, session) visibility.
type CatalogFilter struct {
    TenantID, UserID, SessionID string
    GrantedScopes               []string
    LoadingModes                []LoadingMode  // typically [Always] for prompt; [Always,Deferred] for full discovery
    NameRegex                   *regexp.Regexp
}

// ToolProvider is the seam: one driver per transport class, plus user-defined providers.
type ToolProvider interface {
    Connect(ctx context.Context, rc *RunContext) error
    Discover(ctx context.Context) ([]ToolDescriptor, error)
    Close(ctx context.Context) error
    SourceID() ToolSourceID
}

// LLMClient is the single contract the planner depends on. ONE mode, no toggles.
type LLMClient interface {
    Complete(ctx context.Context, req LLMRequest) (LLMResponse, error)
    Stream(ctx context.Context, req LLMRequest, sink StreamSink) (LLMResponse, error)
}

type LLMRequest struct {
    Model            string
    Messages         []ChatMessage
    Tools            []ToolSpec           // schemas only — never Descriptors
    ToolChoice       ToolChoice           // Auto | Required | Specific(name) | None
    StructuredOutput *StructuredOutputSpec
    Temperature      float32
    MaxTokens        int
    ReasoningEffort  string               // "off" | "low" | "medium" | "high" | ""
    Extra            map[string]any       // sanitized provider passthrough
}

type ChatMessage struct {
    Role  Role             // System | User | Assistant | Tool
    Parts []ContentPart    // Text | ToolCall | ToolResult | Image
}

type ToolCall struct {
    Name          string
    ArgumentsJSON string  // raw — preserves provider round-trip fidelity
    CallID        string
}

type ToolResult struct {
    Name       string
    ResultJSON string
    CallID     string
    IsError    bool
    Artifacts  []ArtifactRef  // heavy outputs reference the artifact store
}
```

The reference's typed parts (`TextPart`, `ToolCallPart`, `ToolResultPart`, `ImagePart` in `~/Repos/Penguiflow/penguiflow/penguiflow/llm/types.py`) are a good pattern to inherit verbatim — the message envelope is provider-agnostic, individual providers adapt to/from these parts.

---

## 3. Public API Surface

Three audiences:

**Tool authors** write a function and register it:

```go
func WeatherLookup(ctx context.Context, args WeatherArgs, rc *harbor.RunContext) (WeatherOut, error) { ... }

harbor.RegisterTool(catalog, harbor.ToolMeta{
    Name:        "weather.lookup",
    Description: "Look up current weather by city name.",
    SideEffects: harbor.External,
    AuthScopes:  []string{"weather:read"},
    Examples:    []harbor.ToolExample{{Args: map[string]any{"city": "Berlin"}}},
}, WeatherLookup)
```

The registration helper uses generics + reflection to derive `ArgsSchema` and `OutSchema` from the `WeatherArgs` / `WeatherOut` types — equivalent to how the reference derives schemas from Pydantic models. No decorator equivalent is needed: type inference replaces it.

**External-source authors** implement `ToolProvider` and register a driver:

```go
provider, _ := mcpdriver.New(harbor.MCPSourceConfig{
    Name: "github", Connection: "stdio:npx -y @modelcontextprotocol/server-github",
    AuthScopes: []string{"gh:read"},
})
catalog.AttachProvider(ctx, provider)
```

**Planners** receive a filtered `[]Tool` view at each step, never a `ToolDescriptor`. This guarantees the planner can be serialized (only schemas + names) and replayed. Dispatch goes through the catalog: `cat.Resolve(name).Invoke(...)`.

**Protocol surface** (consumed by Console / third-party clients per `harbor_protocol.md`):

- `GET /catalog?tenant=...&user=...&session=...` — list visible tools.
- `GET /catalog/sources` — list providers with health, last-discover timestamp.
- `POST /catalog/sources/{id}/refresh` — re-discover.
- Stream: `tool.discovered`, `tool.removed`, `tool.invoked`, `tool.completed`, `tool.failed` events.

---

## 4. Internal Mechanics

**Tool resolution** is two-phase. First, providers `Discover()` produces `ToolDescriptor`s with provider-prefixed names (`github.search_repos`). Second, the catalog applies a `CatalogFilter` to produce the planner-visible `[]Tool`. The planner picks a name; the catalog `Resolve()` returns the descriptor, and dispatch happens through `Invoke`.

**Cross-transport dispatch:** every `ToolDescriptor.Invoke` is the same shape `(ctx, args, rc) -> (ToolResult, error)`. Inside, an MCP descriptor calls FastMCP-equivalent client; an A2A descriptor calls the A2A transport (see §5); an HTTP descriptor builds a request from a UTCP-style manual; an in-process descriptor calls the Go function directly. The planner is unaware.

**Argument validation** runs at the catalog edge. The reference uses Pydantic; Harbor uses a JSON-Schema validator (e.g. `santhosh-tekuri/jsonschema`) plus an optional user-supplied `Validator` for cross-field invariants. Validation failures are *not* tool errors — they are routed back to the planner as a typed `tool.invalid_args` event so the planner can reformulate, with the error fed in via `LLMClient` retry feedback (the reference does this in `~/Repos/Penguiflow/penguiflow/penguiflow/llm/retry.py`).

**Result normalization** is the most subtle part. MCP returns `TextContent | ImageContent | EmbeddedResource | ResourceLink`. A2A returns `TextPart | FilePart | DataPart`. HTTP returns whatever the endpoint returns. Harbor's normalizer is a layered pipeline — explicit field-extraction rules for known tools first, then typed-content-block extraction, then heuristic binary detection, then a size-based safety net (anything over `MaxInlineSize` chars routes to the artifact store). The reference encodes the same five layers in `_transform_output` at `~/Repos/Penguiflow/penguiflow/penguiflow/tools/node.py:1140`. Harbor inherits the layered approach but enforces it: artifacts are mandatory, not opt-in (per `harbor_design_principles.md` seam #5).

**LLM provider quirks** are real and need a single correction layer. Examples observed in the reference (paths in `~/Repos/Penguiflow/penguiflow/penguiflow/llm/`): NIM rejects mixed system/developer-then-user message ordering and needs reorder/collapse (`protocol.py:91`); OpenRouter `x-ai/*` routes need an explicit `reasoning_enabled` flag; some OpenAI-compatible proxies reject `{"allOf": [...]}` schemas without root `"type": "object"`; structured-output downgrades `json_schema → json_object → text` on `invalid_json_schema` errors (`native_policy.py`); some streaming proxies report `0/0` tokens, estimate from byte length when pricing is known. Harbor folds these into **provider drivers** under one `LLMClient` — not a parallel "native vs LiteLLM" mode (§5).

**LLM mode planning.** Per `~/Repos/Penguiflow/penguiflow/penguiflow/llm/schema/plan.py`, `OutputMode = Native | Tools | Prompted` is selected per provider via a `ModelProfile`. Harbor inherits this: `SchemaPlan` decides between provider-native schema mode, function-calling, or schema-in-prompt + parse-retry. Mode is observable.

---

## 5. Sharp Edges from the Source

**Two parallel LLM modes (the toggle smell).** The reference exposes `use_native_llm=True/False` toggling between LiteLLM and a `NativeLLMAdapter` that re-implements provider-specific normalization (`~/Repos/Penguiflow/penguiflow/penguiflow/llm/protocol.py:161`). The two modes ship in parallel because LiteLLM didn't cover provider quirks well enough. **Harbor must pick one architecture and bake the correction in.** The recommendation: ship a thin abstraction over `liter-llm` (the Go LiteLLM client; see `harbor_tech_stack.md`) with the correction layer compiled in as a stack of provider drivers — no toggle. If `liter-llm` falls short of the surface needed (streaming with reasoning deltas, tool calling, structured output across all six provider families, cancellation, cost), we go provider-native via official SDKs and drop the LiteLLM dependency entirely.

**A2A docs lagging the code.** The reference's public docs flag an "A2A compliance gap"; the actual code (`~/Repos/Penguiflow/penguiflow/penguiflow_a2a/models.py`, `core.py`, `server.py`, `transport.py`, ~3500 lines) implements the full A2A spec — Agent Cards with `protocol_versions`, `supported_interfaces`, `capabilities`, `security_schemes`, `skills`, `signatures`; tasks with `TaskState` lifecycle including `INPUT_REQUIRED` and `AUTH_REQUIRED`; `message/send` and `message/stream`; `TaskStatusUpdateEvent` and `TaskArtifactUpdateEvent` as SSE events; `TaskPushNotificationConfig` with bearer-token push. Harbor: **never let docs lag code** — the `feedback_harbor_doc_hygiene.md` rule covers this. The phase that ships A2A also ships the A2A docs.

**A2A wire shape (worth inheriting).** Discovery via Agent Card at `GET /.well-known/agent-card.json`. JSON-RPC dispatch: `message/send` (blocking), `message/stream` (SSE), `tasks/get`, `tasks/cancel`, `tasks/pushNotificationConfig/*`. Streaming events are union types (one of `task | message | statusUpdate | artifactUpdate`). The registry (`~/Repos/Penguiflow/penguiflow/penguiflow_a2a/registry.py`) scores remote skills by tenant, trust tier, latency tier, capability match — the "MCP/A2A/HTTP appear as one tool source" abstraction expressed at the catalog layer.

**Tool-side HITL plumbing.** When a tool needs OAuth, the runtime emits a typed `tool.auth_required` event (auth URL, scopes, state), pauses the run via the runtime pause/resume primitive (brief 02 owns the protocol), waits for callback, stores the token in `TokenStore`, resumes. The reference's `OAuthManager` (`~/Repos/Penguiflow/penguiflow/penguiflow/tools/auth.py`, ~170 lines) is a clean inheritance target. Critical: the pause is **runtime-level, not planner-level** — the same primitive serves A2A's `TaskState.AUTH_REQUIRED`. Both transports converge.

**MCP transport auto-detect.** Stdio if connection looks like a command; SSE vs streamable-HTTP for URLs. Harbor inherits with one knob: `MCPTransportMode = Auto | SSE | StreamableHTTP`. URL connections require explicit headers for auth (no implicit env passthrough).

---

## 6. Tests Required

- **Unit tests:** catalog filtering (every combination of tenant/user/session/scopes); JSON-Schema derivation from Go types; argument validator dispatch; LLM message-part round-trips; cost calculation across providers; structured-output downgrade chain; provider-quirk normalizers (one test per documented quirk).
- **Integration tests:** in-process tool end-to-end; HTTP tool against `httptest.Server`; MCP tool against an in-process mock MCP server (stdio + HTTP); A2A tool against an in-process mock A2A server with the full Agent Card; OAuth tool full pause/resume cycle; mid-call cancellation propagates to all transports.
- **Contract tests:** every transport must pass the same `ToolProvider` conformance suite — same in/out types, same error mapping, same cancellation semantics.
- **LLM client conformance:** for each provider driver, the same generic suite (text completion, streaming, tool calling, structured output via each `OutputMode`, retry on rate-limit, cancellation).
- **Mock servers:** ship a small in-process mock MCP server and mock A2A server in `examples/mock-mcp/` and `examples/mock-a2a/`, used by both integration tests and the dev loop.

---

## 7. Phase Decomposition Suggestion

Sized for the "many phases is fine, never thin" stance. Each phase ships acceptance criteria + smoke check + docs.

- **T-1: Tool catalog core.** `Tool`, `ToolDescriptor`, `ToolCatalog`, `ToolProvider` interfaces; in-process registration via generics + reflection; JSON-Schema derivation; argument validator; `CatalogFilter` keyed on the (tenant, user, session) triple; protocol surface for catalog inspection.
- **T-2: HTTP tools.** UTCP-style manifest format; HTTP `ToolProvider` driver; static auth (API key, bearer, cookie); retry; rate-limit handling.
- **T-3: MCP southbound.** Go MCP client driver (stdio + streamable-HTTP + SSE); auto-detect transport; reconnect-on-failure; tool/resource/prompt mapping into `Tool`; resource subscriptions as a separate event topic.
- **T-4: A2A southbound (full spec).** Agent Card discovery; `message/send`, `message/stream` (SSE); `tasks/get`, `tasks/cancel`, `tasks/pushNotificationConfig/*`; registry with route scoring (trust tier, latency tier, capability match); A2A peers as `Tool` entries via `ToolProvider`.
- **T-5: A2A northbound (V1 candidate).** Expose Harbor as an A2A server so other agents can call us — same code path, opposite direction. Optional V1.
- **L-1: LLM client core.** `LLMClient` interface; `LLMRequest`/`LLMResponse`/`StreamEvent` types; cancellation token; streaming sink contract.
- **L-2: liter-llm integration.** Wire the Go LiteLLM client behind the interface; validate surface (streaming with reasoning deltas, tool calling, structured output, cancellation, cost). If insufficient → L-2-alt: provider-native SDKs.
- **L-3: Provider correction layer (one mode, baked in).** Per-provider drivers compiled into the single `LLMClient` — message reordering, schema normalization, reasoning-effort injection, OpenRouter quirks, usage backfill. No `use_native` toggle.
- **L-4: Structured output strategies.** `OutputMode = Native | Tools | Prompted`; `ModelProfile`; `SchemaPlan` with downgrade chain; `llm.mode_downgraded` events.
- **L-5: Retry with feedback.** Validation/parse failures feed back into the planner via `LLMClient` retry; observable; bounded.
- **H-1: Tool-side OAuth + HITL.** `TokenStore` interface (in-memory + SQLite + Postgres drivers); `OAuthProvider` config; `tool.auth_required` event; pause/resume integration; A2A `AUTH_REQUIRED` converges on the same primitive.
- **H-2: Tool-side approval gates.** Synchronous "approve this tool call" gates using the same pause/resume primitive — distinct from OAuth, simpler payload.

**12 phases** in this slice alone — right-sized for the "50 phases is fine" stance.

---

## 8. Cross-Subsystem Dependencies

- **Pause/resume protocol (brief 02).** Tool-side OAuth and approval gates depend on the runtime pause/resume primitive emitting `tool.auth_required` / `tool.approval_required` events and accepting a resume payload. The tool subsystem does not own this — it only emits the request and awaits the resume.
- **Isolation triple (`harbor_isolation.md`).** `CatalogFilter` keys on `(tenant, user, session)`. Every `ToolDescriptor.Invoke` receives a `RunContext` carrying the triple and propagates it into provider-specific transports (e.g. A2A `metadata.tenant`, MCP `_meta.tenant`).
- **Event bus (brief on events/streaming).** Every catalog mutation, every tool invocation, every LLM call emits typed events. The Console subscribes by topic + isolation triple.
- **Artifact store (brief on artifacts).** Heavy tool outputs route through the artifact store via `ArtifactRef` in `ToolResult`. The size threshold and binary-detection rules live in the tool subsystem; the storage lives in artifacts.
- **State store (brief on state).** `TokenStore` and the catalog cache (last-discovered descriptors per source) persist via the state store interface; in-memory + SQLite + Postgres drivers.
- **Planner (brief on planner).** The planner reads `[]Tool` views and the LLM client; never `ToolDescriptor`s, never raw provider bodies. This is enforced by visibility — planner package only imports the public catalog interface.

---

## 9. Open Questions for the User

1. **`liter-llm` surface validation.** Before locking the LLM client to liter-llm, confirm it covers: (a) streaming with separable text + reasoning deltas, (b) tool calling across all six provider families we care about (OpenAI, Anthropic, Google, Bedrock, Databricks, OpenRouter, NIM), (c) JSON-schema and JSON-object structured output with downgrade hooks, (d) cancellation, (e) cost reporting (or our own pricing table is fine). If any is missing, do we (a) add a thin shim, or (b) drop liter-llm and go provider-native?
2. **A2A protocol version.** The reference targets the A2A draft that defines `TaskState`, Agent Card with `protocol_versions`/`supported_interfaces`/`signatures`, and SSE streaming. Confirm Harbor V1 targets the same version and ships a compatibility matrix. Are extensions (`AgentExtension`) in V1 scope?
3. **HTTP tool definitions.** Do we ship inline HTTP tool definitions (Go code: `RegisterHTTPTool(name, method, urlTemplate, ...)`) or only out-of-process via a UTCP-style manifest file, or both?
4. **A2A northbound in V1?** Is exposing Harbor as an A2A *server* (so other agents can call us) in V1, or do we ship southbound only and add northbound post-V1?
5. **Tool-call audit redaction.** `harbor_isolation.md` and Portico AGENTS.md both forbid logging unredacted tool args/results. Where does the redactor live — tool subsystem (per-descriptor `Redact` hook) or audit subsystem (a single redactor over the event stream)? The latter is cleaner if the event payload is the canonical record.
