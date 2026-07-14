# Phase 178 — tools-annotator

## Summary

The tools catalog projector reads every per-tool annotation — OAuth status,
approval policy, last-used, error-rate metrics, content-size stats, MCP
DisplayModes — through an optional `Annotator` seam (`WithAnnotator`). But **no
production `Annotator` is ever wired**: `mux.go`'s `NewCatalogProjector` supplies
only `WithLoadingResolver`, and the sole implementer of the interface is the
`fakeAnnotator` test double (§17.8). So in production `tools.list` / `tools.describe`
/ `tools.metrics` / `tools.content_stats` ship structural defaults — Healthy
status with zero gauges, empty content-stats, n/a OAuth, auto approval, zero
last-used — while `filter.oauth_statuses`, `filter.approval_policies`, the version
search axis, and the catalog aggregates operate over them (the class Phase 177's
gate honestly *gates* pending this phase). This phase assembles and wires the real
`Annotator` — backed by `tools/auth` (OAuth), `tools/approval` (policy), the
events stream (metrics / last-used / content-stats), and MCP negotiation
(DisplayModes) — flips the Phase 177 annotator-wired capability on, lights up the
gated facets with real data, populates `Tool.Version`, and un-blocks the inert
admin write path (`tools.set_approval_policy` / `tools.revoke_oauth`, which the
projector already delegates to the annotator via the `ApprovalPolicySetter` /
`OAuthRevoker` seams). It is the second member of the HA-24 band (D-314), the
consumer that turns 177's honest gate into real answers on the tools surface.

## RFC anchor

- RFC §5.2
- RFC §6.4
- RFC §6.15
- RFC §7

## Briefs informing this phase

- brief 03
- brief 06
- brief 07

## Brief findings incorporated

- brief 03 §"Tool catalog + transports" (RFC §6.4): per-tool OAuth / approval /
  reliability are annotations layered onto a transport-agnostic tool identity;
  the catalog reads them through a backend, never bakes them into the tool
  record. This phase supplies that backend as a §4.4-shaped concrete behind the
  existing `Annotator` seam — the interface + factory + registry pattern, not a
  concrete dependency baked into the projector.
- brief 06 §"Console — observability + control plane UI" (RFC §7): the Tools page
  renders OAuth/approval facets + a catalog overview card + a per-tool health
  panel; those affordances must reflect real runtime state (the runtime-lens
  principle). Phase 177 gated them honestly; this phase makes them live.
- brief 07 §"Tool transports + approval" (the approval-policy + OAuth gates): the
  approval policy and OAuth binding are runtime state owned by the approval /
  auth subsystems; the catalog is a read lens over them, and the admin write path
  (set policy / revoke OAuth) routes back through the same subsystems — never a
  shadow Console store (D-061).

## Findings I'm departing from (if any)

The phase builds strictly behind the already-shipped `Annotator` seam — no new
interface, no wire-shape break beyond the Phase-177 capability flipping from
unwired to wired. As shipped it carries these bounded deviations (§4.3), each an
honest representable-absence, never a fabrication:

- **Annotator package.** The production concrete lives in a NEW
  `internal/tools/annotate` package rather than `internal/tools/protocol`, so it
  can import `tools/auth` / `tools/approval` / `events` / MCP without those
  imports reaching the projector's package. It satisfies the projector's
  `Annotator` + `ApprovalPolicySetter` + `OAuthRevoker` seams structurally.
- **`Tool.Version` stays honestly empty.** No V1 transport carries a tool version
  — the runtime `tools.Tool` descriptor has no version field and the `Annotator`
  seam surfaces none. The `Name+Version` free-text search axis is honestly
  name-only (representable absence, never a fabricated version); its
  honest-omission entry REMAINS in the projection contract so the gate keeps
  enforcing the absence.
- **Approval-policy persistence is a new store.** `tools/approval` had no per-tool
  auto/gated/denied posture store (its decision engine answers per-invocation).
  This phase adds a StateStore-backed `approval.PolicyStore` in that owning
  subsystem — the persist path for `tools.set_approval_policy` and the read side
  of the annotation. It is session-scoped (isolation-safe; a tenant-wide admin
  posture is a future elevation).
- **`DisplayModes` reads honestly empty.** V1 MCP negotiation advertises host
  render-mode capabilities (a `[]string`), not the per-MIME→mode map the wire
  field models. The map is honestly empty; the seam is retained for a future
  per-MIME negotiation.
