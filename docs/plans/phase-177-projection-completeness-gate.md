# Phase 177 — projection-completeness-gate

## Summary

HA-22 (Phase 174) fixed one instance of a defect *class*: a read surface
declares a typed, wire-visible field, ships a facet / sort / aggregate over it,
and never populates it — so the operation returns **false absence** (an empty
page / a fabricated zero) on a fleet full of matching data. This phase closes
the same class on **three more populate-or-remove surfaces** (tasks, flows,
memory), makes the one already-wired-but-stub enricher (the tasks `serve.Enricher`)
tell the truth, honestly gates the fourth heavy surface (tools) until Phase 178
wires its backend, and — the centrepiece — ships a **registry-gated
projection-completeness gate** with two coverage halves: one FAILS THE BUILD when
a wire field that is filtered / sorted / aggregated over is never ASSIGNED by its
projector (the **never-assigned** variant), and a second FAILS THE BUILD when a
registered surface has no prod-WIRING test exercising its
production-assembled projector through real `mux` wiring (the **never-wired**
variant — a `WithX` enricher the production constructor forgets to install). The
gate is the class-closer: after this phase, BOTH variants of the class D-311
names are mechanically caught (the never-assigned variant by reflection over the
probe, the never-wired variant by the mandatory prod-wiring test). The gate is
the primitive; the four surface fixes plus 174's sessions fix are its consumers
(§13), so it lands green.

## RFC anchor

- RFC §5.2
- RFC §6.1
- RFC §6.4
- RFC §6.6
- RFC §6.8
- RFC §7

## Briefs informing this phase

- brief 03
- brief 04
- brief 05
- brief 06

## Brief findings incorporated

- brief 06 §"Console — observability + control plane UI" (the Playground
  anti-pattern / runtime-lens principle, RFC §7): a Console affordance must
  reflect real runtime data, never a plausible-but-fabricated projection. A
  facet chip or a Budget-meter gauge rendered over a permanently-zero field is
  exactly the lens lying about the runtime — this phase makes every such
  affordance operate on real data OR removes it OR gates it, never leaves it
  silently false.
- brief 05 §"Tasks (unified foreground/background)" (RFC §6.8): the task
  registry record is a lifecycle record — it does not itself model "has an open
  approval gate." That derived signal is owned by the pause/approval registry
  one package over; this phase reads it at projection time (the read-time seam
  the tasks `Projector` already precedents on `tasks.get`), never as a new
  column on the task record.
- brief 04 §"Memory subsystem" (RFC §6.6): V1 memory is session-scoped and has
  no TTL — the record's `ExpiresAt` "zero means no TTL" and no producer ever
  sets it. A facet / aggregate over a field the subsystem structurally can never
  populate is dead surface; this phase removes it rather than shipping a
  never-matching filter.
- brief 03 §"Tool catalog + transports" (RFC §6.4): the tool catalog projects a
  transport-agnostic tool identity; per-tool OAuth / approval / metrics /
  content-stats are annotations layered by a backend the catalog reads through
  an optional seam. When that backend is unwired, the annotations are conservative
  defaults — which is honest ONLY if no facet / search / aggregate operates over
  them as if they were known values (the class rule, D-311).

## Findings I'm departing from (if any)

