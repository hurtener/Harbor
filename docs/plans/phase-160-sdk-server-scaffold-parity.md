# Phase 160 — `sdk/server` facade + `harbor scaffold --with-server` + the parity gate

## Summary

Phase 159 promoted the serve band into `internal/runtime/serve`. This phase
gives it an EXTERNAL consumer: a curated `sdk/server` facade
(`server.Open(ctx, cfg, server.Options{RegisterCatalog})` → a handle with
`Serve`/`Close`) so a scaffolded agent with compiled in-process Go tools can
serve the Protocol at parity with the stock `harbor serve`. The facade is
production-only by construction (always builds the JWKS validator from
`cfg.Identity`, fails loud when absent — no dev-signer, no mock). A new opt-in
`harbor scaffold --with-server` generates a `cmd/<agent>/main.go` that loads
yaml, blank-imports `sdk/drivers/prod`, passes the agent's `RegisterTools` to
`server.Open`, and serves. The wave's acceptance centerpiece is the **parity
gate**: both binaries boot from the SAME base config and prove method-status
parity, dev-surface 404s, and identity/401 behavior; the compiled-tool legs
(discovery + dispatch, approval-wrap-fires) run against the scaffolded binary,
whose config overlay declares the generated tool — with the CI-runnable
mechanics proven in-module via the scripted-LLM pattern and the wire-level
subprocess end-to-end as an env-gated live leg. No new Protocol wire types —
no D-223/D-209 churn.

## RFC anchor

- RFC §3.6
- RFC §5.6
- RFC §5.5
- RFC §6.4
- RFC §8

## Briefs informing this phase

- brief 03
- brief 06
- brief 07

## Brief findings incorporated

- brief 07 §1/§8 (the single-dispatch architecture): tool calling lives at the
  runtime layer — the dispatcher and catalog are runtime-owned, one
  registration/dispatch protocol, collapsing a mode matrix "into a single
  dimension Harbor controls." The compiled-tool registrar therefore rides the
  ONE existing pre-policy registration seam (the `PreRegisterTools` application
  point, before builtin registration and the catalog Builder's `tools.entries`
  wrapping), never a second registration path — a post-assembly
  `Catalog.Register` skips the reliability/approval/OAuth shell and is the
  documented trap (D-292).
- brief 03 §5 ("Two parallel LLM modes (the toggle smell)"): "two modes
  shipping in parallel … is an anti-pattern — Harbor picks one architecture and
  bakes the correction in." A second registration path that bypasses the
  Builder's wrapping is exactly that toggle smell; `RegisterCatalog` is an
  adapter over the one seam, so a compiled tool is wrapped identically to an
  operator's YAML tool.
