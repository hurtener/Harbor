# Phase 72 — Console subscription protocol surface (re-affirmation + scope tightening)

## Summary

Re-affirms the existing `Pending` Phase 72 row (master plan line 771) under the Wave 13 staging
(`docs/plans/wave-13-decomposition.md` §4 row 72). Elevates the already-shipped `events.subscribe`
substrate (Phase 05 bus + Phase 06 replay + Phase 60 `/v1/events` SSE + Phase 61 `auth.ScopeAdmin` /
`auth.ScopeConsoleFleet` gate) into a **first-class Protocol method** named `events.subscribe` —
landing the method-name constant, the identity-rejection wire-status mapping, and the cross-tenant
scope-claim gate as a single binding contract third-party Consoles can target. The §13
primitive-with-consumer rule is discharged in-phase by the integration test consumer
(`test/integration/events_subscribe_scope_test.go`) + a scope-degradation regression suite that pins
every rejection mode. Filter-shape extensions and `events.aggregate` land in the sibling Phase 72a;
this phase is the scope-claim foundation 72a / 73c / 73d / 73g / 73j / 73k all compose on top of.

## RFC anchor

- RFC §5.2 (streaming events row)
- RFC §5.5 (Authentication — scope claims)
- RFC §6.13 (typed event bus — server-enforced identity, admin-scope audit emit)
- RFC §7.3 (Console binding conventions — no page phase without its feeding Protocol-surface phase)

## Briefs informing this phase

- brief 06 (events / observability / DevX — server-enforced identity, admin-scope bypass with audit emit)
- brief 11 (Console feature surface — Events view, §CC-2 Identity-aware UI, §CC-4 search)
- brief 12 (Console deployment + two-surface model — third-party Console parity)

## Brief findings incorporated

- brief 06 §"Subscribe ignores any filter that elides `TenantID`/`UserID`/`SessionID` unless the
  caller has `admin` scope. Cross-tenant subscriptions are an explicit, audited operation. This is
  the runtime analogue of [the project's] tenant-scoped query rules and is one of the three
  load-bearing isolation guarantees in `harbor_isolation.md`." Phase 72 re-affirms this contract at
  the Protocol-method level: the wire surface enforces what the bus already enforces, with the JWT
  scope claim (D-079) replacing the Phase 05 trust-based `Admin: true` boolean as the source of
  truth.
- brief 06 §6: "Cross-tenant isolation tests: subscriber for tenant A receives zero events emitted
  by tenant B; `admin` scope can bypass; assertion on the audit event for the bypass." This phase's
  integration test (`test/integration/events_subscribe_scope_test.go`) consumes this requirement
  verbatim — three tenants × the four scope-claim combinations × an audit assertion on every
  bypass.
- brief 11 §CC-2 "Identity-aware UI": "Every view respects the JWT's identity scope. Tenant-scoped
  users see only their tenant's data. Admin-scoped users see fleet across tenants (with an explicit
  'elevated view' indicator) ... Per-feature gates (impersonation, agent management, OAuth admin)
  require admin scope. CLAUDE.md §6 makes this mandatory — the Console enforces UI gates *and* the
  Protocol enforces server-side." Phase 72 ships the server-side half mechanically; UI gates land
  with the per-page Stage-2 phases.
- brief 12 §"the two-surface model": the scope-claim wire contract MUST be the same one third-party
  Console implementations consume. The method-name constant (`events.subscribe`) and the rejection
  Code (`CodeIdentityScopeRequired`) therefore live in `internal/protocol/methods/` and
  `internal/protocol/errors/` — Console-private hooks are rejection-on-sight per §13.

## Findings I'm departing from (if any)

None.

## Goals

- Elevate `events.subscribe` to a canonical Protocol method name in `internal/protocol/methods/`
  (currently the surface lives behind a transport route `/v1/events`; the method-name constant is
  the wire-contract anchor third-party Consoles consume — same shape as the Phase 54 task-control
  ten).
- Add `CodeIdentityScopeRequired` as a canonical Protocol error code so a cross-tenant filter
  without the `admin` (or `console:fleet`) scope claim returns a stable client-branchable code,
  distinct from `CodeIdentityRequired` (which signals missing identity entirely) and
  `CodeScopeMismatch` (which is reserved for steering-control scope mismatches per RFC §6.3).
- Verify the SSE handler maps the underlying `events.ErrIdentityScopeRequired` /
  `events.ErrAdminScopeRequired` sentinels (Phase 05) onto the new Protocol error code at the wire
  edge — preserving the existing audit emit (`audit.admin_scope_used`) for every elevated
  subscription.