None. This phase extends D-311 (the silent-absence class rule authored by Phase
174) consistently to four more surfaces and hardens it into a mechanical gate.
It does not overturn any brief finding or prior decision. It coordinates with
Phase 174 on the cost-rollup work (the tasks `serve.Enricher` cost stub overlaps
174's session-cost aggregation) — see Dependencies and Risks.

## Goals

- **Ship the projection-completeness gate (the primitive) — two coverage
  halves.** A build-time, registry-gated check with two independent halves,
  because the class has two variants (D-311/D-313) and a single reflection probe
  closes only one:
  - **Half A — never-assigned (reflection).** For every projection surface,
    cross-reference the set of wire fields the filter / sort / aggregate layer
    OPERATES over against the set the production projector actually ASSIGNS (via
    a probe that runs the projector over a fully-populated source record). A
    filtered / sorted / aggregated field the projector leaves at its zero value
    (and not on an explicit, reason-carrying honest-omission allow-list) FAILS
    THE BUILD.
  - **Half B — never-wired (prod-wiring test, closes the variant that motivated
    this band).** Half A cannot catch the never-wired variant: for a
    read-time-enriched field (tasks `HasPendingApproval`, every tools annotator
    field, the enricher cards), the probe wires its own populated fake and passes
    while production `mux.go` can OMIT the `WithX` wiring and ship false absence —
    the exact tools bug. So each `ProjectionContract` MUST ALSO register a
    prod-WIRING test that exercises the surface's projector as assembled through
    real `mux` wiring (not a test-supplied backend) and asserts the enriched
    fields are populated; a registered surface with no prod-wiring test FAILS THE
    BUILD. A forgotten `WithX` in `mux.go` then fails the gate.
  - **Coverage of surfaces.** Every projection surface must be registered; a
    surface that ships without registering cannot dodge either half (mirroring
    the events `RegisteredDrivers()` cross-check, D-305).
- **TASKS — close the sharp false-absence facet.** Populate
  `TaskRow.HasPendingApproval` at projection time from the approval/pause
  registry scoped to the task's run, so `filter.has_pending_approval=true`
  narrows to real rows instead of returning an empty page on a fleet with open
  gates. Make `TaskRow.BackgroundAcknowledged` honest — populate it from the
  `task.background_acknowledged` latch, or represent it as additive/omitempty
  (it is fabricated-false today but no filter reads it, so the bar is
  representable honesty, not a facet fix).
- **FLOWS — close the fabricated-zero aggregate.** `budgetConsumption` sums
  `RequestsUsed` + `CostUSDUsed` but never `TokensUsed` (a non-`omitempty`
  field), so every `FlowSummary` / `FlowDetail` claims "0 tokens used." Add a
  `Tokens` field to `flow.RunRecord` (symmetric with the existing `CostUSD`),
  sum it into `FlowBudgetConsumption.TokensUsed`, and set it at every `RecordRun`
  call site. The Budget meter now renders real token consumption.
- **MEMORY — remove the structurally-dead facet + aggregate; loud-reject the
  agent facet.** V1 memory has no TTL (the producer never sets `ExpiresAt`, the
  type documents "zero = no TTL" — an RFC §6.6 *design* absence), so
  `filter.has_ttl_expiring` always returns an empty page and the `expiring_in_1h`
  aggregate is always 0. REMOVE both (option b — a facet that can never match is
  dead surface). Note this touches `ExpiringIn1h` in TWO type sites —
  `MemoryAggregates` (`internal/protocol/types/memory.go:217`) AND
  `MemoryHealthAggregate` (`:346`), neither `omitempty` — plus the filter branch
  and the two computers (list + health). It is a breaking wire-shape change; the
  §8 deprecation-window exemption is claimed explicitly (Non-goals + Risks): the
  fields were ALWAYS empty / always 0, so no live consumer can depend on a
  meaningful value, and the pinned pre-GA `0.1.0` within-version-removal
  precedent (Phase 171 dropped a fork) applies. For `AgentID`: the V1 mechanism
  is **loud-reject** — `ConversationTurn` (`internal/protocol/types/memory.go`
  turn record) carries NO producer / agent identity and the projection
  (`internal/memory/protocol/protocol.go`) sets only the tenant/user/session
  triple, so there is structurally NO agent to populate from in V1;
  `filter.agent_ids` returns `CodeInvalidRequest` (representable absence + loud
  rejection over an unpopulated field, D-311) rather than a silent empty page.
  Populate is DEFERRED to a follow-up that adds a producer identity to
  `ConversationTurn` — the facet keeps its wire slot precisely because agent
  identity is a not-yet-wired field (unlike TTL, which is a design absence and is
  removed). The `MemoryItem.AgentID` / `.ExpiresAt` ROW fields stay `omitempty`
  and honest-by-omission — the harm was only in the filters / aggregate over
  them.
- **TASKS enricher — un-stub the wired-but-zero concrete.** The tasks `tasks.get`
  Enricher IS wired in production (`mux.go` `WithEnricher(NewEnricher)`), but its
  concrete `serve.Enricher` returns a zero `TaskParentSessionRef{}` (AgentName /
  Status / StartedAt / LatestEventAt permanently empty; SessionID is populated by
  the projector baseline) and a zero `TaskCostRollup`. Populate the parent-session
  card fields (from the session registry) and the cost rollup (from the cost-event
  stream, coordinated with 174). This is display truthfulness, not a gate-triggering
  facet — but it is the sharpest illustration of the class (even the surface Harbor
  got STRUCTURALLY right ships zeros because the concrete is a stub).
- **TOOLS — honest interim gating (the 178 hand-off), one clean capability
  toggle.** No production `Annotator` exists (the only implementer is the
  `fakeAnnotator` test double, §17.8), so in production `OAuthStatus` /
  `ApprovalPolicy` / `LastUsedAt` / `Metrics` / `ContentStats` / `DisplayModes`
  are all structural defaults while `filter.oauth_statuses`,
  `filter.approval_policies`, the `Name+" "+Version` search axis, and the
  `ToolAggregates` (Active / PendingApproval / AwaitingOAuth) all operate over
  them. Gate them behind a SINGLE "annotator-wired" capability toggle (so 178's
  un-gate is one mechanical flip, not scattered edits), with the interim
  behaviour differentiated by surface shape:
  - **Dedicated facet filters** (`filter.oauth_statuses`,
    `filter.approval_policies`, the version search axis) → **loud-reject**
    (`CodeInvalidRequest`) when unwired. There is no row to return for a facet
    that cannot be honoured.
  - **Response-riding aggregates** (Active / PendingApproval / AwaitingOAuth on a
    `tools.list` response, where the tool rows MUST still return) → an explicit
    **`aggregates_partial` / unavailable marker**, NOT loud-reject and NOT a
    silent 0. The Console renders "unavailable," never a real-looking zero. This
    is aligned with 174's chosen `SessionRow.CountersPartial` mechanism — the
    fabricated-zero this class exists to kill must not sneak back in as a lazy
    "honest-partial" 0.

  Phase 178 wires the real annotator, flips the one capability toggle, and both
  behaviours become truthful. This keeps the gate green for tools without waiting
  on 178.
- **SESSIONS — register the 174 surface into the new gate.** Phase 174 predates
  this phase and cannot register into a `projectioncheck` package that does not
  yet exist, so this phase adds the sessions registration itself (a
  `ProjectionContract` whose probe runs the production `projectRow` + `Enricher`
  from `internal/sessions/protocol/`, plus the Half-B prod-wiring test). Without
  this, the coverage half fails on its own most-important surface. This work
  edits `internal/sessions/protocol/lister_projector.go` (which 174 also
  touches), so it is explicitly serialized AFTER 174 (see Dependencies).
- **Pin every projection fixture against its real producer (§17.8).** Extend the
  §17.8 discipline (a fixture must never be richer than the runtime) to all five
  surfaces via the gate's probe, so a fixture that assigns a field the production
  projector cannot produce fails the build.

## Non-goals

- **No tools production `Annotator`.** Assembling the real OAuth / approval /
  metrics / content-stats / display-modes backend (and lighting up the inert
  admin write path) is Phase 178 (D-314). This phase only gates the tools facets
  honestly in the interim.
- **No new Protocol method, no `ProtocolVersion` bump.** The tasks / flows fixes
  populate already-declared fields (no wire-shape change). The memory removal
  (`has_ttl_expiring` + the two `expiring_in_1h` fields on `MemoryAggregates` AND
  `MemoryHealthAggregate`) IS a breaking wire-shape change — full D-223/D-209
  lockstep in the same PR. It is a removal within the pinned version, not a
  version bump, and the RFC §8 deprecation-window requirement is met by explicit
  exemption, NOT by silence (the §8 defect is an unannounced break, not a break):
  the removed fields were ALWAYS empty / always 0 (a facet/aggregate over a field
  V1 can never populate), so no live consumer can depend on a meaningful value,
  and the pre-GA `0.1.0` within-version-removal precedent (Phase 171) applies.
  The `BackgroundAcknowledged` representation, the "annotator-wired" capability
  string, and the tools `aggregates_partial` marker are additive.
- **No first-class session→agent or memory→agent binding link.** Where an agent
  identity is not modelled (memory records with no producer identity), the phase
  makes the absence honest and loud, never invents a binding.
- **No new aggregation store / shadow columns.** Every populated counter is a
  read-time projection from the subsystem that owns the raw data, exactly like
  the tasks / sessions enricher precedent.

## Acceptance criteria

- [ ] **Gate Half A — never-assigned (reflection) bites.** A projection-completeness
      gate (`internal/protocol/projectioncheck`, or the natural home) exposes a
      registry that each projection surface self-registers into with (a) a probe
      that builds a fully-populated source record and runs the production
      projector, (b) the set of wire field json-tags its filter / sort /
      aggregate layer reads, and (c) an honest-omission allow-list (field →
      one-line reason). A conformance test ranges the registry, reflects each
      probe's output, and FAILS when a filtered / sorted / aggregated field is
      left at its zero value and not allow-listed. A **synthetic** projector that
      declares a filter over a deliberately-unassigned field makes the gate test
      FAIL (proving the gate is not vacuous).
- [ ] **Gate Half B — never-wired (prod-wiring) bites.** Each `ProjectionContract`
      MUST register a prod-WIRING test that exercises its projector as assembled
      through real `mux` wiring (not a test-supplied backend), asserting the
      read-time-enriched fields (e.g. tasks `HasPendingApproval`, the enricher
      cards) are populated; a registered surface with NO prod-wiring test FAILS
      the build. A **synthetic** surface whose production assembly OMITS its
      `WithX` enricher (so prod ships zeros while the reflection probe passes with
      its own fake) makes the Half-B test FAIL — the tools never-wired bug is
      caught by construction.
- [ ] **Coverage of surfaces.** Every projection surface known to the runtime
      (sessions, tasks, tools, flows, memory) is registered; the gate asserts the
      registered set covers the known set (a new surface that ships without
      registering fails the build), mirroring `events.RegisteredDrivers()` /
      conformance parity (D-305).
- [ ] **SESSIONS registered by this phase.** Because 174 predates
      `projectioncheck`, this phase adds the sessions `ProjectionContract`
      (`internal/sessions/protocol/`) — a probe over the production `projectRow` +
      `Enricher` AND a Half-B prod-wiring test asserting the production `mux`
      installs the sessions `Enricher` — so the coverage half passes on the
      sessions surface. This edit touches `lister_projector.go` (also touched by
      174) and is serialized AFTER 174 lands.
- [ ] **The honest-omission allow-list is real, reasoned, and minimal.** Only
      fields that are genuinely operated-over AND legitimately left zero appear:
      the tools annotator-backed fields (gated behind the annotator-wired
      capability, pending 178) with a "178: annotator wiring pending" reason. A
      field that is populated (`TaskParentSessionRef.SessionID`) or NOT in a
      surface's `OperatedFields` (`FlowBudget.TokenCap`, the `MemoryItem.AgentID` /
      `.ExpiresAt` ROW fields) needs NO allow-list entry and MUST NOT carry one —
      every allow-list line is a real intentional absence, not bloat.
