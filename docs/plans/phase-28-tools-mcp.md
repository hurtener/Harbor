# Phase 28 — MCP southbound driver

## Summary

Land Harbor's Model Context Protocol (MCP) southbound driver: a `ToolProvider` that connects to a remote MCP server over **stdio**, **SSE**, or **streamable-HTTP**, discovers the server's tools / resources / prompts, and maps them into Harbor `Tool` entries that the planner addresses the same as any in-process tool. Auto-detect picks a transport from the operator-configured `MCPTransportMode = Auto | SSE | StreamableHTTP` (with stdio reserved for `Command` configs). Resource subscriptions emit a new event topic `mcp.resource_updated` on the canonical event bus. Every MCP invocation flows through the `ToolPolicy` reliability shell (D-024) so timeouts / retries / validation are uniform with the in-process driver.

## RFC anchor

- RFC §6.4
- RFC §4

## Briefs informing this phase

- brief 03
- brief 07

## Brief findings incorporated

- **brief 03 §4 ("MCP transport auto-detect").** "Stdio if connection looks like a command; SSE vs streamable-HTTP for URLs. Harbor inherits with one knob: `MCPTransportMode = Auto | SSE | StreamableHTTP`. URL connections require explicit headers for auth (no implicit env passthrough)." Phase 28 implements the knob verbatim — `Auto` tries streamable-HTTP first (newer spec), then SSE, then stdio if a `Command` is configured; explicit `SSE` / `StreamableHTTP` modes skip auto-detect.
- **brief 03 §1 / §4 (cross-transport unification).** "MCP returns `TextContent | ImageContent | EmbeddedResource | ResourceLink`. … Harbor's normalizer is a layered pipeline." Phase 28 lowers MCP content into the `ToolResult.Value` shape: `TextContent` → string; `ImageContent` / `AudioContent` → typed `Image` / `Audio` content struct carrying base64 + MIME; `EmbeddedResource` → typed `EmbeddedResource`. Heavy-output routing through the artifact store is the next layer (Phase 32 + 33 LLM-edge enforcement); this phase preserves the typed shape on the way in.
- **brief 03 §6 ("Integration tests: MCP tool against an in-process mock MCP server (stdio + HTTP)").** The phase ships `mockserver_test.go` that spins up the SDK's `mcp.Server` against an `InMemoryTransport` pair (the SDK's native in-process test seam) AND an `httptest.Server` running `StreamableHTTPHandler` for transport-fallback coverage.
- **brief 07 §1 / §8 (code-level tool dispatch).** Tool dispatch is the runtime's job; the MCP driver does not invent new dispatch semantics — it produces `ToolDescriptor`s, the catalog handles everything else.
- **D-024 ("`ToolPolicy` reliability shell wraps every invocation").** The MCP driver's `Invoke` runs the wire call inside `tools.RunWithPolicy` so timeout / retry / classifier behaviour is identical to in-process / HTTP / A2A.

## Findings I'm departing from (if any)

- **brief 03 §4 ("reconnect-on-failure").** The Go MCP SDK's `StreamableClientTransport` already implements internal reconnect for the standalone SSE stream with exponential backoff. Stdio + SSE transports do not reconnect at the connection level — failed sessions surface as `ErrTransportFailed`. Operator-side, the recovery is `ToolPolicy`'s retry shell, which re-runs `Invoke` and the driver re-connects on demand. Documented under "Risks / open questions"; the alternative (a Phase 28-internal reconnection state machine duplicating what `ToolPolicy` already covers) is the "two parallel implementations" anti-pattern AGENTS.md §13 closes.

## Goals

