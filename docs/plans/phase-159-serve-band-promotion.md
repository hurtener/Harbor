# Phase 159 — Serve-band promotion: the config→listener composition leaves `package main`

## Summary

`harbor serve` (stock binary, yaml) serves the Protocol, but a scaffolded
agent carrying compiled in-process Go tools cannot — the serve composition
(`bootDevStack`, `devBootOptions`, the `devStack` serve/close lifecycle) is
trapped in `cmd/harbor` (`package main`), unreachable to any importer. This
phase promotes that band into ONE importable internal package
(`internal/runtime/serve`), leaving dev-only policy behind in `cmd/harbor`,
composed caller-side through explicit injection seams on the promoted
options/handle. `harbor serve` / `harbor dev` / `harbor console` become thin
callers; the test-kit `harbortest/devstack` is re-wired onto the promoted band
as the second consumer (§13), deleting its hand-mirrored transports/mux block —
the same re-homing move D-197 made for `assemble.Assemble`. The re-homing adds
an honestly-enumerated NEW options/handle seam surface (the injection seams the
two real callers need — see Goals), but ZERO wire changes and no new Protocol
methods. It unblocks Phase 160 (`sdk/server` + `harbor scaffold
--with-server`).

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

- brief 01 §5: "An egress endpoint with two bolted-on modes is a trap. …
  Harbor: pick one model and ship it. … There is no legacy 'before' mode to be
  compatible with." Two coexisting compositions of the same serve surface —
  `cmd/harbor`'s and devstack's hand-mirrored copy — are exactly that trap, and
  they had ALREADY drifted on which transport surfaces they mount (the kit's
  mux omits `WithAgentsService` / `WithAuthSurface` / `WithGovernanceService` /
  `WithGovernanceKeyRotate`). The serve band mounts the auth middleware, so
  single-homing it is integrity-relevant, not just tidiness.
- brief 06 §5 ("Tightly coupled Playground"): a dev/test surface that "both
  *consumes* and *re-implements* runtime concepts" — duplicating routes and
  API wiring — is the named anti-pattern; devstack's hand-mirrored mux block is
  the same shape one layer down, and the promotion deletes it. The same
  brief-06 lesson, as distilled into D-197's recorded text ("when the only
  assembly prior art is `package main` + a `*testing.T` fixture, every
  embedder re-implements boot"), applies to the serve band unchanged — it was
  exactly that shape.
- brief 07 §1/§8 (the single-dispatch architecture): one mechanism the runtime
  owns, parameterized — collapsing a mode matrix "into a single dimension
  Harbor controls" — never parallel modes. Production and dev are ONE promoted
  constructor parameterized by the (required) auth-validator factory plus
  caller-side seam composition; never two serve builders (§13).

## Findings I'm departing from (if any)

None.

## Goals

- The config→listener composition (`bootDevStack`, `devBootOptions`, the
  `devStack` struct + its `serve`/`close`) lives in ONE importable internal
  package, `internal/runtime/serve` (naming: `internal/server` is already the
  protocol-server package — do not collide).
- **The promoted constructor REQUIRES a non-nil auth-validator factory.**
  Identity is mandatory (§6): a nil factory is a loud construction error,
  never an unauthenticated listener. The dev signer NEVER promotes —
  `harbor dev` injects a factory built from its ephemeral dev signer;
  `harbor serve` injects its JWKS factory (`newJWKSValidatorFactory()`,
  unchanged).
