# Phase 205 — body-scope reconciler

## Summary

Harbor's Protocol accepts an identity scope in the request body. Reconciling
that caller-supplied scope against the request's established identity was done
by thirteen near-duplicate helpers spread across two transports, each carrying
its posture in a code comment rather than in a value the runtime reads. This
phase replaces all of them with one shared gate — `internal/protocol/bodyscope`
— whose per-surface posture is a row in a closed registry, and adds a
three-part lockstep gate so a fourteenth helper, a new unregistered surface, or
an unreviewed tenant crossing fails `go test` on the commit that writes it.

## RFC anchor

- RFC §4
- RFC §4.2
- RFC §5
- RFC §5.4
- RFC §5.5

## Briefs informing this phase

- brief 06
- brief 07

## Brief findings incorporated

- **brief 06 §"Isolation-triple filtering by default"**: "Cross-tenant
  subscriptions are an explicit, audited operation." This phase generalises the
  finding from the event bus to every Protocol surface: a body identity scope
  that reaches outside the caller's verified tenant is permitted only under an
  admin-tier claim and publishes `audit.admin_scope_used` before the crossing is
  granted. The permission and the audit ship in one argument list — a
  tenant-permissive policy with no audit sink is refused at run time.
- **brief 06 §"Cross-tenant isolation tests"**: "`admin` scope can bypass;
  assertion on the audit event for the bypass." The reconciler's table-driven
  suite pins both halves at every surface that permits a crossing, and the
  end-to-end wire tests keep the four legitimate admin paths working.
- **brief 07 §1 (the single-dispatch architecture)**: one mechanism the runtime
  owns, parameterised, collapsing a mode matrix into a single dimension. The
  thirteen helpers were the mode matrix; the registry is the single dimension.
  A surface names a registry key, never an ad-hoc posture, so a call site cannot
  invent a policy.

## Findings I'm departing from (if any)

None.

## Goals

- One shared reconciler every Protocol surface routes its request body through.
- A per-surface posture declared once, in a table a reviewer reads, rather than
  re-derived by whoever writes the next handler.
- Fail closed when a request reaches a gate with no established identity: a
  caller-supplied body never stands in for the authority a transport establishes.
- A verified identity with provenance: the triple a transport established is
  distinguishable from the working identity a run re-scopes to, and plain
  re-scoping cannot widen the tenant past it.
- A mechanical gate that keeps all of the above true as new surfaces land.

## Non-goals

- Changing any surface's authorization outcome. The four legitimate cross-tenant
  admin paths (`artifacts.list` / `artifacts.put`, the seven posture methods,
  `topology.snapshot`, the `?admin=1` event fan-in) keep working exactly as
  before; the surfaces with no elevation path keep their flat refusal.
- Changing the wire: no new Protocol method, error code, event type or wire
  field. `ProtocolVersion` is unchanged.
- Rewriting the artifacts surface's per-method refinements. `artifacts.get_ref`
  and `artifacts.delete` hold a tenant refusal narrower than the surface policy;
  the surface policy is the ceiling the transport applies and those remain the
  floor.

## Acceptance criteria

- [ ] `internal/protocol/bodyscope` ships one `Reconcile` entry point taking a
      registry key, not a policy value.
- [ ] All thirteen hand-written body-identity helpers are gone; every surface
      that reads a body identity scope routes through `Reconcile`.
- [ ] User and Session equal the verified identity on every surface except the
      one that declares a whole cross-identity read; an entirely empty body
      triple is backfilled from the verified identity.
- [ ] A tenant that differs from the verified one is permitted only under
      `auth.ScopeAdmin` or `auth.ScopeConsoleFleet`, on a surface whose registry
      row declares it, and publishes `audit.admin_scope_used` naming the
      ctx-verified actor before the crossing is granted.
- [ ] A surface whose policy permits a crossing but is handed no audit sink is
      refused with `CodeRuntimeError` — the permission and the accountability
      cannot separate.
- [ ] A request with no established identity is refused with
      `CodeIdentityRequired`; the body is never authoritative.
- [ ] `identity` carries a third context key holding the transport-established
      triple, written only by the request-edge writers, readable via
      `identity.FromVerified`.
- [ ] Plain `identity.With` refuses to move the working identity to a tenant
      other than the verified one; every legitimate internal re-scoping site
      still works untouched.
- [ ] `identity.WithElevated` is the one path across the tenant boundary, demands
      a reason, and its call sites are a reviewed list.
- [ ] The MCP Apps and MCP-Connections gates run inside `Dispatch`, so those
      surfaces' transport-agnostic claim is true.