- Ship `internal/tools/drivers/mcp/` as a `ToolProvider` driver discovering MCP servers' tools / resources / prompts and surfacing them as `Tool` entries.
- Wrap every transport (stdio / SSE / streamable-HTTP) behind a single `Transport` seam; selection driven by `MCPTransportMode = Auto | SSE | StreamableHTTP`.
- Map MCP `Tool` shapes to Harbor `Tool` (descriptor + JSON schema + scope tags); map MCP `Resource` and `Prompt` shapes to read-only Tool wrappers (`<source>__resource.<uri>`, `<source>__prompt.<name>`) so the planner addresses them through one catalog.
- Register `mcp.resource_updated` event type via `init()` in `internal/tools/drivers/mcp/events.go`; emit when the server sends a resource-update notification on a subscribed URI.
- Every Invoke runs inside `tools.RunWithPolicy` (D-024). Default `ToolPolicy` applies.
- D-025 concurrent reuse: a single `Provider` (and the descriptors it produces) supports ≥100 concurrent invocations under `-race` with no context bleed / cancellation cross-talk / goroutine leaks.
- Extend `ToolsConfig` with `MCPServers []MCPServerConfig` so operators declare MCP attachments at boot.
- Smoke gate `scripts/smoke/phase-28.sh` runs the package test suite under `-race`.

## Non-goals

- No HTTP tool driver (Phase 27 — adjacent, lands in parallel).
- No A2A southbound (Phase 29).
- No northbound MCP server (Harbor as MCP server) — V1.1.
- No tool-side OAuth flow (Phase 30 — `tool.auth_required` event consumes the unified pause/resume primitive). MCP server-side OAuth (`auth.OAuthHandler`) is reachable through `StreamableClientConfig.OAuthHandler`, but Phase 28 leaves the operator to wire it; Phase 30 closes the seam.
- No tool-side approval gates (Phase 31).
- No protocol surface (`GET /catalog/sources`, `POST /catalog/sources/{id}/refresh`) — Phase 60+.
- No automatic provider-level discovery refresh — operators call `Provider.Discover` on a cadence of their choosing; the periodic refresh loop is post-V1.

## Acceptance criteria

- [ ] `internal/tools/drivers/mcp/mcp.go` defines `Provider` (implements `tools.ToolProvider`), `Config` (`Name`, `Command` for stdio, `URL` for HTTP, `TransportMode`, `Headers`, `KeepAlive`), and `New(cfg) (*Provider, error)`.
- [ ] `internal/tools/drivers/mcp/transport_stdio.go`, `transport_sse.go`, `transport_streamable.go` wrap the SDK's `CommandTransport` / `SSEClientTransport` / `StreamableClientTransport` behind a `transport` seam — one selector decides which to instantiate.
- [ ] `internal/tools/drivers/mcp/auto.go` implements `MCPTransportMode = Auto | SSE | StreamableHTTP`, selecting transport from `Config`:
  - `Auto`: streamable-HTTP first when `URL` is set; on connect failure fall back to SSE; if `Command` set, stdio (only attempted when no `URL`).
  - `SSE` / `StreamableHTTP`: select directly; reject if `URL` is empty.
  - `Command` non-empty + no `URL` + `Auto`: stdio.