- brief 03 §7 H-2 ("Tool-side approval gates … using the same pause/resume
  primitive"): the approval-gate wrap is the declared-policy shell riding the
  unified pause/resume primitive — the gate this phase's tests must observe
  FIRING on the generated tool (§17.8: a check that only sees "the tool is
  registered" is a rubber stamp that cannot tell pre-policy from post-policy
  registration).
- D-197's recorded lesson ("when the only assembly prior art is
  `package main` + a `*testing.T` fixture, every embedder re-implements
  boot") — distilled
  from brief 06's devx findings — read forward: Phase 159 made the serve band
  reachable; this phase makes the reachability HONEST for an external module
  by shipping the scaffold that emits the `sdk/server` call and a standing
  smoke that scaffolds → builds → boots it as an external module (the D-206
  external-compile-gate pattern extended to serving).

## Findings I'm departing from (if any)

None.

## Goals

- A curated `sdk/server` facade over `internal/runtime/serve` (D-204 alias
  pattern — a thin forward + the one `Options` adapter, NOT raw re-exports of
  protocol internals). Core API (settled): `server.Open(ctx, cfg,
  server.Options{RegisterCatalog func(tools.ToolCatalog) error}) (*Handle,
  error)` with `Handle.Serve(ctx) error` + `Handle.Close(ctx)`.
- Production-only posture, non-negotiable: `Open` ALWAYS builds the JWKS
  validator from `cfg.Identity` and fails loud (naming the missing field) when
  absent. No dev-signer option, no mock knob, and none of the Phase 159
  caller-side injection seams (routes / auth-surface / LLM override /
  post-boot hook) are exposed on the facade. The local-dev loop is documented
  via `harbor token keygen` → `identity.jwks_file` → `harbor token mint` (§8).
- Config discipline: `Open` accepts a validated `*config.Config`; a
  load-from-path convenience exists; `Open` re-runs `Validate` on
  programmatically-built configs so there is no validation bypass.
- The registrar mechanism, named: a NEW optional
  `assemble.Options.RegisterCatalog func(tools.ToolCatalog) error` field,
  invoked at the existing `PreRegisterTools` application point (the catalog
  band in `internal/runtime/assemble/assemble.go`, today `:703`) — the same
  seam, the same callback shape, BEFORE builtin registration and the
  `tools.entries` Builder wrapping. `sdk/server`'s `Options.RegisterCatalog`
  forwards to it, so compiled tools receive declared approval / OAuth / policy
  wrapping. An adapter over the existing seam, never a second registration
  path (§13); the post-assembly `Catalog.Register` trap is named explicitly.
- `harbor scaffold --with-server` (opt-in; default scaffold stays headless
  RunOnce): generates `cmd/<agent>/main.go` that loads yaml (`--config` /
  `--bind` flag trio mirroring `harbor serve`), blank-imports
  `sdk/drivers/prod`, passes `agent.RegisterTools` to `server.Open`, and
  serves. `harbor serve` itself calls the same promoted constructor with a nil
  registrar (via the internal package directly, not via the sdk facade).
- The parity gate proves `harbor serve` and a scaffolded `--with-server`
  binary reach parity from the same base config, with the compiled-tool legs
  scoped to the scaffolded binary (its config overlay declares the tool).

## Non-goals

- No new Protocol wire types, methods, error codes, or event types — no D-223
  lockstep churn, no D-209 docs regen. (Stated explicitly so a reviewer does
  not look for a manifest diff.)
- No dev-signer / mock / dev-token path on `sdk/server` — production posture
  only. The local-dev loop against `sdk/server` is the `harbor token keygen` →
  `identity.jwks_file` → `harbor token mint` three-command loop (§8) — the
  same loop a self-hosted `harbor serve` operator uses.
- No Console embedding in the scaffolded `--with-server` binary (D-091: the
  Console is served only by `harbor console`). The scaffolded binary is a
  headless Protocol server.
- No change to the default (headless RunOnce) scaffold output — `--with-server`
  is purely additive; a tools-declaring agent without the flag still scaffolds
  and compiles exactly as today (D-206).
- No new top-level directory — `sdk/server` is a package under the existing
  `sdk/` tree; the scaffold emits `cmd/<agent>/` inside the generated module.

## Acceptance criteria

- [x] `sdk/server` package exists: `Open`, `Options{RegisterCatalog}`, the
  handle type + `Serve`/`Close`, and a load-from-path convenience (an `Options`
  `ConfigPath` field — not a separate `OpenFromConfigFile`). It is alias/forward
  over the internal serving band (`internal/runtime/serve/external`, which
  facades `internal/runtime/serve`) plus the ONE `Options`→internal-options
  adapter func `Open` (the D-205 single-carve-out posture; the facade's
  no-behavior guard allow-lists it by name in `phase-112a.sh`/`phase-144.sh`).
- [x] Production posture pinned: `Open` with a `cfg.Identity` missing its JWKS
  source fails loud with a named-field error (`TestOpen_MissingJWKS_FailsLoudNamingField`
  asserts the error names the `identity`/`jwks` field); there is NO code path in
  `sdk/server` that mounts a dev-signer, a bootstrap-token endpoint, or any
  Phase 159 injection seam (grep-gated in `TestFacade_NoDevSurfaces_NoInjectionSeams`,
  mirroring D-205's mock-exclusion assertion).
- [x] Validation is not bypassable: `Open` re-runs `config.Validate` (the full
  serve profile, via the internal band's `Boot`) on the passed config; an
  invalid programmatic config fails loud at `Open`, not at first request
  (`TestOpen_InvalidConfig_FailsLoudAtOpen`).
- [x] The assembly gains the optional `assemble.Options.RegisterCatalog`
  callback at the `PreRegisterTools` application point, and it fires
  pre-policy: a tool registered via `RegisterCatalog` and declared (in yaml)
  behind an approval gate wires the approval gate — `TestAssemble_RegisterCatalog_PrePolicy_ApprovalWrapFires`
  asserts the wrap OBJECT (`stack.Gates[name]`) fires (proving registration
  landed before the Builder's `tools.entries` wrapping). A companion negative
  test (`TestAssemble_PostAssemblyRegister_SkipsTheWrap`) shows a post-assembly
  `Catalog.Register` does NOT get the wrap (the documented trap), plus a
  registrar-error fail-loud test.
- [x] `harbor scaffold --with-server` generates `cmd/<agent>/main.go` (loads
  yaml via `--config`, binds via `--bind`/`--port`, blank-imports
  `sdk/drivers/prod`, calls `server.Open` with `agent.RegisterTools`, serves,
  handles SIGINT/SIGTERM → `Close`). The default (flagless) scaffold is
  unchanged (headless RunOnce) — pinned by
  `TestScaffold_DefaultOutput_HasNoServerSurface`. The scaffolded module
  compiles as an EXTERNAL module against the `sdk/` facade only (the
  replace-directive build in `phase-160.sh`; `cmd_main.go.tmpl` imports no
  `internal/`).
- [x] `harbor serve` calls `internal/runtime/serve` directly with a nil
  registrar (NOT through `sdk/server`) — unchanged from Phase 159; the internal
  path and the facade path are the SAME constructor, parameterized by the
  registrar.
- [x] Parity gate — scoped per leg:
  - **Both binaries, same base config** — (a) **manifest-driven method-status
    parity**: every canonical Protocol method (driven in-module from the
    Go-side `methods.Methods()` registry — NOT mux introspection; a
    script-side probe reads the `wire-manifest.gen.json` methods key) answers
    with the same status class on both; (d) negative: dev-only surfaces
    (bootstrap-token endpoint, dev-token mint) 404 on BOTH; (e) §17.3: real
    drivers, identity propagation asserted end-to-end, ≥1 failure mode (a
    bad/absent token rejected 401), N≥10 concurrency stress, `-race`.
  - **Scaffolded binary only** — (b) discovery + dispatch of the generated
    custom tool through the catalog; (c) a `tools.entries` approval-gate wrap
    of the generated custom tool FIRES (proves pre-policy registration,
    D-292). The `tools.entries[]` block naming the generated tool lives in the
    scaffolded binary's config OVERLAY: a stock `harbor serve` booted against
    that overlay has no compiled registrar and fails loud with
    `ErrToolNotRegistered` — the deliberate fail-closed behavior (a
    declared-but-unregistered tool is a misconfiguration, never a silent
    no-op), which MAY be asserted as a negative leg. A wrap-fires assertion
    MAY additionally be mirrored on both binaries using a builtin tool
    (present in both) to prove the wrapping band itself is identical.
  - **CI/live split (§17.8):** the CI-runnable gate for the (b)/(c) mechanics
    is an in-module `test/integration` test driving the promoted serve band +
    the registrar seam with the scripted-LLM pattern (the 83l / Phase 158
    precedent) under `-race`; the wire-level end-to-end against the real
    scaffolded subprocess binary is an env-gated live leg (`HARBOR_LIVE_*`,
    the Phase 131d precedent) run as the wave's live-verification step.
  - **As-built scoping (honest).** `test/integration/phase160_serve_parity_test.go`
    boots the stock composition via `serve.Boot` (the serve subcommand's exact
    posture: the shared production JWKS factory + `PreferConfigBindAddr`, nil
    registrar) and the scaffolded composition via `external.Open` (the path an
    `sdk/server` binary ships) from one base config, both on REAL listeners
    with a real RSA JWKS file + minted RS256 tokens. Legs (a) (status-CLASS
    comparison per method from `methods.Methods()`), (d), and (e) (incl. the
    N≥16 stress with a goroutine-baseline eventually-poll) pass on both. Leg
    (b) dispatch runs IN CI with the scripted-LLM pattern
    (`TestE2E_Phase160_ScriptedLLM_DispatchAndApprovalGate/dispatch`): a
    canned tool-call drives the compiled tool through the served catalog over
    the wire (`control.start` → `tasks.get`), the terminal envelope carries
    `tool_calls_seen ≥ 1`, and the handler's fixture marker round-trips into
    the follow-up LLM prompt. Leg (c) is observed BEHAVIORALLY
    (`…/approval_gate_fires`): the deny-all entries wrap on the compiled tool
    FIRES on dispatch — `tool.approval_requested` with a Coordinator pause
    token — proving the SERVED descriptor is the wrapped one; the structural
    assembly-seam `stack.Gates` pin and the
    `errors.Is(err, catalog.ErrToolNotRegistered)` fail-closed negative
    (naming the tool) complete the leg. The script-side manifest probe lives
    in `phase-160.sh`: stock `harbor serve` and the scaffolded binary boot
    from the same probe yaml and every `wire-manifest.gen.json` method is
    status-class-compared across both. The env-gated `HARBOR_LIVE_SERVE` live
    leg covers the REAL-LLM wire path end to end (build CLI → keygen →
    scaffold → implement the generated stub → external build → subprocess
    boot → mint → dispatch → fixture-answer + `tool_calls_seen` assertions) —
    the §17.8 CI/live split with CI carrying deterministic dispatch and live
    carrying the real provider.
- [x] §18 same-PR skill + docs updates (below) land in this PR.
- [x] `scripts/smoke/phase-160.sh` OK ≥ 3, FAIL = 0 (as built: OK 6, SKIP 1
  (the env-gated dispatch leg), FAIL 0).

## Files added or changed

- `sdk/server/server.go` (new) — `Open`, `Options`, handle + `Serve`/`Close`,
  the config-path convenience, the one `Options`→`serve.Options` adapter. Godoc
  names the feature (no phase/D jargon).
- `sdk/server/doc.go` (new) — the facade contract statement (production-only
  posture; the `harbor token` local-dev loop; the pre-policy registrar seam).
- `sdk/doc.go` — add `sdk/server` to the inventory statement.
- `internal/runtime/assemble/assemble.go` — the NEW optional
  `Options.RegisterCatalog func(tools.ToolCatalog) error` field, invoked at
  the existing `PreRegisterTools` application point (the catalog band) — an
  assemble surface change, declared here so reviewers see it.
- `internal/runtime/assemble/assemble_test.go` — the pre-policy wrap-fires
  test + the post-assembly-register negative.
- `cmd/harbor/cmd_scaffold.go` (+ templates under the scaffold template home) —
  the `--with-server` flag + the `cmd/<agent>/main.go.tmpl` template. Default
  output unchanged.
- `cmd/harbor/cmd_scaffold_test.go` — golden-output test for `--with-server`
  (module compiles externally; `main.go` calls `server.Open` +
  `RegisterTools`).
- `test/integration/phase160_serve_parity_test.go` (new) — the parity gate
  (CI legs) + the env-gated `HARBOR_LIVE_*` subprocess leg.
- `scripts/smoke/phase-112a.sh` + `scripts/smoke/phase-144.sh` — the sdk
  func-body allow-lists gain the `sdk/server` adapter entry (the enumerated
  allow-list per D-273's amendment of D-205 item 1; entry shape prepared by
  Phase 159).
- `scripts/smoke/phase-160.sh` (new) — the no-LLM external-module leg (see
  Smoke script additions).
- `docs/skills/scaffold-a-harbor-agent/SKILL.md` — a `--with-server` section
  (the opt-in serving scaffold; the `harbor token` local-dev loop).
- `docs/skills/add-an-in-process-tool/SKILL.md` — a note that a compiled tool
  served via `sdk/server` gets its declared policy/approval/OAuth wrapping
  because `RegisterCatalog` rides the pre-policy seam (the D-292 contract, in
  operator language).
- `docs/skills/use-the-harbor-protocol/SKILL.md` — check + update if it claims
  serving is stock-binary-only (add the `sdk/server` external-serving path).
- `docs/skills/configure-production-identity/SKILL.md` — checked + updated:
  the scaffolded `--with-server` binary is a new consumer of the
  production-identity setup (JWKS posture + `harbor token` loop); add the
  external-binary attach path where the skill enumerates serve consumers.
- `docs/recipes/embed-harbor-headless.md` — a companion "serve the Protocol
  from your binary" section (its headless-DEFAULT story remains true; serving
  is the additive opt-in). Retitle only if needed to signal "headless is the
  default, serving is opt-in".
- `docs/site/skills/…` include stubs + `docs/site/recipes/…` stub +
  `docs/site/.vitepress/config.ts` nav (Phase 103 rule — mirror any new
  page/section that gets its own stub).
- `README.md` — one-line pointer in the adopter-paths section (the serving
  scaffold path now works end-to-end).
- `docs/plans/README.md` — Phase 160 row + detail block.
- `docs/decisions.md` — D-292.
- `docs/glossary.md` — "with-server scaffold", "sdk/server facade".
- `RFC-001-Harbor.md` §3.6 / §5.6 / §8 (amended in the plans PR).

## Public API surface

- `sdk/server`:
  - `func Open(ctx context.Context, cfg *config.Config, opts Options) (*Handle, error)`
  - `type Options struct { RegisterCatalog func(tools.ToolCatalog) error; /* + the load-from-path convenience field or a sibling OpenFromConfigFile */ }`
  - `func (*Handle) Serve(ctx context.Context) error`
  - `func (*Handle) Close(ctx context.Context) error`
- `internal/runtime/assemble`: the additive optional
  `Options.RegisterCatalog func(tools.ToolCatalog) error` field (the pre-policy
  registrar seam other embedders may also use via `sdk/assemble`).
- The scaffolded `cmd/<agent>/main.go` is generated code, not a stable API;
  its shape (the `--config`/`--bind` trio + `server.Open(cfg,
  Options{RegisterCatalog: agent.RegisterTools})`) is the contract the parity
  gate pins.
- Nothing new on the wire — no Protocol methods, types, errors, or events.

## Test plan

- **Unit:** `sdk/server` facade-integrity (every re-export resolves; the one
  adapter func is the only `func` body; no dev-signer path and no injection
  seams — grep-gated); `Open` production-posture fail-loud (missing
  `cfg.Identity` JWKS → named-field error); `Open` re-validates the config
  (invalid programmatic config → loud at `Open`); the
  `assemble.Options.RegisterCatalog` pre-policy assertion (approval wrap
  fires) + the post-assembly-`Register` negative (no wrap); scaffold
  golden-output test for `--with-server`.
- **Integration (the CI legs of the parity gate,
  `test/integration/phase160_serve_parity_test.go`):** both compositions from
  one base config — manifest-driven method-status parity (from
  `methods.Methods()`), dev-surfaces-404 on both, identity propagation, a 401
  failure mode, N≥10 concurrency stress, `-race`; plus the compiled-tool
  mechanics against the registrar-bearing composition with the scripted-LLM
  pattern (the 83l / Phase 158 precedent): generated-tool discovery + dispatch
  through the catalog and the approval-wrap-fires observation, with the
  `ErrToolNotRegistered` negative for a registrar-less boot against the tool
  overlay. Uses the in-test ES256/JWKS `harbor token`-style issuer (the D-264
  fixture pattern) so the production JWKS posture is real, not mocked. Real
  drivers on every seam.
- **Live leg (env-gated, §17.8):** the wire-level end-to-end against the REAL
  scaffolded subprocess binary — scaffold → external build → boot with a
  minted JWKS → dispatch the generated tool over the wire — gated behind
  `HARBOR_LIVE_*` (the Phase 131d precedent) so CI skips it; it runs as the
  wave's Stage 2 live-verification step.
- **Conformance:** N/A — no persistence-driver seam changes; the served stack
  composes existing drivers.
- **Concurrency / leak:** the served `Handle` from `sdk/server` inherits the
  Phase 159 D-025 guarantees; this phase adds the parity gate's N≥10 stress and
  asserts the scaffolded binary's goroutine baseline restores after `Close`
  (the external-module teardown proof).

## Smoke script additions

- live-server / build — the no-LLM subset (a smoke cannot spend an LLM turn):
  `harbor scaffold --with-server` into a temp dir as an external module (with
  the `replace github.com/hurtener/Harbor => ${ROOT}` directive appended to
  the generated `go.mod` so it builds against the local checkout — the
  `phase-112b.sh:216` precedent) → `go build` → boot with a
  `harbor token`-minted JWKS posture → `/healthz` 200 → a tool DISCOVERY probe
  (the generated tool is present in the catalog listing) → a request without a
  token is rejected 401. `OK ≥ 3, FAIL = 0` achievable without an LLM turn.
- The tool DISPATCH leg (an LLM-driven invocation through the executor) is
  env-gated: SKIP by default, runs under the `HARBOR_LIVE_*` live leg.
- Env vars (new, §4.2 item 7): `HARBOR_LIVE_SERVE=1` gates the live leg
  (`TestE2E_Live_Phase160_ScaffoldedServer_Dispatch`; requires a real
  `OPENROUTER_API_KEY` in the environment). The generated `--with-server`
  `main.go` honors `HARBOR_BIND` as the flagless `--bind` equivalent
  (mirroring `harbor serve`; documented in the generated README).
- Degradation path: SKIP on a build whose `harbor scaffold` lacks
  `--with-server` (the 404/405/501-style SKIP convention read for a CLI flag —
  an unknown-flag error → SKIP).

## Coverage target

- `sdk/server`: 85%
- `cmd/harbor` (scaffold path): existing CLI target maintained (70% floor;
  the `--with-server` template + flag covered by the golden test).
- `internal/runtime/assemble`: existing package target maintained (the new
  `RegisterCatalog` band covered by the pre-policy + negative tests).

## Dependencies

- 159 (the promoted `internal/runtime/serve` this facades — hard dep),
  112a/112b (D-205/D-206 the `sdk/` facade tree + external-compile-gate pattern
  this extends), 133 (D-267 scaffold-with-tools execution gate — the
  register-and-dispatch discipline the parity gate builds on), 131d (D-264
  `harbor token` — the production JWKS posture + the local-dev loop the facade
  documents), 118 (D-223 lockstep — must stay green; no wire changes).

## Risks / open questions

- **The pre-policy seam is load-bearing and easy to get wrong.** If
  `RegisterCatalog` is wired to a post-assembly `Catalog.Register` instead of
  the `PreRegisterTools` application point, compiled tools silently lose their
  approval/OAuth/policy wrapping — a security-adjacent regression that unit
  tests on the happy path would miss. The approval-wrap-FIRES assertion is the
  guard; it must observe the wrap firing, not merely that the tool is
  registered (§17.8).
- **Manifest-driven parity, not mux introspection.** The method-parity probe
  MUST be driven from the canonical manifest artifacts — the Go-side
  `methods.Methods()` registry for the in-module probe, the
  `wire-manifest.gen.json` methods key for any script-side probe — so a method
  that exists in one binary's mux but not the other is caught; introspecting
  one mux and comparing to itself is a false-green.
- **The compiled-tool legs cannot run on the stock binary.** The generated
  tool only exists where the compiled registrar runs; a stock `harbor serve`
  booted against the tool-declaring overlay fails `ErrToolNotRegistered` by
  design (fail-closed). The gate encodes this as scoping (legs (b)/(c) on the
  scaffolded binary) + an optional negative assertion — a reviewer expecting
  (b)/(c) on both binaries should read D-292 decision 3.
- **Production-only posture vs. adopter first-run friction.** An external
  developer's very first `sdk/server` boot fails loud without a JWKS — by
  design (§13 no-stub-default). The mitigation is documentation: the scaffold's
  `--with-server` output and the skill both lead with the `harbor token keygen`
  → `jwks_file` → `mint` three-command loop. The risk is a confused first-run;
  the answer is NOT a dev-signer knob (that would reopen the D-089/D-220
  posture the whole design avoids).
- **`sdk/server` is the only `sdk/` package that opens a listener.** The facade
  test's no-behavior guard (D-205) must allow the one `Options` adapter func
  while still forbidding stray `func` bodies elsewhere under `sdk/` — the
  allow-list gains one named entry (the D-273 enumerated-allow-list pattern;
  the `phase-112a.sh` / `phase-144.sh` gates updated in the same PR).

## Glossary additions

- "with-server scaffold" (docs/glossary.md, same PR).
- "sdk/server facade" (docs/glossary.md, same PR).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
      (the parity gate asserts identity propagation through the served surface
      on both compositions).
- [ ] **Reusable artifact (the `sdk/server` `Handle`): concurrent-reuse
      coverage — the parity gate's N≥10 stress + the inherited Phase 159 N≥100
      D-025 test cover the served handle under `-race`.** See §5 + §11 + D-025.
- [ ] **Consumes shipped surfaces (serve band + scaffold + tools) AND closes
      the external-serving seam: the parity gate wires real drivers end-to-end,
      asserts identity propagation, covers ≥1 failure mode (401), runs under
      `-race`.** See §17.
- [ ] §18: `scaffold-a-harbor-agent` + `add-an-in-process-tool` +
      `configure-production-identity` (+ `use-the-harbor-protocol` checked)
      updated; recipe companion added; docs/site stubs + nav updated.
- [ ] No new Protocol wire types — no D-223 manifest diff, no D-209 docs regen
      (verified: `make protocol-ts-gen-check` unchanged).
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: N/A — none departed