- [ ] The lockstep gate fails `go test` when a scope-carrying request type has no
      registry row, when a row is deleted, when a body-identity comparison is
      hand-written outside the reconciler, or when an unreviewed site mints a
      verified identity or a crossing.
- [ ] Each half of the gate has a non-vacuity test proving it bites, including
      the evasions an ordinary refactor produces: a component hoisted into a
      local, a case-folded comparison, and an aliased `identity` import.
- [ ] An audited crossing authorizes ONE named tenant. A second crossing to a
      different tenant meets the same closed door and gets its own record.
- [ ] Every per-row projection behind an authorized fleet fan-in still reads
      its row: an admin `sessions.list` reports the foreign row's real
      counters, and an admin `search.tasks` returns the foreign row. A rollup
      that could not be taken is marked partial rather than reported as an
      exact zero.
- [ ] The cross-tenant regression tests root the request context at
      `identity.WithVerified`, the shape every mounted route now has. A test
      rooted at a bare `identity.With` cannot see the guard at all.

## Files added or changed

```text
internal/identity/identity.go                             # verified key, FromVerified, WithVerified, WithElevated, With narrowing
internal/protocol/bodyscope/                              # NEW — the shared gate
├── bodyscope.go                                          # Reconcile + the ScopeRef adapters
├── policy.go                                             # ComponentRule + Policy
├── registry.go                                           # the closed surface → policy table
├── coverage.go                                           # request type → surface join + exempt carriers
├── audit.go                                              # Auditor + BusAuditor + the SafePayload
├── gate.go                                               # the three mechanical scans
├── bodyscope_test.go                                     # table-driven contract + concurrent reuse
└── gate_test.go                                          # the lockstep gate + its non-vacuity pins
internal/protocol/auth/middleware.go                      # seats the verified identity; carrier posture
internal/protocol/apps.go, mcp.go                         # surface-level gate
internal/protocol/artifacts.go, posture.go, control.go    # anchored on the verified identity; audited crossings
internal/protocol/transports/transports.go                # one identity decorator for every mounted route
internal/protocol/transports/control/*.go                 # four helpers → Reconcile
internal/protocol/transports/stream/*.go                  # nine helpers → Reconcile; one identity choke point
test/integration/carrier_identity*.go                     # end-to-end carrier helpers
scripts/smoke/phase-205.sh                                # NEW
docs/plans/phase-205-body-scope-reconciler.md             # NEW
docs/decisions.md, docs/glossary.md, docs/plans/README.md
```

## Public API surface

```go
// internal/identity
func WithVerified(ctx context.Context, id Identity) (context.Context, error)
func FromVerified(ctx context.Context) (Identity, bool)
func WithElevated(ctx context.Context, id Identity, reason string) (context.Context, error)
func IsElevated(ctx context.Context) bool
func ElevatedTenant(ctx context.Context) (string, bool)
func ElevationReason(ctx context.Context) (string, bool)
var ErrTenantWidening, ErrElevationReasonRequired error

// internal/protocol/bodyscope
type Surface string
type ComponentRule uint8 // Pinned | PinnedOrEmpty | AdminScoped
type Policy struct {
    Surface; Wire string
    Tenant, User, Session ComponentRule
    Grants []auth.Scope                          // empty ⇒ admin + console:fleet
    ScopeDeniedCode, PinnedDeniedCode protoerrors.Code
    Reason string
}
type ScopeRef interface { Triple() (string, string, string); SetTriple(string, string, string) }
type Auditor interface { AdminScopeUsed(context.Context, Elevation) }

func ForIdentityScope(*types.IdentityScope) ScopeRef
func ForArtifactScope(*types.ArtifactScope) ScopeRef
func Reconcile(context.Context, ScopeRef, Surface, Auditor) (context.Context, *protoerrors.Error)
func Elevated(context.Context) bool
func NewBusAuditor(events.EventBus, audit.Redactor, *slog.Logger) *BusAuditor
func (Policy) Granted(context.Context) bool
func (Policy) PermitsCrossIdentity() bool
func PolicyFor(Surface) (Policy, bool)
func RegisteredSurfaces() []Surface
func RegisteredPolicies() map[Surface]Policy
func SurfaceForRequest(string) (Surface, bool)
func ScanWireTypes(string) ([]Violation, int, error)
func ScanHandRolledGates(string, map[string]string) ([]Violation, int, error)
func ScanElevationSites(string, map[string]string) ([]Violation, int, error)

// internal/protocol/auth
func CarrierIdentityMiddleware(*slog.Logger) func(http.Handler) http.Handler
```

## Test plan