- [ ] `internal/tools/drivers/mcp/events.go` registers `mcp.resource_updated` and declares the typed payload `ResourceUpdatedPayload`.
- [ ] `Provider.Discover` returns one `ToolDescriptor` per MCP `Tool`, plus one per MCP `Resource` (mapped as `<sourceID>__resource.<uri>` — `read_resource`-style descriptor returning the resource contents) and one per MCP `Prompt` (mapped as `<sourceID>__prompt.<name>` — `get_prompt`-style descriptor returning the prompt messages).
- [ ] Every `ToolDescriptor.Invoke` runs the MCP RPC inside `tools.RunWithPolicy`. Default policy fires on zero-valued `Policy`.
- [ ] `Provider.Connect` establishes the session; `Provider.Close` shuts it down idempotently and joins the resource-update goroutine.
- [ ] MCP content-shape normalization: `TextContent` → string (concatenated); `ImageContent` → `ImageRef{Data, MIMEType}`; `AudioContent` → `AudioRef{Data, MIMEType}`; `ResourceLink` → `LinkRef{URI, Name, Title, MIMEType}`; `EmbeddedResource` → `EmbeddedRef{Resource}`. `IsError == true` results lower to a typed `ErrMCPToolError` wrapping the rendered content text.
- [ ] `Provider.SubscribeResource(ctx, uri)` calls the SDK's `Subscribe`; updates received via the SDK's `ResourceUpdatedHandler` publish `mcp.resource_updated` on the configured `events.EventBus`.
- [ ] Argument validation: the SDK's tool definition carries an `InputSchema`; the driver compiles it once at `Discover` time and uses the resulting `tools.ToolDescriptor.Validate` so invalid args fail at the catalog edge with `ErrToolInvalidArgs`.
- [ ] Identity propagation: `Provider.Discover` and every `Invoke` accept `ctx` and read identity via `identity.From`. The triple is forwarded as MCP `_meta.tenant / _meta.user / _meta.session` on every `CallTool` / `ReadResource` / `GetPrompt` request.
- [ ] Concurrent-reuse test (D-025): N=100 concurrent `Invoke` against one `Provider` instance + one `*ToolDescriptor` under `-race`. No data races, no goroutine leaks, no context bleed.
- [ ] Transport-fallback test: a `Provider` with `Auto` mode against a stub URL that fails streamable-HTTP connect; the driver falls back to SSE and succeeds; observable via a logged `mcp.transport_fallback` audit attribute (or test-observable spy state).
- [ ] Security: when launching a stdio MCP server the driver uses `exec.Command(name, args...)` argv form ONLY. The `Config.Command` field is `[]string` ([0] is binary, [1:] are argv). No `sh -c`.
- [ ] `internal/config`: add `ToolsConfig` with `MCPServers []MCPServerConfig` (name / transport mode / URL / command / headers ref). Validator + example yaml + default.
- [ ] `internal/tools/conformancetest`: the existing suite passes when run against the MCP driver's bridge factory (the in-process mock MCP server provides a known set of tools; the conformance suite exercises identity propagation + invalid args + policy retry through the wire).
- [ ] Coverage on `internal/tools/drivers/mcp`: ≥ 80%. Coverage on `internal/config` (new fields): existing 100% baseline preserved.
- [ ] Smoke `scripts/smoke/phase-28.sh` runs the package's race-detector test pass and asserts OK ≥ 1.

## Files added or changed

- `internal/tools/drivers/mcp/mcp.go` (new) — `Provider` + `Config` + `New` + `Connect` / `Discover` / `Close` / `SourceID` + descriptor builder.
- `internal/tools/drivers/mcp/transport.go` (new) — `transport` seam.
- `internal/tools/drivers/mcp/transport_stdio.go` (new) — stdio binding to the SDK's `CommandTransport`.
- `internal/tools/drivers/mcp/transport_sse.go` (new) — SSE binding to the SDK's `SSEClientTransport`.
- `internal/tools/drivers/mcp/transport_streamable.go` (new) — streamable-HTTP binding to the SDK's `StreamableClientTransport`.
- `internal/tools/drivers/mcp/auto.go` (new) — `MCPTransportMode` + auto-detect logic.
- `internal/tools/drivers/mcp/events.go` (new) — `mcp.resource_updated` registration + typed payload.
- `internal/tools/drivers/mcp/content.go` (new) — MCP `Content` → Harbor `ToolResult.Value` normaliser.
- `internal/tools/drivers/mcp/mockserver_test.go` (new) — in-process mock MCP server for integration tests.
- `internal/tools/drivers/mcp/mcp_test.go` (new) — unit + integration + concurrent-reuse + transport-fallback tests.
- `internal/config/config.go` (modified) — new `ToolsConfig` + `MCPServerConfig` types in the `Config` root.
- `internal/config/validate.go` (modified) — validator for the new section.
- `internal/config/loader.go` (modified) — defaults.
- `examples/harbor.yaml` (modified) — example `tools.mcp_servers` block.
- `scripts/smoke/phase-28.sh` (new — already templated).
- `docs/plans/phase-28-tools-mcp.md` (this file).
- `docs/plans/README.md` (modified) — flip Phase 28 row Status to Shipped + update detail block to reflect the SDK choice.
- `docs/decisions.md` (modified) — D-034 entry: "MCP southbound driver wraps `github.com/modelcontextprotocol/go-sdk@v1.6.0`; transport-reconnect lives in `ToolPolicy`, not in a parallel state machine."
- `docs/glossary.md` (modified) — add `MCPTransportMode`, `mcp.resource_updated`.
- `README.md` (modified) — Status table + tool driver list.
- `go.mod` / `go.sum` (modified) — add `github.com/modelcontextprotocol/go-sdk v1.6.0`.

