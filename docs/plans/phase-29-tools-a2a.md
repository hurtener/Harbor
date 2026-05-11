# Phase 29 — A2A southbound driver (full spec)

## Summary

Land the wire driver for `distributed.RemoteTransport` against the full A2A v1 spec. Phase 22 froze the Go-shape contract surface (every A2A RPC maps 1:1 to a `RemoteTransport` method); Phase 29 ships the actual JSON-RPC-over-HTTP + SSE binding plus Agent Card discovery (`GET <peer>/.well-known/agent-card.json`), a route-scoring registry, and a `ToolProvider` adapter so each remote peer's `AgentSkill` lands in the catalog as a `Tool` with `Transport: TransportA2A`. The wire driver self-registers under the `"a2a"` driver name in the distributed registry; the existing `internal/distributed/conformancetest.RunRemoteTransport` suite gates the implementation.

## RFC anchor

- RFC §6.4
- RFC §6.12
- RFC §3.5
- RFC §4

## Briefs informing this phase

- brief 03
- brief 05

## Brief findings incorporated

- **brief 03 §5 ("A2A wire shape — worth inheriting").** "Discovery via Agent Card at `GET /.well-known/agent-card.json`. JSON-RPC dispatch: `message/send` (blocking), `message/stream` (SSE), `tasks/get`, `tasks/cancel`, `tasks/pushNotificationConfig/*`. Streaming events are union types (one of `task | message | statusUpdate | artifactUpdate`). The registry … scores remote skills by tenant, trust tier, latency tier, capability match." Phase 29 implements each clause: the wire client speaks JSON-RPC; SSE drives streaming; the registry scores by `(trust_tier, latency_tier, capability_match)`; the streaming-response union maps to `a2a.StreamResponse`.
- **brief 03 §7 (T-4 phase scope).** "A2A southbound (full spec). Agent Card discovery; `message/send`, `message/stream` (SSE); `tasks/get`, `tasks/cancel`, `tasks/pushNotificationConfig/*`; registry with route scoring (trust tier, latency tier, capability match); A2A peers as `Tool` entries via `ToolProvider`." Phase 29 ships exactly this slice. The northbound (T-5) remains a V1.1 candidate.
- **brief 05 §2 + §5 ("Distributed contracts ship without backends").** Phase 22 froze the contract surface; Phase 29 lands the in-network driver that lets a Harbor runtime call a remote A2A peer over real HTTPS without changing any `RemoteTransport` consumer.
- **D-007 — A2A: full spec compliance from V1.** Every A2A RPC is mapped on the wire — no "we implement the subset our planner uses." The conformance suite (`internal/distributed/conformancetest.RunRemoteTransport`) is the gate.
- **D-024 — `ToolPolicy` reliability shell wraps every invocation, regardless of transport.** The tools-side adapter routes each A2A skill through the catalog's standard `RunWithPolicyHooked` shell. The wire driver itself does NOT add its own retry/timeout shell — that would double-wrap.
- **D-025 — Concurrent reuse contract.** Both the wire `RemoteTransport` and the `ToolProvider` are constructed once and shared across N concurrent invocations; per-invocation state lives in `ctx`. The conformance suite's `Concurrent_Send_NoRace` covers the transport; an additional `TestRegistry_ConcurrentResolve_NoRace` covers the route-scoring registry's read path.
- **D-031 — Distributed contracts: full A2A v1 surface mapping + loopback V1 driver; vendored proto pinned by commit SHA.** Phase 29 inherits the Go shapes from Phase 22's `internal/distributed/a2a/types.go`; no new shapes are introduced. The vendored proto remains the source of truth for method names + URL templates.

## Findings I'm departing from (if any)

- **None.** Phase 29 lands the slice brief 03 §7 prescribes. Two design choices that are NOT departures but warrant calling out so a later auditor doesn't flag them:
  - **Wire binding = JSON-RPC, not gRPC.** The proto carries both `service A2AService { rpc … }` (gRPC) AND `google.api.http` annotations (HTTP+JSON). Phase 29 implements the JSON-RPC binding per the master-plan Phase 29 detail block and brief 03 §5; gRPC is deferred to a post-V1 phase. The driver's `AgentInterface.ProtocolBinding` match MUST equal `"JSONRPC"` (the canonical Phase 22 constant `a2a.ProtocolBindingJSONRPC`); gRPC and HTTP+JSON bindings on the same peer's AgentCard are read-only metadata until those drivers ship.
  - **Push-notification config store is in-memory at V1.** A wire driver could in principle accept these CRUD calls from another process via the northbound surface, but Phase 29 is southbound-only — calls to `Create/Get/List/DeleteTaskPushNotificationConfig` simply forward to the *peer* (no local mirror) and the peer is responsible for durability. A new decision entry (D-034) is filed below to make this load-bearing.

