# Phase 159 — Serve-band promotion: the config→listener composition leaves `package main`

## Summary

`harbor serve` (stock binary, yaml) serves the Protocol, but a scaffolded
agent carrying compiled in-process Go tools cannot — the serve composition
(`bootDevStack`, `devBootOptions`, the `devStack` serve/close lifecycle) is
trapped in `cmd/harbor` (`package main`), unreachable to any importer. This
phase promotes that band into ONE importable internal package
(`internal/runtime/serve`), leaving dev-only policy behind in `cmd/harbor`.
`harbor serve` / `harbor dev` / `harbor console` become thin callers; the
test-kit `harbortest/devstack` is re-wired onto the promoted band as the
second consumer (§13), deleting its hand-mirrored transports/mux block — the
same re-homing move D-197 made for `assemble.Assemble`. Pure promotion: no new
options surface, no wire changes, no new Protocol methods. It unblocks Phase
160 (`sdk/server` + `harbor scaffold --with-server`).

## RFC anchor

- RFC §5.6
- RFC §5.4
- RFC §5.5
- RFC §6.1
- RFC §8

## Briefs informing this phase

- brief 01
- brief 06
- brief 07

## Brief findings incorporated

- brief 06 §5: "when the only assembly prior art is `package main` + a
  `*testing.T` fixture, every embedder re-implements boot." The serve band is
  the last such trap below the network surface: `bootDevStack` lives in
  `package main` and its only other copy is devstack's `*testing.T`-gated
  serve wiring — exactly the two-hand-ordered-copies drift D-197 closed one
  layer down (assembly). This phase promotes the layer above it (listener
  composition) to ONE home and converts both callers to thin wrappers.