- **`ContentStats` histogram is offload-only.** Tool lifecycle events are
  content-free (name / transport / status / duration — never result bytes), so
  the per-result size histogram is built from the `mcp.resource_offloaded`
  records (the only per-result byte-size signal the runtime emits). Non-offloaded
  results carry no size and are honestly absent from the histogram.
- **The honest-degradation gate stays.** The projector keeps its
  loud-reject-when-unwired behaviour (rather than deleting the gate) so a
  headless / read-only catalog stack still degrades honestly; production wires the
  annotator, so the gate no longer fires there.
- **Cross-phase fix (§17.6).** The integration test surfaced a cross-session bleed
  in the 177 projector's in-memory approval-override map (keyed by tool ID only).
  Fixed in the same PR: the map is now identity-scoped AND bypassed entirely when a
  persisting annotator is wired.

## Goals

- **Assemble a production `Annotator`** (`internal/tools/protocol/annotator.go`
  or `internal/tools/annotate/`) implementing the full interface —
  `OAuthStatus`, `ApprovalPolicy`, `LastUsedAt`, `Metrics`, `ContentStats`,
  `DisplayModes` — plus the `ApprovalPolicySetter` / `OAuthRevoker` admin seams
  the projector already delegates to. Each method reads identity-scoped raw data
  from the subsystem that owns it: OAuth from `tools/auth`, approval policy from
  `tools/approval`, last-used / metrics / content-stats from the events stream,
  DisplayModes from the MCP negotiation state.
- **Wire it in production** at `internal/runtime/serve/mux.go` — append
  `WithAnnotator(realAnnotator)` to `toolsProjectorOpts` exactly as
  `WithLoadingResolver` is appended today (the one-line seam; the weight is the
  annotator assembly, not the wiring).
- **Flip the Phase-177 annotator-wired capability on** so the gated facets
  (`filter.oauth_statuses`, `filter.approval_policies`, the version search axis)
  and the catalog aggregates (`Active` / `PendingApproval` / `AwaitingOAuth`)
  operate over real data instead of loud-rejecting.
- **Populate `Tool.Version`** so the `Name+" "+Version` search axis can match on
  the version substring (or, if no version source exists for a transport, keep
  it honestly empty and document that the search axis is name-only for that
  transport — representable absence, never a fabricated version).
- **Light up the admin write path.** `tools.set_approval_policy` and
  `tools.revoke_oauth` currently return `ErrAdminUnsupported` because no annotator
  implements `ApprovalPolicySetter` / `OAuthRevoker`; the real annotator
  implements them (routing back through `tools/approval` / `tools/auth`), so the
  writes persist and emit audit — never a Console shadow store (D-061).
- **Register nothing new in the completeness gate** beyond flipping the tools
  allow-list entries from "gated, pending 178" to "populated" — the tools surface
  was already registered by Phase 177; this phase removes its honest-omission
  entries as the fields become truly assigned, so the gate now enforces them.

## Non-goals

- **No change to the `Annotator` interface or the projector's seam.** The seam
  shipped correctly; this phase only supplies the missing concrete + wiring.
- **No new Protocol method / `ProtocolVersion` bump.** The tools wire fields are
  already declared (Phase 177 gated them, not removed them). The capability flips
  from unwired to wired — an advertised-set change, not a wire-shape change.
- **No shadow Console store for tool state.** All annotation data flows from the
  runtime subsystems through the Protocol; the admin writes route back through
  `tools/approval` / `tools/auth` (D-061).
- **No new metrics pipeline.** Metrics / last-used / content-stats derive from
  the existing events stream, read-time, exactly like the other read-time
  enrichers — no new store.

## Acceptance criteria

- [ ] A production `Annotator` implements the full interface plus
      `ApprovalPolicySetter` + `OAuthRevoker`, each method identity-scoped and
      reading from the owning subsystem (OAuth: `tools/auth`; approval:
      `tools/approval`; metrics/last-used/content-stats: events; DisplayModes: MCP
      negotiation). No method returns a stub/canned value (§13 — no test-grade
      default on an operator seam).
- [ ] `mux.go` wires the annotator via `WithAnnotator(...)`; a test asserts the
      production tools service is constructed WITH an annotator (the §17.8
      wired-in-prod assertion — the mirror of the `fakeAnnotator`-only-in-tests
      finding).
- [ ] With the annotator wired, `tools.list` `filter.oauth_statuses` /
      `filter.approval_policies` narrow to real rows; the catalog aggregates
      (`Active` / `PendingApproval` / `AwaitingOAuth`) reflect real counts; the
      version search axis matches a real version substring. Proven against real
      drivers end-to-end (§17.1).