## Goals

- Ship `internal/distributed/drivers/a2a/` as the wire driver implementing `distributed.RemoteTransport`.
- Ship `internal/tools/drivers/a2a/` as the `ToolProvider` integration that registers remote `AgentSkill`s as `Tool` entries with `Transport: TransportA2A`.
- Implement Agent Card discovery via `GET <peer>/.well-known/agent-card.json`, including TTL-bounded caching and forced re-fetch on schema mismatch.
- Implement JSON-RPC 2.0 over HTTPS for the eleven A2A RPCs (`message/send`, `message/stream`, `tasks/get`, `tasks/list`, `tasks/cancel`, `tasks/subscribe`, `tasks/pushNotificationConfig/set | get | list | delete`, `agent/getAuthenticatedExtendedCard`).
- Implement SSE handling for `message/stream` + `tasks/subscribe`. Honor `ctx.Done()` so a cancellation closes the underlying HTTP response body cleanly.
- Implement a route-scoring registry that maps `(capability) → ordered-list-of-(peer-url, trust-tier, latency-tier-estimate, capability-match-score)` and picks the highest-scoring candidate.
- HTTPS-only by default. HTTP is allowed only when the peer URL host matches `127.0.0.1`, `::1`, `localhost`, or an operator-configured allowlist (`A2APeerConfig.AllowInsecureLoopback`).
- Caller-side URL allowlist enforcement: a peer that is not in `ToolsConfig.A2APeers` is rejected with `ErrPeerNotAllowed`.
- Audit redaction discipline: the driver never logs unredacted Agent Cards (security schemes / credentials are masked) and never logs request/response bodies (audit redaction runs at the catalog edge per D-020).
- Wire the wire driver into `cmd/harbor/main.go` via blank import so `distributed.OpenRemoteTransport` resolves `"a2a"` after `init()`.
- Extend `DistributedConfig.RemoteDriver` allowlist to include `"a2a"`.
- Add `ToolsConfig` with `A2APeers []A2APeerConfig` (peer base URL, trust tier, latency tier hint, allowlist flags, optional AgentCard TTL override). Validator + example yaml.
- Pass the existing `internal/distributed/conformancetest.RunRemoteTransport` suite against the wire driver using an in-process `httptest.Server` mock A2A server (with the full Agent Card).
- Concurrent-reuse test (D-025): ≥100 concurrent `Send` calls against a shared wire driver under `-race`.

## Non-goals

- **A2A northbound.** Exposing Harbor as an A2A *server* (so other agents can call us) remains the V1.1 candidate per RFC §6.4. Phase 29 ships the *consumer* (southbound) only.
- **gRPC binding.** The proto's gRPC stubs are not implemented. Bindings other than `"JSONRPC"` in a peer's AgentCard are accepted as read-only metadata; if no `JSONRPC` interface is declared, `Connect` fails loudly with `ErrNoJSONRPCInterface`.
- **HTTP+JSON binding.** Same as gRPC — the proto's `google.api.http` annotations are surfaced through the Go shapes but the wire client speaks JSON-RPC over POST per Phase 29's detail block.
- **Tool-side OAuth / `AUTH_REQUIRED` pause.** Phase 30 wires this on the unified pause/resume primitive. Phase 29 surfaces `TaskStateAuthRequired` cleanly in the streaming response but does NOT initiate the OAuth flow.
- **Durable push-notification config storage.** Per the master-plan Phase 29 detail block, "store push-notification configs in-memory at V1"; durable storage is a Phase 23 / 15 / 16 concern.
- **Outbound push-notification dispatch.** A2A peers may POST against a configured callback URL; the inbound surface is V1.1 (northbound).
- **Auto-discovery of peers.** Operators declare peers in `ToolsConfig.A2APeers`; no DNS-SD / `/.well-known/_agents/` directory scan.
- **Result normalisation against `ArtifactStore`.** Heavy outputs from a remote A2A peer SHOULD route through the artifact store; that wiring lives at the tool dispatcher layer (Phase 42+ planner-side) which already speaks `ToolResult` and the heavy-output threshold from `ArtifactsConfig.HeavyOutputThresholdBytes`. The A2A driver returns the raw `a2a.Task` / `StreamResponse` shapes; the upstream caller maps them.