- [ ] **The gate fails on an empty reason (anti-theater).** A test asserts the
      gate test itself FAILS when a `ProjectionContract` carries an
      honest-omission entry with an empty-string reason — the reason is
      load-bearing (an unexplained allow-list entry is how a real false-absence
      bug gets silenced), so an empty reason is a gate failure, matching the
      glossary claim.
- [ ] **TASKS `has_pending_approval` truthful.** With a task whose run holds an
      open approval gate, `tasks.list` returns a row with
      `has_pending_approval=true`, and `filter.has_pending_approval=true` narrows
      to exactly those rows (proven against real drivers end-to-end, §17.1). A
      task with no open gate reports `false` and is excluded by the facet.
- [ ] **TASKS `background_acknowledged` honest.** The field is either populated
      from the `task.background_acknowledged` latch or represented as omitempty;
      a test asserts it is never a fabricated `false` presented as a known value,
      and (per the gate) no facet operates over it.
- [ ] **FLOWS `tokens_used` truthful.** `flow.RunRecord` gains a `Tokens` field;
      `budgetConsumption` sums it into `FlowBudgetConsumption.TokensUsed`; a flow
      with recorded runs returns a `FlowSummary` / `FlowDetail` whose
      `tokens_used` reflects the summed per-run tokens (not 0). `TokenCap` stays
      omitempty and is NOT touched (honest exclusion).
