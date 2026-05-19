# Phase 72b — `IdentityScope` admin-impersonation extension

## Summary

Extends the Protocol wire identity (`internal/protocol/types/IdentityScope` + `internal/protocol/types/StartRequest`) with an admin-impersonation triplet — `actor` / `requester` / `impersonating` — so an operator with the `auth.ScopeAdmin` claim can spawn (or steer) a run "on behalf of" another `(tenant, user)` while every request still carries BOTH the requesting admin's verified identity AND the impersonated identity for audit. Per Brief 11 §PG-5 ("Run as another identity") and `docs/design/console/page-playground.md` §3 / §5 / §12 row "Run as identity". The first consumer is a same-phase integration test (`test/integration/identityscope_impersonation_test.go`) that round-trips a `start` request through the real Phase 60 transport + Phase 61 auth middleware, asserts the audit event (`audit.admin_scope_used`) emits with the impersonation triplet, and pins the non-admin → `CodeScopeMismatch` regression. The Sessions-page "identity column" UI consumer ships in 73c (Stage 2.2) per `docs/plans/wave-13-decomposition.md` §4.

## RFC anchor

- RFC §5.5 (Authentication — JWT-borne identity scope; `(tenant, user, session)` is mandatory)
- RFC §6.16 (Agent Registry — admin scope tier for fleet operations)
- RFC §7 (Console layer — `Run as identity` is a Console-facing privilege, served via the canonical Protocol surface)

## Briefs informing this phase

- brief 11 (Console feature surface — §PG-5 "Run as another identity"; §CC-2 identity-aware UI; the impersonation triplet `actor=admin, requester=admin, impersonating=user_id` is named verbatim)
- brief 12 (deployment + two-surface model — the wire shape must be the SAME `internal/protocol/types/` extension that third-party Console implementations consume; no Console-private impersonation hook)

## Brief findings incorporated

- brief 11 §PG-5: "Run as another identity (admin only — impersonation, with full audit; the request's `(actor=admin, requester=admin, impersonating=user_id)` is captured)". The three-field triplet is the load-bearing primitive this phase ships; we add exactly those field names to `IdentityScope` so the wire shape mirrors the Brief verbatim.
- brief 11 §"Constraints on the Playground": "Identity is mandatory — Playground sessions carry the operator's identity (or the impersonated identity); no anonymous Playground." The extension MUST NOT introduce an identity-downgrading knob: when `impersonating` is set, the request still carries a complete `(tenant, user, session)` triple (the impersonated triple), AND `requester` carries the admin's verified identity. There is no "anonymous admin" mode.
- brief 11 §CC-2: "Per-feature gates (impersonation, agent management, OAuth admin) require admin scope." The Protocol edge gates impersonation on `auth.HasScope(ctx, auth.ScopeAdmin)` per D-079; a present `impersonating` field on a non-admin token is rejected with `CodeScopeMismatch` before any runtime work runs.
- brief 12 §"the two-surface model": "the wire shape MUST be the same one third-party Console implementations consume" — the impersonation triplet lives on `internal/protocol/types/IdentityScope`, NOT in `web/console/`. A third-party Console implementing `harbor console` from scratch sees the same `IdentityScope` shape.

## Findings I'm departing from (if any)

None.

## Goals

- Extend `internal/protocol/types/IdentityScope` with three new optional fields — `Actor`, `Requester`, `Impersonating` — carrying the admin-impersonation triplet per Brief 11 §PG-5.
- Wire the extension through the Phase 60 transport + Phase 61 auth middleware so the impersonation triplet is honored ONLY when the verified `auth.Scope` set contains `auth.ScopeAdmin`; a non-admin request that carries a non-empty `Impersonating` field is rejected loudly at the Protocol edge with `CodeScopeMismatch`.
- Audit-emit `audit.admin_scope_used` (event type ALREADY registered in `internal/events/events.go`) on every accepted impersonation, with a typed payload carrying `(Actor, Requester, Impersonating, Reason="impersonation")` — never the raw JWT, never the unredacted target identity.
- Backward-compatible: when all three impersonation fields are empty, behavior is identical to today's `StartRequest` surface (the verified JWT identity IS the request identity).
- Fail loudly on every malformed impersonation shape (`Impersonating` set without `Actor` / `Requester`; `Actor != verified-admin-identity`; `Requester != verified-admin-identity`; `Impersonating` triple missing any of `tenant_id` / `user_id` / `session_id`).

## Non-goals