- Ship the §13 primitive-with-consumer integration test: cross-tenant subscription rejected unless
  the JWT carries `ScopeAdmin` or `ScopeConsoleFleet`; scope-degradation regression suite (six
  scenarios — missing scope, wrong-tenant scope, expired token, dropped middleware, body-vs-token
  identity mismatch, claim downgrade attempt).
- Concurrent-reuse pin: N≥100 concurrent SSE subscriptions against a single shared `Handler` +
  `EventBus` instance under `-race`, asserting (a) per-subscriber identity isolation, (b) baseline
  goroutine count restored after all subscribers disconnect, (c) `audit.admin_scope_used` emitted
  exactly once per `Admin: true` subscribe — never silently coalesced under concurrent admin fan-in.

## Non-goals

- **Filter-shape extensions** (event-type set, time-window, run-set predicates beyond the existing
  triple + types). Lands in Phase 72a per the Wave 13 decomposition §4. The filter struct shape is
  a 72a deliverable.
- **`events.aggregate` Protocol method** (time-bucketed counts for the per-event-type sparkline).
  Lands in Phase 72a.
- **A new dedicated `events.crosstenant` scope claim.** D-079 settled the closed scope set
  (`ScopeAdmin` + `ScopeConsoleFleet`). The Wave 13 decomposition §4 row 72 phrases the wire intent
  as "cross-tenant claim (D-079)"; this phase consumes the existing two-scope set, not a third.
  Operator may revisit at a future RFC PR; not a Phase 72 deliverable.
- **A new transport route.** The existing `GET /v1/events` SSE route (Phase 60) IS the transport;
  Phase 72 elevates its method-name to a first-class canonical constant + tightens its rejection
  Code map.
- **Console-side saved-view persistence.** D-061 — Console-local. Phase 72h ships the Console DB
  schema.
- **Per-runtime fleet aggregation across multiple `harbor console` connections.** D-091 — Console-side
  aggregator; post-V1 gateway is a separate post-V1 phase.

## Acceptance criteria

- [ ] `internal/protocol/methods/methods.go` declares `MethodEventsSubscribe Method = "events.subscribe"`,
  registers it in `canonicalMethods`, and `IsValidMethod("events.subscribe")` returns `true`.
- [ ] `IsControlMethod("events.subscribe")` returns `false` (it is a streaming-events method, not a
  task-control method; the existing `IsControlMethod` predicate stays exclusive to the Phase 54 nine).
- [ ] `internal/protocol/errors/errors.go` declares `CodeIdentityScopeRequired Code = "identity_scope_required"`
  and registers it in `canonicalCodes`. `Codes()` returns the augmented set in lexicographic order.
- [ ] `internal/protocol/transports/stream/stream.go` maps the underlying
  `events.ErrIdentityScopeRequired` and `events.ErrAdminScopeRequired` sentinels onto
  `CodeIdentityScopeRequired` at the wire edge, with HTTP status 403 (the request is authenticated
  but its scope set does not authorize cross-tenant fan-in; distinct from 401 for
  `CodeIdentityRequired` / `CodeAuthRejected`).
- [ ] The existing `?admin=1` SSE gate (Phase 61) is preserved verbatim: requests carrying
  `ScopeAdmin` OR `ScopeConsoleFleet` succeed; requests with neither are rejected with the new
  Code. **No behavioural regression on Phase 60 / Phase 61 surface** — every prior smoke + every
  Phase 61 integration test still passes.
- [ ] `audit.admin_scope_used` (Phase 05 `EventTypeAdminScopeUsed`) is emitted exactly once per
  `Admin: true` subscribe, even under concurrent fan-in. The concurrent-reuse test pins this with a
  bus-side subscriber counting emits per subscriber's identity.
- [ ] Identity-rejection is fail-loudly per CLAUDE.md §6 rule 9 + D-033 pattern: a Subscribe
  request whose identity triple is missing any component is rejected at the SSE edge with
  `CodeIdentityRequired` (HTTP 401) before any subscription is opened — no silent empty stream, no
  identity-downgrading knob.
