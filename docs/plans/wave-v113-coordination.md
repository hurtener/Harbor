# Harbor v1.13.0 — The Serve-Parity + Historical-Observability Wave (phases 159–165) — wave coordination

> Per Harbor §17.7 wave delivery cadence. This is the coordination artifact for
> the v1.13.0 wave ("Serve parity" + the session-rehydration live-test fix).
> It sequences the phases into staged worktree dispatches, prescribes the
> drain-merge order, the per-phase gates (two adversarial reviews + live
> verification each), the wave-end E2E, and the §17.5 checkpoint audit that
> gates any subsequent v1.13 band.
>
> **Mandate:** close the serve-parity gap. `harbor serve` serves the Protocol
> from stock yaml, but a scaffolded agent carrying compiled in-process Go tools
> cannot — the serve composition is trapped in `package main`. Promote it
> (159), give it a curated `sdk/server` facade + an opt-in serving scaffold and
> prove parity (160). **Added 2026-07-10 (operator):** Phase 161 joins the wave
> as Stage 3 — the session-rehydration live-test fix (reopen loses per-turn
> tokens/cost/latency, TOOL CALLS badges, model chip; D-293). **Extended
> 2026-07-10 (operator, four filed asks):** Stages 4/5 — the historical
> observability + MCP-discovery band: 162 `events.list` (D-294) ∥ 164 MCP
> OAuth requirement discovery (D-297), then 163 the windowed-reads honesty
> pair (D-295 + D-296). Additive public API + additive wire surfaces ⇒
> additive **minor** bump ⇒ ships as **v1.13.0**.

---

## Version label — v1.13.0 (settled)

- The latest released tag is **v1.12.0** (session titles 157 + auto-naming 158,
  cut 2026-07-09). The next minor is **v1.13.0**.
- This wave is purely **additive** public API + one internal re-homing. No
  `ProtocolVersion` bump (RFC §5.3 — that is an RFC change), no breaking
  change, no new wire types (D-291/D-292 both state "Protocol additions: none").
  The product release version moves v1.12.0 → **v1.13.0**.
- The RFC gains §5.6 (external Protocol serving) + a `sdk/server` item on §3.6
  and a §5.3 deprecation-window amendment — settled design encoded in THIS
  plans PR, ahead of the implementation phases (the §16 workflow).

---

## 1. Executive summary

Harbor advertises three adopter paths (embed / CLI / protocol). The v1.8
adopter-path wave made embed + CLI honest and closed the serve-attach cliff for
a stock `harbor serve`. The remaining gap: **an external binary with compiled
in-process Go tools cannot serve the Protocol at all.** The serve composition
(`bootDevStack`, `devBootOptions`, the `devStack` serve/close lifecycle) lives
in `cmd/harbor` (`package main`), unreachable to any importer — the same shape
D-197 fixed one layer down for subsystem assembly. So a scaffolded agent can run
a goal headless (embed) but cannot expose the same wire surface `harbor serve`
does.

v1.13.0 closes that gap — plus the session-rehydration live-test regression —
in three staged phases:

- **159 (Stage 1)** promotes the serve band into ONE importable internal
  package (`internal/runtime/serve`) whose constructor REQUIRES a non-nil
  auth-validator factory, leaving dev-only policy in `cmd/harbor` composed
  caller-side through explicit injection seams; `harbor serve`/`dev`/`console`
  become thin callers and `harbortest/devstack` is re-wired onto it as the §13
  second consumer. New Go-side option/handle seams, ZERO wire changes.
- **160 (Stage 2)** adds the curated `sdk/server` facade (production-only
  posture), the opt-in `harbor scaffold --with-server`, and the acceptance
  centerpiece — the **parity gate**: both binaries from the same base config
  for method-status parity / dev-404s / identity+401, the compiled-tool legs
  on the scaffolded binary, CI mechanics in-module (scripted LLM), the
  subprocess wire end-to-end as an env-gated live leg.
- **161 (Stage 3, added 2026-07-10)** closes the operator-confirmed
  live-test regression: reopening a Playground session loses per-turn
  tokens/cost/latency, TOOL CALLS badges, and the model chip. Verified
  producer-side (not read-path): the `llm.cost.recorded` emit is
  bifrost-driver-local and the `tool.*` lifecycle emits are
  inproc-transport-local with attribution-dead envelopes. One driver-neutral
  emit seam per producer (the mandatory LLM-edge safety wrapper; the
  catalog-build descriptor-wrap shell every dispatch path inherits),
  content-free payloads only, zero wire changes, and the Console
  reducer/rehydration consumer in the same phase — leave-and-return renders
  identical to the live view (D-293).
