# Harbor v1.14 — Events track (Track B) — coordination note

> Per Harbor §17.7 wave delivery cadence. This is the coordination artifact for the
> **events track** of the v1.14 wave — three downstream-team asks against
> `events.aggregate`, all ruled DNA-aligned by the coordinator. It is **Track B of a
> two-track wave**: independent of the MCP/OAuth track (phases 166–170) planned in
> parallel — different subsystem, no shared files, no shared wire types. The two tracks
> merge and release together as **v1.14.0**.

---

## 1. What this track delivers

| Phase | Title | Decision | Size | Wire? |
|-------|-------|----------|------|-------|
| 171 | `events.aggregate` durable-driver parity + conformance-matrix closure (HA-18 + HA-20) | D-305 | L | none (zero-wire) |
| 172 | `events.aggregate` origin-anchored (epoch-aligned) bucket grid (HA-16) | D-306 | S | additive field |
| 173 | `events.aggregate` per-tenant attribution for admin-widened reads (HA-17) | D-307 | S | additive fields |

**The track's thesis (driver parity, §9/§11):** a Protocol method must work on EVERY
registered driver. A driver difference may change WHAT a method returns (retention depth
via `truncated`, an observed horizon) — it must NEVER change WHETHER the method works.
Legitimate differences are encoded as DATA / named sentinels (`ErrReplayUnavailable`,
`truncated`), never normalized away and never surfaced as an HTTP 500. This thesis is
what HA-18 violated (aggregate 500s on durable, works on inmem) and what HA-20 makes
mechanically impossible to reintroduce.

- **171 (HA-18 + HA-20)** fixes the instance AND closes the class in one phase — the
  D-283 pattern. HA-18: `events.aggregate` 500s on the durable driver every call because
  the aggregator replays through the per-session `Replayer.Replay` path with a
  session-less `Filter{Admin:true}` the durable driver correctly refuses; the fix moves
  the aggregator onto the same `HistoryReplayer` cross-session windowed fan-in
  `events.list` already uses, threading the handler's server-derived `widened` decision
  and preserving exactly one `audit.admin_scope_used` per widened request. HA-20: the
  events-driver conformance matrix gains a driver-parametrized `events.aggregate` leg
  (four event-read methods run against every registered driver → same answer or same
  named sentinel) plus a registry gate (a self-registered driver with no conformance run
  fails the build). **Zero-wire** — a manifest diff is a red flag.
- **172 (HA-16)** adds an optional origin/epoch anchor so aggregate buckets fall on a
  fixed, addressable grid; absent ⇒ today's clock-anchored behaviour. Additive wire field.
- **173 (HA-17)** adds opt-in per-tenant attribution to admin-widened aggregates so the
  tenant boundary is independently verifiable on aggregates the way it already is on rows.
  Additive wire fields.

---

## 2. Sequencing (§17.7)

**171 gates 172 and 173.** The aggregate method must work on the durable driver — and
carry the server-derived `widened` decision — before its bucket grid (172) or its tenant
attribution (173) matter. Concretely:

- **Stage 1 (dispatch first):** 171 — the durable-parity fix + the conformance-matrix
  closure. Its deps (72a, 162, 124, 125) are all shipped, so it is buildable now.
- **Stage 2 (after 171 merges):** 172 ∥ 173 — two parallel worktree agents. They both
  touch `internal/events/aggregate.go` and `internal/protocol/types/events.go` (172 adds
  `Anchor`; 173 adds `ByTenant` + `CountsByTenant`), so **the second-merged PR rebases
  onto main and re-runs `make protocol-ts-gen` + `make protocol-docs-gen` before its
  final push** — the committed manifest + generated reference must reflect both fields
  (a stale regen is a silent lockstep break the gates only catch at CI). Both extend the
  same `test/integration/events_aggregate_durable_test.go` — the second-merged agent
  rebases that file too.

Because 172 and 173 share two files, an equally valid option is to run them
**sequentially** (172 then 173) to avoid the rebase dance. The coordinator decides at
Stage 2 based on how the parallel MCP track is draining. Confirm the staging with the
operator before dispatching (§17.7 step 2).

---

## 3. Wire discipline

- **171: NO wire change.** No new method, type, field, error, or event. The aggregator's
  `widened` input is a Go parameter (or an internal, non-wire query field) — NEVER a wire
  field (D-299: authority is never read from the request body). A diff under
  `internal/protocol/types|methods` or `web/console/src/lib/protocol` is a red flag; the
  smoke's static guard asserts it.
- **172 / 173: additive wire fields**, `ProtocolVersion` stays 0.1.0. Full D-223 lockstep
  each: register the field in the `internal/protocol/types` single source, hand-mirror
  into the Console per-page wire module, `make protocol-ts-gen` (regenerate
  `wire-manifest.gen.json`, never hand-edited), `make protocol-docs-gen` (regenerate
  `docs/site/protocol/types.md`, D-209). The three lockstep-check gates
  (`protocol-ts-gen-check` git-diff + Go lockstep test + TS-source scan) and
  `protocol-docs-gen-check` must pass. **§18:** both 172 and 173 update
  `docs/skills/use-the-harbor-protocol/SKILL.md` (the aggregate request/response wire
  shape changed). There is NO `surface: events` skill — do not cite a phantom.

---

## 4. Gates (per phase, binding)