- [ ] Integration test `test/integration/events_subscribe_scope_test.go` ships under `-race`,
  consumes real `events/drivers/inmem` driver + real `protocol/auth.Middleware` + real
  `protocol/transports/stream.Handler`. Scenarios:
  - **Happy-path triple-scoped subscribe** — tenant A subscriber sees tenant A events; receives
    zero tenant B events.
  - **Cross-tenant rejection without scope** — tenant A subscriber requesting `?admin=1` with a
    plain JWT (no scopes) gets 403 + `CodeIdentityScopeRequired`.
  - **Cross-tenant acceptance with `ScopeAdmin`** — same request with a JWT carrying `admin` scope
    gets 200, sees both tenants' events, and the subscription begins with an emitted
    `audit.admin_scope_used` event the subscriber itself observes.
  - **Cross-tenant acceptance with `ScopeConsoleFleet`** — same as above with `console:fleet`
    instead of `admin`. Both scopes satisfy the gate.
  - **Scope-degradation regression: expired token** — Phase 61 sentinels surface as
    `CodeAuthRejected` (401); Phase 72's scope code is NOT reached.
  - **Scope-degradation regression: dropped middleware** — a request reaching the Handler with no
    `auth.ScopesFrom`-detected scopes (no-middleware fallback) is rejected from cross-tenant fan-in
    by default (the existing Phase 60 trust-based posture, never honoured for `?admin=1` since
    Phase 61).
- [ ] Concurrent-reuse test `internal/protocol/transports/stream/concurrent_scope_test.go` (or
  extending the existing `internal/protocol/transports/concurrent_test.go`): N=128 concurrent
  subscribers against ONE shared `Handler` + ONE shared `EventBus`, half triple-scoped and half
  admin-scoped, under `-race`. Asserts (a) no data races, (b) per-subscriber identity captured on
  every event is the originating subscriber's triple (no context bleed), (c)
  `runtime.NumGoroutine()` returns to baseline after every subscriber disconnects, (d) each admin
  subscriber's emitted `audit.admin_scope_used` is the originating subscriber's, never another
  goroutine's.
- [ ] `scripts/smoke/phase-72.sh` exercises the surface against the live `harbor dev` binary, with
  404/405/501 → SKIP per CLAUDE.md §4.2. At minimum: (a) `GET /v1/events` with a complete identity
  triple returns 200 + SSE shape; (b) `GET /v1/events?admin=1` without `ScopeAdmin` returns 403 +
  `identity_scope_required` Code in body; (c) `GET /v1/events` with missing `X-Harbor-Tenant`
  returns 401; (d) the method-name constant `events.subscribe` is discoverable via the Phase 59
  capability handshake when it exists, otherwise SKIP that probe.
- [ ] `docs/plans/README.md` row 96 — Phase 72 — flips from `Pending` to `Shipped` in this PR. The
  detail block (line 771) gains a one-line pointer to this phase plan file.

## Files added or changed

```text
internal/protocol/methods/methods.go                              # +MethodEventsSubscribe const + registration
internal/protocol/methods/methods_test.go                         # +TestMethods_EventsSubscribe_Registered
internal/protocol/errors/errors.go                                # +CodeIdentityScopeRequired const + registration
internal/protocol/errors/errors_test.go                           # +TestCodes_IdentityScopeRequired
internal/protocol/transports/stream/stream.go                     # rejection-Code mapping: events.Err*ScopeRequired → CodeIdentityScopeRequired (HTTP 403)
internal/protocol/transports/stream/stream_test.go                # +TestServeHTTP_CrossTenantWithoutScope_Returns403
internal/protocol/transports/stream/concurrent_scope_test.go      # N=128 concurrent subscribers under -race (D-025)
internal/protocol/transports/control/status.go                    # +CodeIdentityScopeRequired → 403 mapping
test/integration/events_subscribe_scope_test.go                   # the §13 primitive-with-consumer + scope-degradation regression suite
scripts/smoke/phase-72.sh                                         # surface assertions (404/405/501 → SKIP)
docs/plans/README.md                                              # Phase 72 row Pending → Shipped + detail-block file pointer
docs/glossary.md                                                  # +events.subscribe, +identity_scope_required, +scope-degradation regression
```

## Public API surface

```go
// internal/protocol/methods/methods.go
const MethodEventsSubscribe Method = "events.subscribe"

// internal/protocol/errors/errors.go
const CodeIdentityScopeRequired Code = "identity_scope_required"
// Returned at the wire edge when a Subscribe request's identity scope is
// insufficient for the requested fan-in — typically a cross-tenant
// (?admin=1) request from a JWT lacking ScopeAdmin or ScopeConsoleFleet.
// HTTP status 403 (the request is authenticated; the scope set does not
// authorize the operation). Distinct from CodeIdentityRequired (missing
// triple, 401) and CodeAuthRejected (token invalid, 401). Maps from
// events.ErrIdentityScopeRequired and events.ErrAdminScopeRequired.
```