- brief 06 §5 (metrics/observability edge): the promoted constructor keeps the
  transport mux composition (`transports.NewMux` + the per-surface option
  wiring) in one place, so the two callers can no longer drift on WHICH
  surfaces they mount (devstack's mux block had already accreted a mirror of
  cmd's option list — `harbortest/devstack/devstack.go` ~1300).
- brief 07 §5 (elegance/one-dispatch principle read structurally): a single
  serve constructor with a single posture seam (the auth-validator factory)
  is one mechanism, not a with-flag / without-flag fork (§13). Production
  posture and dev posture are the SAME code path selected by whether the
  factory is non-nil — never two parallel serve builders.
- brief 01 §5: two hand-ordered copies of a boot fan-out are two modes of the
  same feature and drift on security-adjacent surfaces; the serve band mounts
  the auth middleware, so keeping it single-homed is integrity-relevant, not
  just tidiness.

## Findings I'm departing from (if any)

None.

## Goals

- The config→listener composition (`bootDevStack`, `devBootOptions`, the
  `devStack` struct + its `serve`/`close`) lives in ONE importable internal
  package, `internal/runtime/serve` (naming: `internal/server` is already the
  protocol-server package — do not collide).
- `harbor serve`, `harbor dev`, and `harbor console` are thin callers of the
  promoted package. `harbor serve` continues to inject its JWKS
  `authValidatorFactory` (production posture); `harbor dev` continues to pass a
  nil factory (dev posture).
- Dev-only policy STAYS in `cmd/harbor`: the mock-LLM escape hatch
  (`validateLLMProvider` + the `devmock.go` blank import, D-089), the
  hot-reload supervisor (D-099), the dev-token mint + bootstrap-token
  endpoint, draft scaffolding, and Console embedding (D-091). The promoted
  constructor carries NO `allowMock` knob and NO dev-signer.
- The auth-validator factory remains the single posture seam: a non-nil
  factory = production posture (dev-only surfaces not mounted); nil = dev
  posture.
- `harbortest/devstack` is re-wired to consume the promoted band, deleting its
  hand-mirrored per-caller transports/mux block — the D-197 second-consumer
  move.

## Non-goals

- No new options on the serve surface, no new knobs, no speculative embedder
  wishlist (§13 options-creep guard). `Options` is exactly the union the two
  real callers (cmd + devstack) need today.
- No wire changes, no new Protocol methods, no new event types, no
  `ProtocolVersion` bump — pure Go re-homing (the D-197 posture).
- No `sdk/server` facade and no `harbor scaffold --with-server` — those are
  Phase 160 (the promotion's first EXTERNAL consumer).
- No change to the dev-only surfaces themselves (mock gate, hot-reload,
  dev-token mint, drafts, Console) — they stay byte-for-byte in `cmd/harbor`,
  only their call into the serve band changes.
- No change to `assemble.Assemble` (D-197) — the promoted serve band sits
  ABOVE assembly and calls it; the boundary "Assemble ends where the network
  surface begins" (D-197 point 4) is preserved, this phase just gives the
  network-surface half an importable home.

## Acceptance criteria

- [ ] A new package `internal/runtime/serve` exports the promoted serve band:
  the constructor (the `bootDevStack` body, renamed to a feature name — e.g.
  `serve.Boot(ctx, Options) (*Handle, error)` — with NO godoc phase/D-number
  jargon, §13), the `Options` struct (the promoted `devBootOptions`, renamed),
  and the handle type (the promoted `devStack`, renamed — e.g. `serve.Handle`)
  with `Serve(ctx) error` + `Close(ctx)` methods.
- [ ] The auth-validator factory field on `Options` is the posture seam: a
  non-nil factory builds the production JWKS validator and does NOT mount the
  dev-only bootstrap-token endpoint / dev-token surfaces; a nil factory keeps
  the dev posture. A table test pins BOTH postures (production: dev surfaces
  return 404; dev: they answer).
- [ ] `cmd/harbor/cmd_serve.go` calls `serve.Boot` with its
  `newJWKSValidatorFactory()` injected (unchanged behavior); `harbor serve`
  still mints no token (D-220 invariant intact — asserted by the existing
  serve smoke).
- [ ] `cmd/harbor/cmd_dev.go` calls `serve.Boot` with a nil factory and layers
  the dev-only policy (mock gate, hot-reload supervisor, dev-token mint,
  drafts, Console embed) AROUND it — these stay in `cmd/harbor`, not in the
  promoted package. `harbor console` likewise thin-calls.
- [ ] `harbortest/devstack` consumes the promoted band: its hand-mirrored
  transports/mux composition block (`harbortest/devstack/devstack.go` ~1300,
  the `muxOpts`/`transports.NewMux` fan-out) is DELETED and replaced by a call
  into `serve` (the `SkipTransports`/test-kit knobs are preserved as promoted
  `Options`, per the devstack precedent). No behavior change to the kit's
  public surface (`DevStack.Handler` / `DevStack.Mux` semantics unchanged).
- [ ] Godoc hygiene: no `Phase NN` / `D-NNN` / `brief NN` / wave-band strings
  in the promoted package's non-test Go source (the drift-audit godoc gate);
  every promoted identifier is named for its FEATURE, not its origin phase.
- [ ] Concurrent-reuse: the promoted `Handle` is a compiled artifact (built
  once, serves many requests) — a D-025 test runs N≥100 concurrent requests
  against one served instance under `-race`, asserting no data races, no
  identity bleed across concurrent requests, and goroutine baseline restored
  after `Close`.
- [ ] Cross-phase regression: every existing smoke that boots `harbor dev` /
  `harbor serve` still passes against the new build (no serve-surface
  regression); `make preflight` green.

## Files added or changed

- `internal/runtime/serve/serve.go` (new) — the promoted constructor
  (`Boot`), `Options`, `Handle` + `Serve`/`Close`, the auth-factory posture
  seam. Package godoc names the feature (serve band), not the phase.
- `internal/runtime/serve/serve_test.go` (new) — posture table test (prod vs
  dev), the D-025 concurrent-reuse test, goroutine-baseline teardown.
- `cmd/harbor/cmd_serve.go` — `bootDevStack(...)` call becomes
  `serve.Boot(...)`; `newJWKSValidatorFactory()` stays (it is production
  policy that the caller injects).
- `cmd/harbor/cmd_dev.go` — `bootDevStack` / `devBootOptions` / `devStack` +
  its `serve`/`close` DELETED (moved to `internal/runtime/serve`); the dev-cmd
  becomes a thin caller that layers dev-only policy (mock gate, hot-reload,
  dev-token mint, drafts, Console) around `serve.Boot`.
- `cmd/harbor/cmd_console.go` (if separate) — thin-call conversion.
- `cmd/harbor/cmd_dev_test.go` — unit tests for the dev-only helpers that stay
  (`validateLLMProvider`, `parsePortFromBind`, `newDevSigner`) keep passing;
  serve-band tests move to the promoted package.
- `harbortest/devstack/devstack.go` — the hand-mirrored transports/mux block
  (~1300–1370) deleted; `assembleWith`/serve wiring re-pointed at
  `internal/runtime/serve`. The test-kit-only knobs (`SkipTransports`,
  signer, draft temp-dir, httptest-able `Handler`) stay in devstack (D-197
  boundary).
- `harbortest/devstack/devstack_test.go` — kit-surface parity assertions
  unchanged / re-pointed.
- `scripts/smoke/phase-159.sh` (new) — serve/dev boot-parity assertions.
- `docs/plans/README.md` — Phase 159 row Status + detail block.
- `docs/decisions.md` — D-291.
- `docs/glossary.md` — "serve band", "serve constructor".
- `RFC-001-Harbor.md` §3.6 / §5.3 / §5.6 (amended in the plans PR).

## Public API surface

- `serve.Boot(ctx context.Context, opts serve.Options) (*serve.Handle, error)`
  — the promoted constructor (renamed `bootDevStack`; returns the partial
  handle on error, the D-197 lifecycle contract).
- `serve.Options` — the promoted `devBootOptions`: config, logger, the
  `AuthValidatorFactory` posture seam, the test-kit `Skip*` knobs, and exactly
  the fields the two real callers pass today. No speculative additions.
- `serve.Handle.Serve(ctx) error` / `serve.Handle.Close(ctx)` — the promoted
  `devStack.serve` / `devStack.close`.
- Nothing new is exported from `sdk/` (Phase 160) and no new Protocol wire
  types (none).

## Test plan

- **Unit:** posture table test (non-nil factory → production: dev-only
  endpoints 404; nil factory → dev: they answer); `Boot` partial-failure
  returns the partial `Handle` for `Close` to drain (the D-197 contract);
  `Serve`/`Close` idempotency; the auth-factory error path (a factory that
  errors fails `Boot` loud, never a silent unauthenticated fallback).
- **Integration:** `test/integration/` — boot the promoted `serve.Boot` with
  real drivers (`state/inmem`, `events/inmem`, `audit/patterns`) + an injected
  JWKS factory, drive `/healthz` + one canonical Protocol method over the
  wire, assert identity propagation through the auth middleware to a
  scope-checked read; a missing-identity request is rejected (≥1 failure
  mode); prove the devstack thin-caller and the cmd thin-caller mount the SAME
  surface set (the anti-drift assertion — the reason the block was single-homed).
- **Conformance:** N/A — no persistence-driver seam changes; the promoted band
  composes existing drivers.
- **Concurrency / leak:** D-025 — N≥100 concurrent requests against one served
  `Handle` under `-race` (no identity bleed, no race); goroutine baseline
  restored after `Close` (the serve band starts the notification subscriber /
  session GC sweeper via the assembled stack it wraps — teardown must drain
  them). Plus an N≥10 Boot/Close cycle stress (the D-197 pattern) proving no
  listener/handle leak across repeated composition.

## Smoke script additions

- live-server: `/healthz` returns 200 on a `harbor serve`-postured boot (or,
  where a production-postured boot needs a JWKS the smoke can't easily mint,
  assert the dev-postured `/healthz` + one canonical method round-trip and
  document that the production-posture probe is covered by the integration
  test + Phase 160's `phase-160.sh`). The skeleton lands with the 404/405/501
  → SKIP convention; real assertions land with the implementation PR.
- Assert `harbor serve` boot still prints "mints no token" (D-220 invariant)
  and that a `harbor dev` boot still answers `/healthz` — the no-regression
  parity check.

## Coverage target

- `internal/runtime/serve`: 85%
- `cmd/harbor`: existing package target maintained (the promotion removes code
  from `cmd/harbor`; coverage on the remaining dev-only policy must not drop
  below the current CLI target).

## Dependencies

- 64 (`harbor dev` v1 — the `bootDevStack` this promotes), 110d (D-197
  `assemble.Assemble` — the layer the serve band sits above), 118 (D-223
  lockstep gate — must stay green even though no wire changes land), the
  `harbor serve` band and `harbor token` (D-264, already shipped) that the
  production posture and its local-dev loop rely on.

## Risks / open questions

- **Import-cycle risk.** The promoted `internal/runtime/serve` package imports
  `internal/runtime/assemble`, `internal/protocol/transports`, and the
  per-surface service packages. `cmd/harbor` then imports `serve`. The risk is
  a package the serve band needs that currently imports `cmd/harbor` symbols —
  there should be none (cmd is a leaf), but the promotion must verify no
  service package reaches back into `main`. If one does, that reach-back is
  itself the thing to fix (it was a `package main` leak).
- **What exactly is "dev-only".** The line is drawn at the auth-factory
  posture seam: anything gated behind `factory == nil` (bootstrap-token
  endpoint, dev-token mint print, mock gate, drafts, Console embed, hot-reload
  supervisor) stays in `cmd/harbor`. The serve band mounts only what BOTH
  postures share. The posture table test is the guard that this line held.
- **Naming collision.** `internal/server` is the protocol-server package;
  `internal/runtime/serve` is the config→listener composition. The two names
  are close — the package godoc must state the distinction so a future
  contributor doesn't merge them.

## Glossary additions

- "serve band" (docs/glossary.md, same PR).
- "serve constructor" (docs/glossary.md, same PR).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
      (the served handle carries the identity middleware — the D-025 test
      asserts no cross-request identity bleed).
- [ ] **Reusable artifact (the served `Handle`): concurrent-reuse test passes
      — N≥100 concurrent requests against one instance under `-race`, no
      races, no identity bleed, no goroutine leak after `Close`.** See §5 +
      §11 + D-025.
- [ ] **Consumes a shipped subsystem's surface (assembly + transports) AND
      closes the promotion seam for two callers: an integration test wires
      real drivers end-to-end, asserts identity propagation, covers ≥1 failure
      mode, runs under `-race`.** See §17.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: N/A — none departed
