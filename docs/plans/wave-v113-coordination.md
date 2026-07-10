# Harbor v1.13.0 — The Serve-Parity Wave (opening: phases 159–160) — wave coordination

> Per Harbor §17.7 wave delivery cadence. This is the coordination artifact for
> the v1.13.0 opening wave ("Serve parity"). It sequences the two phases into
> staged worktree dispatches, prescribes the drain-merge order, the per-phase
> gates (two adversarial reviews + live verification each), the wave-end E2E,
> and the §17.5 checkpoint audit that gates any subsequent v1.13 band.
>
> **Mandate:** close the serve-parity gap. `harbor serve` serves the Protocol
> from stock yaml, but a scaffolded agent carrying compiled in-process Go tools
> cannot — the serve composition is trapped in `package main`. Promote it
> (159), give it a curated `sdk/server` facade + an opt-in serving scaffold and
> prove parity (160). Additive public API (`sdk/server`, `harbor scaffold
> --with-server`) + one importable internal package ⇒ additive **minor** bump ⇒
> ships as **v1.13.0**.

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

v1.13.0 opens by closing that gap in two staged phases:

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

Decision numbers are **pre-assigned** (D-291 for 159, D-292 for 160) so the two
worktree agents never collide in `docs/decisions.md`.

| Phase | Title | Decision | Stage | Size |
|-------|-------|----------|-------|------|
| 159 | Serve-band promotion (`internal/runtime/serve`) | D-291 | 1 | L |
| 160 | `sdk/server` facade + `harbor scaffold --with-server` + parity gate | D-292 | 2 | L |

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

---

## 4. Sequencing (§17.7 waves)

**Stage 1 (dispatch now):** 159 — the promotion. Nothing depends on it landing
before it CAN be built (its deps — 64, 110d, 118 — are all shipped). It leads
because 160 hard-deps the promoted package.

**Stage 2 (after Stage 1 merges):** 160 — the facade + scaffold + parity gate.
It imports `internal/runtime/serve`, so it cannot start until 159's package
exists on `main`.

**Primitive-with-consumer (§13):** 159 ships the promoted constructor WITH its
second consumer (`harbortest/devstack` re-wire) in the same phase — never
"later." 160 ships `sdk/server` WITH its consumer (`harbor scaffold
--with-server`) and the parity gate that exercises both end-to-end. The RFC
§5.6 primitive (external serving as a decided contract) lands in THIS plans PR
with both phases that make it real scheduled behind it.

**Wave-end:** 160 bundles the parity gate (which doubles as the wave-end E2E for
this two-phase opening — real drivers across the serve surface, identity
propagation, ≥1 failure mode, N≥10 stress). Then the §17.5 checkpoint audit PR.
**Do not scope any subsequent v1.13 band until the audit merges.**

---

## 5. Gates (per-phase, binding)

Per the operator's mandate for this wave, EACH phase clears:

1. **Two adversarial reviews.** A read-only adversarial pass after
   implementation (hunting the specific failure shapes the plan's Risks section
   names — for 159 the import-cycle / dev-vs-prod-posture line; for 160 the
   pre-policy-seam wiring + the manifest-vs-mux false-green), then a second pass
   after the first round of fixes lands. The implement→adversarial→fix loop is
   the one validated 5× across the 114–118 sequence (it found real bugs on
   115/117/118 and LIVE Console bugs on 118).
2. **Live verification.** 159: boot the promoted `serve` band under both
   compositions (a `harbor serve` boot with a `harbor token`-minted JWKS, and
   a `harbor dev` boot) and confirm the surface parity + the dev-only-404
   composition split by hand, not just in-test. 160: run the env-gated
   `HARBOR_LIVE_*` leg — scaffold `--with-server` into a real temp module,
   build it externally, boot it with a minted JWKS, and drive the generated
   tool through the wire (the honest external-adopter path; this live leg is
   the wire-level half of the §17.8 CI/live split, not a CI job).
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
cited RFC sections (§5.6, §3.6, §5.4, §5.5, §6.1, §6.4, §8) + the informing
briefs (01/06/07 for 159; 03/06/07 for 160) + the predecessor plans in `Deps` +
the §16 workflow; fills every template section of its already-authored plan file
(the plans exist — the agent IMPLEMENTS against them, it does not re-author the
design); replaces its `scripts/smoke/phase-15N.sh` skeleton's `skip` with real
assertions; keeps its pre-assigned `D-NNN` block (D-291 / D-292 — already
authored in this PR; the implementation PR updates its **Status** /**As-built**
notes, markdownlint-clean — blank lines around `---` and `## D-NNN`); updates any
§18 skill/recipe/site surface it touches in the same PR (160 only); regenerates
nothing on the wire (both phases: no wire changes — a manifest diff is a red
flag); and runs `make drift-audit` + `markdownlint-cli2` + `make preflight`
green before committing.

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