No new wire-payload types — `events.Filter` (Phase 05) stays the in-runtime shape; the SSE transport
continues to compose it from header carriers (Phase 60) + the verified scope set on `ctx` (Phase 61).
Phase 72a adds the cross-wire filter struct in `internal/protocol/types/events.go`.

## Test plan

- **Unit:**
  - `internal/protocol/methods/methods_test.go::TestMethods_EventsSubscribe_Registered` — pins the
    new constant + asserts `IsValidMethod("events.subscribe")` is true, `IsControlMethod("events.subscribe")`
    is false, and `Methods()` returns the augmented sorted set.
  - `internal/protocol/errors/errors_test.go::TestCodes_IdentityScopeRequired` — pins the new code,
    asserts `Codes()` returns it in lexicographic order, and asserts
    `IsValidCode("identity_scope_required")` is true. Includes a string-stability assertion: the
    wire value is exactly `"identity_scope_required"` (third-party Consoles branch on the string).
  - `internal/protocol/transports/control/status_test.go::TestStatusFor_CodeIdentityScopeRequired_Returns403`
    — pins the HTTP-status mapping.
  - `internal/protocol/transports/stream/stream_test.go::TestServeHTTP_CrossTenantWithoutScope_Returns403`
    — direct httptest with a context carrying no scopes + `?admin=1`; assert 403 + body Code
    `identity_scope_required`.
  - `internal/protocol/transports/stream/stream_test.go::TestServeHTTP_BusReturnsAdminScopeRequired_Maps403`
    — inject a bus whose `Subscribe` returns `events.ErrAdminScopeRequired`; assert 403 +
    correct Code. (Defensive — Phase 61's `?admin=1` gate normally short-circuits before
    `Subscribe`, but the mapping must hold if a future filter variant lets the bus return the
    error.)
- **Integration:**
  - `test/integration/events_subscribe_scope_test.go` — the §13 same-phase consumer. Wires real
    `events/drivers/inmem`, real `protocol/auth.Middleware` over the real ES256 testdata keypair
    (`internal/protocol/auth/testdata/`), real `transports.NewMux`. Six scenarios per the
    Acceptance Criteria list. Identity-propagation assertion on every event: the receiving
    subscriber's identity matches the emitting publisher's triple (or, for admin, both publisher's
    triples are seen by the admin subscriber).