## Acceptance criteria

- [ ] `internal/distributed/drivers/a2a/a2a.go` defines the wire driver implementing `distributed.RemoteTransport`. Self-registers via `init()` under `"a2a"`.
- [ ] `internal/distributed/drivers/a2a/transport_jsonrpc.go` implements JSON-RPC 2.0 over HTTPS. Methods covered: `message/send`, `tasks/get`, `tasks/list`, `tasks/cancel`, `tasks/pushNotificationConfig/set`, `tasks/pushNotificationConfig/get`, `tasks/pushNotificationConfig/list`, `tasks/pushNotificationConfig/delete`, `agent/getAuthenticatedExtendedCard`.
- [ ] `internal/distributed/drivers/a2a/transport_sse.go` implements SSE consumption for `message/stream` + `tasks/subscribe`. Each SSE `data:` line is parsed into an `a2a.StreamResponse`.
- [ ] `internal/distributed/drivers/a2a/agentcard.go` implements `GET <peer>/.well-known/agent-card.json` with TTL-bounded caching (`AgentCardTTL`, default 10 minutes) and re-fetch on schema mismatch.
- [ ] `internal/distributed/drivers/a2a/registry.go` implements the route-scoring registry. Score formula: `score = (TrustWeight × trust_tier) + (LatencyWeight / max(1, latency_tier_ms)) + (CapabilityWeight × capability_match)` with constants documented in package godoc + this plan's "Risks / open questions". Tie-breaker: lower latency wins.
- [ ] `internal/tools/drivers/a2a/a2a.go` implements `tools.ToolProvider`. `Connect` fetches the AgentCard via the wire driver; `Discover` materialises each `AgentSkill` as a `ToolDescriptor` with `Transport: TransportA2A`; `Close` calls the wire driver's `Close`.
- [ ] HTTPS-only enforcement: a peer URL whose scheme is `http://` AND whose host is NOT loopback (and not operator-allowlisted) is rejected with `ErrInsecureScheme` at `Connect` time. The check lives in `internal/distributed/drivers/a2a/security.go`.
- [ ] `DistributedConfig.RemoteDriver` validator accepts `"a2a"` (alongside `"loopback"`).
- [ ] `ToolsConfig` struct introduced; `ToolsConfig.A2APeers []A2APeerConfig` with `URL`, `TrustTier`, `LatencyTierMS`, `AllowInsecureLoopback`, `AgentCardTTL`. Validator rejects malformed URLs + non-positive tiers.
- [ ] `examples/harbor.yaml` (or sibling) documents a sample `A2APeers` entry.
- [ ] `cmd/harbor/main.go` blank-imports both `internal/distributed/drivers/a2a` and `internal/tools/drivers/a2a` so `init()` registrations fire.
- [ ] The existing `internal/distributed/conformancetest.RunRemoteTransport` suite passes against the wire driver using a mock A2A server (`internal/distributed/drivers/a2a/mockserver_test.go` builds the server via `httptest.NewServer`; HTTPS is enforced for non-loopback peers — the mock is loopback so HTTP is accepted).
- [ ] **Concurrent-reuse test (D-025).** ≥100 concurrent `Send` calls + ≥50 concurrent `Stream` reads on a single shared `RemoteTransport` instance under `-race`. Identity isolation asserted via per-request triple checks server-side.
- [ ] Identity propagation: every JSON-RPC request carries the `tenant` path parameter from `identity.From(ctx)`; missing identity surfaces as `ErrIdentityRequired` at the caller boundary.
- [ ] Streaming respects `ctx.Done()`: cancelling the parent ctx closes the response body and `Recv` returns `ctx.Err()` promptly (`< 500ms` under typical load).
- [ ] Smoke `scripts/smoke/phase-29.sh` runs `go test -race -count=1 -timeout 180s ./internal/distributed/drivers/a2a/... ./internal/tools/drivers/a2a/...` and asserts OK ≥ 1.
- [ ] `docs/decisions.md` D-034 entry filed for the route-scoring weights + the in-memory push-config-store decision.
- [ ] `docs/glossary.md` adds `A2A peer`, `Route scoring`, `Agent Card cache`.
- [ ] `docs/plans/README.md` Phase 29 row flips from `Pending` → `Shipped`.
- [ ] `README.md` Status table updated.

