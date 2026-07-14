# Phase 175 — fleet-retention-horizons

## Summary

Closes HA-23: the retention-horizon block HA-14 (D-296) shipped on `runtime.health` reports the three durable surfaces' horizons at THREE DIFFERENT scopes — `events` runtime-wide, `tasks` scoped to the caller's session, `sessions` scoped to the caller's tenant — so the one consumer that exists to observe the fleet (a coordinator polling under a dedicated `svc:` service identity that owns no sessions/tasks) receives ONLY the `events` horizon; the `tasks` + `sessions` horizons are structurally empty and, on the wire, indistinguishable from "the surface retains nothing." This phase makes the `tasks` + `sessions` horizons OBSERVABLE at runtime-wide scope to a verified admin / `console:fleet` caller (server-derived authority per D-299, riding `runtime.health` — no new method, no ordinary-caller scope relaxation), and makes the absence of a value REPRESENTABLE on the wire (a per-surface `scope` marker so an unobservable scope can never masquerade as an empty result — the HA-18/20/21/22/23 through-line, D-311).

## RFC anchor
<!-- Required. List the RFC sections this phase implements. Format exactly: RFC §6.X. -->
- RFC §5.2
- RFC §5.5
- RFC §6.1
- RFC §6.13
- RFC §6.14
- RFC §6.16
- RFC §7

## Briefs informing this phase
<!-- Required. -->
- brief 06
- brief 12

