# Phase 26 + 26a — Tool catalog + ToolPolicy + Flow-as-Tool

## Summary

Land Harbor's unified tool catalog and reliability shell in a single PR. Phase 26 ships the `Tool` / `ToolDescriptor` / `ToolCatalog` / `ToolProvider` types, the in-process driver (`tools.RegisterFunc` with generics + reflection-derived JSON-Schemas), `CatalogFilter` keyed on the `(tenant, user, session)` triple plus `GrantedScopes`, argument validation at the catalog edge (`santhosh-tekuri/jsonschema/v6`), and the `ToolPolicy` reliability shell (D-024) wrapping every invocation with timeout / exponential-backoff retry / validation regardless of transport. Phase 26a builds `flow.Definition`, `flow.Compose(def) → Engine`, and `flow.RegisterAsTool(catalog, def, eng)` on top, so a 3-node Flow registers as a Tool whose schema reflects entry-input → exit-output; the per-flow `Budget` (D-023) composes with parent-run + identity-tier ceilings via `min()`, and exceedance emits `flow.budget_exceeded` + returns `ErrFlowBudgetExceeded`. The two phases ship together because the seam they share (`ToolPolicy` wrapping a tool dispatch whose transport is `Flow`) only works if a single author lands both halves consistently.

## RFC anchor

- RFC §6.4
- RFC §6.1
- RFC §3.5
- RFC §4

## Briefs informing this phase

- brief 03
- brief 07

## Brief findings incorporated

- **brief 03 §1 (planner sees ONE concept: a `Tool`).** "The unification is at the **type level** — every `Tool` is *the same struct* regardless of source, and the dispatch is one switch in one place." Phase 26 implements this verbatim: `Tool` is a value type; `Transport` discriminates source; `Dispatcher` is one switch.
- **brief 03 §2 (data shapes).** `Tool`, `ToolDescriptor`, `ToolCatalog`, `ToolProvider`, `CatalogFilter` lifted verbatim with the V1 shape; brief's `LLMClient` shape is rejected per D-010 — no `Tools`, `ToolChoice`, `FunctionCall` types in this PR.
- **brief 03 §3 (registration ergonomics).** "The registration helper uses generics + reflection to derive `ArgsSchema` and `OutSchema` from the `WeatherArgs` / `WeatherOut` types." Phase 26 implements `tools.RegisterFunc[I, O any](catalog, name, fn, opts...)` — input + output shapes derived from `I` / `O`; no operator-side schema authoring required.
- **brief 03 §4 (argument validation at the catalog edge).** Failures yield `ErrToolInvalidArgs` and the dispatcher's policy shell maps them to a typed `tool.invalid_args` event.
- **brief 07 §1 (code-level tool calling).** Tool calling at the runtime/orchestration layer, not the LLM provider layer. Phase 26 owns dispatch entirely.
- **brief 07 §8 (tool dispatcher is INSIDE the runtime).** Phase 26's `Dispatcher` is `internal/tools` — every `Tool.Invoke` is wrapped in the policy shell; the planner-step fan-out lives at a later phase but consumes the same `Tool.Invoke` surface.

For Phase 26a:

- **RFC §6.1 (Flow-as-Tool registration).** `flow.Definition` shape lifted verbatim (Entry / Exit / Nodes / Budget / InSchema / OutSchema).
- **RFC §6.1 + D-023 ("resilience composition").** Per-node retry / backoff / timeout / validation come from `NodePolicy` (already shipped in Phase 11). Per-flow caps come from `flow.Budget`. **No double-wrapping**: the dispatcher's `ToolPolicy` wraps the OUTER flow invocation; the per-node `NodePolicy` runs INSIDE the flow's engine.

## Findings I'm departing from (if any)

- **brief 03 §3 (`LLMRequest` carries `Tools []ToolSpec` + `ToolChoice`).** Rejected; D-010 settles code-level tool calling. The LLM client is one method and tool dispatch lives in the runtime.
- **brief 03 §7 ("12 phases in this slice alone").** Slimmed: T-1 (this phase) is one PR; HTTP / MCP / A2A drivers are deferred to Phases 27 / 28 / 29.

## Goals