- [ ] **MEMORY dead facet + aggregate removed (with §8 exemption stated).**
      `filter.has_ttl_expiring` and BOTH `expiring_in_1h` fields —
      `MemoryAggregates` (`memory.go:217`) AND `MemoryHealthAggregate` (`:346`) —
      are removed from the wire, along with the filter branch and the two
      computers (list + health); the removal is mirrored under D-223 lockstep and
      the D-209 generated type/method reference is regenerated in the same PR. The
      PR body and D-313 state the RFC §8 deprecation-window exemption explicitly
      (always-empty field → no live consumer + the `0.1.0` within-version-removal
      precedent, Phase 171). A test asserts the surface no longer advertises a
      facet the runtime can never satisfy.
- [ ] **MEMORY `agent_ids` loud-rejected (V1 mechanism).** `ConversationTurn`
      carries no producer identity and the projection sets only the
      tenant/user/session triple, so there is no agent to populate from in V1;
      `filter.agent_ids` returns `CodeInvalidRequest` (loud), never a silent empty
      page. Populate is DEFERRED to a follow-up that adds producer identity to
      `ConversationTurn`; the facet keeps its wire slot (a not-yet-wired field,
      unlike the removed TTL design absence). The mechanism + deferral are
      recorded in D-313.
- [ ] **TASKS enricher un-stubbed.** `serve.Enricher.ParentSession` returns a
      populated card (AgentName / Status from the session registry; StartedAt /
      LatestEventAt copied through), and `serve.Enricher.Cost` returns a real cost
      rollup (from the cost-event stream, coordinated with 174). A `tasks.get`
      over a task with a known parent session and recorded cost returns non-zero
      card fields and a non-zero rollup.
