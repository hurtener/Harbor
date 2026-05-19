# Phase 72f — Runtime posture surface

## Summary

Phase 72f ships five read-only Protocol methods that expose the live Runtime's posture to a Protocol client (Console, CLI, third-party): `runtime.info` (build identity + version + uptime + advertised capabilities), `runtime.health` (per-subsystem readiness rollup), `runtime.counters` (low-cardinality live counters for the sidebar/footer chips), `runtime.drivers` (configured driver names per persistence-shaped subsystem), and `metrics.snapshot` (a Protocol-shaped projection over the Phase 56 `MetricsRegistry`). The same-phase consumer is an integration test (`test/integration/runtime_posture_test.go`) that probes each method against a live Runtime assembled via `harbortest/devstack` with real drivers; the page-side consumers (Overview counter cards + Settings Runtime Info card) land as Stage-2 phases (73a, 73m). Identity-scope at the edge is mandatory; cross-tenant reads require the `admin` scope per D-079.

## RFC anchor

- RFC §5.3
- RFC §6.15
- RFC §7

## Briefs informing this phase

- brief 11
- brief 12
- brief 06

## Brief findings incorporated

- **brief 11 §"Settings view (SETTINGS)":** the Settings page exposes a Connected-Runtimes card plus a Runtime Info / version / drivers panel. Phase 72f ships the Protocol surface that card reads — `runtime.info` (build identity + version + uptime + capabilities) + `runtime.drivers` (configured driver names per persistence-shaped subsystem). The page wiring lands in Stage-2 73m; Phase 72f's first consumer is the integration test that probes the surface end-to-end.
- **brief 11 §"Left sidebar — navigation + global context" (footer counters):** "Footer: persistent live counters — `Events / sec`, `Tasks Running`, `Background Jobs`, `MCP Connections`." Phase 72f ships `runtime.counters` as the canonical, low-cardinality, identity-scoped source for those chips. The metric labels stay low-cardinality by construction (no run_id / task_id / session_id on a counter — RFC §6.14 + the Phase 56 cardinality lint); identity flows through the request, not through the metric label.
- **brief 11 §CC-1 "Multi-runtime context":** the Console can attach to multiple runtimes; each attachment needs a stable "this is the runtime I'm looking at" projection. `runtime.info` is exactly that projection — the Console's per-attachment heading reads from one method, not by composing five state queries.
- **brief 11 §"Settings view" + §CC-1 (deployment posture):** the Connected-Runtimes card needs a per-subsystem driver readout (in-mem / SQLite / Postgres). `runtime.drivers` returns the configured driver names per subsystem so the operator can see "is this dev (in-mem) or production (Postgres)?" without grepping config. No driver-internal state leaks — only the configured driver *name*.
- **brief 12 §"The two-surface model":** the Console is a Protocol client; the Runtime is headless. Phase 72f's five methods are deliberately *projection* methods — they read from the same canonical Runtime state the events bus emits, never opening a fast-path debug seam. `metrics.snapshot` is a Protocol-shaped projection over the Phase 56 `MetricsRegistry`, NOT a re-export of OpenTelemetry SDK internals.
- **brief 06 §1 "protocol-grade event bus" / decoupling rule:** "the Console NEVER reads runtime internals." Phase 72f's wire types are Protocol-owned structs in `internal/protocol/types/` — never re-exports of `internal/telemetry`'s `MetricsRegistry` Go shape. The `MetricsSnapshot` struct contains the per-counter / per-histogram values as flat numbers, with the same identity-scope discipline every other Protocol response carries.

## Findings I'm departing from (if any)

None. Brief 11's only nuance is that brief-time speculated whether "Runtime Info" lived in Settings or on Overview; the wave-13 decomposition (§4 row 72f) pins both as consumers — Overview hosts the live counter cards (via `runtime.counters` + `pause.list` via 72e), Settings hosts the static Runtime Info card (via `runtime.info` + `runtime.drivers`). Phase 72f's surface honours both consumers without forcing a re-litigation of the layout.

## Goals