1. **Two adversarial reviews** — a read-only pass hunting the specific failure shapes each
   plan's Risks section names (for 171: the audit-amplification trap on the widened path,
   the aggregation-cap silent-undercount, and the registry-gate harness wiring the durable
   StateStore for real; for 172: the grid-edge-vs-`now` semantics and absent-anchor golden
   equivalence; for 173: the entitled-set guard + the `Σ by-tenant == total` reconciliation
   as the verifiability property), then a second pass after the first fix round.
2. **Live verification** — 171: boot with the durable events driver and confirm by hand
   that `events.aggregate` returns (not 500s) for a session-less admin request, and that a
   non-admin own-session aggregate emits NO `audit.admin_scope_used`. 172: two anchored
   aggregate calls a short interval apart share a `bucket_start` coordinate. 173: a
   widened aggregate with `by_tenant: true` returns reconciling per-tenant counts; a
   non-elevated caller is refused before attribution.
3. **The standard gate:** `make drift-audit` + `markdownlint-cli2` repo-wide +
   `make check-mirror` (no AGENTS/CLAUDE touch) + `make preflight`. Coverage ≥ 85% on
   touched packages.

---

## 5. §17.8 real-driver / real-StateStore rule

The durable-driver conformance and integration legs run against a **real StateStore at the
seam** (a real inmem `state` store, exactly as `internal/events/drivers/durable/conformance_test.go`
already wires — no mock at the boundary). The registry-parity gate constructs each
registered driver's harness in one test package so durable gets its real StateStore; a
generic `OpenDriver`-only sweep cannot inject it.

---

## 6. Placement ritual + dispatch checklist (§17.7 step 3)

Each dispatched worktree agent operates ONLY inside its own worktree (`pwd` first; STOP if
a path resolves outside it; NEVER `git merge main`; NEVER leave conflict markers). Per §16
each agent reads its master-plan detail block, the cited RFC sections (§6.13, §5.2, §6.5,
§4, §7), the informing briefs (06/07 for 171; 06/11 for 172 and 173), the predecessor
plans in `Deps` (notably 162/D-294 and 72a), and the §16 workflow; fills every template
section of its already-authored plan file (the plans exist — the agent IMPLEMENTS against
them); replaces its `scripts/smoke/phase-17N.sh` skeleton's `skip` with real assertions;
keeps its pre-assigned `D-NNN` (D-305/306/307), updating its **Status**/**As-built** notes
markdownlint-clean (blank lines around `---` and `## D-NNN`); handles the wire correctly
(171 zero-wire — a manifest diff is a red flag; 172/173 additive with the FULL regen
committed, second-merged rebases); and runs `make drift-audit` + `markdownlint-cli2` +
`make preflight` green before committing.

**Godoc hygiene (§13/phase-102):** no `Phase NN`, inline `D-NNN`, `brief NN`, or wave-band
references in non-test Go under `internal/`/`cmd/`/`sdk/`. Name the FEATURE, not the number
(the aggregator's new `widened` parameter and the anchor/attribution fields get
feature-named godoc, not "Phase 171 added this").

**No new top-level directory.** All work is under existing trees (`internal/events`,
`internal/protocol`, `web/console`, `test/integration`). §3's binding layout is unchanged.

---

## 7. Wave-end

- 171 bundles `test/integration/events_aggregate_durable_test.go` (real drivers + real
  StateStore, identity propagation, ≥1 failure mode, `-race`); 172 and 173 each extend it.
- The §17.5 checkpoint audit for the v1.14 wave runs AFTER BOTH tracks (events 171–173 and
  MCP/OAuth 166–170) merge, and covers all of them. **Do not scope any subsequent band
  until the audit merges.**
- Decision numbers are pre-assigned (D-305 for 171, D-306 for 172, D-307 for 173) so the
  parallel MCP/OAuth track (which takes the D-300..D-304 band) never collides in
  `docs/decisions.md`.

---

## 8. Open questions — resolved before dispatch

1. ~~Does the HA-18 fix change `Replayer.Replay`?~~ **No** — Replay stays the per-session
   SSE-reconnect path; the aggregator moves onto the `HistoryReplayer` windowed fan-in
   (the same substrate `events.list` uses). Making Replay fan in session-less would add a
   path with no live consumer (§13). D-305.
2. ~~How is the widened aggregate's single audit preserved?~~ The aggregator issues ONE
   windowed fan-in read (one `audit.admin_scope_used`) with a generous aggregation cap;
   a window past the cap fails loud (`ErrAggregateWindowTooLarge`), never a silent
   undercount. Paging-to-completion (N audits) is the rejected alternative. D-305.
3. ~~Anchor field vs `{since,until,bucket}` for HA-16?~~ **Anchor field** — smallest
   additive surface, composes with the existing `Window`/`Bucket`, does not duplicate the
   `Filter.Since/Until` clamp. D-306.
4. ~~Attribution per-bucket or per-response for HA-17?~~ **Per-bucket** (`EventBucket.CountsByTenant`)
   so it composes with the time series and 172's grid; a response rollup is derivable by
   summing buckets. D-307.
5. ~~The high-numbered RFC section the asks named?~~ **Does not exist.** The downstream
   asks pointed at an RFC section number that RFC-001 does not have (the RFC tops out in
   the §6.x/§7 range). The real anchors are §6.13 (the event bus), §5.2 (the Protocol read
   surface), §6.5/§4 (identity + elevated scope), §7 (the Console lens). No RFC PR is
   required — this track is additive over existing surfaces.