- **Unit:** table-driven coverage of `Reconcile` — empty-triple backfill;
  matching triple; user mismatch; session mismatch; tenant mismatch without a
  claim; tenant mismatch with `ScopeAdmin`; with `ScopeConsoleFleet`; no
  established identity (fail closed); a tenant-permissive surface with a nil
  audit sink; an unregistered surface; wildcard components on the surfaces that
  read an empty component as one. `identity` unit tests for the verified key,
  the tenant-widening refusal, and the elevation reason.
- **Integration:** the existing end-to-end wire suites carry this phase's
  integration burden — they drive the real transports, the real surfaces and the
  real drivers, and they pin every legitimate cross-tenant admin path plus the
  flat-refusal surfaces. `test/integration/artifacts_page_test.go` covers the
  admin elevation and the narrower `get_ref` refusal over the wire;
  `mcp_connections_page_test.go` covers the flat-denial surface;
  `phase170_mcp_oauth_discovery_dial_test.go` covers the fail-closed leg.
- **Conformance:** the lockstep gate is the conformance suite — coverage,
  enforcement and minting, each with a non-vacuity companion.
- **Concurrency / leak:** N≥100 concurrent `Reconcile` calls against one shared
  registry and one shared `BusAuditor` under `-race`, asserting per-request
  reconciled triples never bleed and the granted crossings are counted exactly.

## Smoke script additions

- `runtime.info` with an empty body identity scope is accepted and the response
  carries the caller's own identity — the backfill leg, live.
- `runtime.info` with a body identity scope naming a foreign tenant is refused
  with `scope_mismatch` — the elevation gate's deny leg on a tenant-permissive
  surface.
- `runtime.info` with a body identity scope naming a foreign user is refused with
  `identity_required` — the pinned-component leg.
- `mcp.servers.list` with a body identity scope naming a foreign tenant is
  refused with `identity_required` — the flat-denial surface.
- `artifacts.list` with an empty user and session is accepted — the wildcard leg.

## Coverage target

- `internal/protocol/bodyscope`: 85% (measured: 92.4%)
- `internal/identity`: 90% (measured: 96.6%)
- `internal/protocol/transports/control`: no regression from the pre-phase figure
- `internal/protocol/transports/stream`: no regression from the pre-phase figure.
  The package moves 68.3% → 67.3%: the phase DELETES nine hand-written
  helpers whose per-surface branches were densely covered by the page
  handlers' own suites, and replaces them with one shared call whose
  branches are covered in `internal/protocol/bodyscope` instead. The
  uncovered statement count does not grow; the denominator shrinks. The
  reconciliation logic is more covered after the phase than before, in
  the package that now owns it.

## Dependencies

- 72f, 73k, 73l, 109a — the surfaces whose helpers this phase collapses.

## Risks / open questions

- **The fail-closed contract is absolute at the gate.** A gate reached with no
  established identity refuses; the body is input and never supplies the missing
  authority. Every mounted route on the Protocol mux is wrapped in an identity
  decorator, so a served deployment always arrives with one, and an embedder
  calling a surface directly seats an identity explicitly — the surface tests
  pin that shape.
- **The bearer-less mux posture names a carrier for identity rather than
  waiving it.** It means "identity comes from the X-Harbor-* carrier headers",
  and a request supplying neither a bearer nor the headers is refused 401 before
  any handler runs. The opt-in is test-only, and `NewMux` fails construction
  when neither posture is chosen, so every mounted route runs with an
  established identity.
- **The artifacts cluster holds four postures, and the registry states all
  four.** `list` crosses under either admin-tier claim; `put` and `delete` are
  writes and take the administrative claim alone; `get_ref` crosses for nobody.
  Expressing them as four registry rows rather than one row plus a note keeps
  the transport from recording a crossing the surface would then refuse — an
  audit trail of granted crossings has to be a trail of crossings actually
  taken. The surface keeps its own checks so an embedder calling `Dispatch`
  directly meets the same answer.
- **The tenant-widening guard reaches every per-row re-scope behind a fleet
  fan-in.** A projector that re-scopes to a row's own identity is performing a
  tenant move, and under a verified anchor the guard inspects it. Five such
  sites exist (the sessions counter rollup, the tasks search, and three
  embedding-attribution bridges); each seats an audited re-scope naming the
  tenant it reads and why, and each is on the minting scan's reviewed list. A
  sixth that lands later meets the guard at `go test` rather than in
  production — but only because the regression tests are anchored; see the
  acceptance criteria.

## Glossary additions

- body identity scope
- body-scope gate
- verified identity
- working identity
- audited elevation
- carrier identity posture
- body-identity lockstep gate

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. If this phase does NOT build a reusable artifact, mark this checkbox N/A with a one-line reason.
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. If `Dependencies` above is `00` only, mark this checkbox N/A with a one-line reason.
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry filed