- [ ] **TOOLS honestly gated behind one capability toggle.** With no annotator
      wired (production default), a SINGLE "annotator-wired" capability toggle
      gates the interim behaviour so 178's un-gate is one mechanical flip: the
      **dedicated facet filters** (`filter.oauth_statuses`,
      `filter.approval_policies`, the version search axis) **loud-reject**
      (`CodeInvalidRequest`); the **response-riding aggregates**
      (`Active` / `PendingApproval` / `AwaitingOAuth` on a `tools.list` response,
      whose tool rows must still return) carry an explicit **`aggregates_partial`
      / unavailable marker** (Console renders "unavailable"), NEVER a silent 0.
      Tests assert an unwired build (a) loud-rejects the facets and (b) never
      ships a real-looking zero aggregate — the marker is set. The gate's tools
      registration allow-lists these fields with a "178: annotator wiring pending"
      reason.
- [ ] **Concurrent reuse.** The gate's probes are pure; every touched projector /
      enricher stays immutable-after-construction with per-call state in
      args/locals — a concurrent-reuse test (N≥100 invocations against one shared
      instance, `-race`) passes for the tasks projector and the un-stubbed
      `serve.Enricher` (§5, D-025).
- [ ] **Cross-session isolation.** The populated `has_pending_approval` /
      enricher reads are identity-scoped — session A's open gate / cost never
      bleeds into session B's row; an isolation test asserts it.
- [ ] **Console consistency (§4.5 / D-121).** The Console Tasks page's
      `Has pending approval` facet + right-rail approvals tab now operate on real
      data; the Tasks-page Parent-Session + Cost cards render real values; the
      Flows-page Budget meter renders real tokens; the Memory page loses the dead
      TTL facet chip. All on the shared `HarborClient`, tokens only, Svelte 5
      runes; wire changes ride the D-223 lockstep in the same PR. See the Console
      consistency section below.
- [ ] `scripts/smoke/phase-177.sh` asserts (live-server) a populated
      `has_pending_approval` / `tokens_used` reaches the wire once activity exists
      (else SKIPs), that `memory.list` no longer accepts `has_ttl_expiring`, and
      (static) that the projection-completeness gate test is present and wired.

## Files added or changed

- `internal/protocol/projectioncheck/projectioncheck.go` *(new)* — the
  projection-completeness registry: `Register(ProjectionContract)`, the
  `ProjectionContract` shape (surface name, probe func, filtered/sorted/aggregated
  field set, honest-omission allow-list, prod-wiring-test presence flag), and the
  cross-check helper. Self-registration from each surface's init(), mirroring the
  §4.4 driver-registry pattern.
- `internal/protocol/projectioncheck/projectioncheck_test.go` *(new)* — the
  build-gating conformance test: Half A ranges the registry, reflects each probe,
  fails on a filtered-but-unassigned field; Half B asserts every registered
  surface declares a prod-wiring test; asserts the surface-coverage half; asserts
  an empty honest-omission reason fails; synthetic violating surfaces (an
  unassigned filtered field; a surface whose prod assembly omits `WithX`) fail the
  gate.
- `internal/sessions/protocol/lister_projector.go` — register the sessions
  surface into projectioncheck (probe over the production `projectRow` +
  `Enricher`); serialized AFTER 174 (which also edits this file). Includes the
  Half-B prod-wiring test in the sessions package's `_test.go`.
- `internal/tasks/protocol/registry_projector.go` — populate
  `HasPendingApproval` at projection time (via an approval-registry accessor
  threaded into the projector, list-side); populate/represent
  `BackgroundAcknowledged`; register the tasks surface into projectioncheck.