## Public API surface

```go
package mcp

// MCPTransportMode picks the wire transport. Auto inspects Config
// to decide; SSE / StreamableHTTP / Stdio are explicit selections.
type MCPTransportMode string

const (
    TransportAuto           MCPTransportMode = "auto"
    TransportSSE            MCPTransportMode = "sse"
    TransportStreamableHTTP MCPTransportMode = "streamable_http"
    TransportStdio          MCPTransportMode = "stdio"
)

// Config configures one MCP attachment. Operator-supplied at boot.
type Config struct {
    Name          string            // descriptor source ID prefix; must be unique
    TransportMode MCPTransportMode
    URL           string            // for SSE / streamable-HTTP (required for both)
    Command       []string          // for stdio — argv form ONLY (no shell)
    Headers       map[string]string // HTTP auth headers (no implicit env passthrough)
    KeepAlive     time.Duration     // ping interval; zero = disabled
    Logger        *slog.Logger      // optional; defaults to discard logger
    Bus           events.EventBus   // mandatory — receives mcp.resource_updated
}

// New constructs a Provider. Returns ErrInvalidConfig on a malformed
// Config (missing URL when mode requires it, missing Command for stdio,
// etc.). The Provider is not connected; call Connect.
func New(cfg Config) (*Provider, error)

// Provider implements tools.ToolProvider. Safe for concurrent use after
// Connect returns (D-025).
type Provider struct { /* unexported */ }

func (p *Provider) Connect(ctx context.Context) error
func (p *Provider) Discover(ctx context.Context) ([]tools.ToolDescriptor, error)
func (p *Provider) Close(ctx context.Context) error
func (p *Provider) SourceID() tools.ToolSourceID

// SubscribeResource registers a server-side resource subscription;
// updates emit mcp.resource_updated on the configured event bus.
func (p *Provider) SubscribeResource(ctx context.Context, uri string) error

// Sentinels.
var (
    ErrInvalidConfig     = errors.New("mcp: invalid config")
    ErrTransportFailed   = errors.New("mcp: transport failed")
    ErrNotConnected      = errors.New("mcp: provider not connected")
    ErrMCPToolError      = errors.New("mcp: server returned tool error")
)

// EventTypeMCPResourceUpdated is the canonical event type for
// resource subscription notifications.
const EventTypeMCPResourceUpdated events.EventType = "mcp.resource_updated"

// ResourceUpdatedPayload is the typed payload for
// EventTypeMCPResourceUpdated. SafePayload by construction (no
// user-controlled bytes survive; the URI is operator-trust-equivalent
// since it comes from an operator-configured MCP server).
type ResourceUpdatedPayload struct {
    events.SafeSealed
    Identity   identity.Quadruple
    Source     tools.ToolSourceID
    URI        string
    OccurredAt time.Time
}
```

```go
package config

// ToolsConfig is owned by the tools subsystem phases. Phase 28 adds
// MCPServers; later phases (HTTP / A2A) add their own slots.
type ToolsConfig struct {
    MCPServers []MCPServerConfig `yaml:"mcp_servers,omitempty"`
}

type MCPServerConfig struct {
    Name          string            `yaml:"name"`
    TransportMode string            `yaml:"transport_mode"` // "auto" | "sse" | "streamable_http" | "stdio"
    URL           string            `yaml:"url,omitempty"`
    Command       []string          `yaml:"command,omitempty"`
    Headers       map[string]string `yaml:"headers,omitempty" secret:"true"`
    KeepAlive     time.Duration     `yaml:"keep_alive,omitempty"`
}
```

## Test plan