- **162 (Stage 4, added 2026-07-10)** ships `events.list` — the durable,
  time-ranged, cross-session raw-event read (the biggest
  historical-observability gap: subscribe is live-only, aggregate is
  counts-only, state.history is per-session) — reusing the existing
  `EventFilter` + the `state.history` paging grammar + row projection, with
  §6-derived fleet widening, and the Console Events page historical window
  as the D-062 consumer (D-294).
- **164 (Stage 4, parallel with 162)** makes Harbor discover a connected MCP
  server's advertised OAuth requirement (401 challenge → RFC 9728 →
  RFC 8414 chain) and surface it VERBATIM as inert Protocol data on the
  connection view — never running the flow, never holding a token (D-271
  stays PULL), SSRF-bounded discovery fetches, spec-derived fixtures
  (§17.8), MCP Connections page consumer (D-297).
- **163 (Stage 5, ∥ 165)** is the windowed-reads honesty pair: optional
  `since`/`until` on `flows.runs.list` mirroring `TaskFilter` (D-295), and
  retention horizons as Protocol data on `runtime.health` — the OBSERVED
  `oldest_retained_at` per durable surface (the filed ask's
  configured-retention premise was verified false; the durable log is
  untrimmed in V1), plus the counters/metrics TSDB re-recorded as a
  decided-NO (D-296).
- **165 (Stage 5, ∥ 163, operator-added 2026-07-11)** is structured
  reasoning-steps rehydration: Phase 161 restored per-turn stats + flat
  reasoning text + tool badges on reopen, but the ordered per-ReAct-step
  reasoning (the live `ReasoningAccordion`, interleaved with the tool calls
  each step preceded) is lost on reopen. VERDICT ZERO-WIRE, Console-only:
  the live steps come from the tasks.get enricher over the in-memory
  trajectory (evicted on reopen), so the reducer reconstructs the ordered
  `reasoningSteps` from the durable `planner.decision.ReasoningTrace` +
  `tool.*` events `state.history` already carries — reopen renders identical
  to live (D-298).

---

## 2. Verified facts the design rests on (live tree, 2026-07-10)

These were confirmed against the current checkout and are load-bearing for the
plan; agents should re-verify against their worktree, not re-derive the design:

- `harbor serve` already calls `bootDevStack` with an injected JWKS
  `authValidatorFactory` (`cmd/harbor/cmd_serve.go:142`,
  `newJWKSValidatorFactory()` at `:178`).
- Everything below the serve band is already promoted:
  `internal/runtime/assemble.Assemble` (D-197).
- `assemble.Options.PreRegisterTools` registers on the catalog BEFORE builtin
  registration and the Builder's `tools.entries` wrapping
  (`internal/runtime/assemble/assemble.go:132` field, applied at `:703`,
  wrapping downstream). This is the seam D-292's `RegisterCatalog` rides.
- `harbor token` (keygen + mint) already exists (D-264) — the production JWKS
  local-dev loop the `sdk/server` facade documents.
- The mock LLM driver is internal-only and excluded from BOTH prod aggregators
  (D-089) — the promoted serve constructor must not be able to seat it.
- The dev-surface mounts are already caller-side, signer-gated material: the
  `signer != nil` gates at `cmd/harbor/cmd_dev.go:1403`/`:1453` guard the
  draft + bootstrap mounts (`:1429`/`:1460`), the dev key-rotate surface
  threads at `:1331` (`transports.WithAuthSurface`), the mock's LLM config
  mutation sits at `:440-443`, the Console mount at `:1496-1502`, and fixture
  seeding at `:1599` — exactly the surfaces 159 re-expresses as caller-side
  composition through explicit injection seams on the promoted Options/Handle
  (the promoted constructor itself REQUIRES a non-nil auth-validator factory;
  nil is a loud error — identity is mandatory §6).
- `harbortest/devstack` carries a hand-mirrored transports/mux block
  (`harbortest/devstack/devstack.go` ~877–1310, the `muxOpts` fan-out +
  `transports.NewMux` at `:1300`) — the copy 159 deletes (the D-197 move).

---

## 3. Phases

Decision numbers are **pre-assigned** (D-291 for 159, D-292 for 160, D-293 for
161, D-294 for 162, D-295/D-296 for 163, D-297 for 164) so parallel worktree
agents never collide in `docs/decisions.md`.

| Phase | Title | Decision | Stage | Size |
|-------|-------|----------|-------|------|
| 159 | Serve-band promotion (`internal/runtime/serve`) | D-291 | 1 | L |
| 160 | `sdk/server` facade + `harbor scaffold --with-server` + parity gate | D-292 | 2 | L |
| 161 | Session rehydration carries per-turn metadata | D-293 | 3 | M |
| 162 | `events.list`: durable time-ranged cross-session raw-event read | D-294 | 4 | L |
| 164 | MCP OAuth requirement discovery, surfaced as data | D-297 | 4 | M |
| 163 | Windowed-reads honesty pair (flows since/until + retention horizons) | D-295 + D-296 | 5 | S |