- Extend `internal/protocol/methods/methods.go` with five new method constants: `MethodRuntimeInfo`, `MethodRuntimeHealth`, `MethodRuntimeCounters`, `MethodRuntimeDrivers`, `MethodMetricsSnapshot`. Add them to the `canonicalMethods` map. No other place declares a Protocol method string (CLAUDE.md §8).
- Extend `internal/protocol/types/` with the five response wire types: `RuntimeInfo`, `RuntimeHealth`, `RuntimeCounters`, `RuntimeDrivers`, `MetricsSnapshot`. Add a shared `RuntimeInfoRequest` (identity scope only — these are read methods, not control methods) consumed by all five. No Protocol message struct is defined outside `internal/protocol/types/`.
- Extend `internal/protocol/singlesource.CanonicalWireTypes` with the new entries (`RuntimeInfo` → `types`, `RuntimeHealth` → `types`, `RuntimeCounters` → `types`, `RuntimeDrivers` → `types`, `MetricsSnapshot` → `types`, `RuntimeInfoRequest` → `types`). The Phase 58 lint stays green.
- Add a new Protocol capability constant — `CapRuntimePosture` — in `internal/protocol/types/version.go`, registered in `canonicalCapabilities`. A Protocol client can negotiate "does this Runtime advertise the posture surface?" via `VersionHandshake.Accepts(CapRuntimePosture)`. No version bump — the addition is backward-compatible per RFC §5.3 ("minor bump"-class change with no real removals; the version pin moves Patch only, deferred to a Wave 13 audit consolidation PR rather than risking parallel-dispatch collisions).
- Add a new "posture" surface on the existing `protocol.ControlSurface` (or a sibling `protocol.PostureSurface` — see §"Risks / open questions" for the construction call) that owns the five read-only handlers. Identity-scope enforcement at the edge: every request fails closed with `CodeIdentityRequired` on an incomplete triple; a cross-tenant query (`Identity.Tenant != caller's verified tenant`) requires the `admin` scope per D-079 or returns `CodeScopeMismatch`.
- The Phase 60 wire transport (REST control) maps the five methods to their handlers — no new HTTP route, no new mux file; only the route table grows.
- Ship the same-phase consumer: `test/integration/runtime_posture_test.go` that probes each method against a live assembled Runtime (via `harbortest/devstack.Assemble`) with real drivers (in-mem `EventBus`, in-mem `StateStore`, real `MetricsRegistry`), asserts the response shape per method, runs an identity-scope failure mode (cross-tenant rejection), and runs an N≥100 concurrent-reuse stress against one shared surface under `-race`.
- Author `scripts/smoke/phase-72f.sh` with the `# PREFLIGHT_REQUIRES: live-server` header; it probes the surface against the booted dev server plus runs the package + integration tests.

## Non-goals

- **UI consumers** (Overview counter cards + Settings Runtime Info card). Those land as Stage-2 73a (Overview) and 73m (Settings); the wave-13 decomposition pins them explicitly. Phase 72f's first consumer is the integration test that exercises every method end-to-end.
- **`runtime.info` mutation surface** (rename runtime, change region, etc.). Mutation is a *control* method, not a *read* method; if a future phase adds runtime-administrative mutation (e.g. dev-only `runtime.shutdown`), it lands in its own phase with its own scope claim.
- **High-cardinality metric labels.** `runtime.counters` returns the *roll-up* values (events/sec, tasks running, background jobs, MCP connections) — never per-run / per-task / per-session breakdowns. The Phase 56 cardinality lint already gates this on the SDK side; the Protocol response shape mirrors that posture.
- **A separate `governance.posture` / `llm.posture` surface.** Those are Phase 72g's scope. Phase 72f stops at the runtime-shaped subsystems (build identity, health, counters, drivers, metrics rollup); the governance / LLM tier rollups are Phase 72g's read methods.
- **`runtime.health` deep checks** (per-subsystem latency probes, dependency reachability synth tests). Phase 72f returns a structural readiness rollup: each registered subsystem reports `ready` / `degraded` / `unavailable` from its own state, not from a synthetic probe. Synthetic deep-checks are a post-V1 follow-up.

## Acceptance criteria

