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
`server.Open`, and serves. The wave's acceptance centerpiece is a
`test/integration` **parity gate** that boots `harbor serve` and a scaffolded
`--with-server` binary from the SAME config and proves they answer the
canonical method surface identically, dispatch the generated custom tool
through the catalog, both FIRE the tool's approval-gate wrap (proving
pre-policy registration, D-292), and both 404 the dev-only surfaces. No new
Protocol wire types — no D-223/D-209 churn.

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

- brief 07 §5 (code-level tool calling / one dispatch path): the compiled-tool
  registrar MUST ride the ONE existing registration seam
  (`assemble.Options.PreRegisterTools`, applied before builtin registration
  and the catalog Builder's `tools.entries` wrapping), never a second
  registration path — a post-assembly `Catalog.Register` skips the
  reliability/approval/OAuth shell and is the documented trap (D-292).
  `RegisterCatalog` is an adapter over that seam, so a compiled tool is wrapped
  identically to an operator's YAML tool.
- brief 03 §5 (tools + integrations): a tool's declared policy (approval gates,
  per-tool retry/timeout, OAuth binding) is applied by the catalog Builder at
  assembly time; a registration that arrives AFTER the Builder ran is
  unwrapped. The parity gate asserts an approval-gate wrap FIRES on the
  generated custom tool in BOTH binaries — the empirical proof that
  `RegisterCatalog` landed at the pre-policy point (§17.8 "a fixture that can't
  tell right-field from wrong-field is a rubber stamp": the gate must observe
  the wrap firing, not merely that the tool is registered).
- brief 06 §5 (devx / adopter reachability): "when the only assembly prior art
  is `package main`, every embedder re-implements boot." Phase 159 fixed that
  for the serve band; this phase makes the reachability HONEST for an external
  module by shipping the scaffold that emits the `sdk/server` call and a
  standing smoke that scaffolds → builds → boots it as an external module (the
  D-206 external-compile-gate pattern extended to serving).
- brief 07 §5 (elegance): `harbor serve` and `sdk/server` both call the ONE
  promoted constructor — `harbor serve` with a nil registrar (yaml-only
  tools), `sdk/server` with the agent's `RegisterCatalog`. Not two serve
  builders; one, parameterized. The parity gate is the proof the fork does not
  exist.

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
  absent. No dev-signer option, no mock knob on the facade. The local-dev loop
  is documented via `harbor token keygen` → `identity.jwks_file` → `harbor
  token mint` (§8).
- Config discipline: `Open` accepts a validated `*config.Config`; a
  load-from-path convenience exists; `Open` re-runs `Validate` on
  programmatically-built configs so there is no validation bypass.
- `RegisterCatalog` fires at the `assemble.Options.PreRegisterTools` seam —
  before builtin registration and the `tools.entries` Builder wrapping — so
  compiled tools receive declared approval / OAuth / policy wrapping. Adapter
  over the existing seam, never a second registration path (§13). The plan
  names the post-assembly-`Catalog.Register` trap explicitly.
- `harbor scaffold --with-server` (opt-in; default scaffold stays headless
  RunOnce): generates `cmd/<agent>/main.go` that loads yaml (`--config` /
  `--bind` flag trio mirroring `harbor serve`), blank-imports
  `sdk/drivers/prod`, passes `agent.RegisterTools` to `server.Open`, and
  serves. `harbor serve` itself calls the promoted constructor with a nil
  registrar via the internal package directly (NOT via the sdk facade).
- The parity gate proves `harbor serve` and a scaffolded `--with-server`
  binary reach parity from the same config.

## Non-goals

- No new Protocol wire types, methods, error codes, or event types — no D-223
  lockstep churn, no D-209 docs regen. (Stated explicitly so a reviewer does
  not look for a manifest diff.)