## Files added or changed

- `internal/distributed/drivers/a2a/a2a.go` (new) — wire driver entry point + `init()` registration.
- `internal/distributed/drivers/a2a/transport_jsonrpc.go` (new) — JSON-RPC client.
- `internal/distributed/drivers/a2a/transport_sse.go` (new) — SSE client.
- `internal/distributed/drivers/a2a/agentcard.go` (new) — discovery + cache.
- `internal/distributed/drivers/a2a/registry.go` (new) — route-scoring registry.
- `internal/distributed/drivers/a2a/security.go` (new) — URL allowlist + HTTPS-only guard.
- `internal/distributed/drivers/a2a/errors.go` (new) — sentinel errors.
- `internal/distributed/drivers/a2a/a2a_test.go` (new) — unit tests + conformance gate.
- `internal/distributed/drivers/a2a/mockserver_test.go` (new) — in-process JSON-RPC + SSE mock A2A server.
- `internal/distributed/drivers/a2a/registry_test.go` (new) — route-scoring unit tests + concurrent reuse.
- `internal/distributed/drivers/a2a/agentcard_test.go` (new) — TTL + re-fetch tests.
- `internal/distributed/drivers/a2a/security_test.go` (new) — scheme allowlist tests.
- `internal/tools/drivers/a2a/a2a.go` (new) — `ToolProvider` integration.
- `internal/tools/drivers/a2a/a2a_test.go` (new) — integration tests against the mock A2A server.
- `internal/config/config.go` (modified) — add `ToolsConfig` + `A2APeerConfig`.
- `internal/config/loader.go` (modified) — defaults for `ToolsConfig` (empty `A2APeers` list is valid).
- `internal/config/validate.go` (modified) — `validateTools` + extend `allowedDistributedRemoteDrivers` to include `"a2a"`.
- `internal/config/validate_test.go` (modified) — coverage for the new validator branches.
- `examples/harbor.yaml` (modified) — optional `tools.a2a_peers` example block.
- `cmd/harbor/main.go` (modified) — blank-import the two new drivers.
- `docs/plans/phase-29-tools-a2a.md` (this file).
- `docs/plans/README.md` (modified) — flip Phase 29 row to `Shipped`.
- `docs/decisions.md` (modified) — new D-034 entry (route-scoring weights + push-config V1 in-mem).
- `docs/glossary.md` (modified).
- `README.md` (modified) — Status table.
- `scripts/smoke/phase-29.sh` (new) — package-test smoke.

## Public API surface

```go
package a2a // internal/distributed/drivers/a2a

// Driver name registered with distributed.RegisterRemoteTransport.
const DriverName = "a2a"

// New constructs a wire RemoteTransport. Exposed for tests; production
// wiring goes through distributed.OpenRemoteTransport.
func New(deps distributed.Dependencies, opts ...Option) (distributed.RemoteTransport, error)

// Option configures the driver.
type Option func(*config)

// WithHTTPClient overrides the default *http.Client (timeout, transport tuning).
func WithHTTPClient(c *http.Client) Option
// WithAgentCardTTL overrides the AgentCard cache TTL (default 10min).
func WithAgentCardTTL(d time.Duration) Option
// WithRegistry seeds the route registry with a fixed peer list.
func WithRegistry(r *Registry) Option

// Registry maps capability → ordered (peer, score) candidates.
type Registry struct{ /* internally synchronized */ }
func NewRegistry() *Registry
func (r *Registry) AddPeer(p PeerSpec) error
func (r *Registry) Resolve(capability string) ([]Route, error)

// PeerSpec is a discovered peer's contact info + tier hints.
type PeerSpec struct {
    URL                   string
    TrustTier             int            // 1 (untrusted) .. 5 (first-party)
    LatencyTierMS         int            // operator-supplied hint
    AllowInsecureLoopback bool
    AgentCardTTL          time.Duration  // overrides driver-level default
    Capabilities          []string       // discovered AgentSkill.Tags + AgentSkill.ID
}

// Route is a scored candidate returned by Registry.Resolve.
type Route struct {
    PeerURL          string
    TrustTier        int
    LatencyTierMS    int
    CapabilityScore  int
    CompositeScore   float64
}

// Sentinels.
var (
    ErrInsecureScheme         error // peer URL is http:// and host not loopback / allowlisted
    ErrPeerNotAllowed         error // peer URL not registered with the driver
    ErrNoJSONRPCInterface     error // peer's AgentCard declares no JSONRPC AgentInterface
    ErrAgentCardSchemaInvalid error // discovered card is malformed
    ErrJSONRPCError           error // peer returned a JSON-RPC error envelope
    ErrSSEStreamMalformed     error // SSE frame did not parse to a StreamResponse
)
```