- `internal/tasks/protocol/list.go` — unchanged filter logic (now backed by a
  real `HasPendingApproval`); covered by new truthful-data tests.
- `internal/runtime/serve/enricher.go` — un-stub `ParentSession` (session-registry
  read) + `Cost` (cost-event aggregation, coordinated with 174).
- `internal/runtime/flow/registry.go` — add `RunRecord.Tokens int64`.
- `internal/runtime/flow/protocol/catalog.go` — `budgetConsumption` sums
  `c.TokensUsed += rec.Tokens`; register the flows surface into projectioncheck.
- `cmd/harbor/devseed.go` — set `Tokens` on seeded `RunRecord`s (symmetric with
  `CostUSD`).
- `internal/memory/protocol/list.go` — remove the `has_ttl_expiring` filter
  branch and the `expiring_in_1h` aggregate; `agent_ids` populate-or-loud-reject;
  register the memory surface.
- `internal/memory/protocol/health.go` — remove the `ExpiringIn1h` health counter.
- `internal/memory/protocol/protocol.go` — register the memory surface; no
  `AgentID` populate in V1 (the turn record carries no producer identity — see
  `agent_ids` loud-reject).
- `internal/protocol/types/memory.go` — remove `MemoryFilter.HasTTLExpiring` +
  BOTH `expiring_in_1h` fields (`MemoryAggregates.ExpiringIn1h` `:217` AND
  `MemoryHealthAggregate.ExpiringIn1h` `:346`) (breaking wire-shape change,
  D-223/D-209 lockstep, §8 dead-facet exemption stated in the PR body).