- [ ] `tools.metrics` returns real per-tool error-rate gauges over the window and
      `tools.content_stats` returns a real result-size histogram, both
      identity-scoped, for a tool with recorded invocations.
- [ ] `tools.set_approval_policy` persists the policy (through `tools/approval`)
      and `tools.revoke_oauth` revokes the binding (through `tools/auth`), each
      emitting audit; neither returns `ErrAdminUnsupported`. A test asserts a
      round-trip (set → read-back reflects the new policy).
- [ ] `Tool.Version` is populated where a transport carries a version, or
      honestly empty (with the search axis documented name-only) where it does
      not — never fabricated.
- [ ] The Phase-177 completeness gate now enforces the tools annotator-backed
      fields (their honest-omission allow-list entries are removed); the gate's
      probe for tools runs with the annotator wired and asserts the fields are
      assigned.
- [ ] Cross-session isolation: annotator reads are identity-scoped — tool
      last-used / metrics for session A never bleed into session B's projection;
      an isolation test asserts it.
- [ ] Concurrent reuse: the annotator is immutable-after-construction with
      per-call state in args/locals; a concurrent-reuse test (N≥100 invocations
      against one shared instance, `-race`) passes (§5, D-025).
- [ ] Console consistency (§4.5 / D-121): the Tools page OAuth/approval facets,
      overview aggregates, and per-tool health panel render real data; the admin
      set-policy / revoke-OAuth controls persist through the Protocol. No new page
      composition. See below.
- [ ] `scripts/smoke/phase-178.sh` asserts (live-server) `tools.metrics` returns a
      real gauge for an invoked tool, an OAuth/approval facet narrows real rows,
      and `tools.set_approval_policy` round-trips (SKIP where the surface/activity
      is absent). 404/405/501 → SKIP.

## Files added or changed

- `internal/tools/protocol/annotator.go` *(new)* (or `internal/tools/annotate/`)
  — the production `Annotator` + `ApprovalPolicySetter` + `OAuthRevoker`
  implementation aggregating from `tools/auth`, `tools/approval`, the events
  stream, and MCP negotiation state.
- `internal/runtime/serve/mux.go` — append `WithAnnotator(realAnnotator)` to
  `toolsProjectorOpts` (the one-line seam) + assemble the annotator's deps.
- `internal/tools/protocol/catalog_projector.go` — remove the annotator-wired
  capability gate now that the backend exists (facets/search/aggregates go live);
  the projector's existing `ApprovalPolicySetter` / `OAuthRevoker` delegation now
  resolves a real annotator.
- `internal/tools/protocol/filter.go` — un-gate the annotator-backed facets/search.
- `internal/tools/protocol/*_test.go` — real-annotator truthful-facet / metrics /
  content-stats / admin-write round-trip / isolation / concurrent-reuse tests;
  the §17.8 wired-in-prod assertion; the completeness-gate tools allow-list
  entries removed.
- `internal/protocol/projectioncheck/*` — flip the tools honest-omission entries
  to enforced (coordinate with the Phase 177 registration).
- `test/integration/tools_annotator_test.go` *(new)* — real drivers end-to-end
  (§17.1).
- `web/console/src/lib/protocol/*` + `wire-manifest.gen.json` — D-223 lockstep IF
  the capability advertisement changes; the Tools route renders real facets /
  aggregates / health + wires the admin controls.
- `docs/skills/observe-with-the-console/SKILL.md` +
  `docs/skills/use-the-harbor-protocol/SKILL.md` — §18 same-PR skill update
  (the tools health / facets / admin-write steps go live).
- `scripts/smoke/phase-178.sh` *(new)*.

## Public API surface

```go
// internal/tools/protocol (or internal/tools/annotate)

// NewAnnotator builds the production tools Annotator over the OAuth,
// approval, events, and MCP-negotiation dependencies. The returned value
// implements protocol.Annotator + ApprovalPolicySetter + OAuthRevoker and
// is safe for concurrent reuse (immutable after construction; per-call
// state in args/locals).
func NewAnnotator(deps AnnotatorDeps) (*Annotator, error)

type AnnotatorDeps struct {
    OAuth    tools/auth binding-reader   // OAuthStatus / RevokeOAuth
    Approval tools/approval policy store // ApprovalPolicy / SetApprovalPolicy
    Events   events read-side            // LastUsedAt / Metrics / ContentStats
    Display  MCP DisplayMode negotiation // DisplayModes
}
```