- **The constructor mounts ONLY the surfaces every caller shares. Dev-only
  surfaces are composed CALLER-SIDE by `cmd/harbor`** through explicit
  injection seams the promoted `Options`/`Handle` expose:
  - **extra pre-CORS routes** (the draft-scaffolding + bootstrap-token mounts,
    today `cmd_dev.go:1429`/`:1460`);
  - **the transports auth-surface option** (the dev key-rotate surface threaded
    at `:1331`);
  - **an LLM snapshot override** (the mock's config mutation at `:440-443`);
  - **a post-boot hook receiving subsystem handles** (the fixture seeding at
    `:1599`);
  - the Console mount stays a `cmd_console.go` composition over the same route
    seam (`:1496-1502`).
  These seams are the phase's honestly-enumerated NEW options/handle surface —
  exactly what the two real callers need today, no speculative additions.
- Dev-only POLICY stays in `cmd/harbor`: the mock-LLM escape hatch
  (`validateLLMProvider` + the `devmock.go` blank import, D-089), the
  hot-reload supervisor (D-099), the dev signer + dev-token mint/print +
  bootstrap-token endpoint, draft scaffolding, post-boot fixture seeding
  (`seedDevFixtures`), and Console embedding (D-091). The promoted constructor
  carries NO `allowMock` knob and NO dev-signer.
- `harbor serve`, `harbor dev`, and `harbor console` are thin callers of the
  promoted package.
- `harbortest/devstack` is re-wired to consume the promoted band, deleting its
  hand-mirrored per-caller transports/mux block — the D-197 second-consumer
  move. The kit's `Skip*` knobs STAY devstack-side as kit policy layered over
  the promoted `Options`.

## Non-goals

- No speculative embedder wishlist on the promoted `Options` (§13
  options-creep guard) — the surface is exactly the injection seams + fields
  the two real callers (cmd + devstack) need today, enumerated in Goals.
- No wire changes, no new Protocol methods, no new event types, no
  `ProtocolVersion` bump (the D-197 posture).
- No `sdk/server` facade and no `harbor scaffold --with-server` — those are
  Phase 160 (the promotion's first EXTERNAL consumer).
- No change to the dev-only surfaces' BEHAVIOR (mock gate, hot-reload,
  dev-token mint, drafts, Console) — they stay in `cmd/harbor`; only their
  composition path changes (through the promoted seams).
- No change to `assemble.Assemble` (D-197) — the promoted serve band sits
  ABOVE assembly and calls it; the boundary "Assemble ends where the network
  surface begins" (D-197 point 4) is preserved, this phase just gives the
  network-surface half an importable home.

## Acceptance criteria

- [x] A new package `internal/runtime/serve` exports the promoted serve band:
  the constructor (the `bootDevStack` body, renamed to a feature name — e.g.
  `serve.Boot(ctx, Options) (*Handle, error)` — with NO godoc phase/D-number
  jargon, §13), the `Options` struct (the promoted `devBootOptions`, renamed),
  and the handle type (the promoted `devStack`, renamed — e.g. `serve.Handle`)
  with `Serve(ctx) error` + `Close(ctx)` methods.
- [x] **Posture split, pinned at two levels.** The in-package
  `internal/runtime/serve` tests pin: (a) a nil auth-validator factory fails
  `Boot` loud (named error, no listener); (b) the constructor mounts ONLY the
  shared surfaces (no dev route, no auth rotate surface, no Console mount
  unless injected); (c) each injection seam works — an injected pre-CORS
  route / auth-surface option / LLM snapshot override / post-boot hook is
  observed exactly where the caller placed it. The caller-level `cmd/harbor`
  tests pin: the dev surfaces (bootstrap-token endpoint, dev mint, drafts)
  ANSWER under the `harbor dev` composition and 404 under the `harbor serve`
  composition.
- [x] `cmd/harbor/cmd_serve.go` calls `serve.Boot` with its
  `newJWKSValidatorFactory()` injected and no dev seams composed (unchanged
  behavior); `harbor serve` still mints no token (D-220 invariant intact —
  the statement lives in `serve --help`, and the existing serve smoke asserts
  the posture).
- [x] `cmd/harbor/cmd_dev.go` calls `serve.Boot` with a dev-signer-built
  factory and composes the dev-only policy (mock gate via the LLM snapshot
  override, dev-token mint, drafts + bootstrap via the pre-CORS route seam,
  rotate via the auth-surface seam, fixture seeding via the post-boot hook,
  hot-reload supervisor around the handle) — all in `cmd/harbor`, none in the
  promoted package. `cmd/harbor/cmd_console.go` (a separate file) thin-calls
  the same way and adds the Console mount.
- [x] `harbortest/devstack` consumes the promoted band: its hand-mirrored
  transports/mux composition block (`harbortest/devstack/devstack.go`
  ~877–1310, the `muxOpts`/`transports.NewMux` fan-out) is DELETED and replaced
  by a call into `serve`. The kit's `Skip*` knobs stay devstack-side as kit
  policy over the promoted `Options`. **Owned behavior change:** the kit's mux
  GAINS the options its mirror omitted (`WithAgentsService`, `WithAuthSurface`,
  `WithGovernanceService`, `WithGovernanceKeyRotate`) — closing that drift IS
  the point of single-homing; a kit-surface test pins the new parity.
- [x] Godoc hygiene: no `Phase NN` / `D-NNN` / `brief NN` / wave-band strings
  in the promoted package's non-test Go source (the drift-audit godoc gate);
  every promoted identifier is named for its FEATURE, not its origin phase.
- [x] Concurrent-reuse: the promoted `Handle` is a compiled artifact (built
  once, serves many requests) — a D-025 test runs N≥100 concurrent requests
  against one served instance under `-race`, asserting no data races, no
  identity bleed across concurrent requests, and goroutine baseline restored
  after `Close`.
- [x] Cross-phase regression: every existing smoke that boots `harbor dev` /
  `harbor serve` still passes against the new build, and the enumerated set of
  smokes whose greps target moved symbols (see Files) is re-pointed in the
  same PR; `make preflight` green.

## Files added or changed

- `internal/runtime/serve/serve.go` (new) — the promoted constructor
  (`Boot`), `Options` (incl. the injection seams: pre-CORS routes, auth-surface
  option, LLM snapshot override, post-boot hook; plus the fields the callers
  already pass — `MCPDefaultIdentity`, version/instance/DisplayName stamps),
  `Handle` + `Serve`/`Close`, the required-factory check. Package godoc names
  the feature (serve band) and its distinction from `internal/server`.
- `internal/runtime/serve/serve_test.go` (new) — nil-factory fail-loud,
  shared-surfaces-only + per-seam injection tests, the D-025 concurrent-reuse
  test, goroutine-baseline teardown.
- **Promoted alongside the band (production functions, all-internal imports):**
  - `cmd/harbor/cmd_dev_runloop.go::newPerTaskRunLoopDriver` — the per-task
    run-loop driver moves with the band (a sibling file under
    `internal/runtime/serve/`).
  - the MCP connection attacher/detacher
    (`cmd/harbor/cmd_dev_mcp_attacher.go` and
    `cmd/harbor/cmd_dev_mcp_detacher.go`).
  - the session ensurer adapter (`cmd/harbor/session_ensurer.go`).
  - `devEnricher` (`cmd/harbor/dev_enricher.go`, wired at `cmd_dev.go:1057`) —
    despite the name it is production tasks.get enrichment: it is wired into
    the tasks projector unconditionally (a `harbor serve` boot carries it
    today) and wraps the promoted run-loop driver's `TrajectoryByTaskID`, so
    it moves with the band; its devstack mirror
    (`harbortest/devstack/enricher.go`) is deleted in favor of the promoted
    version (below).
- **Stays cmd-side (dev policy):**
  - `validateLLMProvider` + the `devmock.go` mock gate (D-089).
  - the hot-reload supervisor (D-099).
  - `seedDevFixtures` (`cmd/harbor/devseed.go`) — invoked via the post-boot
    hook seam.
  - the dev identity constants (`DevTenant`/`DevUser`/`DevSession`) — the
    promoted `Options` takes `MCPDefaultIdentity` explicitly; the constants
    stay dev-cmd vocabulary.
  - version/instance/DisplayName stamps — passed as `Options` fields.
- `cmd/harbor/cmd_serve.go` — `bootDevStack(...)` call becomes
  `serve.Boot(...)`; `newJWKSValidatorFactory()` stays (production policy the
  caller injects).
- `cmd/harbor/cmd_dev.go` — `bootDevStack` / `devBootOptions` / `devStack` +
  its `serve`/`close` DELETED (moved); the dev-cmd becomes a thin caller that
  composes dev-only policy through the promoted seams.
- `cmd/harbor/cmd_console.go` — thin-call conversion + the Console mount over
  the route seam.
- `cmd/harbor/cmd_dev_test.go` — unit tests for the dev-only helpers that stay
  (`validateLLMProvider`, `parsePortFromBind`, `newDevSigner`) keep passing;
  serve-band tests move to the promoted package; NEW caller-level posture tests
  (dev surfaces answer under dev, 404 under serve).
- `harbortest/devstack/devstack.go` — the hand-mirrored transports/mux block
  (~877–1310) deleted; serve wiring re-pointed at `internal/runtime/serve`;
  `Skip*` knobs preserved as kit policy. The kit's hand-mirrored driver files
  are DELETED in favor of the promoted versions in the same phase: the
  `newDevStackRunLoopDriver` run-loop-driver mirror,
  `harbortest/devstack/devstack_mcp_attacher.go`,
  `harbortest/devstack/devstack_mcp_detacher.go`,
  `harbortest/devstack/session_ensurer.go`, `harbortest/devstack/enricher.go`
  (where the promoted equivalents replace them; kit-only pieces stay).
- `harbortest/devstack/devstack_test.go` — kit-surface parity assertions
  re-pointed + the new gained-options parity pin.
- **Mechanical smoke fallout (same PR):**
  - `scripts/smoke/phase-112a.sh` + `scripts/smoke/phase-144.sh` — the sdk
    func-body allow-lists gain the `sdk/server` entries when Phase 160 lands;
    THIS phase updates the allow-list entry SHAPE so the exact-func-count
    constraint can express a named per-file extension (keeping the gate green
    across both phases).
  - the enumerated set of smokes that grep `cmd_dev.go` /
    `cmd_dev_runloop.go` for symbols the promotion moves is re-pointed at the
    new homes in the same PR — e.g. `phase-110d.sh:48` (asserts
    `assemble.Assemble(` in `cmd_dev.go`), `phase-83f.sh` (greps
    `cmd_dev_runloop.go` for `memory.MemoryStore` /
    `runctx.FetchMemoryBlocks`), plus the rest surfaced by
    `grep -rln cmd_dev scripts/smoke/` (107e, 110a–d, 111b/d/e/f, 120, 126b,
    138, 139, 146, 150, 65, 73c, 73f, 83i, 83w, …) — the implementor re-runs
    that grep for the authoritative list and updates each grep target to the
    promoted file path.
- `scripts/smoke/phase-159.sh` (new) — serve/dev boot-parity assertions.
- `docs/plans/README.md` — Phase 159 row Status + detail block.
- `docs/decisions.md` — D-291.
- `docs/glossary.md` — "serve band", "serve constructor".
- `RFC-001-Harbor.md` §3.6 / §5.3 / §5.6 (amended in the plans PR).

## Public API surface

- `serve.Boot(ctx context.Context, opts serve.Options) (*serve.Handle, error)`
  — the promoted constructor (renamed `bootDevStack`; returns the partial
  handle on error, the D-197 lifecycle contract; REQUIRES a non-nil
  auth-validator factory).
- `serve.Options` — the promoted `devBootOptions`: config, logger, the
  REQUIRED `AuthValidatorFactory`, the caller-side injection seams (extra
  pre-CORS routes, transports auth-surface option, LLM snapshot override,
  post-boot hook receiving subsystem handles), `MCPDefaultIdentity`,
  version/instance/DisplayName stamps, and the test-kit `Skip*` knobs the kit
  layers over — exactly the fields the two real callers pass today. No
  speculative additions.
- `serve.Handle.Serve(ctx) error` / `serve.Handle.Close(ctx)` — the promoted
  `devStack.serve` / `devStack.close`.
- Nothing new is exported from `sdk/` (Phase 160) and no new Protocol wire
  types (none).

## Test plan

- **Unit:** nil-factory → loud error, no listener; shared-surfaces-only
  posture table (no dev route / rotate surface / Console mount unless
  injected); per-seam injection tests (route, auth-surface, LLM snapshot
  override, post-boot hook each observed); `Boot` partial-failure returns the
  partial `Handle` for `Close` to drain (the D-197 contract); `Serve`/`Close`
  idempotency; the auth-factory error path (a factory that errors fails `Boot`
  loud, never a silent unauthenticated fallback). Caller-level `cmd/harbor`
  tests: dev surfaces answer under the dev composition, 404 under the serve
  composition.
- **Integration:** `test/integration/` — boot the promoted `serve.Boot` with
  real drivers (`state/inmem`, `events/inmem`, `audit/patterns`) + an injected
  JWKS factory, drive `/healthz` + one canonical Protocol method over the
  wire, assert identity propagation through the auth middleware to a
  scope-checked read; a missing-identity request is rejected (≥1 failure
  mode); prove the devstack thin-caller and the cmd thin-caller mount the SAME
  surface set (the anti-drift assertion — the reason the block was
  single-homed).
- **Conformance:** N/A — no persistence-driver seam changes; the promoted band
  composes existing drivers.
- **Concurrency / leak:** D-025 — N≥100 concurrent requests against one served
  `Handle` under `-race` (no identity bleed, no race); goroutine baseline
  restored after `Close` (the serve band starts the notification subscriber /
  session GC sweeper via the assembled stack it wraps — teardown must drain
  them). Plus an N≥10 Boot/Close cycle stress (the D-197 pattern) proving no
  listener/handle leak across repeated composition.

## Smoke script additions

- live-server: `/healthz` returns 200 + one canonical Protocol method
  round-trips on the preflight dev boot — the no-regression boot-parity check
  for the promoted band. The constraint on deeper smoke probing is that a
  smoke cannot spend an LLM turn (minting a production JWKS is trivial via
  `harbor token`; an LLM-driven dispatch is not smoke material) — the
  production-posture and seam-behavior proofs live in the in-package +
  caller-level tests and the integration test, and Phase 160's `phase-160.sh`
  adds the production-postured external boot.
- Assert the D-220 posture line ("mints no token") in `harbor serve --help`
  output (the string lives in the help text, not boot output) and that a
  `harbor dev` boot still answers `/healthz`.
- Done-definition: `OK ≥ 2, FAIL = 0` once the phase ships (the skeleton's
  `skip` is replaced by the assertions above).

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
- **The seam inventory is the line, and it must not grow silently.** Dev-only
  behavior reaches the promoted band ONLY through the enumerated injection
  seams (routes, auth-surface option, LLM snapshot override, post-boot hook).
  The temptation during implementation is to promote "just one more" dev
  surface into the constructor for convenience — that re-opens the posture
  hole the required-factory + caller-composition design closes. The
  shared-surfaces-only in-package test plus the caller-level 404 tests are the
  guards that the line held.
- **Naming collision.** `internal/server` is the protocol-server package;
  `internal/runtime/serve` is the config→listener composition. The two names
  are close — the package godoc must state the distinction so a future
  contributor doesn't merge them.

## Glossary additions

- "serve band" (docs/glossary.md, same PR).
- "serve constructor" (docs/glossary.md, same PR).

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
      (the served handle carries the identity middleware — the D-025 test
      asserts no cross-request identity bleed).
- [x] **Reusable artifact (the served `Handle`): concurrent-reuse test passes
      — N≥100 concurrent requests against one instance under `-race`, no
      races, no identity bleed, no goroutine leak after `Close`.** See §5 +
      §11 + D-025.
- [x] **Consumes a shipped subsystem's surface (assembly + transports) AND
      closes the promotion seam for two callers: an integration test wires
      real drivers end-to-end, asserts identity propagation, covers ≥1 failure
      mode, runs under `-race`.** See §17.
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: N/A — none departed

## As-built notes + deviations (§4.3)

The phase shipped as specified. Three faithful realizations worth recording:

1. **The "LLM snapshot override" seam is a builder, not a mutator.** The plan
   named the seam as the mock's config mutation (`:440-443`). It landed as
   `Options.BuildLLMSnapshot func(*config.Config) (*llm.ConfigSnapshot, error)`
   — the dev caller's builder runs the fail-loud provider gate
   (`validateLLMProvider`, which BOTH `harbor dev` and `harbor serve` already
   ran) AND applies the mock override, folding the two into one seam. This
   avoids a second `config.Load` (the promoted `Boot` loads the config once)
   while keeping all dev LLM policy caller-side. A nil builder is the default
   `llm.SnapshotFromConfig` projection.

2. **The dev signer is built ONCE caller-side and reused across hot-reload
   reboots.** The pre-promotion `bootDevStack` minted a fresh dev signer on
   every boot, so a hot-reload reboot silently invalidated the
   previously-printed dev token. Moving the signer out of the boot body into
   `cmd/harbor/devcompose.go` (captured by the factory + auth-surface closures
   the supervisor re-passes) makes the printed token stable across reloads — a
   behaviour-improving side effect, not a regression.

3. **`harbortest/devstack` composes the promoted building blocks rather than
   routing its whole assembly through `serve.Boot`.** The kit's public API
   (`AssembleOpts` + `DevStack` + `Skip*`/override knobs) is consumed by 40+
   integration tests and must stay stable, so the single-homing landed at the
   building-block level: the shared `serve.BuildMux` fan-out (deleting the
   kit's hand-mirrored mux block and gaining the omitted agents / auth-rotate /
   governance-override / governance-key-rotate surfaces) plus the promoted
   `serve.RunLoopDriver` / `serve.NewMCPConnectionAttacher` /
   `serve.NewMCPConnectionDetacher` / `serve.NewSessionEnsurerAdapter` /
   `serve.NewEnricher` (deleting the kit's driver + glue mirror files). The
   anti-drift integration test asserts the cmd thin-caller and the devstack
   thin-caller mount the same shared surface set.