- `internal/protocol/types/tools.go` — the "annotator-wired" capability wiring
  (via the capabilities single source; ONE toggle so 178's un-gate is mechanical),
  plus the additive `aggregates_partial` marker on the tools-list response; no
  field removed.
- `internal/tools/protocol/filter.go` / `catalog_projector.go` — gate the
  annotator-backed surface behind the ONE annotator-wired capability toggle:
  facet filters loud-reject when unwired; the response-riding aggregates set the
  `aggregates_partial` marker (never a silent 0); register the tools surface with
  the gated fields on the allow-list.
- `docs/site/protocol/types.md` / `docs/site/protocol/methods.md` — D-209
  regenerated (`make protocol-docs-gen`) because the memory wire shape changes.
- `web/console/src/lib/protocol/*` + `wire-manifest.gen.json` — D-223 lockstep for
  the memory removal + the capability string; the Memory / Tasks / Flows routes
  render truthfully (drop the dead TTL chip; approvals/cost/tokens now real).
- `internal/*/*_test.go` — truthful-facet, un-stub, removed-facet,
  concurrent-reuse, isolation tests; de-enriched fixtures pinned by the gate.
- `test/integration/projection_completeness_test.go` *(new)* — real drivers
  end-to-end (§17.1), incl. the Half-B prod-wiring assertions (a
  production-assembled projector with a missing `WithX` fails).
- `docs/skills/observe-with-the-console/SKILL.md` +
  `docs/skills/use-the-harbor-protocol/SKILL.md` — §18 same-PR skill update.
- `scripts/smoke/phase-177.sh` *(new)*.

## Public API surface

```go
// internal/protocol/projectioncheck

// ProjectionContract is what one projection surface registers so the
// completeness gate can prove it never filters/sorts/aggregates over a
// field its projector leaves unassigned (Half A) AND that its production
// assembly actually wires the enricher the probe assumes (Half B).
type ProjectionContract struct {
    // Surface is the projection surface name (e.g. "sessions", "tasks").
    Surface string
    // Probe builds a fully-populated source record, runs the PRODUCTION
    // projector, and returns the projected wire row as an `any` the gate
    // reflects over (Half A — never-assigned). Pure — no shared state.
    Probe func() any
    // OperatedFields is the set of wire-row json tags the surface's
    // filter / sort / aggregate layer reads. ONLY tags that are both
    // operated-over AND legitimately zero belong in HonestOmissions —
    // a populated or non-operated field needs no omission entry.
    OperatedFields []string
    // HonestOmissions maps a json tag the projector legitimately leaves
    // zero to the one-line reason it is honest (a capability-gated facet
    // pending a later phase). Every entry MUST carry a NON-EMPTY reason —
    // the gate FAILS on an empty-string reason (anti-theater).
    HonestOmissions map[string]string
    // ProdWiringTest names the test that exercises this surface's
    // projector as assembled through real `mux` wiring (Half B —
    // never-wired). A contract with an empty ProdWiringTest FAILS the
    // gate: a read-time-enriched surface whose probe uses a fake but whose
    // mux.go omits the WithX enricher would otherwise pass Half A while
    // shipping false absence in prod (the tools bug).
    ProdWiringTest string
}

// Register installs a surface's contract. Surfaces self-register from
// init(); the gate test blank-imports the surfaces + the prod aggregator
// so the registered set reflects the REAL shipped surfaces (§4.4 / D-305).
func Register(c ProjectionContract)

// RegisteredSurfaces returns the sorted registered surface names — the
// gate cross-checks this against the known surface set (surface coverage).
func RegisteredSurfaces() []string

// internal/runtime/flow — RunRecord gains:
//   Tokens int64 // the run's recorded token consumption (symmetric with CostUSD).
```

## Console consistency (§4.5 / D-121)

Every affordance this phase touches already ships on a Console page — this phase
makes them TRUTHFUL, it does not add page composition:

- **Tasks page** — the `Has pending approval` facet and the per-job right-rail
  Pending-approvals tab (`web/console/src/routes/(console)/tasks/`) today filter
  on a permanently-false field; they now narrow real rows. The Tasks-page
  right-rail Parent-Session card + Cost-Breakdown card (fed by `tasks.get`) now
  render real agent/status/timestamps + real cost instead of blanks/zeros.
- **Flows page** — the Budget meter's tokens bar
  (`FlowBudgetConsumption.TokensUsed`) now renders real consumption instead of a
  flat 0.
- **Memory page** — the dead `has_ttl_expiring` facet chip is removed (V1 has no
  TTL); no other change.
- **Tools page** — the OAuth / approval facet chips are disabled (loud-reject on
  submit) until Phase 178 lands the backend; the catalog overview aggregates
  render "unavailable" off the `aggregates_partial` marker rather than a
  silently-empty / real-looking-zero result. (178 flips the one capability toggle
  and both go live.)

All work stays on the shared `HarborClient` + `connection.ts`, tokens only,
Svelte 5 runes (D-092). The memory wire removal + the capability string ride the
D-223 lockstep + D-209 regen in the same PR. Affected operator skills (§18):
`observe-with-the-console` (surface `console`) and `use-the-harbor-protocol`
(surface `protocol`) — both updated in the same PR.

## Test plan

- **Unit:** the projectioncheck gate — Half A (probe reflection, unassigned-field
  fails), Half B (a surface with no prod-wiring test fails; a synthetic
  omitted-`WithX` assembly fails), surface-coverage, and an empty
  honest-omission reason fails; tasks `has_pending_approval` truthful narrowing +
  `background_acknowledged` honesty; flows `tokens_used` summed; memory dead-facet
  removed (both `expiring_in_1h` sites) + `agent_ids` loud-reject; the un-stubbed
  `serve.Enricher` parent-session/cost; tools facet filters loud-reject +
  aggregates set `aggregates_partial` (not a silent 0) when unwired; de-enriched
  fixtures pinned by the gate (§17.8).
- **Integration:** `test/integration/projection_completeness_test.go` — real
  drivers (events inmem + durable StateStore, real task/approval registry, real
  session registry): open an approval gate on a task and assert
  `has_pending_approval` reaches the wire and the facet narrows; record cost and
  assert the enricher rollup is non-zero; assert cross-session isolation (session
  A's gate/cost never appears on session B's row); ≥1 failure mode (a
  `filter.agent_ids` over an unpopulated memory agent field fails loud). Runs
  under `-race`.
- **Conformance:** the projection-completeness gate IS the conformance suite for
  the projection-surface class — it ranges the registry and asserts coverage +
  no-unassigned-filtered-field across every registered surface (the §4.4/D-305
  registry-gate shape applied to projectors).
- **Concurrency / leak:** N≥100 concurrent projector / enricher invocations
  against one shared instance under `-race` (tasks projector + `serve.Enricher`);
  assert no data race, no context bleed (per-run identity assertions), no
  goroutine leak (§5, D-025).

## Smoke script additions

- `phase-177.sh`: (live-server) once a task holds an open approval gate,
  `tasks.list` returns a row with `has_pending_approval=true` and the facet
  narrows (SKIP until activity exists); a flow with recorded runs reports non-zero
  `tokens_used` (SKIP otherwise); `memory.list` with `filter.has_ttl_expiring`
  returns `invalid_request` (the facet is gone) rather than an empty 200; a
  `memory.list` `filter.agent_ids` over an unpopulated agent field returns
  `invalid_request`, never a false-empty 200. (static-only) the
  `internal/protocol/projectioncheck` gate test file exists and the memory dead
  facet is absent from the wire manifest. 404/405/501 → SKIP.

## Coverage target

- `internal/protocol/projectioncheck`: 90%
- `internal/tasks/protocol`: 85%
- `internal/runtime/flow/protocol`: 85%
- `internal/memory/protocol`: 85%
- `internal/runtime/serve` (enricher): 85%

## Dependencies

- 174 (HA-22 — the sessions instance + D-311). **Must land first** (the
  coordinator serializes 174's plan PR ahead of 177 so the D-309/D-311
  cross-refs resolve): the gate MUST cover the sessions surface 174 fixes, this
  phase's sessions registration edits `internal/sessions/protocol/lister_projector.go`
  which 174 also edits (serialize AFTER 174 to avoid a conflict), and the tasks
  enricher cost work coordinates with 174's session-cost aggregation.
- 08 (session registry), 54 (pause/approval registry), 60/72a (events substrate
  for cost), 73c/107a (tasks projection + enricher precedent), the flows catalog
  and memory projection surfaces. All shipped.

## Risks / open questions

- **Coordination with Phase 174 on cost.** The tasks `serve.Enricher.Cost`
  un-stub and 174's session cost rollup both aggregate `llm.cost.recorded`
  events. The two must share the aggregation helper (or one wraps the other) to
  avoid two divergent cost readers. If 174's cost helper is not yet extractable
  when this phase lands, the enricher cost un-stub drops to Phase 178 and the
  parent-session card un-stub stays here (both are display-only, not
  gate-triggering). Flagged, not silently forked.
- **`has_pending_approval` per-row lookup cost.** Populating the facet requires a
  per-row approval-registry lookup on `tasks.list` (bounded by the page limit).
  The lookup is O(page) and identity-scoped; if it proves expensive under high
  cardinality, the fallback is the D-311 capability-gate (advertise the facet only
  when the approval-registry accessor is wired, loud-reject otherwise) — the same
  ladder 174 uses (WARN-3). Recorded in D-313.
- **Memory `agent_ids`: loud-reject in V1, populate deferred.** `ConversationTurn`
  carries no producer identity and the projection sets only the triple, so V1 has
  no agent to populate from — the facet loud-rejects (representable absence,
  D-311). Populate is a follow-up that adds producer identity to the turn record;
  the facet keeps its wire slot (a not-yet-wired field). Not faked (§13).
- **Memory facet removal is a breaking change — §8 exemption, not silence.**
  Removing `has_ttl_expiring` + both `expiring_in_1h` fields is a breaking
  wire-shape change; RFC §8's deprecation-window requirement is satisfied by
  stating the exemption explicitly (the fields were always empty/0 → no live
  consumer can depend on a value; the pre-GA `0.1.0` within-version-removal
  precedent, Phase 171). The §8 defect the class guards against is an
  *unannounced* break; the PR body + D-313 announce it.
- **Flows `tokens_used` maturity.** `RunRecord.CostUSD` is itself written only by
  `cmd/harbor/devseed.go` today (the real flow engine does not yet call
  `RecordRun`); `Tokens` inherits exactly that maturity — it is truthful wherever
  a run is recorded and demo-seeded like cost. This phase does NOT build a real
  flow-run recorder (a larger net-new); it makes `tokens_used` symmetric with the
  already-shipped `cost_usd_used`. Named honestly, not overstated.
- **The gate's probe cannot prove semantic correctness, only assignment.** The
  gate catches "declared-but-never-assigned filtered field," not "assigned with a
  wrong value." That is the exact class HA-24 closes; deeper correctness stays the
  per-surface truthful-data tests. Scoped deliberately.

## Glossary additions

- **projection-completeness gate** — see `docs/glossary.md`.
- **honest-omission allow-list** — see `docs/glossary.md`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] Reusable-artifact concurrent-reuse test passes (tasks projector + enricher, N≥100, `-race`)
- [ ] Integration test exists (real drivers, identity propagation, ≥1 failure mode, `-race`)
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (D-313)
- [ ] If the wire shape / capability changes: D-223 lockstep + D-209 regen in the same PR
- [ ] §18 skill hygiene: `observe-with-the-console` + `use-the-harbor-protocol` updated