- Ship a `Tool` / `ToolDescriptor` / `ToolCatalog` surface that the planner (Phase 42+) can read against, unified at the type level.
- Ship the **in-process driver** (`internal/tools/drivers/inproc/`) with `tools.RegisterFunc[I, O]` and reflection-derived schemas.
- Ship the **`ToolPolicy` reliability shell (D-024)**: timeout + exponential-backoff retry + validation regardless of transport. Defaults: 3 retries / 100ms→30s exponential backoff / 30s timeout / validate=both.
- Ship `CatalogFilter` keyed on the `(TenantID, UserID, SessionID)` triple plus `GrantedScopes` with deterministic visibility semantics.
- Ship argument validation at the catalog edge via `santhosh-tekuri/jsonschema/v6`.
- Ship the `flow.Definition` shape, `flow.Compose(def) → engine.Engine` builder, and `flow.RegisterAsTool(catalog, def, eng)` wiring with `Transport: TransportFlow`.
- Ship the per-flow `Budget` composition (`min(flow.Budget, parent.RunContext.Budget, identity-tier.Budget)`); exceedance emits `flow.budget_exceeded` and returns `ErrFlowBudgetExceeded`.
- Ship the conformance suite (`internal/tools/conformancetest/`) that future drivers consume verbatim.
- Concurrent-reuse contract per D-025: catalog, every Tool, the policy executor, and flow.Engine are reusable across N≥100 concurrent invocations.

## Non-goals

- No HTTP / MCP / A2A drivers (Phases 27 / 28 / 29).
- No planner-side dispatcher (`internal/runtime/dispatch/`) — Phase 42+.
- No tool-side OAuth / approval gates (Phase 30 / 31).
- No declarative YAML recipe loader for Flows (V1.1, Phase 100).
- No A2A northbound (V1.1 candidate).
- No identity-tier `Budget` enforcement — Phase 36a wires Governance.

## Acceptance criteria

### Phase 26

- [ ] `internal/tools/tools.go` defines `Tool`, `ToolDescriptor`, `ToolCatalog`, `ToolProvider`, `CatalogFilter`, `TransportKind`, `SideEffect`, `LoadingMode`, `ToolExample`, `ToolSourceID`, `ToolResult`. Sentinels: `ErrToolNotFound`, `ErrToolInvalidArgs`, `ErrToolPolicyExhausted`, `ErrToolDuplicateName`.
- [ ] `internal/tools/policy.go` defines `ToolPolicy` + `RunWithPolicy(...)` executor. Default zero-value yields 3-retry / 100ms→30s exponential backoff (mult=2) / 30s timeout / Validate=ValidateBoth / RetryOn=[transient, timeout, 5xx].
- [ ] `internal/tools/events.go` registers `tool.invoked`, `tool.completed`, `tool.failed`, `tool.invalid_args`, `tool.policy_exhausted` event types.
- [ ] `internal/tools/catalog.go` — concrete in-memory `ToolCatalog`.
- [ ] `internal/tools/drivers/inproc/inproc.go` — `RegisterFunc[I, O]` with reflection-derived schemas.
- [ ] `internal/tools/conformancetest/conformancetest.go` — shared suite covering register/resolve/list/filter/policy/cancellation/identity-propagation/D-025 concurrent reuse.
- [ ] Default `ToolPolicy` produces 3-retry / 100ms→30s exponential backoff / 30s timeout shell on transient errors.
- [ ] `tools.WithPolicy(...)` overrides each axis.
- [ ] Identity propagation: every `ToolDescriptor.Invoke` reads identity from `ctx`.
- [ ] Validation failure wraps `ErrToolInvalidArgs`.
- [ ] **Concurrent-reuse test (D-025)**: N≥100 concurrent `Invoke` against a shared `ToolDescriptor` with a misbehaving stub; no races, no goroutine leaks, no context bleed.

### Phase 26a

- [ ] `internal/runtime/flow/flow.go` defines `Definition`, `NodeSpec`, `NodeID`, `Budget`, `Compose(def Definition) (engine.Engine, error)`, `RegisterAsTool(cat tools.ToolCatalog, def Definition, eng engine.Engine) (tools.Tool, error)`. Sentinels: `ErrFlowBudgetExceeded`, `ErrFlowInvalidDefinition`, `ErrFlowEntryExitMismatch`.
- [ ] `internal/runtime/flow/events.go` registers `flow.budget_exceeded` + typed `BudgetExceededPayload`.
- [ ] `Budget` composition math: `min(self, parent)` per axis (Deadline / HopBudget / CostCap), zero-value inherits parent. The accumulator is **lock-free** (`atomic.Int64`).
- [ ] A 3-node flow registers as a Tool with derived `Tool.ArgsSchema` / `OutSchema` via `WithSchemasFrom[I, O]`.
- [ ] Per-flow budget exceedance emits `flow.budget_exceeded` and returns `ErrFlowBudgetExceeded`.
- [ ] Parent ctx deadline (via `WithBudget` or directly) propagates into the flow and aborts it.
- [ ] **No double-wrapping**: documented + tested.
- [ ] **Concurrent-reuse test (D-025)**: N≥100 concurrent flow invocations; budget state never bleeds.
- [ ] Smoke scripts `scripts/smoke/phase-26.sh` and `scripts/smoke/phase-26a.sh` pass.