### Stage 1 — the promotion (159)

**159 — Serve-band promotion (internal/runtime, L) — leads the wave (D-291).**

Promote `bootDevStack` / `devBootOptions` / `devStack` (+ serve/close) out of
`cmd/harbor` into `internal/runtime/serve` (naming: `internal/server` is already
the protocol-server package — do NOT collide). The promoted constructor
REQUIRES a non-nil auth-validator factory (nil = loud error; identity is
mandatory §6) and mounts ONLY the surfaces every caller shares — the dev
signer NEVER promotes. Dev-only surfaces are composed CALLER-SIDE by
`cmd/harbor` through explicit injection seams on the promoted Options/Handle
(extra pre-CORS routes, the transports auth-surface option, an LLM snapshot
override, a post-boot hook with subsystem handles); dev-only POLICY stays
cmd-side: mock-LLM escape hatch (D-089), hot-reload supervisor (D-099), dev
signer + dev-token mint + bootstrap-token endpoint, drafts, Console embed
(D-091). `harbor serve`/`dev`/`console` become thin callers. **Second consumer
same-wave (§13):** `harbortest/devstack` re-wired onto the promoted band, its
hand-mirrored transports/mux block (~877–1310) deleted (the D-197 move); the
kit's mux GAINS the options its mirror omitted (`WithAgentsService` /
`WithAuthSurface` / `WithGovernanceService` / `WithGovernanceKeyRotate`) — an
owned behavior change; closing that drift is the point. An honestly-enumerated
NEW options/handle seam surface, but zero wire changes.

Gate: `scripts/smoke/phase-159.sh` (boot-parity `/healthz` + one canonical
method); the D-025 served-handle N≥100 `-race` + goroutine-baseline test; the
integration test proving cmd + devstack thin callers mount the SAME surface set
(the anti-drift assertion). `make preflight` green (all existing serve/dev
smokes still pass — no regression).

**Decision D-291:** external Protocol serving is a decided contract — one
promoted serve constructor (required auth-validator factory; dev surfaces
caller-composed) + a curated `sdk/server` facade, production-only posture;
supersedes the SDK's deliberate Protocol-server omission recorded in D-205
item 2 (amended in place; cites the D-197 / D-204 precedents).

### Stage 2 — the facade + scaffold + parity gate (160) · Dep 159

**160 — `sdk/server` + `harbor scaffold --with-server` + parity gate (sdk +
cmd/harbor scaffold + test/integration, L, D-292).**

Curated `sdk/server` facade — `server.Open(ctx, cfg, Options{RegisterCatalog})`
→ handle with `Serve`/`Close`, alias/forward over the promoted constructor.
Production-only by construction (always builds JWKS from `cfg.Identity`, fails
loud when absent; re-runs `Validate`; no dev-signer, no mock, no injection
seams). The registrar mechanism: a NEW optional
`assemble.Options.RegisterCatalog func(tools.ToolCatalog) error` invoked at the
existing `PreRegisterTools` application point (adapter, not a second
registration path; the post-assembly `Catalog.Register` trap is named).
`harbor scaffold --with-server` (opt-in; default stays headless RunOnce)
generates `cmd/<agent>/main.go`. **Parity gate, scoped per leg:** BOTH binaries
from the SAME base config — (a) manifest-driven method-status parity (from the
Go-side `methods.Methods()` registry in-module; the `wire-manifest.gen.json`
methods key for any script-side probe), (d) dev-only surfaces 404 on both,
(e) §17.3 real drivers + identity propagation + ≥1 failure mode (401) + N≥10
stress + `-race`. SCAFFOLDED BINARY ONLY — (b) generated-tool discovery +
dispatch, (c) approval-gate wrap FIRES (pre-policy proof); the `tools.entries[]`
block naming the generated tool lives in the scaffolded binary's config
OVERLAY — a stock serve booted against it fails loud `ErrToolNotRegistered`
(deliberate fail-closed; assertable as a negative), and a wrap-fires mirror on
both binaries MAY use a builtin tool. **CI/live split (§17.8):** the (b)/(c)
mechanics gate is an in-module `test/integration` scripted-LLM test (the
83l/158 precedent) under `-race`; the wire-level end-to-end against the real
scaffolded subprocess binary is an env-gated `HARBOR_LIVE_*` live leg (the
131d precedent) — it IS Stage 2's live-verification step (§5 item 2). No new
wire types — no D-223/D-209 churn.