```go
package a2a // internal/tools/drivers/a2a

// Provider implements tools.ToolProvider for an A2A peer.
type Provider struct{ /* immutable after construction */ }

// New constructs a Provider backed by an a2a wire RemoteTransport.
//
// `peerURL` is the A2A peer's base URL (matches an A2APeerConfig.URL).
// `transport` is the wire RemoteTransport (typically obtained via
// distributed.OpenRemoteTransport(... cfg{RemoteDriver: "a2a"})).
func New(peerURL string, transport distributed.RemoteTransport, opts ...Option) (*Provider, error)

// Option configures the Provider.
type Option func(*config)
// WithDescriptorOptions injects per-skill option helpers passed to the inner tool registration.
func WithDescriptorOptions(opts ...tools.DescriptorOption) Option
```

```go
// internal/config additions.
package config

// ToolsConfig configures the unified tool catalog's transport drivers.
// V1 ships only the A2APeers list; HTTP / MCP peer configs land in
// Phases 27 / 28.
type ToolsConfig struct {
    A2APeers []A2APeerConfig `yaml:"a2a_peers,omitempty"`
}

// A2APeerConfig declares an A2A peer the southbound driver may connect
// to. URL is required; the driver rejects HTTP schemes unless the host
// is loopback or AllowInsecureLoopback is true.
type A2APeerConfig struct {
    URL                   string        `yaml:"url"`
    TrustTier             int           `yaml:"trust_tier"`
    LatencyTierMS         int           `yaml:"latency_tier_ms"`
    AllowInsecureLoopback bool          `yaml:"allow_insecure_loopback,omitempty"`
    AgentCardTTL          time.Duration `yaml:"agent_card_ttl,omitempty"`
}
```

## Test plan

- **Unit:**
  - JSON-RPC request envelope build / parse (request shape, error envelope, batch rejection).
  - SSE frame parsing (single event, multi-line `data:`, comment lines, terminator).
  - AgentCard cache TTL expiry + re-fetch on schema mismatch.
  - Route-scoring math: monotonic on `trust_tier`; tie-breaker on `latency`.
  - URL scheme enforcement: `http://localhost` accepted; `http://example.com` rejected; `https://example.com` accepted; allowlist override works.
  - Identity propagation: `tenant` path parameter populated from `identity.From(ctx)`.