- No dev-signer / mock / dev-token path on `sdk/server` — production posture
  only. A dev loop uses `harbor token` (§8). (An external dev who wants the
  full dev conveniences uses `harbor dev`, not `sdk/server`.)
- No Console embedding in the scaffolded `--with-server` binary (D-091: the
  Console is served only by `harbor console`). The scaffolded binary is a
  headless Protocol server.
- No change to the default (headless RunOnce) scaffold output — `--with-server`
  is purely additive; a tools-declaring agent without the flag still scaffolds
  and compiles exactly as today (D-206).
- No new top-level directory — `sdk/server` is a package under the existing
  `sdk/` tree; the scaffold emits `cmd/<agent>/` inside the generated module.

## Acceptance criteria

- [ ] `sdk/server` package exists: `Open`, `Options{RegisterCatalog}`, the
  handle type + `Serve`/`Close`, and a load-from-path convenience
  (`server.OpenFromConfigFile` or an `Options` config-path field — one, not
  both). It is alias/forward over `internal/runtime/serve` plus the ONE
  `Options`→`serve.Options` adapter func (the D-205 single-carve-out posture;
  the facade's no-behavior guard allow-lists it by name).
- [ ] Production posture pinned: `Open` with a `cfg.Identity` missing its JWKS
  source fails loud with a named-field error (test asserts the error names the
  field); there is NO code path in `sdk/server` that mounts a dev-signer or a
  bootstrap-token endpoint (grep-gated in the facade test, mirroring D-205's
  mock-exclusion assertion).
- [ ] Validation is not bypassable: `Open` re-runs `config.Validate` (the
  serve profile) on the passed config; an invalid programmatic config fails
  loud at `Open`, not at first request.
- [ ] `RegisterCatalog` fires at the pre-policy point: a tool registered via
  `RegisterCatalog` and declared (in yaml) behind an approval gate is INVOKED
  through the gate — a unit/integration test asserts the approval wrap fires
  (proving registration landed before the Builder's `tools.entries` wrapping).
  A companion negative test shows a post-assembly `Catalog.Register` does NOT
  get the wrap (the documented trap, pinned so a future refactor can't silently
  move the seam).
- [ ] `harbor scaffold --with-server` generates `cmd/<agent>/main.go` (loads
  yaml via `--config`, binds via `--bind`, blank-imports `sdk/drivers/prod`,
  calls `server.Open` with `agent.RegisterTools`, serves, handles SIGTERM →
  `Close`). The default (flagless) scaffold is unchanged (headless RunOnce).
  The scaffolded module compiles as an EXTERNAL module against the `sdk/`
  facade only (no `internal/` imports).
- [ ] `harbor serve` calls `internal/runtime/serve` directly with a nil
  registrar (NOT through `sdk/server`) — the internal path and the facade path
  are the SAME constructor, parameterized by the registrar.
- [ ] Parity gate (`test/integration/`): boots `harbor serve` and a scaffolded
  `--with-server` binary from the SAME config and asserts —
  (a) **manifest-driven probe**: every canonical Protocol method (driven from
  the generated methods manifest, NOT mux introspection) answers with the same
  status class on both binaries;
  (b) discovery + dispatch of the generated custom tool through the catalog on
  both;
  (c) a `tools.entries` approval-gate wrap of the generated custom tool FIRES
  on both (proves pre-policy registration, D-292);
  (d) negative: dev-only surfaces (bootstrap-token endpoint, dev-token mint)
  404 on BOTH;
  (e) §17.3: real drivers, identity propagation asserted end-to-end, ≥1
  failure mode (a bad/absent token rejected 401), N≥10 concurrency stress,
  `-race`.
- [ ] §18 same-PR skill + docs updates (below) land in this PR.
- [ ] `scripts/smoke/phase-160.sh` OK ≥ 3, FAIL = 0.

## Files added or changed

- `sdk/server/server.go` (new) — `Open`, `Options`, handle + `Serve`/`Close`,
  the config-path convenience, the one `Options`→`serve.Options` adapter. Godoc
  names the feature (no phase/D jargon).
- `sdk/server/doc.go` (new) — the facade contract statement (production-only
  posture; the `harbor token` local-dev loop; the pre-policy registrar seam).
- `sdk/doc.go` — add `sdk/server` to the inventory statement.
- `internal/tools/…` — no change to the seam; `RegisterCatalog` is wired
  through `assemble.Options.PreRegisterTools` (already exists).
- `cmd/harbor/cmd_scaffold.go` (+ templates under `cmd/harbor/templates/` or
  wherever scaffold templates live) — the `--with-server` flag + the
  `cmd/<agent>/main.go.tmpl` template. Default output unchanged.
- `cmd/harbor/cmd_scaffold_test.go` — golden-output test for `--with-server`
  (module compiles externally; `main.go` calls `server.Open` +
  `RegisterTools`).
- `test/integration/phase160_serve_parity_test.go` (new) — the parity gate.
- `scripts/smoke/phase-160.sh` (new) — scaffold `--with-server` → external
  build → token-minted JWKS boot → `/healthz` + tool dispatch (degradation
  SKIP on builds without the flag).
- `docs/skills/scaffold-a-harbor-agent/SKILL.md` — a `--with-server` section
  (the opt-in serving scaffold; the `harbor token` local-dev loop).
- `docs/skills/add-an-in-process-tool/SKILL.md` — a note that a compiled tool
  served via `sdk/server` gets its declared policy/approval/OAuth wrapping
  because `RegisterCatalog` rides the pre-policy seam (the D-292 contract, in
  operator language).
- `docs/skills/use-the-harbor-protocol/SKILL.md` — check + update if it claims
  serving is stock-binary-only (add the `sdk/server` external-serving path).
- `docs/recipes/embed-harbor-headless.md` — a companion "serve the Protocol
  from your binary" section (its headless-DEFAULT story stays true; serving is
  the additive opt-in). Retitle only if needed to signal "headless is the
  default, serving is opt-in".
- `docs/site/skills/…` include stubs + `docs/site/recipes/…` stub +
  `docs/site/.vitepress/config.ts` nav (Phase 103 rule — mirror any new
  page/section that gets its own stub).
- `README.md` — one-line pointer in the adopter-paths section (the serving
  scaffold path now works end-to-end).
- `docs/plans/README.md` — Phase 160 row + detail block.
- `docs/decisions.md` — D-292.
- `docs/glossary.md` — "with-server scaffold", "sdk/server facade".
- `RFC-001-Harbor.md` §3.6 / §5.6 (amended in the plans PR).

## Public API surface

- `sdk/server`:
  - `func Open(ctx context.Context, cfg *config.Config, opts Options) (*Handle, error)`
  - `type Options struct { RegisterCatalog func(tools.ToolCatalog) error; /* + the load-from-path convenience field or a sibling OpenFromConfigFile */ }`
  - `func (*Handle) Serve(ctx context.Context) error`
  - `func (*Handle) Close(ctx context.Context) error`
- The scaffolded `cmd/<agent>/main.go` is generated code, not a stable API;
  its shape (the `--config`/`--bind` trio + `server.Open(cfg,
  Options{RegisterCatalog: agent.RegisterTools})`) is the contract the parity
  gate pins.
- Nothing new on the wire — no Protocol methods, types, errors, or events.

## Test plan

- **Unit:** `sdk/server` facade-integrity (every re-export resolves; the one
  adapter func is the only `func` body; no dev-signer path — grep-gated);
  `Open` production-posture fail-loud (missing `cfg.Identity` JWKS →
  named-field error); `Open` re-validates the config (invalid programmatic
  config → loud at `Open`); the `RegisterCatalog` pre-policy assertion (approval
  wrap fires) + the post-assembly-`Register` negative (no wrap); scaffold
  golden-output test for `--with-server`.
- **Integration:** the parity gate
  (`test/integration/phase160_serve_parity_test.go`) — `harbor serve` vs the
  scaffolded `--with-server` binary from one config: manifest-driven method
  parity, generated-tool discovery + dispatch, approval-wrap-fires on both,
  dev-surfaces-404 on both, identity propagation, a 401 failure mode, N≥10
  concurrency stress, `-race`. Uses the in-test ES256/JWKS `harbor token`-style
  issuer (the D-264 fixture pattern) so the production JWKS posture is real, not
  mocked. Real drivers on every seam.
- **Conformance:** N/A — no persistence-driver seam changes; the served stack
  composes existing drivers.
- **Concurrency / leak:** the served `Handle` from `sdk/server` inherits the
  Phase 159 D-025 guarantees; this phase adds the parity gate's N≥10 stress and
  asserts the scaffolded binary's goroutine baseline restores after `Close`
  (the external-module teardown proof).

## Smoke script additions

- live-server / build: `harbor scaffold --with-server` into a temp dir as an
  external module → `go build` → boot with a `harbor token`-minted JWKS posture
  → `/healthz` 200 → dispatch the generated custom tool through the catalog and
  assert it answers. Degradation path: SKIP on a build whose `harbor scaffold`
  lacks `--with-server` (the 404/405/501-style SKIP convention read for a CLI
  flag — `harbor scaffold --with-server` printing an unknown-flag error →
  SKIP).

## Coverage target

- `sdk/server`: 85%
- `cmd/harbor` (scaffold path): existing CLI target maintained (70% floor;
  the `--with-server` template + flag covered by the golden test).

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
  `assemble.Options.PreRegisterTools`, compiled tools silently lose their
  approval/OAuth/policy wrapping — a security-adjacent regression that unit
  tests on the happy path would miss. The parity gate's "approval wrap FIRES on
  both binaries" assertion is the guard; it must observe the wrap firing, not
  merely that the tool is registered (§17.8).
- **Manifest-driven parity, not mux introspection.** The method-parity probe
  MUST be driven from the generated methods manifest so a method that exists in
  one binary's mux but not the other is caught; introspecting one mux and
  comparing to itself is a false-green.
- **Production-only posture vs. adopter first-run friction.** An external
  developer's very first `sdk/server` boot fails loud without a JWKS — by
  design (§13 no-stub-default). The mitigation is documentation: the scaffold's
  `--with-server` output and the skill both lead with the `harbor token keygen`
  → `jwks_file` → `mint` three-command loop. The risk is a confused first-run;
  the answer is NOT a dev-signer knob (that would reopen the D-089/D-220 posture
  the whole design avoids).
- **`sdk/server` is the only `sdk/` package that opens a listener.** The facade
  test's no-behavior guard (D-205) must allow the one `Options` adapter func
  while still forbidding stray `func` bodies elsewhere under `sdk/` — the
  allow-list gains one named entry (the D-273 enumerated-allow-list pattern).

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
      on both binaries).
- [ ] **Reusable artifact (the `sdk/server` `Handle`): concurrent-reuse
      coverage — the parity gate's N≥10 stress + the inherited Phase 159 N≥100
      D-025 test cover the served handle under `-race`.** See §5 + §11 + D-025.
- [ ] **Consumes shipped surfaces (serve band + scaffold + tools) AND closes
      the external-serving seam: the parity gate wires real drivers end-to-end,
      asserts identity propagation, covers ≥1 failure mode (401), runs under
      `-race`.** See §17.
- [ ] §18: `scaffold-a-harbor-agent` + `add-an-in-process-tool` (+
      `use-the-harbor-protocol` checked) updated; recipe companion added;
      docs/site stubs + nav updated.
- [ ] No new Protocol wire types — no D-223 manifest diff, no D-209 docs regen
      (verified: `make protocol-ts-gen-check` unchanged).
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: N/A — none departed
