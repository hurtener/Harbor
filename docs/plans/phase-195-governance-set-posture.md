# Phase 195 — governance identity-tier policy write (`governance.set_posture`, HA-29)

## Summary

The write sibling of the read-only `governance.posture`: a new
`governance.set_posture` Protocol method that makes the identity-tier
policy table (per tier — budget-ceiling USD / max-tokens cap / rate-limit
capacity, plus the default-tier assignment) administrable over the wire.
The write is a **full replace through the projected shared validator**
(never a partial merge — a write that omits or zeroes an enforced ceiling
is rejected fail-closed), authority is derived server-side from the
verified session and gated on the `auth.ScopeAdmin` claim ONLY, and the
tier policy graduates from hot-reloadable boot config to a StateStore-backed
record layered over the config-declared defaults (in-mem / SQLite / Postgres
conformance, forward-only migration). It round-trips faithfully with the
read.

## RFC anchor

- RFC §6.15
- RFC §5.2
- RFC §7
- RFC §9

## Briefs informing this phase

- brief 06

## Brief findings incorporated

- brief 06 §"Event bus is the single source of truth for observability":
  every governance state transition must surface as a typed event on the
  bus, not just mutate silently. This phase's `set_posture` emits a
  `governance.posture_set` admin-write audit event (actor tenant, non-secret
  before/after tier summary, SafePayload) through the wired Redactor + Bus
  as ONE fail-closed unit with the StateStore mutation — mirroring the
  `governance.set_tenant_overrides` emit path (`internal/governance/
  tenantoverride.go::emitSet`), so an operator watching the bus sees the
  policy change the moment it lands and an un-auditable write is never left
  observably applied.
- brief 06 §"Redaction is mandatory at the boundary, never optional": the
  tier table carries no secrets, but the admin-write event still travels
  through `audit.Redactor` — the phase does NOT special-case
  "non-sensitive" payloads out of the redaction path (CLAUDE.md §7 rule 6:
  no event bypasses the redactor).
- brief 06 §"Console reads the Protocol, never the Runtime": the Console
  Governance page drives the change in as an authenticated admin and keeps
  ONLY Console-local state (saved views / annotations, D-061) — it never
  holds a projected copy or shadow of the tier values. The runtime stays
  the sole owner and enforcer; the page re-reads `governance.posture` after
  a successful `set_posture` rather than optimistically mirroring what it
  wrote.

## Findings I'm departing from (if any)

Governance has no dedicated research brief (per `docs/research/INDEX.md`
the LLM-client + governance-adjacent rows cite 03 / 06 / 08). I cite
**brief 06** (events + observability + Console-as-Protocol-client) because
this phase's load-bearing concerns are the admin-write event, the
redaction boundary, and the Console consumer posture — all squarely brief
06 territory. This is an honest departure from the "primary subsystem
brief" ideal only because no governance-primary brief exists; the design
itself departs from no brief finding. None of brief 06's findings is
contradicted.

## Goals

- Ship `governance.set_posture` — the pinned canonical method name (D-332),
  the write sibling of `governance.posture` — as an admin-scoped Protocol
  write.
- Write the whole identity-tier policy table (per tier: `BudgetCeilingUSD`
  / `MaxTokens` / `RateLimit{Capacity, RefillTokens, RefillInterval}`, plus
  `DefaultTier`) as a **FULL REPLACE** through the SAME shared validator the
  read projects (the D-302 full-replace pattern), never a partial merge.
- Fail closed on a write that omits or zeroes an enforced ceiling — never
  silently widen to unbounded, never a budget-widening default.
- Derive authority server-side from the verified session (D-219) and gate
  on `auth.ScopeAdmin` ONLY — explicitly NOT the two-scope
  (`admin` OR `console:fleet`) set that gates the read (D-066 / D-079).
- Graduate the tier policy from hot-reloadable boot config to a
  StateStore-backed policy record layered over the config-declared defaults
  (in-mem / SQLite / Postgres §9 conformance, forward-only migration); a
  runtime with no written override enforces exactly its config defaults.