- **Unit:** transport-mode resolution (`MCPTransportMode` → concrete transport per Config), Config validator, content-shape normaliser (`TextContent` / `ImageContent` / `ResourceLink` / `EmbeddedResource` round-trip).
- **Integration:** in-package end-to-end against an in-process `mcp.Server` over `InMemoryTransport`: `Discover` returns mapped tools; `Invoke` round-trips with identity propagation; `Subscribe`-then-`ResourceUpdated` emits `mcp.resource_updated` on the bus.
- **Conformance:** `internal/tools/conformancetest.Run(t, factory)` — the factory wires a catalog with the MCP `Provider` attached so the suite runs over the wire. Identity propagation + invalid args + policy retry tests cover the seam.
- **Concurrency / leak:** `TestProvider_ConcurrentReuse_D025` — N=100 concurrent `Invoke` against one shared `Provider` + one `*ToolDescriptor` under `-race`; baseline-restored goroutine count.
- **Transport-fallback:** `TestProvider_Auto_FallsBackToSSE` — a streamable-HTTP-rejecting `httptest.Server` + a working SSE `httptest.Server`; Auto mode connects via SSE; observable via test-visible state.
- **Security:** assert that the stdio transport never invokes a shell — the test rejects any `Command` field with a single-string entry that contains shell metacharacters; argv-form-only.

## Smoke script additions

- `scripts/smoke/phase-28.sh`: `go test -race -count=1 -timeout 180s ./internal/tools/drivers/mcp/...` → OK; skip the HTTP/Protocol surface stub.

## Coverage target

- `internal/tools/drivers/mcp`: 80%.

## Dependencies

- Phase 01 (identity ctx propagation).
- Phase 02 (config loader).
- Phase 05 (events bus + `RegisterEventType`).
- Phase 26 (tool catalog + ToolPolicy + ToolProvider interface).

## Risks / open questions

- **SDK version pinning.** Pinned `github.com/modelcontextprotocol/go-sdk v1.6.0`; SDK floor Go 1.25 ≤ Harbor floor Go 1.26 (compatible). The SDK's public surface (`Client.Connect`, `ClientSession.{CallTool, ListTools, ListResources, ReadResource, ListPrompts, GetPrompt, Subscribe}`) is stable; transport types (`CommandTransport`, `SSEClientTransport`, `StreamableClientTransport`) are stable. Bumps should be deps PRs with conformance suite re-run.
- **Transport-level reconnect.** Only `StreamableClientTransport` reconnects internally (its standalone SSE stream). Stdio + SSE failures surface as `ErrTransportFailed`; `ToolPolicy`'s retry shell handles the connection-rebuild dance. Documented under "Findings I'm departing from" above so the absence of a per-driver reconnect state machine is intentional.
- **MCP resource → Tool naming collisions.** `<sourceID>__resource.<uri>` is the documented shape; the `__` separator prevents collision with operator tool names. Same for `__prompt.`.
- **Content size routing.** MCP `EmbeddedResource` can carry blobs ≥ the heavy-output threshold (D-022). Phase 28 preserves the typed shape; Phase 33 LLM-edge enforcement (D-026) rejects the bytes if they reach `LLMClient` un-materialized. The mandatory routing happens in the LLM client layer, not in this driver.
- **MCP `Subscribe` lifecycle.** Subscriptions persist for the session. `Provider.Close` MUST cancel the goroutine reading update notifications so it doesn't leak; tested by the concurrent-reuse goroutine-baseline check.

## Glossary additions

- **`MCPTransportMode`** — `auto` / `sse` / `streamable_http` / `stdio`. Selects the wire transport for an MCP southbound attachment. `auto` tries streamable-HTTP first, then SSE, then stdio. Phase 28.
- **`mcp.resource_updated`** — canonical event type emitted when an MCP server pushes a resource-update notification for a previously-subscribed URI. SafePayload. Phase 28.
- **`ToolsConfig`** — operator-supplied configuration block for the tools subsystem; ships `MCPServers []MCPServerConfig` at Phase 28; later phases add `HTTPTools` (Phase 27), `A2APeers` (Phase 29).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (covered by `TestProvider_ConcurrentReuse_D025` + identity propagation conformance assertion)
- [ ] **Concurrent-reuse test passes (D-025).** N≥100 concurrent invocations against one `Provider` instance under `-race`.
- [ ] **Integration test exists** — `mockserver_test.go` wires a real `mcp.Server` end-to-end over `InMemoryTransport`; identity propagates; failure mode covered (invalid args + transport disconnect).
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (D-034)