- **Conformance:**
  - Extend `internal/protocol/conformance/conformance.go`'s `wantSet` (methods matrix) to include
    `MethodEventsSubscribe`. The matrix-exhaustiveness check at the top of `RunSuite` will fail
    until the entry lands. Add `runEventsSubscribeNegotiation` to exercise the method via the
    over-the-wire `httptest.Server` profile.
  - Extend the error-code matrix to include `CodeIdentityScopeRequired` with its 403 mapping. The
    matrix-exhaustiveness check derives the canonical set from `errors.Codes()` (D-080 amendment
    per PR #91) so a new code without a matrix entry surfaces by name.
- **Concurrency / leak:**
  - `internal/protocol/transports/stream/concurrent_scope_test.go::TestStreamHandler_ConcurrentScopedReuse`
    — D-025 contract. N=128 concurrent subscribers against ONE shared `Handler` + ONE shared
    `EventBus`, half triple-scoped (distinct tenants T1..T64) and half admin-scoped (`ScopeAdmin`
    or `ScopeConsoleFleet` alternating). Race detector on. Asserts per-subscriber identity capture
    (no context bleed), one `audit.admin_scope_used` per admin subscribe (no coalescing under
    contention, no double-emit), and `runtime.NumGoroutine()` returns to baseline after every
    subscriber's request goroutine returns.

## Smoke script additions

`scripts/smoke/phase-72.sh` header: `# PREFLIGHT_REQUIRES: live-server`. Helpers used:
`api_url`, `protocol_call`, `assert_status`, `skip_if_404`, `assert_json_path`, `smoke_summary`,
`skip`. The 404/405/501 → SKIP convention is honoured per CLAUDE.md §4.2 so the script is harmless
on builds pre-dating Phase 72 (and on builds post-Phase-60 that haven't yet adopted the new Code).

Assertions:

- **Surface probe** — `skip_if_404 "$(api_url /v1/events)" "phase 72: SSE route present"` gates
  the rest. (Phase 60 ships the route; on a pre-Phase-60 build the script SKIPs cleanly.)
- **Method-name constant** — `protocol_call 'events/subscribe' '{}'` exercises the canonical method
  name through the transport. (Stub today; flips to OK when the Protocol layer carries the
  method-name lookup.)
- **Triple-scoped happy path** — `assert_status 200 "$(api_url /v1/events)"` with the dev-token
  header set (the existing Phase 60 + Phase 61 carrier headers).
- **Missing identity → 401** — `assert_status 401` against the same URL with no identity headers.
- **Cross-tenant without scope → 403** — `assert_status 403 "$(api_url /v1/events?admin=1)"` with
  a non-admin token. The integration test pins the Code in the body; the smoke pins the status.
- **Cross-tenant with scope → 200** — `assert_status 200 "$(api_url /v1/events?admin=1)"` with an
  admin-scoped token. Subscribers may not be testable in a shell smoke (long-lived SSE), so the
  assertion is on the open-of-stream status only.
- **Method registration** — `protocol_call 'events/subscribe' '{"missing":"identity"}'` exercises
  the rejection path through the canonical method route; SKIPs until the Protocol method-name
  router lands.

## Coverage target

- `internal/protocol/methods`: 100% (constants + tiny predicate functions; the existing target stays).
- `internal/protocol/errors`: 100% (same reason).
- `internal/protocol/transports/stream`: ≥ 86.6% (the Phase 61 floor — the new mapping addition
  must not drop coverage).
- `internal/protocol/transports/control`: ≥ 89.5% (Phase 61 floor; `status.go` gains the new
  mapping line).
- `test/integration` is non-coverage-tracked per the existing convention.

## Dependencies

**Same-wave (Wave 13, Stage 1 Batch A — parallel-able with 72a, 72b, 72c, 72d):**

- None. Phase 72 depends only on already-shipped phases.

**Already shipped (pre-Wave 13):**

- Phase 05 (events bus + identity-gated Subscribe + `audit.admin_scope_used` +
  `ErrIdentityScopeRequired` / `ErrAdminScopeRequired` — `Shipped`).
- Phase 06 (Bus replay + cursor + Replayer + admin-scope replay audit emit — `Shipped`).
- Phase 58 (Protocol single-source enforcement — `Shipped`; ensures the new method constant +
  error code land in their canonical packages and only there).
- Phase 59 (Protocol version handshake — `Shipped`; the method-name constant feeds the
  `Capabilities()` set in 72a + later phases).
- Phase 60 (Protocol wire transport — `Shipped`; the `/v1/events` route + SSE handler + Last-Event-ID
  reconnect cursor + carrier-header identity resolution).
- Phase 61 (Protocol auth — `Shipped`; the JWT validator + `auth.Middleware` + `ScopeAdmin` +
  `ScopeConsoleFleet` + `?admin=1` gate + `CodeAuthRejected`).
- Phase 62 (Protocol conformance — `Shipped`; the matrix-exhaustiveness check is the gate that
  forces the new method + code to land with a scenario, not as a free-floating addition).

## Risks / open questions

- **Method-name string stability.** Once `events.subscribe` ships as a Protocol method constant,
  third-party Console implementations branch on the exact string. Changing it later is a Protocol
  version bump (RFC §5.3). The chosen name matches the Wave 13 decomposition §4 row 72 wording
  verbatim and the page-events.md §12 reconciliation; pinning it now closes that drift surface.
- **Whether to mint a new dedicated `events.crosstenant` scope claim** (as the Wave 13
  decomposition §4 row 72 phrasing hints at). Recommendation: **NO** — D-079 already settled the
  closed scope set (`ScopeAdmin` + `ScopeConsoleFleet`) and Brief 11 §CC-2 maps both to the
  cross-tenant case (admin = full fleet; console:fleet = fleet observation). Introducing a third
  scope here would re-litigate D-079 without new evidence and would also leak into Phase 73's
  state-inspection methods (sessions.inspect, tasks.get, etc.) which are also scope-gated. **If
  the operator wants a finer-grained scope vocabulary, that is an RFC PR + new decisions entry,
  not a Phase 72 deliverable.** Documented here so the question doesn't re-emerge at review.
- **HTTP status for the new Code.** 403 (Forbidden — the request is authenticated but the scope
  set does not authorize the operation) vs 401 (Unauthorized — the request lacks an authenticated
  identity). Phase 61's `?admin=1` gate already returns 403 with a body string `"scope_mismatch:
  …"` — Phase 72 preserves that status and adds the typed Code so a client can branch on the
  Code rather than the body prose. Documented for review-time confirmation.
- **Two pre-existing sentinels (`ErrIdentityScopeRequired` + `ErrAdminScopeRequired`) map to one
  Code.** Phase 05 distinguishes them at the Go-level for callers that want to branch on "filter
  elided the triple AND admin was false" vs "admin was true but the operator-side scope claim was
  absent." The wire surface deliberately collapses both into `CodeIdentityScopeRequired`: from
  the third-party Console's perspective the operator-actionable answer is the same — attach a
  scope-bearing token. The Go-level distinction stays available for in-process callers that need
  it. **Operator may flag in review** if a wire-level distinction is desired; documented here so
  the question doesn't re-emerge.
- **Capability handshake (Phase 59) advertised set.** Phase 59 advertises `CapTaskControl`
  today. Once `events.subscribe` is a canonical method, an `events_subscribe` capability constant
  should be added to `types.Capabilities()` so a client can negotiate the surface explicitly. This
  is mechanical and lands as part of this phase; Phase 72a's `events.aggregate` adds another
  capability constant on the same shape.

## Glossary additions

- **`events.subscribe`** — canonical Protocol method name (Phase 72) for opening a server-filtered
  event subscription. Wire-transport route: `GET /v1/events` SSE (Phase 60). Identity-mandatory; a
  request with `?admin=1` requires `auth.ScopeAdmin` or `auth.ScopeConsoleFleet` (D-079) and emits
  `audit.admin_scope_used` on every elevated subscribe.
- **`identity_scope_required`** — canonical Protocol error Code (Phase 72) returned when a
  Subscribe request's scope set is insufficient for the requested cross-tenant fan-in. HTTP 403.
  Maps from `events.ErrIdentityScopeRequired` and `events.ErrAdminScopeRequired`. Distinct from
  `identity_required` (missing triple, 401) and `auth_rejected` (token invalid, 401).
- **Scope-degradation regression suite** — Phase 72 integration-test surface
  (`test/integration/events_subscribe_scope_test.go`) that pins six rejection modes: missing
  scope, expired token, dropped middleware, body-vs-token identity mismatch, claim downgrade
  attempt, and cross-tenant without scope. Re-asserts the §17.6 "fix what the integration test
  finds — no matter where the bug lives" contract at the scope-claim boundary.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (the Phase 61 floors hold for
  `transports/stream` + `transports/control`; the new constants in `methods` + `errors` keep those
  packages at 100%).
- [ ] **Multi-isolation paths changed: cross-session isolation test passes.** Binding for this
  phase — the scope-claim shape is the multi-isolation contract's wire boundary. The integration
  test asserts cross-tenant subscription rejection AND admin-scope bypass with audit emission.
- [ ] **Concurrent-reuse test passes** — N=128 concurrent SSE subscribers against one shared
  `Handler` + one shared `EventBus` under `-race`, asserting no cross-talk, no goroutine leak
  after teardown, exactly-one `audit.admin_scope_used` per admin subscribe. (D-025)
- [ ] **Integration test exists** — `test/integration/events_subscribe_scope_test.go` wires real
  `events/drivers/inmem` + real `protocol/auth.Middleware` + real `transports.NewMux` under
  `-race`, with six scope-degradation scenarios + identity propagation across the seam. (§17)
- [ ] **Protocol conformance suite extended** — `internal/protocol/conformance/conformance.go`'s
  method matrix + error-code matrix exhaustiveness checks include the new constant + new code.
  (D-080 / Phase 62)
- [ ] If Protocol types changed: every reference still compiles (the canonical-source files in
  `internal/protocol/methods/` + `internal/protocol/errors/` are the only authoritative additions;
  the lockstep test in `internal/protocol/singlesource` flags any leak).
- [ ] **Master plan row + detail block updated** — `docs/plans/README.md` Phase 72 row flips
  `Pending` → `Shipped` and the detail block at line 771 gains a one-line pointer to this phase
  plan file. (CLAUDE.md §4.2 item 11)
- [ ] **Coordinator-verify pass complete** before the PR is opened for operator review (Wave 13
  decomposition §12 lock-in item 3 + the binding coordinator-verify protocol).
- [ ] Glossary updated with `events.subscribe`, `identity_scope_required`, and
  `Scope-degradation regression suite`.
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
  (None for this phase.)