- [ ] `internal/protocol/methods/methods.go` declares five new method constants (`MethodRuntimeInfo`, `MethodRuntimeHealth`, `MethodRuntimeCounters`, `MethodRuntimeDrivers`, `MethodMetricsSnapshot`); each is registered in `canonicalMethods`; `IsValidMethod` returns `true` for each; `Methods()` returns the lexicographically-sorted set including the five.
- [ ] `internal/protocol/types/` declares the six new wire types: `RuntimeInfoRequest`, `RuntimeInfo`, `RuntimeHealth`, `RuntimeCounters`, `RuntimeDrivers`, `MetricsSnapshot`. Each round-trips through `encoding/json` (a `types_test.go` case per type).
- [ ] `internal/protocol/singlesource.CanonicalWireTypes` lists the six new types with `"types"` as their home. The Phase 58 single-source lint stays green; `TestSingleSource_CanonicalWireTypesInLockstep` passes.
- [ ] `internal/protocol/types/version.go` declares `CapRuntimePosture = "runtime_posture"` and registers it in `canonicalCapabilities`. `IsValidCapability(CapRuntimePosture)` returns `true`; `Capabilities()` includes it; `CurrentHandshake().Accepts(CapRuntimePosture)` returns `true`.
- [ ] A new `protocol.PostureSurface` type (or a `PostureSurface` extension on `protocol.ControlSurface` — see §"Risks") owns the five read-only handlers. The handler set has its own `Dispatch(ctx, method, req)` entry point if it lands as a sibling, OR routes through the existing `ControlSurface.Dispatch` if it lands as an extension. Either shape, the new methods route to their handlers via `methods.IsValidMethod`'s extended set.
- [ ] Every handler reads identity from `ctx` (via `identity.From`) AND validates the request's `IdentityScope` triple via the existing per-control pattern; an incomplete triple fails closed with `CodeIdentityRequired`. The `Identity.Tenant` field is REQUIRED; the `User` and `Session` fields are required when the request's projection is session-scoped (`runtime.counters` for a `Tenant`-only scope returns tenant-wide rollups; the same call with a `Session` returns the session's slice — both shapes mandate identity).
- [ ] Cross-tenant queries (request `Identity.Tenant != ctx-verified-tenant`) require the `admin` scope per D-079; without the scope the response is `CodeScopeMismatch`. The integration test asserts this with a real ES256-signed bearer.
- [ ] `runtime.info` returns build info (`BuildVersion`, `BuildCommit`, `BuildDate`, `BuildGoVersion`), protocol info (`ProtocolVersion`, `Capabilities`), uptime (`UptimeSeconds`), and a runtime-display name (`InstanceID` — a UUID minted at boot + `DisplayName` from config — never the host's machine name without operator config).
- [ ] `runtime.health` returns the subsystem-readiness rollup: `{Subsystem: string, Status: "ready"|"degraded"|"unavailable", Detail: string}` for each registered subsystem the runtime knows about (`events`, `state`, `tasks`, `sessions`, `tools`, `memory`, `artifacts`, `llm`, `governance`, `metrics`). Status is read from each subsystem's own posture seam; an unregistered subsystem returns `"unavailable"` with a stable `Detail` reason.
- [ ] `runtime.counters` returns the low-cardinality live counters: `EventsPerSecond`, `TasksRunning`, `BackgroundJobsActive`, `MCPConnectionsHealthy`, `SessionsActive`. The shape matches brief 11's "footer chips" rollup; identity-scoped per the §"Goals" pattern.
- [ ] `runtime.drivers` returns `{Subsystem: string, Driver: string, Mode: string}` per persistence-shaped subsystem the runtime configured (`state`, `artifacts`, `memory`, `eventlog`). `Driver` is the configured driver name ("inmem" / "sqlite" / "postgres"); `Mode` is the optional posture detail ("readwrite" / "readonly" / "embedded"). No driver-internal state leaks.
- [ ] `metrics.snapshot` returns a Protocol-shaped projection over the Phase 56 `MetricsRegistry`: counters (name + value), histograms (name + count + sum + per-bucket counts), gauges (name + current value). The wire shape is a flat `[]NamedMetric` per kind; no OpenTelemetry SDK type leaks across the Protocol boundary.
- [ ] `PostureSurface` is a D-025 compiled artifact — a concurrent-reuse test runs N≥100 concurrent `Dispatch` calls against one shared surface under `-race` (no data races, no context bleed, no cross-cancellation, no goroutine leaks). The test asserts that per-goroutine identity quadruples flow through unblended.
- [ ] `test/integration/runtime_posture_test.go` exists, wires real drivers (assembled via `harbortest/devstack.Assemble`), asserts identity propagation across all five methods, covers ≥1 failure mode (cross-tenant rejection without `admin` scope OR missing-identity rejection at the surface edge), runs an N≥10 concurrency stress (N concurrent operators reading `runtime.counters` against the live surface; assert the goroutine baseline is restored after teardown), and passes under `-race`.
- [ ] `scripts/smoke/phase-72f.sh` (executable, `# PREFLIGHT_REQUIRES: live-server` header) probes each of the five methods against the booted dev server (per the 404/405/501 → SKIP convention while transports don't yet route them), runs the package + integration tests under `-race`, and includes static guards (single-source preserved; no Console import; no OTel SDK type leaks into `internal/protocol/types/`).
- [ ] `internal/protocol/transports/control/control.go`'s route table grows to include the five new methods (the transport adapter dispatches Protocol method → handler the same way it does for the ten Phase 54 methods). No change to the SSE stream transport — the surface is request/response only.
- [ ] Coverage on touched packages: `internal/protocol` ≥ 85%, `internal/protocol/types` ≥ 85%, `internal/protocol/methods` ≥ 85% (master-plan baseline preserved).

## Files added or changed

```text
internal/protocol/
  posture.go                    # PostureSurface + NewPostureSurface + the five handlers
  posture_test.go               # unit: per-method dispatch, identity/scope failure modes
  posture_concurrent_test.go    # D-025 concurrent-reuse (N>=100)
  methods/
    methods.go                  # +5 new method constants + canonicalMethods extension
    methods_test.go             # extend exhaustiveness coverage
  types/
    posture.go                  # 6 new wire types (RuntimeInfoRequest + 5 responses)
    posture_test.go             # JSON round-trip per type
    version.go                  # +CapRuntimePosture + canonicalCapabilities extension
    version_test.go             # extend capabilities coverage
  singlesource/
    singlesource.go             # extend CanonicalWireTypes with the 6 new entries
    singlesource_test.go        # extend lockstep coverage
  transports/control/
    control.go                  # extend route table with the 5 new methods
    control_test.go             # extend coverage
test/integration/
  runtime_posture_test.go       # the §13 first consumer — real drivers, identity propagation, ≥1 failure mode, N>=10 concurrency
docs/plans/phase-72f-runtime-posture.md   # this plan
docs/plans/README.md            # add Phase 72f row (Pending — Wave 13)
docs/decisions.md               # append D-110 (provisional — coordinator may renumber if Wave 13 stage-1 sibling phases collide)
docs/glossary.md                # +runtime.info, runtime.health, runtime.counters, runtime.drivers, metrics.snapshot, RuntimeInfo, RuntimeHealth, MetricsSnapshot
scripts/smoke/phase-72f.sh      # the smoke (live-server class)
README.md                       # Phase 72f status row (Wave 13, Pending)
```

No new top-level directory — `internal/protocol/` is already in CLAUDE.md §3.

## Public API surface

```go
package methods

const (
    MethodRuntimeInfo     Method = "runtime.info"
    MethodRuntimeHealth   Method = "runtime.health"
    MethodRuntimeCounters Method = "runtime.counters"
    MethodRuntimeDrivers  Method = "runtime.drivers"
    MethodMetricsSnapshot Method = "metrics.snapshot"
)

// IsPostureMethod reports whether m is one of the five read-only posture
// methods (Phase 72f). The PostureSurface uses this to branch in
// transport adapters that want a single route table over all canonical
// methods.
func IsPostureMethod(m Method) bool

package types

// RuntimeInfoRequest is the shared request shape for the five Phase 72f
// posture methods. It carries only the identity scope — the methods are
// read-only and take no payload.
type RuntimeInfoRequest struct {
    Identity IdentityScope `json:"identity"`
}

type RuntimeInfo struct {
    InstanceID      string       `json:"instance_id"`
    DisplayName     string       `json:"display_name"`
    BuildVersion    string       `json:"build_version"`
    BuildCommit     string       `json:"build_commit"`
    BuildDate       string       `json:"build_date"`
    BuildGoVersion  string       `json:"build_go_version"`
    ProtocolVersion string       `json:"protocol_version"`
    Capabilities    []Capability `json:"capabilities"`
    UptimeSeconds   int64        `json:"uptime_seconds"`
}

type RuntimeHealth struct {
    Subsystems []SubsystemHealth `json:"subsystems"`
}

type SubsystemHealth struct {
    Subsystem string `json:"subsystem"`
    Status    string `json:"status"` // "ready" | "degraded" | "unavailable"
    Detail    string `json:"detail,omitempty"`
}

type RuntimeCounters struct {
    EventsPerSecond       float64 `json:"events_per_second"`
    TasksRunning          int64   `json:"tasks_running"`
    BackgroundJobsActive  int64   `json:"background_jobs_active"`
    MCPConnectionsHealthy int64   `json:"mcp_connections_healthy"`
    SessionsActive        int64   `json:"sessions_active"`
    SnapshotAt            int64   `json:"snapshot_at"` // unix-millis
}

type RuntimeDrivers struct {
    Subsystems []SubsystemDriver `json:"subsystems"`
}

type SubsystemDriver struct {
    Subsystem string `json:"subsystem"`
    Driver    string `json:"driver"` // "inmem" | "sqlite" | "postgres" | ...
    Mode      string `json:"mode,omitempty"`
}

type MetricsSnapshot struct {
    Counters   []NamedCounter   `json:"counters"`
    Histograms []NamedHistogram `json:"histograms"`
    Gauges     []NamedGauge     `json:"gauges"`
    SnapshotAt int64            `json:"snapshot_at"`
}

type NamedCounter struct {
    Name   string            `json:"name"`
    Value  float64           `json:"value"`
    Labels map[string]string `json:"labels,omitempty"`
}

type NamedHistogram struct {
    Name    string            `json:"name"`
    Count   uint64            `json:"count"`
    Sum     float64           `json:"sum"`
    Buckets []HistogramBucket `json:"buckets"`
    Labels  map[string]string `json:"labels,omitempty"`
}

type HistogramBucket struct {
    UpperBound float64 `json:"upper_bound"`
    Count      uint64  `json:"count"`
}

type NamedGauge struct {
    Name   string            `json:"name"`
    Value  float64           `json:"value"`
    Labels map[string]string `json:"labels,omitempty"`
}

const CapRuntimePosture Capability = "runtime_posture"

package protocol

// PostureSurface is the transport-agnostic Harbor Protocol runtime-
// posture handler (Phase 72f). It is built once per Runtime process and
// shared across every Protocol request; Dispatch is safe for concurrent
// use by N goroutines (D-025).
type PostureSurface struct { /* unexported */ }

// PostureDeps bundles the runtime-side seams a PostureSurface reads
// through. Every dependency is read-only; the surface mutates none of
// them.
type PostureDeps struct {
    Build       BuildInfo
    Clock       func() time.Time
    Health      func(ctx context.Context) []types.SubsystemHealth
    Counters    func(ctx context.Context, ident identity.Identity) types.RuntimeCounters
    Drivers     func() []types.SubsystemDriver
    Metrics     func(ctx context.Context) types.MetricsSnapshot
    DisplayName string
    InstanceID  string
}

func NewPostureSurface(deps PostureDeps) (*PostureSurface, error)

func (s *PostureSurface) Dispatch(ctx context.Context, method methods.Method, req any) (any, error)
```

The `PostureSurface` is a sibling of `ControlSurface`, not an extension — see §"Risks / open questions" for the construction call (the existing `ControlSurface` carries no telemetry / build / health seams; threading them through would balloon `NewControlSurface`'s signature).

## Test plan

- **Unit:** `methods_test.go` — exhaustiveness now covers all 15 method names (10 control + 5 posture). `types/posture_test.go` — JSON round-trip per response type + the request type. `types/version_test.go` — `CapRuntimePosture` is registered; `CurrentHandshake().Accepts(CapRuntimePosture)` returns `true`. `singlesource/singlesource_test.go` — lockstep test covers the 6 new entries. `posture_test.go` — `Dispatch` routes each of the five methods to the right handler; unknown method → `CodeUnknownMethod`; incomplete identity → `CodeIdentityRequired`; cross-tenant without admin → `CodeScopeMismatch`; nil request → `CodeInvalidRequest`. Each handler test injects a deterministic `PostureDeps` (controlled clock, fixed `BuildInfo`, fixed health snapshot) so the assertions are byte-stable.
- **Integration:** `test/integration/runtime_posture_test.go` — boots an assembled Runtime via `harbortest/devstack.Assemble` (D-094), constructs a real `PostureSurface` wired to the live `events.EventBus` + `state.StateStore` + `tasks.TaskRegistry` + `MetricsRegistry`, drives a real `start` to populate non-zero counters, then invokes each of the five posture methods through the surface. Asserts: (a) identity propagation through every layer (per-call identity quadruple matches the request); (b) `runtime.info.Capabilities` includes `CapRuntimePosture`; (c) `runtime.health` returns one entry per registered subsystem; (d) `runtime.counters.TasksRunning ≥ 1` while the run is live; (e) `runtime.drivers` lists the assembled drivers; (f) `metrics.snapshot.Counters` includes at least one bus-emit counter. Cross-tenant isolation: two tenants T1 + T2 each get a separate session; T1's `runtime.counters` request with `Identity.Tenant=T2` and no `admin` scope is rejected `CodeScopeMismatch`; T1's request with `admin` scope returns T2's counters. N=10 concurrent operators reading `runtime.counters` against the live surface; assert the goroutine baseline is restored after teardown.
- **Conformance:** N/A — `PostureSurface` ships no multi-driver subsystem. The five methods are themselves added to the Phase 62 conformance suite's method matrix (a one-line registration) so a third-party Runtime is asserted to expose them; the matrix expansion lands in this PR alongside the posture surface.
- **Concurrency / leak:** `posture_concurrent_test.go` — N≥100 concurrent `Dispatch` calls against one shared `PostureSurface` under `-race`. Distinct per-goroutine identity quadruples; a context bleed surfaces as a foreign triple in the response. The handler reads run-specific values only from `ctx` + `req`; no per-call mutation on the struct. `runtime.NumGoroutine` returns to baseline after join.

## Smoke script additions

- `scripts/smoke/phase-72f.sh` (header: `# PREFLIGHT_REQUIRES: live-server`) probes each of the five methods against the booted dev server (per the 404/405/501 → SKIP convention while transports don't yet route them in builds that pre-date this phase):
  - `assert_status 200 "$(api_url /v1/control/runtime.info)" "runtime.info responds"` (POST a minimal `RuntimeInfoRequest`).
  - `assert_status 200 "$(api_url /v1/control/runtime.health)" "runtime.health responds"`.
  - `assert_status 200 "$(api_url /v1/control/runtime.counters)" "runtime.counters responds"`.
  - `assert_status 200 "$(api_url /v1/control/runtime.drivers)" "runtime.drivers responds"`.
  - `assert_status 200 "$(api_url /v1/control/metrics.snapshot)" "metrics.snapshot responds"`.
  - `assert_json_path '.protocol_version' "$ProtocolVersion" "$(api_url /v1/control/runtime.info)" "runtime.info pins the Protocol version"` — gated by `HARBOR_DEV_TOKEN` per the existing inspect-* smoke pattern (Phase 69).
  - Identity-rejection probe: a request with an empty `identity.tenant` returns `401`/`identity_required` (mapped per the existing `errors.Code → HTTP status` table).
  - Cross-tenant probe (when `HARBOR_DEV_TOKEN` lacks `admin` scope): a request with `identity.tenant != verified_tenant` returns `CodeScopeMismatch` (HTTP 403).
- Runs `go test -race -count=1 -timeout 180s ./internal/protocol/...` (covers the new methods + types + singlesource + posture surface + D-025 concurrent-reuse).
- Runs `go test -race -count=1 -timeout 240s -run TestE2E_RuntimePosture ./test/integration/...`.
- Static guards:
  - `internal/protocol/types/posture.go` does NOT import `go.opentelemetry.io/otel` / `go.opentelemetry.io/otel/sdk` — the `MetricsSnapshot` wire shape is Protocol-owned, not an OTel SDK re-export.
  - `internal/protocol/posture.go` does NOT import the Console (`web/console`) — the Runtime never imports Console code (CLAUDE.md §13).
  - No Protocol method string is hardcoded outside `internal/protocol/methods/` (defence-in-depth over the Phase 58 lint).
  - No Protocol message struct is defined outside `internal/protocol/types/` (same defence-in-depth).

## Coverage target

- `internal/protocol`: 85%
- `internal/protocol/types`: 85%
- `internal/protocol/methods`: 85%
- `internal/protocol/singlesource`: 85% (the lockstep test gates the new entries)
- `internal/protocol/transports/control`: 90% (existing target; the route-table extension preserves it)

## Dependencies

- Phase 60 (Protocol wire transport — SSE + REST) — Shipped. The REST control adapter's route table is what grows to include the five new methods.
- Phase 61 (Protocol auth — JWT validator + middleware + scopes) — Shipped. The cross-tenant `admin`-scope check uses `auth.HasScope` per D-079.
- Phase 56 (Metrics — `MetricsRegistry`) — Shipped. `metrics.snapshot` projects over its values; the `PostureDeps.Metrics` seam is the read-only adapter.
- Phase 25 (SQLite + Postgres memory drivers) — Shipped. `runtime.drivers` reads the configured memory driver name from config.
- Phase 18 (SQLite + Postgres artifacts) — Shipped. `runtime.drivers` reads the configured artifact driver name from config.

(Also depends on Phase 58 — Protocol single-source — for the `CanonicalWireTypes` lockstep; on Phase 59 — Protocol versioning — for `CapRuntimePosture` registration; on Phase 71 — `harbortest/devstack` — for the integration test's assembled-runtime fixture; on Phase 62 — Protocol conformance — for the method-matrix expansion.)

## Risks / open questions

- **`PostureSurface` vs. `ControlSurface` extension.** The five posture methods are *read* methods (no runtime mutation); the existing `ControlSurface` is a *control* surface (RFC §5.2 row "Task control"). A future operator could legitimately want one combined `Dispatch` entry point, but the Phase 54 `ControlSurface` would balloon if it grew five new dependency seams (telemetry / build / health / counters / drivers). **Recommendation:** ship `PostureSurface` as a *sibling*, with its own `Dispatch` entry, and let the transport adapter (`internal/protocol/transports/control`) dispatch over the union via `methods.IsValidMethod` → switch on `IsControlMethod` vs `IsPostureMethod` vs `IsStartMethod`. A future Phase 73 state-inspection surface (`sessions.inspect`, `tasks.get`, ...) will be its own third sibling. The pattern keeps each surface's dependency set narrow and reviewable, in line with §4.4's "no optional-capability ceremony" — every surface implements its full method set without `Supports*`.
- **`metrics.snapshot` cardinality.** The wire shape carries optional `Labels` on each metric; if a future Phase 56 deepening adds a high-cardinality label, the response could explode. **Mitigation:** the Phase 56 cardinality lint already rejects `run_id` / `trace_id` on metric labels; `metrics.snapshot` mirrors that posture by re-running the lint at the projection boundary (a runtime-side belt-and-braces check on label keys, before returning the response). A label that fails the lint is dropped with a `metrics.snapshot.label_rejected` audit event; the metric is still returned with its remaining low-cardinality labels.
- **`runtime.info.InstanceID` provenance.** The instance ID is minted at boot (a UUID stored on the runtime config); a Console attached to multiple runtimes uses it as the per-attachment stable key. **Mitigation:** the ID is written to the StateStore at boot under a reserved key (`runtime.instance_id`) so a restart with the same data dir reuses the ID; a fresh data dir mints a new ID. This stops the Console's per-attachment identity from flickering across reboots while a single deployment is live.
- **`runtime.drivers` operator information disclosure.** The driver names ("sqlite" / "postgres") are tame; the *DSN* / *connection string* MUST NOT leak. **Mitigation:** the `SubsystemDriver` shape carries only `Driver` + `Mode`; the integration test pins that no DSN-shaped string appears in the response payload (a grep-style assertion on the JSON for `://` / `postgresql://` substrings). The audit redactor double-checks the path on the way out, the same way it does for every other Protocol response.
- **Wave 13 stage-1 D-NNN collision risk.** Stage 1 dispatches 11 phases in parallel. The dispatch prompt pre-assigns this phase a `D-NNN`; this plan provisionally references **D-110** (Stage-1 phases 72/72a..72h/74/75 nominally take D-105..D-115). The coordinator may renumber during merge if a sibling stage-1 phase races us to a number — the phase plan's reference is "to be filed as D-110 when the implementor lands," and the implementor's PR is the authoritative number.
- **Method-name dot syntax.** The five new methods use lowercase dot syntax (`runtime.info`) — distinct from the Phase 54 task-control set, which uses lowercase snake_case (`inject_context`). The dot syntax matches RFC §5.2's "namespace.method" convention for read methods. The methods package's existing `Method` type is a plain string, so the addition is mechanical; the test extension pins each new constant's wire string verbatim.

## Glossary additions

- **`runtime.info`** — Protocol read method (Phase 72f) returning the Runtime's build identity (version / commit / Go toolchain / build date), Protocol version, advertised capabilities, uptime, instance ID, and operator-configured display name. The Console's Settings page Runtime Info card consumes this method.
- **`runtime.health`** — Protocol read method (Phase 72f) returning a per-subsystem readiness rollup (`ready` / `degraded` / `unavailable`) across the runtime's registered subsystems. Status is read from each subsystem's own posture seam; no synthetic deep-checks.
- **`runtime.counters`** — Protocol read method (Phase 72f) returning the low-cardinality live counters the Console's footer/sidebar chips render: events/sec, tasks running, background jobs active, MCP connections healthy, sessions active. Identity-scoped; the response is the *roll-up*, never a per-run / per-task breakdown.
- **`runtime.drivers`** — Protocol read method (Phase 72f) returning the configured driver names per persistence-shaped subsystem (`state`, `artifacts`, `memory`, `eventlog`). The Console's Settings page Connected-Runtimes card reads this to render "this runtime is dev (in-mem) vs production (Postgres)" without grepping config. Returns the driver name + an optional posture mode — never the DSN.
- **`metrics.snapshot`** — Protocol read method (Phase 72f) returning a Protocol-shaped projection over the Phase 56 `MetricsRegistry`: counters (name + value), histograms (name + count + sum + per-bucket counts), gauges (name + current value). Wire-shaped, not an OpenTelemetry SDK re-export.
- **`RuntimeInfo`** — Phase 72f wire type (`internal/protocol/types/posture.go`) carrying build identity + protocol version + capabilities + uptime + instance ID + display name. The `runtime.info` response shape.
- **`RuntimeHealth`** — Phase 72f wire type carrying the per-subsystem readiness rollup. Each `SubsystemHealth` entry pins one subsystem's status + an optional detail string.
- **`MetricsSnapshot`** — Phase 72f wire type carrying the Protocol-shaped projection over the Phase 56 metrics registry: typed counters, histograms, and gauges with low-cardinality labels. Wire-owned; no OpenTelemetry SDK type crosses the Protocol boundary.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — `test/integration/runtime_posture_test.go` asserts cross-tenant isolation across two tenants with one operator scoped to each.
- [ ] **If this phase builds a reusable artifact:** `PostureSurface` is a D-025 compiled artifact — `posture_concurrent_test.go` pins N≥100 concurrent `Dispatch` calls against one shared instance under `-race`, asserting no data races, no context bleed, no cross-cancellation, no goroutine leaks (baseline restored after join).
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam:** Yes — Phase 56 (`MetricsRegistry`), Phase 60 (transport route table), Phase 61 (auth scopes), Phase 71 (`harbortest/devstack`). `test/integration/runtime_posture_test.go` wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode (cross-tenant rejection without `admin` scope), runs an N≥10 concurrency stress, and passes under `-race`.
- [ ] If new vocabulary: glossary updated (eight entries — `runtime.info`, `runtime.health`, `runtime.counters`, `runtime.drivers`, `metrics.snapshot`, `RuntimeInfo`, `RuntimeHealth`, `MetricsSnapshot`).
- [ ] If a brief finding was departed from: N/A — no departures (the §"Findings I'm departing from" section justifies the lack).