- Round-trip faithfully with the read: what `set_posture` writes is what
  the next `governance.posture` returns.
- Add an admin-gated write affordance to the Console Governance page
  (D-121), driven through the typed Protocol client.

## Non-goals

- No consumer-side policy engine or re-enforcement — the runtime stays the
  sole authoritative enforcer (D-332 "Explicitly not part of this").
- No new identity axis; no change to how a tier is resolved for a caller
  (the runtime's `TierResolver` is untouched).
- No scope-gate relaxation on the READ (`governance.posture` keeps its
  two-scope `admin` OR `console:fleet` gate).
- No change to `governance.set_tenant_overrides` (per-tenant LLM defaults —
  model / temperature / max-tokens / reasoning-effort) — that write covers
  a DIFFERENT surface and stays exactly as shipped. `set_posture` writes
  the identity-tier POLICY TABLE, which `set_tenant_overrides` does not
  touch.
- No merge / patch semantics — there is no `governance.patch_posture`. One
  write shape, full-replace, per CLAUDE.md §13 "two parallel implementations
  of the same conceptual feature".
- No partial-object streaming, no bulk multi-tenant write in one call (the
  tier policy is a single runtime-level record, not per-tenant).

## Acceptance criteria

- [ ] `governance.set_posture` exists as a canonical method in
      `internal/protocol/methods/methods.go`, added to
      `canonicalGovernanceAdminMethods` so `IsGovernanceAdminMethod`
      returns true for it, with the wire-transport route
      `POST /v1/governance/set_posture`.
- [ ] The request wire type (`GovernanceSetPostureRequest`, in
      `internal/protocol/types/governance.go`) REUSES the existing posture
      shapes (`IdentityTierView` + `DefaultTier`) — it carries the full
      `IdentityTiers` map + `DefaultTier`, and it carries NO identity /
      scope field in the body (authority is server-side, D-219). The
      response (`GovernanceSetPostureResponse`) echoes the resolved,
      persisted posture so the caller sees exactly what the next read
      returns.
- [ ] The write validates and replaces the WHOLE table through the shared
      validator the read projects: a submitted tier missing (or zeroing) an
      enforced ceiling that the current effective policy enforces is
      **rejected fail-closed** with a typed error (`ErrPolicyWidening` /
      `ErrInvalidPosture`) — never silently persisted as unbounded, never a
      budget-widening default. An empty `IdentityTiers` map and a
      partial-tier write are both covered by this rejection path.
- [ ] Authority is derived server-side from the verified session (D-219),
      NEVER the request body, and gated on `auth.ScopeAdmin` ONLY: a caller
      bearing `console:fleet` (the read's second gate) but NOT `admin` is
      rejected with the canonical permission-denied Code (403), proving a
      leaked read-only fleet token cannot widen a budget (D-066 / D-079).
- [ ] The tier policy is StateStore-backed: a forward-only migration adds
      the policy record table; the write persists through the StateStore
      (in-mem / SQLite / Postgres), and a §9 conformance test asserts
      identical behavior across all three drivers — including the
      partial/empty-write fail-closed case.
- [ ] Layering: a runtime with NO written override enforces exactly its
      config-declared `IdentityTiers` defaults (the write is additive and
      backward-compatible); once written, the StateStore record is the
      effective policy and is what `governance.posture` reflects.
- [ ] Round-trip: a successful `set_posture` followed by `governance.posture`
      returns byte-faithful the tiers + `DefaultTier` that were written
      (what you set is what you read).
- [ ] The write emits a `governance.posture_set` admin-write audit event
      (SafePayload — actor tenant, non-secret tier-summary before/after, no
      raw secrets) through the wired Redactor + Bus as ONE fail-closed unit
      with the StateStore mutation (audit-emit failure COMPENSATES / rolls
      back the mutation — the `adminwrite.Apply` posture the sibling admin
      verbs reuse); an un-auditable write is never left observably applied.
- [ ] The Console Governance page gains an admin-gated write affordance
      (`docs/design/console/CONVENTIONS.md`, D-121): the edit form is
      visible/enabled only for a caller whose session carries `admin`,
      goes through the typed `HarborClient` (no hand-rolled `fetch`), and
      re-reads `governance.posture` after a successful write rather than
      mirroring its own submission. A "Console consistency" section cites
      CONVENTIONS.md.
- [ ] Full D-223 / D-209 lockstep for the new method + request/response
      wire types (`make protocol-ts-gen` regenerates the manifest;
      `make protocol-docs-gen` regenerates `methods.md` / `types.md`);
      `ProtocolVersion` stays `0.1.0` (additive method, no breaking change).
- [ ] `scripts/smoke/phase-195.sh` exercises a live `set_posture` round-trip
      and a fail-closed reject of a ceiling-zeroing write, plus the
      `console:fleet`-but-not-`admin` scope rejection.
- [ ] §18: `use-the-harbor-protocol` and the Console/governance-surfaced
      skill(s) (grepped per §18 — `surface: protocol` / `surface: console`)
      are updated in the same PR; the docs-site nav + generated protocol
      reference regenerate in the same PR.

## Files added or changed

```text
internal/protocol/methods/methods.go                 # MethodGovernanceSetPosture + admin-method set membership
internal/protocol/errors/errors.go                   # (if needed) ErrCodePolicyWidening / reuse invalid-argument
internal/protocol/types/governance.go                # GovernanceSetPostureRequest/Response (reuse IdentityTierView + DefaultTier)
internal/protocol/governance.go                       # wire handler: admin gate (ScopeAdmin only) + dispatch to the write policy
internal/governance/posture.go                        # PostureProvider gains a StateStore-backed effective-policy layer over Config defaults
internal/governance/setposture.go                     # SetPosturePolicy: full-replace validate + StateStore write + fail-closed event (new)
internal/governance/setposture_test.go                # unit: full-replace, fail-closed widening/zeroing/empty, round-trip
internal/governance/migrations/<driver>/NNNN_governance_posture.sql   # forward-only policy-record table (per §9 driver dirs)
internal/governance/conformance_inmem_test.go         # extend: set_posture conformance across drivers (or new conformance file)
internal/governance/events.go                          # GovernancePostureSetPayload (SafePayload)
internal/config/config.go                              # (doc only) IdentityTiers stays the config-default layer set_posture layers over
web/console/src/routes/(console)/.../governance/+page.svelte  # admin-gated write affordance
web/console/src/lib/protocol/governance.ts             # typed set_posture client + wire types
web/console/src/lib/protocol/wire-manifest.gen.json    # regenerated (make protocol-ts-gen)
docs/site/protocol/methods.md                          # regenerated (make protocol-docs-gen)
docs/site/protocol/types.md                            # regenerated
docs/skills/use-the-harbor-protocol/SKILL.md           # set_posture write + round-trip note
docs/skills/<console/governance skill>/SKILL.md         # admin-gated Governance write affordance (grep-identified)
docs/glossary.md                                        # 2 new terms
examples/*.yaml                                         # note: identity_tiers is the config-default layer set_posture layers over
scripts/smoke/phase-195.sh
```

## Public API surface

```go
// internal/protocol/types/governance.go — REUSES the read's shapes.

// GovernanceSetPostureRequest is the `governance.set_posture` request body.
// It carries the WHOLE identity-tier policy table as a full replace — never
// a partial merge. It carries NO identity/scope field: authority is derived
// server-side from the verified session (D-219), never the request body.
type GovernanceSetPostureRequest struct {
    // DefaultTier is the default-tier assignment applied to an identity
    // that resolves to no explicit tier. Required (empty is rejected when
    // any tier is enforced).
    DefaultTier string `json:"default_tier"`
    // IdentityTiers is the complete tier table keyed by tier name. A tier
    // present in the current effective policy but ABSENT here — or present
    // with a zeroed enforced ceiling — is a fail-closed rejection, not a
    // silent widening.
    IdentityTiers map[string]IdentityTierView `json:"identity_tiers"`
}

// GovernanceSetPostureResponse echoes the persisted, resolved posture — it
// is byte-faithful to what the next `governance.posture` read returns.
type GovernanceSetPostureResponse struct {
    DefaultTier   string                     `json:"default_tier"`
    IdentityTiers map[string]IdentityTierView `json:"identity_tiers"`
}
```

```go
// internal/governance/setposture.go

// SetPosturePolicy owns the StateStore-backed effective identity-tier
// policy record layered over the config-declared defaults. Immutable after
// construction (the StateStore is the mutable seam); safe to share across N
// goroutines.
type SetPosturePolicy struct { /* state.StateStore, events.EventBus, config defaults, validator, clock */ }

// Set validates the submitted table as a FULL REPLACE through the shared
// validator, rejects a widening/zeroing/empty write fail-closed
// (ErrPolicyWidening / ErrInvalidPosture), persists the record through the
// StateStore, and emits governance.posture_set as ONE fail-closed unit with
// the mutation. Returns the resolved persisted posture.
func (p *SetPosturePolicy) Set(ctx context.Context, actor identity.Quadruple, req SetPostureSpec) (Snapshot, error)

// Sentinels callers compare with errors.Is.
var (
    ErrPolicyWidening = errors.New("governance: set_posture omits or zeroes an enforced ceiling (fail-closed; never budget-widening)")
    ErrInvalidPosture = errors.New("governance: set_posture policy failed validation")
)
```

## Test plan

- **Unit:**
  - `setposture_test.go`: a full valid table replaces cleanly and
    round-trips; a table OMITTING a currently-enforced tier is rejected
    `ErrPolicyWidening`; a tier present with a ZEROED `BudgetCeilingUSD` /
    `MaxTokens` / `RateLimit.Capacity` (where the current effective policy
    enforces it) is rejected; an EMPTY `IdentityTiers` map is rejected when
    any ceiling is currently enforced; a runtime with no written override
    reads back exactly its config defaults.
  - `posture_test.go` (extended): `governance.posture` reflects the written
    record when present and the config defaults when absent (the layering).
  - wire handler test: `set_posture` with a session carrying `console:fleet`
    but NOT `admin` → permission-denied; with `admin` → accepted; authority
    is read from the verified session, a body-supplied scope field (there is
    none) cannot elevate.
  - `internal/protocol/types` reflective test: `GovernanceSetPostureRequest`
    carries no identity/scope field (authority is server-side).
- **Integration:**
  - `test/integration/governance_setposture_test.go` (or in-package
    handler adapter test): real StateStore driver + real Redactor + real Bus
    on the seam; a `set_posture` write emits `governance.posture_set` on the
    bus AND a following `governance.posture` returns the written table;
    identity propagation asserted (the actor's verified triple is on the
    event); ≥1 failure mode — a forced Redactor error COMPENSATES the write
    (the StateStore record is rolled back, the next read shows the prior
    policy). Runs under `-race`.
- **Conformance:**
  - Extend the governance conformance suite so all three StateStore drivers
    (in-mem / SQLite / Postgres) pass the SAME `set_posture` behavior —
    including the partial/empty-write fail-closed case (D-332's explicit
    §9 conformance requirement) and the migration applying cleanly on a
    fresh DB and on an existing DB.
- **Concurrency / leak:**
  - `TestConcurrentReuse_SetPosturePolicy_NoBleed`: N≥100 concurrent `Set` +
    read invocations against a single shared `SetPosturePolicy` instance
    under `-race` — no data race on the StateStore seam, no torn read
    between a write and a concurrent `governance.posture`, no goroutine leak
    after teardown. (The record is a single runtime-level policy, so the
    test asserts last-writer-wins linearizability, not cross-identity
    isolation.)

## Smoke script additions

- `scripts/smoke/phase-195.sh` (`PREFLIGHT_REQUIRES: live-server`):
  - Static trip-wire: `governance.set_posture` present in
    `wire-manifest.gen.json` and `docs/site/protocol/methods.md` (SKIP on a
    pre-195 build).
  - Probe `POST /v1/governance/set_posture` — 404/405/501/000 → SKIP
    gracefully (the sacred convention).
  - With `HARBOR_DEV_TOKEN` (admin dev token): a valid full-table
    `set_posture` returns 200, then `governance.posture` returns the written
    tiers + `default_tier` (round-trip OK).
  - Fail-closed: a `set_posture` that ZEROES an enforced `budget_ceiling_usd`
    (or omits an enforced tier) returns 400/422 and the following
    `governance.posture` still shows the PRIOR enforced ceiling (never
    widened).
  - Scope gate: a request bearing a `console:fleet`-only token (no `admin`)
    returns 403 — the leaked-read-token-cannot-widen-a-budget invariant.

## Coverage target

- `internal/governance` (touched — `setposture.go`, `posture.go`): 85%
- `internal/protocol/types` (touched): 85%
- `internal/protocol` (governance handler touched): 85%

## Dependencies

- Gate-0 (the v1.17 RFC amendment: RFC §6.15 + D-332 — already shipped on
  the branch base). No sibling-phase dependency; 195 parallelizes with 196
  in Stage 2.

## Risks / open questions

- **Design choice the decision left for the phase to confirm — the exact
  fail-closed comparison basis.** D-332 says a write that "omits or zeroes
  an ENFORCED ceiling" is rejected, but the reference point for "enforced"
  admits two readings: (a) enforced by the CURRENT effective policy (the
  StateStore record if present, else config defaults), or (b) enforced by
  the CONFIG DEFAULTS as an immovable floor. This plan takes reading (a):
  the current effective policy is the comparison basis, so an operator can
  legitimately lower a ceiling from the config default but cannot drop
  enforcement of a tier that is currently enforced. **Flag for the
  coordinator to confirm** — reading (b) would make config the permanent
  floor and forbid loosening below it, which is a stricter posture D-332
  does not explicitly demand. This plan documents (a) and asks for sign-off;
  either is a one-line change to the validator's basis argument.
- **"Zeroing" semantics vs a legitimately unlimited tier.** A tier that is
  DELIBERATELY unlimited (no ceiling) is a real config today (zero = latent
  "no enforcement" per `governance.TierConfig`). The fail-closed rule
  therefore cannot be "any zero is a rejection" — it is "a zero that DROPS
  enforcement the current effective policy HAS is a rejection." The
  validator distinguishes a tier that was never enforced (zero stays a valid
  "no enforcement") from a tier being silently de-enforced (rejection). This
  is the load-bearing subtlety the conformance test pins.
- **Migration ordering across the three drivers.** The forward-only policy
  record migration must be idempotent (`INSERT OR IGNORE INTO
  schema_migrations` per §9) and land in each driver's migration dir; the
  conformance test asserts a fresh DB and an existing DB both converge.

## Glossary additions

- **`governance.set_posture`**
- **identity-tier policy record (StateStore-backed)**

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: N/A — the tier policy is a single
      runtime-level record, not identity-scoped data; the admin gate + the
      concurrent-reuse linearizability test cover it. (The read's cross-tenant
      posture is unchanged.)
- [ ] This phase builds a reusable artifact (`SetPosturePolicy`):
      concurrent-reuse test passes — N≥100 concurrent invocations against a
      single shared instance under `-race`.
- [ ] This phase consumes a shipped subsystem's surface (StateStore, Bus,
      Redactor): integration test exists, wires real drivers end-to-end,
      asserts identity propagation on the event, covers ≥1 failure mode
      (Redactor-error compensation), runs under `-race`.
- [ ] Glossary updated (2 terms above)
- [ ] N/A — no brief finding departed from (the brief-selection note above
      is a sourcing explanation, not a design departure)