- **Integration:** (in-package adapter tests)
  - `mockserver_test.go` builds a full AgentCard (per Phase 22's `a2a.AgentCard` shape) and serves the eleven JSON-RPC methods + the SSE streams. The conformance suite runs against it.
  - `internal/tools/drivers/a2a/a2a_test.go` registers a Provider against the mock server, calls `Discover`, verifies the catalog now lists each `AgentSkill` as a `Tool`; calls `Resolve` + `Invoke` end-to-end through the `ToolPolicy` shell.
- **Conformance:**
  - `internal/distributed/conformancetest.RunRemoteTransport` runs verbatim against the wire driver factory; the factory binds Agents on the mock A2A server.
- **Concurrency / leak (D-025):**
  - `TestWireTransport_ConcurrentSend_D025`: N=128 goroutines calling `Send` against a shared transport; assert no races, baseline goroutines restored after teardown.
  - `TestRegistry_ConcurrentResolve_NoRace`: N=128 goroutines reading from a single `Registry`; mutex contention measured ≤ 5%.
  - `TestAgentCardCache_ConcurrentFetch_Coalesces`: N concurrent first-time `Discover` calls collapse into one underlying HTTP GET (cache stampede prevention).

## Smoke script additions

- `scripts/smoke/phase-29.sh`: `go test -race -count=1 -timeout 180s ./internal/distributed/drivers/a2a/... ./internal/tools/drivers/a2a/...` → OK; skip the HTTP/Protocol surface stub.

## Coverage target

- `internal/distributed/drivers/a2a`: 80% (network code requires the mock server; targetting 80% leaves room for unreachable error branches).
- `internal/tools/drivers/a2a`: 80%.
- `internal/config` validator additions: covered by `validate_test.go` table-driven cases.

## Dependencies

- Phase 22 (RemoteTransport contracts + a2a Go shapes).
- Phase 26 (Tool / ToolDescriptor / ToolCatalog / ToolProvider + ToolPolicy + the tools-conformance suite).
- Phase 26a (Flow-as-Tool registrar). Not strictly consumed but its `RegisterAsTool` helper inspires the A2A Provider's `Discover → Register` shape.

## Risks / open questions

- **Route-scoring weights.** `CompositeScore = (5 × TrustTier) + (1000 / max(1, LatencyTierMS)) + (10 × CapabilityScore)`. Trust outranks latency for safety; latency is the tie-breaker among similarly-trusted peers; capability match is a bonus on top. Settled in this plan + filed as `D-034` so a later auditor doesn't churn it. Weights tunable post-V1; not in V1 scope.
- **SSE reconnect-on-failure.** Phase 29 does NOT auto-reconnect a closed SSE stream. The caller (planner runtime) decides reconnect policy via `ToolPolicy.MaxRetries` at the *call* level. Documented inline.
- **JSON-RPC error mapping.** Wire driver maps standard JSON-RPC errors (`-32700` parse / `-32600` invalid request / `-32601` method-not-found / `-32602` invalid-params / `-32603` internal) plus the A2A application errors documented in the spec. `ErrTaskNotFound` is recognised on application code `1` (per the A2A spec's `TaskNotFoundError`).
- **HTTP/1.1 vs HTTP/2 for SSE.** Go's stdlib `*http.Client` does HTTP/2 by default; SSE is HTTP/1.1-friendly but the spec is transport-agnostic. We use the default client — no forced downgrade.
- **Identity propagation via `tenant` path.** The proto's `additional_bindings` use `/{tenant}/…`; Phase 29 uses the path-parameterised form when `identity.From(ctx).TenantID != ""`. Empty tenants surface `ErrIdentityRequired` at the caller-side boundary BEFORE the HTTP request fires (consistent with Phase 22's contract).
- **Push-notification config storage.** V1 forwards CRUD to the peer (no local mirror); the peer is responsible for durability. Filed in `D-034`.
- **Agent Card cache staleness.** TTL is the only invalidation lever in V1; an operator who hot-swaps a peer's AgentCard must wait up to TTL or restart. Acceptable for V1 — adding a Console-driven invalidation surface is Phase 60+ work.

## Glossary additions

- **A2A peer** — a remote agent Harbor connects to as a *client* via the A2A protocol. Declared in `ToolsConfig.A2APeers`. Distinct from "A2A northbound", which is the not-yet-shipped server surface. RFC §6.4, D-007.
- **Agent Card cache** — the wire driver's in-memory TTL cache for `GET <peer>/.well-known/agent-card.json` responses. Default 10 minutes; per-peer override via `A2APeerConfig.AgentCardTTL`. RFC §6.4.
- **Route scoring** — the wire driver's deterministic selection of an A2A peer when more than one declares the same capability. Score formula documented at `internal/distributed/drivers/a2a/registry.go`. RFC §6.4, D-034.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes (Phase 22's conformance suite already covers this via `Send_PropagatesIdentityToAgent` + `Concurrent_Send_NoRace`)
- [ ] **If this phase builds a reusable artifact: concurrent-reuse test passes** — `TestWireTransport_ConcurrentSend_D025`, `TestRegistry_ConcurrentResolve_NoRace`, `TestAgentCardCache_ConcurrentFetch_Coalesces` all under `-race` (D-025)
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists** — `internal/tools/drivers/a2a/a2a_test.go` runs the full Provider → Discover → Resolve → Invoke path against the mock A2A server with real catalog + policy shell (Phase 26 surface) and real wire driver (Phase 22 surface)
- [ ] If new vocabulary: glossary updated (yes)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (none departed; route-scoring weights + push-config storage filed as new D-034)