## Console consistency (§4.5 / D-121)

The Tools page already renders the OAuth/approval facet chips, the right-rail
catalog-overview aggregate card, and the per-tool health panel — Phase 177 gated
them honestly (disabled / "unavailable" when the annotator is unwired). This
phase makes them LIVE: real OAuth/approval narrowing, real Active / PendingApproval
/ AwaitingOAuth counts, real metrics gauges + content-stats histograms, and the
admin set-approval-policy / revoke-OAuth controls persisting through the Protocol
(never a Console shadow store, D-061). No new page composition. All on the shared
`HarborClient` + `connection.ts`, tokens only, Svelte 5 runes (D-092). If the
capability advertisement changes shape, the typed per-page Protocol client +
`wire-manifest.gen.json` update in the same PR under D-223. Affected operator
skills (§18): `observe-with-the-console` (surface `console`) and
`use-the-harbor-protocol` (surface `protocol`) — both updated in the same PR.

## Test plan

- **Unit:** each annotator method reads the right identity-scoped source;
  truthful facet narrowing / aggregates / version search; metrics + content-stats
  shape; admin set-policy / revoke-OAuth round-trip; the §17.8 wired-in-prod
  assertion; completeness-gate tools entries enforced.
- **Integration:** `test/integration/tools_annotator_test.go` — real drivers
  (real `tools/auth` + `tools/approval` + events inmem): invoke a tool, assert
  `tools.metrics` / `LastUsedAt` reflect it; bind + revoke OAuth and assert the
  facet + write path; assert cross-session isolation (session A's metrics never
  on session B's projection); ≥1 failure mode (a set-policy on an unknown tool
  fails loud). Runs under `-race`.
- **Conformance:** covered by the Phase 177 projection-completeness gate — with
  the annotator wired, the tools probe now asserts the previously-gated fields are
  assigned (the allow-list entries are gone).
- **Concurrency / leak:** N≥100 concurrent annotator invocations against one
  shared instance under `-race`; assert no data race, no context bleed, no
  goroutine leak (§5, D-025).

## Smoke script additions

- `phase-178.sh`: (live-server) `tools.metrics` returns a real error-rate gauge
  for a tool with recorded invocations (SKIP until activity exists); a
  `tools.list` with `filter.oauth_statuses` / `filter.approval_policies` narrows
  real rows; `tools.set_approval_policy` round-trips (set then `tools.describe`
  reflects the new policy). 404/405/501 → SKIP.

## Coverage target

- `internal/tools/protocol` (incl. annotator): 85%
- `internal/runtime/serve` (wiring): 85%

## Dependencies

- 177 (D-313 — the projection-completeness gate + the annotator-wired capability
  this phase flips on; the tools surface is registered by 177). Must land first.
- 28 (MCP registry / DisplayMode negotiation), the `tools/auth` OAuth subsystem,
  the `tools/approval` policy store, 60/72a (events read-side for
  metrics/last-used/content-stats). All shipped.

## Risks / open questions

- **Metrics / content-stats derivation cost.** Reading per-tool error-rate +
  content-size histograms from the events stream at projection time is a
  bounded, identity-scoped read (per visible tool, ≤ page limit). If it proves
  expensive, the fallback is a cached read-side the annotator owns (internally
  synchronized), never a shadow store. Flagged.
- **DisplayModes source.** The negotiated MCP DisplayMode map lives in the MCP
  connection state; the annotator must read it without importing Console code and
  without widening the isolation boundary. Scoped to the MCP negotiation state
  the runtime already holds.
- **`Tool.Version` availability varies by transport.** In-proc / HTTP tools may
  carry no version; the search axis is honestly name-only for those (representable
  absence), never a fabricated version. Recorded in D-314.
- **Admin write blast radius.** Lighting up `set_approval_policy` /
  `revoke_oauth` makes a previously-inert write path live; each write is
  identity/scope-gated and audited (§7). The isolation + audit assertions are
  binding ACs, not follow-ups.

## Glossary additions

- **tools annotator (production)** — see `docs/glossary.md`.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] Reusable-artifact concurrent-reuse test passes (annotator, N≥100, `-race`)
- [ ] Integration test exists (real drivers, identity propagation, ≥1 failure mode, `-race`)
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (D-314)
- [ ] If the wire shape / capability changes: D-223 lockstep + D-209 regen in the same PR
- [ ] §18 skill hygiene: `observe-with-the-console` + `use-the-harbor-protocol` updated