### Cross-cutting

- [ ] `docs/decisions.md` updated if a non-obvious design call is made.
- [ ] `docs/glossary.md` updated.
- [ ] `docs/plans/README.md` Phase 26 + 26a rows flip to `Shipped`.
- [ ] `README.md` Status table updated.
- [ ] Coverage on `internal/tools`: ≥ 85%; on `internal/runtime/flow`: ≥ 85%.

## Files added or changed

- `internal/tools/tools.go`, `policy.go`, `catalog.go`, `events.go` (new).
- `internal/tools/drivers/inproc/inproc.go` (new).
- `internal/tools/conformancetest/conformancetest.go` (new).
- `internal/tools/tools_test.go`, `policy_test.go`, `concurrent_test.go`, `integration_test.go` (new).
- `internal/tools/drivers/inproc/inproc_test.go` (new).
- `internal/runtime/flow/flow.go`, `events.go` (new).
- `internal/runtime/flow/flow_test.go`, `budget_test.go`, `concurrent_test.go`, `integration_test.go`, `testhelpers_test.go` (new).
- `go.mod` / `go.sum` (modified) — add `github.com/santhosh-tekuri/jsonschema/v6`.
- `scripts/smoke/phase-26.sh`, `scripts/smoke/phase-26a.sh` (new).
- `docs/plans/phase-26-tools.md` (this file).
- `docs/plans/README.md` (modified).
- `docs/glossary.md` (modified).
- `README.md` (modified).

## Public API surface

```go
package tools

type TransportKind string
const (
    TransportInProcess TransportKind = "inprocess"
    TransportHTTP      TransportKind = "http"
    TransportMCP       TransportKind = "mcp"
    TransportA2A       TransportKind = "a2a"
    TransportFlow      TransportKind = "flow"
)

type Tool struct {
    Name, Description string
    ArgsSchema, OutSchema json.RawMessage
    SideEffects SideEffect
    Tags, AuthScopes []string
    CostHint, SafetyNotes string
    LatencyHint time.Duration
    Loading LoadingMode
    Examples []ToolExample
    Source ToolSourceID
    Transport TransportKind
    Policy ToolPolicy
}

type ToolDescriptor struct {
    Tool     Tool
    Invoke   func(ctx context.Context, args json.RawMessage) (ToolResult, error)
    Validate func(args json.RawMessage) error
}

type ToolCatalog interface {
    Register(d ToolDescriptor) error
    Resolve(name string) (ToolDescriptor, bool)
    List(filter CatalogFilter) []Tool
}

type ToolProvider interface {
    Connect(ctx context.Context) error
    Discover(ctx context.Context) ([]ToolDescriptor, error)
    Close(ctx context.Context) error
    SourceID() ToolSourceID
}

type ToolPolicy struct {
    TimeoutMS   int
    MaxRetries  int
    BackoffBase time.Duration
    BackoffMult float64
    BackoffMax  time.Duration
    RetryOn     []ErrorClass
    Validate    ValidateMode
}

func DefaultPolicy() ToolPolicy
func RunWithPolicy(ctx, args, invoke, validateIn, validateOut, policy) (ToolResult, error)
func NewCatalog(opts ...CatalogOption) ToolCatalog

var (
    ErrToolNotFound, ErrToolInvalidArgs, ErrToolPolicyExhausted, ErrToolDuplicateName error
)
```

```go
package inproc

func RegisterFunc[I any, O any](
    cat tools.ToolCatalog, name string,
    fn func(ctx context.Context, in I) (O, error),
    opts ...tools.DescriptorOption,
) error
```