- A new Protocol METHOD for impersonation. Impersonation is a wire-field extension; the existing `start` / `redirect` / `user_message` methods carry it through their existing `IdentityScope` field. **The shipped Protocol-method set stays at exactly 10 (Phase 54): `start`, `cancel`, `pause`, `resume`, `redirect`, `inject_context`, `approve`, `reject`, `prioritize`, `user_message`.**
- Per-tool-call impersonation downgrade. A run that started under impersonation runs end-to-end as the impersonated identity (MCP OAuth token, tool approval gate, memory scope) per brief 09 §"Multiple identity scopes" — re-shifting identity mid-run is post-V1.
- Console UI for the impersonation selector. The Sessions-page "identity column" UI lands in 73c; the Playground "Run as identity" selector lands in 73n. 72b ships the wire primitive + a test consumer, not a UI consumer (per the §13 primitive-with-consumer rule, the test consumer suffices).
- Cross-tenant impersonation rate limiting / audit summarisation dashboards. Post-V1 governance work.
- Impersonation-token issuance / management. Operator IdP territory (RFC §5.5); Harbor only verifies the JWT it receives.

## Acceptance criteria

- [ ] `internal/protocol/types/control.go` extends `IdentityScope` with three optional fields — `Actor IdentityScope` / `Requester IdentityScope` / `Impersonating IdentityScope` — wire-tagged `actor,omitempty` / `requester,omitempty` / `impersonating,omitempty`. The nested `IdentityScope` shape (NOT a flat string) carries the full `(tenant, user, session)` so the impersonated identity is unambiguous.
- [ ] The triplet semantics are pinned by godoc on the new fields: `Actor` = the verified admin identity at the request edge (echoed for audit), `Requester` = the originating admin identity (same as Actor at V1; distinct in delegated-impersonation post-V1), `Impersonating` = the target identity the run executes under.
- [ ] `internal/protocol/transports/control/control.go` extends `assertBodyMatchesAuthedIdentity` so: (a) when `Impersonating` is empty, behavior is unchanged (the existing JWT-identity == body-identity check applies); (b) when `Impersonating` is non-empty, the verified JWT MUST carry `auth.ScopeAdmin`, the body's `Identity.Actor.Tenant/User/Session` MUST match the verified JWT identity, AND the body's `Identity.Requester` MUST equal `Identity.Actor` (V1 invariant: requester = actor; the field exists for post-V1 delegated impersonation per D-NEW-1).
- [ ] On a present `Impersonating` field without `auth.ScopeAdmin`, the surface rejects with `*protocol.Error{Code: CodeScopeMismatch}` BEFORE `Dispatch` runs (defence in depth at the transport edge, mirroring Phase 61 D-079 §4).
- [ ] On a present `Impersonating` field with `Tenant` / `User` / `Session` missing, the surface rejects with `*protocol.Error{Code: CodeIdentityRequired}` — identity is mandatory; the impersonated triple is identity too. NEVER silently downgrades.
- [ ] On acceptance, the surface emits the `audit.admin_scope_used` canonical event onto the bus via the wired `events.Bus`, with a typed `AdminScopeUsedPayload` carrying `(Actor IdentityScope, Requester IdentityScope, Impersonating IdentityScope, Reason="impersonation", Method protocol-method-name)`. The payload is a `SafePayload` by construction (only the three flat-string-shaped identities + two bounded enum strings; no caller-controlled bytes).
- [ ] The audit event is emitted via the wired `audit.Redactor` (per CLAUDE.md §7 rule 6 + D-079 §1 amendment): a `Validator` / `ControlSurface` without an attached `Redactor` fails at construction (the impersonation extension does NOT introduce a noop redactor escape hatch).
- [ ] `internal/protocol/types/types_test.go` (or a new sibling) pins the JSON round-trip: an `IdentityScope` with the impersonation triplet round-trips byte-identical through `json.Marshal` / `json.Unmarshal`; an `IdentityScope` without the triplet omits the three fields entirely (`omitempty` enforcement).
- [ ] `test/integration/identityscope_impersonation_test.go` runs the round-trip end-to-end through the REAL Phase 60 transport mux + REAL Phase 61 `Validator` (RS256 keypair from `testdata/`) + REAL `audit.Redactor` from `audit/drivers/patterns` + REAL `events.Bus` from `events/drivers/inmem`. Asserts: (a) admin token + impersonation triplet → 200 + audit event on the bus with the expected payload; (b) non-admin token + impersonation triplet → 401/403 with `CodeScopeMismatch`; (c) admin token + impersonation triplet missing a triple component → 401/403 with `CodeIdentityRequired`; (d) admin token + empty impersonation fields → 200 with NO audit event (backward-compat); (e) impersonation through `redirect` / `user_message` methods works identically to `start`. Runs under `-race`.
- [ ] Identity-rejection regression: a `StartRequest` whose body claims a verified identity different from the JWT's still rejects (Phase 61 defence-in-depth still holds — impersonation does NOT widen the body-identity-vs-JWT check, it adds a separate field that's verified separately).
- [ ] `audit.admin_scope_used` event payload extension lands as an ADDITION to the existing event (Phase 06 already registered the type); the typed payload struct `AdminScopeUsedPayload` is defined where the existing emit site lives (`internal/events/drivers/inmem` + `internal/events/drivers/durable`) OR — preferred — promoted to `internal/protocol/auth/events.go` alongside `AuthRejectedPayload` if the existing emit is currently anonymous. Coordinator picks the home in implementation; the plan flags both as acceptable.
- [ ] `scripts/smoke/phase-72b.sh` invokes `start` with each shape (admin + impersonation accepted; non-admin + impersonation rejected; admin + impersonation + missing triple rejected) and asserts the audit event surfaces on the events stream within a bounded window.