§18 same-PR: `scaffold-a-harbor-agent` + `add-an-in-process-tool` +
`configure-production-identity` (+ `use-the-harbor-protocol` checked),
`embed-harbor-headless` recipe companion, docs/site stubs + nav, README
pointer.

Gate: `scripts/smoke/phase-160.sh` — the no-LLM subset (scaffold
`--with-server` → external build with the `replace` directive → token-minted
JWKS boot → `/healthz` → tool-discovery probe → 401-without-token; `OK ≥ 3`;
the dispatch leg is env-gated SKIP by default; SKIP on a build without the
flag) + the in-module parity-gate CI legs under `-race`.

**Decision D-292:** the compiled-tool registrar rides the pre-policy catalog
seam; `harbor scaffold --with-server` is the opt-in consumer
(registrar-before-`entries`-wrapping semantics; the scaffold boundary).

### Stage 3 — session rehydration metadata (161) · added 2026-07-10

**161 — Session rehydration carries per-turn metadata (internal/llm +
internal/runtime/dispatch + internal/tools + web/console, M, D-293).**

The operator's live test found reopening a Playground session loses per-turn
tokens/cost/latency (header shows "no turns yet"), the TOOL CALLS badges, and
the model chip. Root cause verified producer-side (the read path strips
nothing — probed): `llm.cost.recorded` is emitted only inside the bifrost
driver; `tool.*` lifecycle events only by the inproc transport (MCP/HTTP/A2A
tools emit none) with attribution-dead empty envelope run ids. Fix: ONE
driver-neutral emit seam per producer — the cost emit promotes to the
MANDATORY LLM-edge safety wrapper (`llm.Open` wraps every driver; bifrost's
internal emit deleted; one emit per driver-level completion — per attempt
under retry, today's bifrost cadence); the tool lifecycle emits land at the
CATALOG-BUILD DESCRIPTOR-WRAP seam (`catalog.Register` wraps every
descriptor's `Invoke` once), because `desc.Invoke` has FOUR production call
sites (single executor, parallel-executor branches — the default for native
N>1 — the MCP-Apps proxy, and the declarative-action re-invoke) and an
executor-side emit would silently regress three of them; all four inherit
the emit by construction, with the full run quadruple (inproc's per-driver
emits + the orphaned `tools.WithBus` option deleted; also fixes the latent
LIVE attribution bug, §17.6). Payloads stay content-free
(name/status/duration + usage/cost/model figures — never args/results, §7;
sentinel-redaction test). Read path untouched (D-254 posture); works on
inmem within process lifetime. ZERO wire changes — no D-223/D-209 churn
(zero-diff proven). The §13/D-062 consumer ships same-phase:
`reduceHistoryTurns` folds the metadata into a widened `HistoryTurn` and
`hydratePastTurns` populates header stats + badges + model chip —
leave-and-return renders IDENTICAL to the live view.

Gate: `scripts/smoke/phase-161.sh` (scripted mock run via the `start` method
→ `state.history` page carries cost `Usage`/`Model` keys +
`planner.decision` `DecisionKind` + populated envelope `run` + the no-args
negative; `OK ≥ 3`); the integration test (real drivers; the MCP leg against
the real stdio fixture, §17.8; stream-vs-readback key equivalence;
cross-identity refusal) under `-race`; the CallParallel-branch lifecycle pin
(≥2 quadruple-stamped `tool.invoked` per parallel turn) + the direct-invoke
non-executor pin; Console vitest (reducer folding + rehydration regression);
live rehydration verification (see §5).

**Decision D-293:** durable-log read-back carries content-free turn metadata
(usage/cost/latency/model/tool-name+status); session reopen reconstructs what
the live stream showed — one driver-neutral emit seam per producer on the one
bus (safety wrapper; catalog descriptor-wrap); D-026/§7 boundaries preserved;
D-062 surface+consumer same phase.

### Stage 4 — historical events read ∥ MCP OAuth discovery (162 ∥ 164) · added 2026-07-10

Two parallel worktree agents (the phases share no code seam). **BOTH touch
the wire manifest** (162: a new method + request/response types; 164:
additive connection-view types) — the SECOND-merged PR rebases onto main and
re-runs `make protocol-ts-gen` + `make protocol-docs-gen` before its final
push, so the committed manifest + generated reference reflect both surfaces
(named explicitly here because a stale regen is a silent lockstep break the
gates only catch at CI).

**162 — `events.list` (internal/events + internal/protocol + web/console,
L, D-294).** The durable, time-ranged, cross-session raw-event read: the
existing `EventFilter` (already carrying `since`/`until`) + a tail-first
sequence-based cursor mirroring `state.history`'s grammar, rows = the
existing `StateEvent` projection (no new row shape); non-admin reads scope
to the verified triple, fleet widening requires the verified admin claim
derived server-side + one `audit.admin_scope_used` per request (§6 item 5);
`truncated` at the retention edge; the `HistoryReplayer` seam on BOTH
drivers (durable = real windows; inmem = ring + honest truncation, no
capability ceremony); redaction + D-026 by-reference unchanged. D-062
consumer same phase: the Console Events page historical window (existing
`WINDOW_SPEC` picker drives it; live tail unchanged; retention-gap notice).
Operator latitude exercised as NO additions (each candidate adjacency is
answerable from the rows or the aggregate). Gate:
`scripts/smoke/phase-162.sh` (scripted run → rows + cursor paging + 401;
`OK ≥ 3`); the two-identity isolation + widened-read-audit integration legs
under `-race`; full D-223/D-209 regen.

**164 — MCP OAuth requirement discovery (internal/tools/auth +
internal/tools/drivers/mcp + internal/protocol + web/console, M, D-297).**
Detect the MCP auth-spec challenge (401 + `WWW-Authenticate`
`resource_metadata` — net-new capture; nothing handles 401 in the driver
today) or an `mcp.servers.probe`; walk RFC 9728 → `authorization_servers[]`
→ RFC 8414/OIDC metadata REUSING the existing `Provider.resolveEndpoints`
(RFC 7591 registration reported, never invoked); surface verbatim +
provenance as an additive `oauth_requirement` on `MCPServerView`. Hard
boundaries: no flow execution, no token custody (D-271 stays PULL), inert
untrusted data (report, don't follow), SSRF-bounded discovery fetches
(same-origin default, redirect/timeout/size caps, https-only off-loopback,
no credentials — each negative-tested). §17.8: spec-derived fixtures
(wrong-field mutation must fail). D-062 consumer same phase: the MCP
Connections page requirement card. SSRF guardrails are PER HOP (the
RFC 8414 authorization-server hop is inherently cross-origin → requires the
explicit per-connection origin allowance; allowed fetches also refuse
private-range/IP-literal hosts). Sibling reconciliation: the ready 85b and
the parked 92p (reserved D-246) each reuse this single-homed discovery
chain and add only their flow legs (Phase 148 precedent) — one discovery
mechanism, N consumers; 85b's plan gains a pointer note in this PR.
`mcp.servers.probe` triggers discovery (its `MCPProbeRow` return unchanged;
the requirement is read via `get`/`list`). Gate:
`scripts/smoke/phase-164.sh` (unit-tests class: discovery + SSRF
go-test leg + manifest grep; `OK ≥ 2`); the fixture-server integration test
with its recording assertions under `-race`; full D-223/D-209 regen.

### Stage 5 — the honesty pair (163) ∥ reasoning-steps rehydration (165)

**163 — flows `since`/`until` + retention horizons (internal/protocol +
internal/events + web/console, S, D-295 + D-296).** (1) Optional
`since`/`until` on `FlowRunsListRequest` mirroring `TaskFilter` exactly
(additive; bounds on `StartedAt` before pagination; scope rules unchanged);
consumer = the flow detail page's run-history date filter. (2) Retention
horizons as data: an additive `retention` block on `runtime.health` with
the OBSERVED per-surface `oldest_retained_at` (events/tasks/sessions) — the
filed ask's configured-retention premise was verified FALSE (the durable
log is "gap-free and untrimmed in V1", `durable.go:776`; no retention knob
exists anywhere), so the observed horizon is the honest v1 shape; pairs
with the at-read `truncated` flag; consumer = the Events page window-edge
honesty banner composing with 162. The counters/metrics TSDB is
re-recorded as a decided-NO so it is not re-opened. Gate:
`scripts/smoke/phase-163.sh` (`runtime.health` retention block +
flows-bounds acceptance; `OK ≥ 2`); the seeded-runs bounded-window +
horizon-accuracy integration legs under `-race`; full D-223/D-209 regen.

**165 — structured reasoning-steps rehydration (web/console, Console-only,
D-298).** Extends 161's session-reopen rehydration to the STRUCTURED
reasoning steps — the ordered per-ReAct-step native thinking interleaved
with the tool calls each step preceded. **Zero-wire, Console reducer only**
(verified live probe + code trace, 2026-07-11): the live path's reasoning
steps come from the tasks.get enricher projection over the IN-MEMORY
trajectory (`serve/enricher.go:62-73`), evicted on reopen — the enricher
cannot serve a reopened run (the probed run's `tasks.get` returned
`not_found`), so the ONLY durable source is the event stream, which already
carries `planner.decision` (one per trajectory step, ordered by `sequence`,
each with `ReasoningTrace`) + `tool.*`. Byte-equivalence is by construction
(`emitDecision` and `rc.OnReasoning` feed the same `resp.Reasoning`).
`reduceHistoryTurns` folds the trace into `HistoryTurn.reasoningSteps
{index, reasoning_trace}` (index = the per-run decision ordinal matching the
enricher's sparse-index-into-full-sequence); `hydratePastTurns` sets
`message.reasoningSteps`, which the bubble already prefers over the flat
text. No file conflict with 163 (163 = flows/retention Go+wire; 165 = the
Console Playground reducer). Gate: `scripts/smoke/phase-165.sh`
(state.history carries `planner.decision.ReasoningTrace` + `tool.*`
interleaved by `sequence`; `OK ≥ 2`) + the Console vitest (ordered
reconstruction + byte-equivalence vs `parseReasoningSteps` + page-boundary),
plus the rehydration regression; ZERO wire — no D-223/D-209 churn.

---

## 4. Sequencing (§17.7 waves)

**Stage 1 (dispatch now):** 159 — the promotion. Nothing depends on it landing
before it CAN be built (its deps — 64, 110d, 118 — are all shipped). It leads
because 160 hard-deps the promoted package.

**Stage 2 (after Stage 1 merges):** 160 — the facade + scaffold + parity gate.
It imports `internal/runtime/serve`, so it cannot start until 159's package
exists on `main`.

**Stage 3 (after Stage 2 merges; added 2026-07-10):** 161 — the
session-rehydration metadata fix. Its deps (125, 157, 118, 124) are all
shipped, so it is buildable at any point; it runs as Stage 3 so the wave
drains serially through the serve band.

**Stage 4 (after Stage 3 merges; added 2026-07-10):** 162 ∥ 164 — two
PARALLEL worktree agents (no shared code seam: events/protocol vs
tools-auth/mcp). Both touch the wire manifest: the SECOND-merged PR rebases
onto main and re-runs `make protocol-ts-gen` + `make protocol-docs-gen`
before its final push (see the Stage 4 section's explicit note).

**Stage 5 (after Stage 4 drains; LAST):** 163 ∥ 165 — the honesty pair and
the reasoning-steps rehydration, two PARALLEL worktree agents with no file
conflict (163 = flows/retention Go+wire; 165 = the Console Playground
reducer). 163 runs in Stage 5 because its Events-page banner composes with
162's page work; 165 joins here (operator-added 2026-07-11) as a small
Console-only slice touching no wire — no manifest regen, no second-merged
rebase concern.

**Primitive-with-consumer (§13):** 159 ships the promoted constructor WITH its
second consumer (`harbortest/devstack` re-wire) in the same phase — never
"later." 160 ships `sdk/server` WITH its consumer (`harbor scaffold
--with-server`) and the parity gate that exercises both end-to-end. 161 ships
the producer-side event enrichment WITH its D-062 Console consumer (the
reducer + rehydration) and the tests that exercise the metadata end-to-end.
162 ships `events.list` WITH the Console Events historical window; 164 ships
the discovery surfacing WITH the MCP Connections requirement card; 163 ships
both additive fields WITH the flow-detail date filter + the Events
window-edge banner; 165 IS its own consumer (a Console reducer + rendering
change over already-flowing events — the zero-wire case where the consumer
is the whole phase). The RFC §5.6 primitive (external serving as a decided
contract) lands in the serve-parity plans PR with both phases that make it
real scheduled behind it.

**Wave-end:** 160 bundles the parity gate (real drivers across the serve
surface, identity propagation, ≥1 failure mode, N≥10 stress); 161 bundles the
rehydration integration test (real drivers, the MCP stdio fixture leg,
stream-vs-readback equivalence, cross-identity refusal, `-race`); 162/163/164
each bundle their real-driver integration legs (§17.3; 164 additionally the
§17.8 spec-derived fixture discriminator); 165 bundles the Console
rehydration regression + reducer vitest (Console-only — no Go/wire leg). The
phantom
`top_p` MUST-FIX is carried by the wave-end §17.5 checkpoint punch list (the
audit PR's artifact) — NOT by any phase; 161's plan lists it as an explicit
non-goal for the same reason. The §17.5 checkpoint audit runs AFTER
both Stage-5 phases (163 ∥ 165) merge and covers ALL SEVEN phases
(159–165). **Do not scope any subsequent band until the audit merges.**

---

## 5. Gates (per-phase, binding)

Per the operator's mandate for this wave, EACH phase clears:

1. **Two adversarial reviews.** A read-only adversarial pass after
   implementation (hunting the specific failure shapes the plan's Risks section
   names — for 159 the import-cycle / dev-vs-prod-posture line; for 160 the
   pre-policy-seam wiring + the manifest-vs-mux false-green; for 161
   double-emission on the cutover, content leakage past the content-free
   boundary, and window-flooding mistaken for the regression; for 162 the
   admin-fan-in disclosure edge + cursor stability under clock skew; for 164
   the SSRF guardrail table + the report-don't-follow boundary + the 92p
   one-mechanism reconciliation; for 163 the observed-horizon derivation
   cost + the premise-correction fidelity), then a second
   pass after the first round of fixes lands. The implement→adversarial→fix
   loop is the one validated 5× across the 114–118 sequence (it found real
   bugs on 115/117/118 and LIVE Console bugs on 118).
2. **Live verification.** 159: boot the promoted `serve` band under both
   compositions (a `harbor serve` boot with a `harbor token`-minted JWKS, and
   a `harbor dev` boot) and confirm the surface parity + the dev-only-404
   composition split by hand, not just in-test. 160: run the env-gated
   `HARBOR_LIVE_*` leg — scaffold `--with-server` into a real temp module,
   build it externally, boot it with a minted JWKS, and drive the generated
   tool through the wire (the honest external-adopter path; this live leg is
   the wire-level half of the §17.8 CI/live split, not a CI job). 161: LIVE
   REHYDRATION verification against the real test agent (real LLM + MCP
   tools) — run a tool-calling turn in the Playground, leave, reopen, and
   confirm by hand that header stats, per-message badges, TOOL CALLS, and
   the model chip render identical to the live view (the exact regression
   the operator reported). 162: the Console Events page historical window
   against a LIVE runtime — pick a 7-day window, see real rows from before
   the page was opened, scroll up through pages, and see the retention-gap
   notice on the inmem posture. 164: discovery from a REAL
   OAuth-challenging MCP fixture rendered in the Console MCP Connections
   page (challenge → probe → the requirement card shows endpoints/scopes/
   PKCE + source URL). 163: smoke-driven (the health retention block + the
   flows bounds acceptance; the banner is exercised in 162's live pass by
   re-checking after 163 merges).
3. **The standard gate:** `make drift-audit` + `markdownlint-cli2` repo-wide +
   `make check-mirror` (no AGENTS/CLAUDE touch) + `make preflight`. Coverage ≥
   the plan's 85% target on new packages.

**Auto-merge authority:** granted by the operator for this wave and recorded in
the wave's plans PR (#470) description — "operator authorized auto-merge for
this wave, 2026-07-10." The coordinator clears the gates, lands the PR, and
proceeds to the next stage without a manual merge handoff. The §17.5 audit
still gates any subsequent band.

---

## 6. §16 placement ritual + dispatch checklist (§17.7 step 3)

Each dispatched worktree agent operates **only inside its own worktree** (`pwd`
first; STOP if a path resolves outside it; NEVER `git merge main`; NEVER leave
conflict markers). Per §16, each agent: reads the master-plan detail block + the
cited RFC sections (§5.6, §3.6, §5.4, §5.5, §6.1, §6.4, §8 for 159/160;
§6.13, §5.2, §6.4, §6.5, §7 for 161; §6.13, §5.2, §4, §7 for 162; §5.2,
§6.1, §6.13, §6.14, §7 for 163; §6.4, §5.2, §6.15, §7 for 164) + the
informing briefs (01/06/07 for 159; 03/06/07 for 160; 06/07/11 for 161;
06/11 for 162 and 163; 09/14 for 164) + the predecessor plans in `Deps` + the
§16 workflow; fills every template section of its already-authored plan file
(the plans exist — the agent IMPLEMENTS against them, it does not re-author the
design); replaces its `scripts/smoke/phase-NNN.sh` skeleton's `skip` with real
assertions; keeps its pre-assigned `D-NNN` block (D-291…D-297 —
already authored in the plans PRs; the implementation PR updates its
**Status** /**As-built** notes, markdownlint-clean — blank lines around `---`
and `## D-NNN`); updates any §18 skill/recipe/site surface it touches in the
same PR (160's list is enumerated in its plan; for 161 the grep-then-decide
ritual was run: checked `drive-the-playground` (`surface: playground`) — its
token/cost chip + history description becomes TRUER after the fix and no
step goes stale, so no edit is required; no other skill names the reopen
surface; for 162/163/164 the implementor re-runs the ritual — the
`observe-with-the-console` (`surface: console`) and
`use-the-harbor-protocol` (`surface: protocol`) skills are the likely
matches for 162/163's new reads, and any hit is updated in the same PR);
handles the wire correctly per phase (159/160/161: NO wire changes — a
manifest diff is a red flag; 162/163/164: additive wire changes with the
FULL `make protocol-ts-gen` + `make protocol-docs-gen` regen committed, and
the Stage-4 second-merged PR re-runs both after its rebase); and runs
`make drift-audit` + `markdownlint-cli2` + `make preflight` green before
committing.

Each dispatch prompt MUST carry: the master-plan detail block; the mandatory
reading list; the §16 workflow; the validation gate; the **pre-assigned
`D-NNN`**; the **workspace warning**; and the **markdownlint hygiene reminder**.

**Godoc-visible-source discipline (§13/phase-102).** No `Phase NN` / `phase-NN`,
inline `D-NNN`, `brief NN`, or wave-band references in non-test Go source under
`internal/`, `cmd/`, or `sdk/` (the public facade — the most adopter-visible
surface). Name the FEATURE, not the number. This is acute for 159/160 because
the promoted `internal/runtime/serve` + the new `sdk/server` are fresh
godoc-visible packages — the drift-audit godoc gate will fail on a stray
`bootDevStack`-was-Phase-159 comment.

**No new top-level directory.** `internal/runtime/serve` is under the existing
`internal/runtime/` tree; `sdk/server` is under the existing `sdk/` tree; the
scaffold emits `cmd/<agent>/` inside the generated external module. §3's binding
layout is unchanged — no RFC layout PR needed.

---

## 7. Open questions — resolved before dispatch

1. ~~Where does the promoted serve band live?~~ **RESOLVED:**
   `internal/runtime/serve` (distinct from `internal/server`, the protocol
   server it composes). D-291.
2. ~~Does the promoted constructor carry a dev/mock knob?~~ **RESOLVED: no** —
   the constructor REQUIRES a non-nil auth-validator factory (nil = loud
   error) and mounts only shared surfaces; dev-only surfaces are composed
   caller-side by `cmd/harbor` through explicit injection seams; the dev
   signer never promotes. D-291.
3. ~~Is `sdk/server` dev-capable?~~ **RESOLVED: production-only by
   construction** — always builds JWKS from `cfg.Identity`, fails loud when
   absent; the local-dev loop is `harbor token`; no dev-signer, no mock, and
   the injection seams are curated out of the facade. D-292.
4. ~~How does a compiled tool get its policy/approval/OAuth wrapping?~~
   **RESOLVED:** a NEW optional `assemble.Options.RegisterCatalog` callback
   invoked at the existing `PreRegisterTools` application point — an adapter,
   never a second registration path; the post-assembly `Catalog.Register`
   bypass is the named trap. D-292.
5. ~~Does `harbor serve` go through `sdk/server`?~~ **RESOLVED: no** — it calls
   the promoted internal constructor directly with a nil registrar; the internal
   and facade paths are the SAME constructor, parameterized. D-292.
6. ~~Any wire/Protocol changes?~~ **RESOLVED: none** — Go re-homing + new
   Go-side option/handle seams + facade; no methods/types/errors/events, no
   `ProtocolVersion` bump, no D-223/D-209 churn.
7. ~~Can the parity gate's compiled-tool legs run on both binaries in CI?~~
   **RESOLVED: no, by design** — (b)/(c) are scaffolded-binary-only (the tool
   overlay + `ErrToolNotRegistered` fail-closed on stock serve); the CI gate
   for their mechanics is the in-module scripted-LLM test, and the subprocess
   wire end-to-end is the env-gated `HARBOR_LIVE_*` live-verification leg.
   D-292.

---

## 8. Stage-1 (159) as-built notes for the Stage-2 dispatch + audit

- 159 shipped with two adversarial review rounds; all findings fixed in-PR.
  Notables for 160 + the §17.5 audit: the promoted `serve.Options` gained
  `PreferConfigBindAddr` (production-only opt-in — the bind-address
  discriminator regression the reviews caught live; `sdk/server`'s `Open`
  must set it, matching `harbor serve`), and the sdk func-body allow-lists
  in `phase-112a.sh` / `phase-144.sh` now use a single spec-list shape
  (`file|name-regex|name|func-count`) so 160's `sdk/server` entries append
  one line each instead of rewriting the constraints.
- **Carried follow-up for the §17.5 checkpoint audit:** a driver-options
  parity pin for the residual Boot↔devstack composition band — the kit
  composes the promoted building blocks (`serve.BuildMux` +
  `serve.NewRunLoopDriver`) rather than calling `serve.Boot`, so the
  `RunLoopDriverOptions` / `MuxInput` field sets each caller passes remain
  the one hand-maintained mirror. The mounted-surface half is pinned by
  `test/integration/phase159_serve_band_test.go`; the options-field half
  needs a reflective parity pin.