```go
package flow

type Definition struct {
    Name, Description string
    Entry, Exit NodeID
    Nodes map[NodeID]NodeSpec
    Budget Budget
    InSchema, OutSchema json.RawMessage
}

type Budget struct {
    Deadline  time.Duration
    HopBudget int
    CostCap   float64
}

func Compose(def Definition, opts ...ComposeOption) (engine.Engine, error)
func RegisterAsTool(cat tools.ToolCatalog, def Definition, eng engine.Engine) (tools.Tool, error)
func WithBudget(ctx context.Context, b Budget) context.Context
func WithSchemasFrom[I any, O any](def Definition) (Definition, error)

var (
    ErrFlowBudgetExceeded, ErrFlowInvalidDefinition, ErrFlowEntryExitMismatch error
)
```

## Test plan

- **Unit:** tools — filter visibility math; policy defaults firing; backoff growth + cap; argument-validation; sentinel error wrapping. Flow — Definition validator; Budget composition `min()` math.
- **Integration:** in-package adapter tests wire catalog + inproc driver + identity ctx end-to-end; flow + tool catalog coexistence; parent ctx deadline propagation through flow.
- **Conformance:** `internal/tools/conformancetest/Run(t, factory)` — Phase 27 / 28 / 29 consume verbatim.
- **Concurrency / leak (D-025):** `TestCatalog_ConcurrentReuse_D025` (catalog), `TestRunWithPolicy_ConcurrentReuse_D025` (policy executor), `TestFlow_ConcurrentReuse_NoBudgetBleed` (flow). N≥100 under `-race`, baseline goroutines restored.

## Smoke script additions

- `scripts/smoke/phase-26.sh`: `go test -race -count=1 -timeout 120s ./internal/tools/...` → OK; skip the HTTP/Protocol surface stub.
- `scripts/smoke/phase-26a.sh`: `go test -race -count=1 -timeout 120s ./internal/runtime/flow/...` → OK; skip the HTTP/Protocol surface stub.

## Coverage target

- `internal/tools`: 85%.
- `internal/tools/drivers/inproc`: 85%.
- `internal/runtime/flow`: 85%.

## Dependencies

- Phase 01 (identity ctx propagation).
- Phase 05 (events bus + RegisterEventType).
- Phase 09 (envelope / identity quadruple).
- Phase 11 (NodePolicy + reliability shell — flow nodes reuse this).
- Phase 14 (subflows + routers + concurrency utilities).

## Risks / open questions

- **Schema-derivation reflection fidelity.** Reflection-derived JSON-Schema covers the common cases (scalars, slices, maps with string keys, nested struct, time.Time, json.RawMessage). Exotic shapes (interfaces, channels, function-typed fields) are rejected at `RegisterFunc` time with a typed `ErrUnsupportedType`.
- **Budget-composition math under concurrent flow invocations.** Per-invocation atomic accumulator; concurrent-reuse test pins this.
- **No double-wrapping.** The dispatcher's `ToolPolicy` wraps the outer flow call; per-node `NodePolicy` wraps each step inside. Tested.
- **Validation BEFORE policy shell.** Input-validation runs ONCE, BEFORE the retry loop — retrying on invalid args never converges. Documented inline.

## Glossary additions

- **`Tool`** — Harbor's planner-addressable unit. Same struct regardless of `Transport`. RFC §6.4.
- **`ToolDescriptor`** — the callable binding: `Tool` + `Invoke` + `Validate`. RFC §6.4.
- **`ToolCatalog`** — three-method interface. RFC §6.4.
- **`ToolProvider`** — interface for external sources (HTTP / MCP / A2A). RFC §6.4.
- **`CatalogFilter`** — visibility predicate keyed on `(tenant, user, session)` + `GrantedScopes`. RFC §6.4.
- **`TransportKind`** — discriminator: `inprocess` / `http` / `mcp` / `a2a` / `flow`. RFC §6.4.
- **`SideEffect`** — `pure` / `read` / `write` / `external` / `stateful`. RFC §6.4.
- **`LoadingMode`** — `always` / `deferred`. RFC §6.4.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Multi-isolation paths: `CatalogFilter` + identity propagation tested
- [ ] **Concurrent-reuse test passes** — `TestCatalog_ConcurrentReuse_D025`, `TestRunWithPolicy_ConcurrentReuse_D025`, `TestFlow_ConcurrentReuse_NoBudgetBleed` all under `-race` (D-025)
- [ ] **Integration test passes** — in-package adapter tests wire catalog + flow + events bus end-to-end with real drivers
- [ ] Glossary updated (yes)
- [ ] Brief departure: `LLMRequest.Tools` from brief 03 §3 rejected per D-010 — recorded above