## Files added or changed

```text
internal/protocol/types/control.go                       # +Actor / +Requester / +Impersonating on IdentityScope; godoc pins semantics
internal/protocol/types/types_test.go                    # JSON round-trip; omitempty regression
internal/protocol/transports/control/control.go          # +impersonation gate in assertBodyMatchesAuthedIdentity; +audit emit
internal/protocol/transports/control/control_test.go     # gate edge cases (5 shapes from acceptance row 9)
internal/protocol/auth/events.go                         # +AdminScopeUsedPayload (or co-located with existing emit site — coordinator picks)
internal/protocol/auth/events_test.go                    # payload round-trip + SafePayload assertion
internal/protocol/singlesource/lockstep_test.go          # if CanonicalWireTypes needs an entry — coordinator confirms
test/integration/identityscope_impersonation_test.go     # 5-shape end-to-end + -race
scripts/smoke/phase-72b.sh                               # protocol_call assertions (5 shapes)
docs/glossary.md                                         # +"impersonation", +"actor (impersonation)", +"requester (impersonation)", +"impersonating (impersonation)"
docs/decisions.md                                        # +D-NEW-1 — pre-assigned per dispatch
```

## Public API surface

```go
// internal/protocol/types/control.go (extends the existing IdentityScope)

// IdentityScope is the flat wire identity a Protocol task-control request
// carries. (existing godoc preserved.)
//
// Wave 13 (Phase 72b) extension — admin impersonation per Brief 11 §PG-5.
// The three new fields are optional and mutually-required: an
// IdentityScope MAY carry zero impersonation fields (today's behavior,
// the verified JWT identity IS the request identity) OR all three set
// (admin-on-behalf-of-user). The runtime rejects any other shape loudly
// at the Protocol edge — never silently degrades.
type IdentityScope struct {
    // Tenant / User / Session / Run / Scope are the existing fields
    // (unchanged from Phase 54). When impersonation is in use, these
    // fields carry the IMPERSONATED identity — the identity the run
    // executes under.
    Tenant  string `json:"tenant"`
    User    string `json:"user"`
    Session string `json:"session"`
    Run     string `json:"run,omitempty"`
    Scope   string `json:"scope,omitempty"`

    // Actor is the verified admin identity at the request edge — the
    // identity whose JWT claim was validated by the Phase 61 middleware.
    // V1 invariant: Actor MUST equal the JWT's verified `(tenant, user,
    // session)` triple; the transport rejects a body claiming a
    // different Actor with CodeScopeMismatch. The Actor's audit trail
    // ("admin X impersonated user Y at time T") is what makes
    // impersonation accountable.
    Actor *IdentityScope `json:"actor,omitempty"`

    // Requester is the originating admin identity for delegated
    // impersonation chains (e.g. an admin acting on behalf of another
    // admin's audited request). At V1: Requester MUST equal Actor; the
    // field exists so post-V1 delegated impersonation does not require
    // a wire-shape break. The runtime rejects Requester != Actor with
    // CodeScopeMismatch.
    Requester *IdentityScope `json:"requester,omitempty"`

    // Impersonating is the target identity the run executes under. When
    // non-empty, MUST carry a complete `(tenant, user, session)` triple
    // — identity is mandatory; the impersonated triple is identity too.
    // Setting Impersonating gates on auth.ScopeAdmin on the verified
    // JWT; a non-admin request with Impersonating set is rejected with
    // CodeScopeMismatch before Dispatch runs.
    //
    // V1 semantics: the top-level Tenant/User/Session fields MUST equal
    // the Impersonating triple when impersonation is in use — the run
    // executes as the impersonated identity. The Actor field carries
    // the audit-visible record of WHO impersonated.
    Impersonating *IdentityScope `json:"impersonating,omitempty"`
}