## Brief findings incorporated
<!-- Required. -->
- brief 06 §5 ("Metrics cardinality footgun" + the one-bus lesson): the retention
  horizon belongs to the DURABLE surfaces (their oldest-retained head is
  derivable live from the one bus / the stores), and Harbor never becomes a
  time-series DB. This phase keeps that posture — it changes only the SCOPE at
  which two of the three durable horizons are READ, never adds a stored series,
  and does not touch the counters/metrics snapshots (D-296's decided-NO TSDB).
- brief 06 §5 (honesty at the observability edge): a merged "last N days" fleet
  view over heterogeneous runtimes must be able to mark "this runtime retains
  only back to X" rather than imply completeness. HA-14 delivered the signal but
  only to a caller that is a real tenant with its own sessions/tasks; the fleet
  consumer is exactly the caller that cannot observe two of the three horizons.
  This phase closes that gap so the honesty signal reaches the consumer it was
  designed for.
- brief 12 (Console deployment posture — the fleet/observability lens): the
  Console attaches to many runtimes and reads a runtime-wide operational posture,
  never per-caller slices. `runtime.health` is an operator + Protocol surface
  (CLAUDE.md §18); a fleet-lens reader needs the runtime-wide horizon, and the
  elevated `console:fleet` scope (RFC §7) is the intended key for it.

## Findings I'm departing from (if any)
<!-- Required (can be "None"). -->
- None. This phase RATIFIES D-296's observed-not-configured horizon model and
  D-299's server-derived elevation discipline; it extends the READ SCOPE of two
  horizons for an elevated caller and adds an absence-representable wire marker.
  It departs from nothing — it completes what HA-14 shipped for the fleet caller.

## Goals
<!-- What this phase must achieve. Outcomes, not implementation. -->
- A verified admin / `console:fleet` caller reads the `tasks` and `sessions`
  retention horizons at RUNTIME-WIDE scope on `runtime.health` — the same
  identity-free runtime-wide scope the `events` horizon already uses — so a
  fleet/`svc:` coordinator observes all three horizons.
- The elevation is SERVER-DERIVED from the verified session's scope (D-299),
  never from the request body; a caller without the scope is unaffected.
- The ordinary (non-elevated) caller's per-tenant / per-session horizon read is
  UNCHANGED — it stays a fail-closed control, no widening, no downgrade knob.
- The absence of a horizon value at a caller's scope is REPRESENTABLE on the
  wire: an unobservable-at-scope surface is distinguishable from a
  surface-retains-nothing surface, so a consumer degrades HONESTLY (marks a fleet
  window's completeness unverifiable) instead of silently trusting a shorter or
  absent horizon as runtime-wide truth (the class rule, D-311).
- No new Protocol method, no new capability bit, no `ProtocolVersion` bump — the
  change rides `runtime.health` + the existing `runtime_posture` capability +
  the existing admin / `console:fleet` scope, and is additive on the wire.

## Non-goals
<!-- Explicit out-of-scope items. -->
- **No change to the `events` horizon** — it is already runtime-wide and correct
  (`events.RetentionReporter.OldestRetainedAt(ctx)`, identity-free). It gains
  only the `scope:"runtime"` label; its value and read path are untouched.
- **No retention/pruning knob.** Harbor has none — the durable event log is
  gap-free and untrimmed (`internal/events/drivers/durable/durable.go` godoc);
  the horizon stays OBSERVED, never a configured claim (D-296). This phase adds
  no configured-retention field.
- **No relaxation of the ordinary caller's scope.** The per-tenant / per-session
  fold stays a fail-closed control (CLAUDE.md §13 — no identity-downgrading
  knob). Widening is gated on the verified elevated scope alone.
- **No redaction / retention-mechanism change**, no change to
  `RuntimeCounters` / `metrics.snapshot` (D-296's rejected counters/metrics TSDB
  stays rejected).
- **No cross-RUNTIME federation.** A single runtime reports its own runtime-wide
  horizons; merging across runtimes stays coordinator-side (the D-284 posture).
- **Does not, by itself, make the fleet window COMPLETE.** It makes the window's
  completeness VERIFIABLE. Reachability of the session-less cross-session
  enumeration is HA-21's job (the events-fold work); this phase composes with it
  — HA-21 makes the enumeration reachable, this makes its completeness checkable.

## Acceptance criteria
<!-- Required. Bulleted, testable. These are binding. -->
- [ ] A `runtime.health` read by a caller carrying a verified `auth.ScopeAdmin`
      OR `auth.ScopeConsoleFleet` claim (server-derived from `identity.From(ctx)`
      + the scope set, per D-299 — NEVER read from the request body) returns the
      `tasks` and `sessions` horizons computed at RUNTIME-WIDE scope, each
      labelled `scope:"runtime"`.
- [ ] A `runtime.health` read by a caller with NO elevated scope returns the
      `tasks` horizon scoped to the caller's session (`scope:"session"`) and the
      `sessions` horizon scoped to the caller's tenant (`scope:"tenant"`) — byte-
      for-byte the HA-14 behaviour plus the new `scope` label. No widening.
- [ ] The `events` horizon is always runtime-wide and labelled `scope:"runtime"`
      for every caller (unchanged value, new label only).
- [ ] Absence is representable: for a wired retention seam, the three known
      surfaces (`events`, `tasks`, `sessions`) each report an entry carrying
      `surface` + `scope`, with `oldest_retained_at` OMITTED when the surface
      holds no rows AT THAT SCOPE. A consumer can therefore distinguish
      `scope:"runtime"` + no-timestamp ("runtime retains nothing — trustworthy
      empty") from `scope:"session"`/`"tenant"` + no-timestamp ("nothing at your
      scope — runtime-wide truth NOT observable here"). A nil retention seam
      still omits the whole block (older/headless wiring unaffected).
- [ ] The widened path (a read where the server-derived elevated scope caused a
      runtime-wide fan-in) emits exactly one `audit.admin_scope_used` event
      through the wired Redactor + Bus, anchored on the ACTOR's verified
      identity; the redactor pass precedes the publish; an emit failure FAILS
      LOUD (`CodeRuntimeError`) — the read already crossed the tenant boundary,
      so the operator MUST see the audit (the PostureSurface `*.posture_read_admin`
      precedent). A non-widened read emits NO audit.
- [ ] The runtime-wide `tasks` / `sessions` horizon is read through an
      identity-free oldest-retained reader (mirroring `events.RetentionReporter`),
      discovered by type assertion at the posture wiring seam — NO `Supports*`
      capability protocol (CLAUDE.md §4.4 no-ceremony; the D-296 as-built
      precedent). A store that does not implement the reader contributes no
      runtime-wide entry (its horizon is absent, never fabricated).
- [ ] `ProtocolVersion` stays `0.1.0`. The one additive wire field
      (`RetentionHorizon.Scope`) triggers the full D-223 lockstep (all canonical
      homes + `make protocol-ts-gen`) and D-209 regen (`make protocol-docs-gen`);
      the `use-the-harbor-protocol` SKILL.md and the docs-site protocol stub are
      updated in the same PR (CLAUDE.md §18).
- [ ] Cross-tenant isolation holds: the ordinary caller never observes another
      tenant's horizon; the widened caller observes ONLY the runtime-wide roll-up
      (a bare oldest-retained instant, no per-tenant / per-session content) —
      never an enumeration of other tenants' rows.
- [ ] `scripts/smoke/phase-175.sh` passes against the build (OK ≥ 2, FAIL = 0);
      the widened path is proven in the Go integration test (dev is trust-based —
      no verified scope over HTTP), the smoke asserts the `scope` marker shape.

## Files added or changed
<!-- Tree-style list. -->
- `internal/protocol/types/posture.go` — add `RetentionHorizon.Scope` (additive
  `scope` field; the three canonical values `runtime`/`tenant`/`session`);
  godoc the absence-representable semantics.
- `internal/protocol/posture.go` — `handleHealth` computes the server-derived
  `widened` decision (verified `ScopeAdmin`/`ScopeConsoleFleet` from ctx, D-299),
  threads it into the retention seam as a Go input, and emits the widened-path
  `audit.admin_scope_used` (fail-loud). Extend the `retention` seam signature to
  carry the `widened` flag.
- `internal/runtime/posture/posture.go` — `RetentionProvider` reads the
  runtime-wide `tasks` / `sessions` oldest-retained reader when `widened`, else
  the existing per-session / per-tenant read; stamps `Scope` on every entry;
  always emits an entry for a wired-but-empty-at-scope known surface.
- `internal/tasks/…` + `internal/sessions/…` — an identity-free
  `OldestRetainedAt(ctx) (time.Time, bool, error)` reader (mirroring
  `events.RetentionReporter`) on the tasks registry and the session lister,
  discovered by type assertion at the wiring seam. (Runtime-wide read; no wire
  type — no lockstep for these internal seams.)
- `internal/protocol/methods/…` — none (no new method); the wire manifest regen
  reflects the `RetentionHorizon.Scope` field only.
- `web/console/src/lib/protocol/*.ts` + `web/console/src/lib/protocol/wire-manifest.gen.json`
  — hand-mirror the `scope` field + `make protocol-ts-gen` (D-223 lockstep).
- `docs/site/protocol/types.md` — regenerated via `make protocol-docs-gen` (D-209).
- `docs/skills/use-the-harbor-protocol/SKILL.md` — note the fleet-scoped horizon
  and the `scope` marker on `runtime.health` (CLAUDE.md §18).
- `docs/plans/phase-175-fleet-retention-horizons.md` — this plan.
- `docs/plans/README.md` — row + detail block (Pending).
- `docs/decisions.md` — D-310.
- `docs/glossary.md` — new terms.
- `scripts/smoke/phase-175.sh` — retention-scope-marker assertions.

## Public API surface
<!-- Interface signatures (Go-flavored). Do NOT include internal types. -->
- Wire (additive): `types.RetentionHorizon.Scope string` (`json:"scope,omitempty"`),
  one of `"runtime"` / `"tenant"` / `"session"`. No other wire change.
- Runtime seam (internal, no wire): an optional identity-free reader both the
  tasks registry and the session lister may implement —
  `OldestRetainedAt(ctx context.Context) (oldest time.Time, present bool, err error)`
  — the runtime-wide analogue of `events.RetentionReporter`, discovered by type
  assertion (no `Supports*` protocol).
- The `PostureDeps.Retention` seam signature gains a server-derived `widened bool`
  input (a Go parameter, never a wire field): the widening decision computed by
  the surface from the verified scope, threaded into the provider.

## Test plan
<!-- Categorize. -->
- **Unit:**
  - `internal/protocol/types`: `RetentionHorizon` JSON round-trip carries `scope`;
    omitted-timestamp + scope combinations serialize as specified; the field is
    additive (an old-shape decode ignores it).
  - `internal/protocol` (`handleHealth`): the server-derived `widened` decision
    fires ONLY on a verified `ScopeAdmin`/`ScopeConsoleFleet` ctx and NEVER from a
    body scope; a widened read emits exactly one `admin_scope_used` (redactor
    then publish); an emit failure returns `CodeRuntimeError`; a non-widened read
    emits none.
  - `internal/runtime/posture`: `RetentionProvider` stamps `scope` correctly per
    surface; picks the runtime-wide reader when widened, the per-scope read when
    not; always emits an entry for a wired-but-empty-at-scope known surface; a
    store read error degrades that one surface to absent while the others report.
- **Integration:** `test/integration/retention_horizon_fleet_test.go` — REAL
  drivers on the seam (a real events bus implementing `RetentionReporter`, the
  real tasks engine, the real session registry / lister). Seed sessions + tasks
  across ≥2 tenants under distinct triples. Assert: (1) a caller with a verified
  `console:fleet` scope on ctx (a `svc:` principal owning NO sessions/tasks)
  observes runtime-wide `tasks` + `sessions` horizons (`scope:"runtime"`) that
  reflect the OTHER tenants' oldest rows; (2) an ordinary caller (no scope) gets
  its scoped view only (`scope:"session"`/`"tenant"`), never another tenant's
  horizon (identity propagation / cross-tenant isolation); (3) failure mode — a
  Redactor forced to refuse the widened `admin_scope_used` payload makes the
  widened read FAIL LOUD (`CodeRuntimeError`), never silently succeed. Run under
  `-race`.
- **Conformance:** N/A — no new driver and no multi-driver interface (the
  runtime-wide reader is an optional single-method seam discovered by type
  assertion, like `events.RetentionReporter`; drivers that omit it contribute no
  entry, which is the honest absence, not a conformance gap).
- **Concurrency / leak:** `runtime.health` is a long-lived shared server seam —
  an N≥10 concurrent-reader leg mixing elevated + ordinary callers against one
  shared `PostureSurface` under `-race`, asserting no cross-talk (an ordinary
  caller never sees a widened horizon) and no goroutine leak after teardown.

## Smoke script additions
<!-- Required. -->
`scripts/smoke/phase-175.sh` (live-server): after a scripted `start` run seeds
the bus/tasks/sessions, POST `runtime.health` and assert —

- the additive `retention` block is present and every entry carries a `scope`
  field whose value is one of `runtime`/`tenant`/`session` (OK #1 — the
  absence-representable marker shipped);
- the `events` entry is `scope:"runtime"` with an RFC-3339 `oldest_retained_at`
  (OK #2 — the runtime-wide horizon is labelled and non-empty);
- the `tasks`/`sessions` entries carry a `scope` and, when present, an RFC-3339
  timestamp — a scoped entry with no timestamp is NOT a FAIL (honest absence).
- route-probe → SKIP on 404/405/501; SKIP (not FAIL) when the dev bearer / `jq`
  is unavailable — the widened-path fan-in is Go-integration-covered because
  `harbor dev` is trust-based (no verified elevated scope over HTTP).

## Coverage target
<!-- Per touched package. -->
- `internal/runtime/posture`: 85%
- `internal/protocol` (posture handler additions): ≥ current package coverage;
  the new `widened`-decision + widened-audit branches are covered.
- `internal/protocol/types` (posture wire types): ≥ current package coverage.

## Dependencies
<!-- Phase numbers that must land before this one. -->
- 163 (HA-14 / D-296 — the retention block + `RetentionProvider` this phase
  extends).
- 118 (D-223 — the wire-lockstep gate the additive `scope` field runs through).
- Composes with the HA-21 events-fold work (makes the session-less cross-session
  enumeration reachable); not a hard build dependency — the two are orthogonal
  (reachable vs verifiable) and can land in either order.

## Risks / open questions
<!-- Surface real risks. -->
- **Runtime-wide read cost.** The runtime-wide `tasks`/`sessions` oldest-retained
  read must be cheap (an oldest-head lookup, not a full-fleet enumeration). The
  `events` horizon already proves the pattern (the durable driver seeds its
  horizon from the persisted head; the ring reads its head live). The optional
  reader seam is specified as an oldest-head accessor for exactly this reason —
  NOT a `ListTenant`-style scan. If a store cannot answer cheaply it simply omits
  the runtime-wide entry (honest absence).
- **Absence encoding — open question (Harbor's call in review).** The plan
  commits to a single additive `scope` field + always-emit-for-known-surfaces so
  omitted-vs-explicit-timestamp is the absence signal. An equivalent encoding is
  an explicit `observable bool` marker per surface. The ACs assert the
  DISTINGUISHABILITY PROPERTY, not the field name; the concrete field the PR
  ships is `scope` unless review prefers `observable`. Either satisfies D-311.
- **Does the widened `runtime.health` need audit at all**, given the horizon is a
  bare instant with no tenant content? Decision: yes — consistency with the
  D-299 / D-284 / D-305 "a server-derived widened read emits `admin_scope_used`"
  discipline; the fan-in crossed the tenant boundary even though the projection
  is content-free. Cheap and uniform beats a special-case exemption.
- **`console:fleet` vs `admin`.** Both are accepted (the same set the existing
  cross-tenant posture gate honours, `internal/protocol/posture.go`), matching
  RFC §7's fleet-observation entitlement — a read-only fleet token must reach the
  horizon without the higher `admin` control scope.

## Glossary additions
<!-- If this phase introduces new vocabulary, list here AND add to docs/glossary.md. -->
- **fleet-scoped retention horizon**
- **retention-horizon scope marker**

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **If this phase builds a reusable artifact:** N/A — `PostureSurface` is an
      already-shipped reusable artifact carrying its concurrent-reuse test; this
      phase adds a read branch, covered by the new concurrency leg (§17.3) rather
      than a fresh artifact test.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-
      subsystem seam:** integration test exists (`test/integration/retention_horizon_fleet_test.go`),
      wires real drivers (events bus + tasks engine + session registry), asserts
      identity propagation + cross-tenant isolation, covers the audit-fail failure
      mode, runs under `-race`.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry
      filed — N/A (no departure)