// internal/protocol/auth/events.go (extends Phase 61's events.go, or
// co-located with the existing audit.admin_scope_used emit site).

// AdminScopeUsedPayload is the typed payload on the
// audit.admin_scope_used canonical event when the emit source is an
// impersonation request (vs. an events.Subscribe admin filter — that
// emit predates this phase and carries no typed payload at V1; an
// untyped emit stays valid until that subsystem types its payload).
//
// SafePayload by construction: every field is a bounded-string-shaped
// identity component plus two enum strings. No caller-controlled bytes
// reach the bus.
type AdminScopeUsedPayload struct {
    events.SafeSealed
    // Actor is the verified admin identity at the Protocol edge.
    Actor IdentityTriple
    // Requester is the originating admin identity (V1: == Actor).
    Requester IdentityTriple
    // Impersonating is the target identity the run executes under.
    Impersonating IdentityTriple
    // Reason is the stable sentinel name. "impersonation" for this
    // phase's emit; future emit sites add new sentinels.
    Reason string
    // Method is the Protocol method that carried the impersonation
    // (one of the ten canonical methods: typically "start" but
    // "redirect" / "user_message" are also acceptable).
    Method string
}

// IdentityTriple is the flat audit-visible shape of an IdentityScope
// (no nested Actor/Requester/Impersonating — those collapse to their
// triple at the payload boundary).
type IdentityTriple struct {
    Tenant  string
    User    string
    Session string
}
```

## Test plan

- **Unit:**
  - `internal/protocol/types/types_test.go` — JSON round-trip: an `IdentityScope` with all three impersonation fields set round-trips byte-identical; an `IdentityScope` with none set produces zero `actor` / `requester` / `impersonating` keys in the JSON output (`omitempty` regression).
  - `internal/protocol/transports/control/control_test.go` — five-shape gate table: (1) admin + impersonation + body-triple == impersonating triple → accepted; (2) admin + impersonation + body-triple != impersonating triple → `CodeScopeMismatch`; (3) admin + impersonation + missing component on impersonating → `CodeIdentityRequired`; (4) non-admin + impersonation → `CodeScopeMismatch`; (5) admin + no impersonation → accepted (backward-compat).
  - `internal/protocol/auth/events_test.go` — `AdminScopeUsedPayload` round-trip + SafePayload structural assertion (the payload composes `events.SafeSealed` so it is bus-publishable; no caller-controlled string fields).
- **Integration:**
  - `test/integration/identityscope_impersonation_test.go` — REAL Phase 60 transport mux (`httptest.Server` per the Phase 62 conformance pattern) + REAL Phase 61 `Validator` (RS256 keypair from `internal/protocol/auth/testdata/`) + REAL `audit.Redactor` from `audit/drivers/patterns` + REAL `events.Bus` from `events/drivers/inmem`. Runs the five shapes from the unit table end-to-end through the actual wire. Asserts the audit event on the bus subscriber. Cross-method coverage: same five shapes against `start`, `redirect`, `user_message`. Under `-race`. Identity-propagation cross-check: the audit event's Actor / Requester / Impersonating fields match the request shape exactly.
  - **Identity-rejection regression**: a body whose top-level Tenant/User/Session does not match the verified JWT triple AND whose Impersonating is empty still rejects (Phase 61 defence-in-depth still holds).
- **Conformance:**
  - The five-shape table is added to the Phase 62 `internal/protocol/conformance` suite as a new scenario row (`runImpersonationScenarios`). The suite runs against BOTH the in-process `ControlSurface.Dispatch` AND the over-the-wire mux. Asserts identical wire shapes from both transports.
- **Concurrency / leak:**
  - **N/A for 72b's primitive** — `IdentityScope` is a wire type, not a reusable artifact. The `ControlSurface` it flows through IS a D-025 reusable artifact (Phase 54 already pins N≥128 in `control_concurrent_test.go`); 72b adds a sub-test that fans out impersonation requests across that existing stress to assert no Actor/Requester/Impersonating field cross-contamination under load. (The §11 concurrent-reuse contract is satisfied at the ControlSurface level; 72b's contribution is the per-field assertion in the existing stress.)

## Smoke script additions

`scripts/smoke/phase-72b.sh` (header: `# PREFLIGHT_REQUIRES: live-server`):

- `protocol_call 'start' '{"identity": {"tenant": "t1", "user": "u-target", "session": "s1", "actor": {"tenant": "t1", "user": "u-admin", "session": "s1"}, "requester": {"tenant": "t1", "user": "u-admin", "session": "s1"}, "impersonating": {"tenant": "t1", "user": "u-target", "session": "s1"}}, "query": "test"}'` (admin token) → assert 200; `assert_json_path '.task_id | type' 'string'`.
- `protocol_call 'start' '{"identity": {"tenant": "t1", "user": "u-target", "session": "s1", "impersonating": {"tenant": "t1", "user": "u-target", "session": "s1"}}, "query": "test"}'` (non-admin token) → `assert_status 401` or `403` (CodeScopeMismatch on the body).
- `protocol_call 'start' '{"identity": {"tenant": "t1", "user": "u-target", "session": "s1", "actor": {"tenant": "t1", "user": "u-admin", "session": "s1"}, "requester": {"tenant": "t1", "user": "u-admin", "session": "s1"}, "impersonating": {"tenant": "t1", "user": "u-target", "session": ""}}, "query": "test"}'` (admin token, missing session on Impersonating) → `assert_status 401` or `403` (CodeIdentityRequired).
- `protocol_call 'start' '{"identity": {"tenant": "t1", "user": "u-admin", "session": "s1"}, "query": "test"}'` (admin token, no impersonation) → assert 200 (backward-compat).
- After the accepted shape, `skip_if_404 "$(api_url /protocol/events/subscribe)" 'phase 72b: audit event observable on bus'` then `protocol_call 'events/subscribe' '{"filter": {"event_types": ["audit.admin_scope_used"]}}'` and `assert_json_path '.payload.reason' 'impersonation'` — assert the audit event surfaced on the events stream with `Reason=impersonation`. (Surface-existence guards the assertion against pre-72a builds where the filter shape is not yet shipped.)

## Coverage target

- `internal/protocol/types`: 92% (was 90%; the impersonation extension is small and entirely testable).
- `internal/protocol/transports/control`: maintain ≥ 89.5% (Phase 61's D-079 target; the new gate code adds testable surface but no degradation).
- `internal/protocol/auth`: maintain ≥ 90% (Phase 61's D-079 target; events.go gains AdminScopeUsedPayload + its test).

## Dependencies

**Same-wave (Wave 13, Stage 1, Batch A):**

- Phase 72 (`events.subscribe` scope foundation — co-Batch-A, lands in same wave).

**Already shipped (pre-Wave 13):**

- Phase 06 (events bus + `audit.admin_scope_used` event type already registered — `Shipped`).
- Phase 54 (Protocol task-control surface — `Shipped`; supplies the ten canonical methods + `IdentityScope`).
- Phase 60 (Protocol wire transport — `Shipped`; supplies the HTTP mux + the `assertBodyMatchesAuthedIdentity` defence-in-depth gate).
- Phase 61 (Protocol auth — `Shipped`; supplies the JWT `Validator` + `auth.HasScope(ctx, auth.ScopeAdmin)` gate + `audit.rejected` event pattern).
- Phase 62 (Protocol conformance — `Shipped`; supplies the `RunSuite` matrix the impersonation scenarios fold into).

## Risks / open questions

- **Field shape: nested `IdentityScope` vs. flat `IdentityTriple` for `Actor` / `Requester` / `Impersonating`.** The phase plan settles on nested `*IdentityScope` so the wire shape composes naturally (the Brief's verbatim triplet is `(tenant, user)`-shaped, which is a subset of `IdentityScope`). The nested type intentionally re-uses `IdentityScope` rather than introducing a `(tenant, user)` flat-pair so a Console reading the wire never has to unpack a second shape. The audit payload's flat `IdentityTriple` is the deliberate boundary at the audit layer — that surface does NOT carry `Run` / `Scope`, which are not audit-load-bearing for the impersonation record. **Coordinator may flag this** if the operator prefers a flat triple on the wire too.
- **`Actor` vs `Requester` redundancy at V1.** The two fields carry the same value at V1 (`Requester == Actor`). The redundancy is deliberate forward-compat: the post-V1 delegated-impersonation shape ("admin A acting on behalf of admin B's audited request") needs the two to diverge, and the wire shape MUST NOT break when that lands. Brief 11 §PG-5 names both fields verbatim, so the wire reflects the brief.
- **Audit event payload home — `internal/protocol/auth/events.go` vs. `internal/events/`.** The `audit.admin_scope_used` event type is registered in `internal/events/events.go` but the existing emit site (the events Subscribe admin filter, `internal/events/drivers/inmem/inmem.go`) currently emits it with an untyped payload. 72b adds a TYPED payload (`AdminScopeUsedPayload`) for the impersonation emit; the existing emit site stays untyped at V1 (no behavior change). Promoting the existing emit to the typed payload is a follow-up deferred to a Wave 13 audit cleanup PR. **Coordinator may flag** if the operator prefers the typed payload land everywhere in this phase.
- **`Identity is mandatory` and impersonation.** The CLAUDE.md §6 rule 9 "no identity-downgrading knob" is the binding rule. 72b satisfies it by requiring the FULL `(tenant, user, session)` triple on `Impersonating` — there is no "admin without target" mode, no "anonymous impersonation," no environment-variable override. The Protocol fails closed on every malformed shape.
- **D-NEW-1 entry (operator pre-assignment).** This phase introduces a wire-shape extension that warrants a decisions.md entry: "Admin-impersonation triplet (Actor / Requester / Impersonating) lives on `internal/protocol/types/IdentityScope`; identity is mandatory; emission rides the existing `audit.admin_scope_used` event with a new typed payload; V1 invariant `Requester == Actor`." The D-NNN number is pre-assigned by the coordinator at dispatch time per the §17.7 §3 dispatch rule.

## Glossary additions

- **impersonation** — Admin-only Protocol feature: a `auth.ScopeAdmin`-bearing request supplies an `IdentityScope.Impersonating` triple so the run executes as that target identity while the audit trail records the admin's verified identity in `IdentityScope.Actor`. Audited via `audit.admin_scope_used` with a typed `AdminScopeUsedPayload`. Identity is mandatory — the impersonated triple MUST be a complete `(tenant, user, session)` triple; missing components fail loudly with `CodeIdentityRequired`. Brief 11 §PG-5, Phase 72b.
- **`actor` (impersonation)** — Field on `internal/protocol/types/IdentityScope` carrying the verified admin identity at the Protocol edge during an impersonation request. V1 invariant: equals the JWT's verified `(tenant, user, session)` triple. Phase 72b.
- **`requester` (impersonation)** — Field on `internal/protocol/types/IdentityScope` carrying the originating admin identity for delegated-impersonation chains. V1 invariant: equals `actor` (single-hop impersonation only); the field exists for post-V1 delegated impersonation without a wire-shape break. Phase 72b.
- **`impersonating` (impersonation)** — Field on `internal/protocol/types/IdentityScope` carrying the target identity the run executes under. Requires `auth.ScopeAdmin` on the verified JWT; rejects with `CodeScopeMismatch` otherwise. Phase 72b.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — **binding for this phase.** The impersonation triplet touches the isolation tuple; the integration test asserts no Actor/Requester/Impersonating field cross-contamination across N concurrent requests under `-race`.
- [ ] **Concurrent-reuse test for the new wire field**: marked N/A — 72b extends a wire type (`IdentityScope`), not a reusable artifact. The `ControlSurface` that consumes it is already covered by Phase 54's D-025 concurrent-reuse test; 72b adds a per-field cross-contamination sub-assertion to that existing stress, not a new artifact-level test. (Per CLAUDE.md §11 the N/A reason is explicit.)
- [ ] **Integration test exists** — `test/integration/identityscope_impersonation_test.go` wires real Phase 60 transport + real Phase 61 `Validator` + real `audit.Redactor` + real `events.Bus`, asserts identity propagation across the seam, covers ≥1 failure mode (non-admin → 403; missing identity component → 401/403; body-triple mismatch → 401/403), runs under `-race`. (§17, binding when Deps lists shipped phases.)
- [ ] Glossary updated with the four new entries (`impersonation`, `actor (impersonation)`, `requester (impersonation)`, `impersonating (impersonation)`)
- [ ] Decisions log entry filed under the coordinator-pre-assigned D-NNN number (per the §17.7 §3 dispatch rule)
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (None for this phase.)
- [ ] **Coordinator-verify pass complete** before the PR is opened for operator review (per `docs/plans/wave-13-decomposition.md` §12 lock-in item 3 — Wave 13 dispatches always require the coordinator-verify protocol)
