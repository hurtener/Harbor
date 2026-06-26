# Harbor v1.8.0 — The Adopter-Path Wave (phases 131–140) — wave coordination

> Per Harbor §17.7 wave delivery cadence. This is the coordination artifact
> for the v1.8.0 adopter-path wave. It sequences the phases into staged
> worktree dispatches, prescribes the drain-merge order, the wave-end E2E,
> and the §17.5 checkpoint audit that gates the next band.
>
> **Mandate:** make all three advertised adopter paths (embed / CLI /
> protocol) work end-to-end, and make the public site honest about what is
> delivered. The wave adds public API (`Stack.RunOnce`, `runctx.NewRunContext`,
> the `harbor token` subcommand, `cmd/harbor-protocol-ts-types`, new examples
> and skills) ⇒ additive **minor** bump per semver ⇒ ships as **v1.8.0**.

---

## Version label — v1.8.0 (settled)

- The latest released tag is **v1.7.0** (`eb732c68`, the 128–130 Protocol-edge
  hardening band, cut 2026-06-26). The next minor after v1.7.0 is **v1.8.0** —
  there is no phantom intermediate release; the "v1.7.0 must be cut first"
  prerequisite is **already satisfied**.
- This wave is purely **additive** public API + docs. No `ProtocolVersion`
  bump (RFC §5.3 — that is an RFC change), no breaking change. The product
  release version moves v1.7.0 → **v1.8.0**.
- Phase 140 authors the **`[1.8.0]`** CHANGELOG heading, names the wave-end
  test `test/integration/wave_v18_test.go` (matching the `wave_v17_test.go`
  precedent), runs a `chore(checkpoint): v1.8.0 audit fixes` PR, and gates the
  **next** band's scoping (not "v1.7.2").

---

## 1. Executive summary

Harbor v1.7 advertises three adopter paths. Only **one** is honest end-to-end
today.

- **EMBED** — `assemble.Assemble` is a genuine one-call composer, the headless
  recipe is integration-test-gated, and `harbortest/` is externally
  importable. But **running a goal after assembly** still costs ~15–27 lines
  of `RunContext`/`RunSpec` ceremony per run, there is no production one-call
  runner (`RunOnce` is test-kit only — `harbortest/runonce.go`), and there are
  **zero `Example_` functions** anywhere under `sdk/` (verified:
  `grep -r "func Example" sdk/` returns nothing; `sdk/` has no `_test.go`
  files at all).
- **CLI** — `init → scaffold(toolless) → validate → dev → console` all work.
  But the advertised **scaffold-WITH-tools** path is compile-gated only: the
  scaffolded `agent_test.go.tmpl` never calls `RegisterTools`, so a
  tools-declaring agent compiles and its tests pass while no tool is ever
  invoked. And `harbor dev` does not recompile on `.go` edits — worse than a
  no-op, it drives an in-process reboot that emits
  `dev.hot_reload.completed{Success=true}` while the Go change is never
  compiled in (a **loud false-success**). The user-facing over-claim is in the
  marketing site (`landingSpec.ts:236`, "hot reload on your laptop"), **not**
  the README (which has zero hot-reload mentions).
- **PROTOCOL** — `harbor serve` boots a correct JWKS verifier and (by design,
  **D-220**) mints **no token**. But there is **no documented path, no skill,
  and no worked client example** to obtain a JWT and **attach** the Console or
  a custom client — and no on-ramp at all for an adopter who has **no IdP** or
  is **issuing their own** tokens. The connect step is a hard cliff (**P0**).
  TS type generation is still deferred (D-132), and the conformance suite is
  not externally importable (internal-only, by D-210).

**v1.8.0** closes the serve-attach cliff (P0, leads the wave) with two honest
on-ramps, delivers a production one-call runner with first-class streaming,
makes scaffold-with-tools actually execute a tool, ships a thin TS wire-type
generator and a conformance worked example, adds `sdk` `Example_`
discoverability, verifies an MCP tool reaches the planner through the executor,
and makes the public site honest now.

---

## 2. Path readiness

| Path | Verdict | Top blockers |
|------|---------|--------------|
| **embed** | Assembly honest & gated; running a goal is ~15–27 lines of ceremony with no one-call runner and no discoverable examples. | `RunOnce` missing; `RunContext` factory missing; no `Example_` in `sdk/*`; scaffold-with-tools execution unverified |
| **cli** | Core loop works; scaffold-with-tools compile-only; `.go` hot-reload reports false success. | Scaffolded test never calls `RegisterTools`; `.go` hot-reload false-success vs honesty; MCP agent-call leg unverified; stale test comment |
| **protocol** | serve + JWKS + Console correct; **no path to obtain a token and attach a client**, and no on-ramp for the no-IdP / self-issuing adopter — a cliff. | No production-identity guide; no `harbor token` bring-your-own on-ramp; no skill; no worked OIDC client; no TS generator (D-132); conformance not importable |

---

## 3. The serve-attach resolution (the P0, settled)

The review's #1 blocker was the original plan's "gated dev-token mint inside
`harbor serve`" — which silently reversed **D-220** ("serve mints nothing"),
never said how a minted token would pass serve's IdP-only JWKS verifier, and
would have **widened serve's production trust edge** (a second signing key
baked into what the production auth surface trusts). **Rejected.**

**Resolution — two honest on-ramps, `serve`'s verifier untouched:**

`harbor serve` already verifies `/v1/*` JWTs against whatever JWKS the operator
configures via `identity.jwks_url` / `identity.jwks_file` (README:192–194).
It does not care whether that JWKS belongs to Auth0 or to a key the operator
controls. So the cliff is not "you must buy an IdP" — it is "we never
documented the self-issuing path and gave no tool for it." Two on-ramps close
it:

1. **Have an IdP** (Auth0 / Okta / Keycloak / Cognito) → the **131a**
   production-identity guide + the **131c** worked OIDC client. Unchanged from
   the original plan.
2. **No IdP / issuing your own** → **131d**, a new `harbor token` subcommand:
   - `harbor token keygen` — generate an asymmetric keypair (ES256 default;
     RS256 opt-in — both on serve's §7 allowlist) and emit the matching
     **JWKS** (public) file.
   - `harbor token mint` — mint a Harbor JWT with the claim shape serve's
     parser enforces (`tenant`/`user`/`session`/`scopes`/`exp` **plus
     matching `iss`/`aud`** — authoritative source is the parser
     `auth.go:421-484`), signed with that key.
   - The operator points `identity.jwks_file` at the emitted JWKS, mints with
     `--issuer`/`--audience` equal to their `serve.yaml`, and attaches.

**Why this is correct and safe:**

- **`serve`'s verifier is unchanged.** It trusts the `harbor token` key for
  exactly one reason: the operator explicitly configured `jwks_file` to point
  at it — identical to pointing at Auth0. **No code change to serve's auth
  edge, no composite keyset, no trust-edge widening.**
- **D-220 stays literally true.** `harbor serve` still mints nothing; a
  *separate* subcommand mints, and serve only accepts the result because the
  operator chose to trust its JWKS. **No D-220 supersede is required** — this
  is the "your chosen issuer mints; serve verifies" contract D-220 already
  describes, now with a tool for the self-issuing case.
- **Honest about grade.** 131d's docs carry a clear callout: *these tokens are
  signed by a key you manage — protect the private key; for multi-user / SSO
  production, graduate to a real IdP (see `production-identity-setup.md`).*
  Single-issuer self-hosting is a legitimate small-prod posture; multi-user
  SSO is not what this is.
- **`mock-OIDC` drops to a test fixture only.** It is no longer a user-facing
  quickstart (that role goes to `harbor token`). It survives as a hermetic
  in-test ES256 JWKS issuer that proves the **IdP leg** in the 131c / 140
  smokes. The mock-OIDC → serve → `runtime.info` round-trip stays the
  **binding** production-leg smoke; the `harbor token` → `jwks_file` → serve
  leg is an **additional** assertion. Neither replaces the other (avoids the
  §17.8 "fixture that can't tell right-field from wrong-field" anti-pattern).

This makes the §1 EMBED/PROTOCOL honesty fixes real (a first-attach path that
actually exists), not just softened prose.

---

## 4. Phases

Decision numbers are pre-assigned from the **D-263** block (latest shipped is
D-262) so parallel worktree agents never collide in `docs/decisions.md`.

| Phase | Title | Decision | Stage | Size |
|-------|-------|----------|-------|------|
| 131a | Production identity setup guide | D-263 | 1 | M |
| 131b | `configure-production-identity` skill | — (skill) | 1 | S |
| 131c | Worked OIDC client + serve round-trip smoke | — (reaffirms D-263) | 1 | M |
| 131d | `harbor token` bring-your-own-issuer subcommand | D-264 | 1 | L |
| 132 | Embed production runner (`RunOnce`) + `NewRunContext` factory | D-265 | 1 | M |
| 132-stream | `WithStream` sink on `RunOnce` | D-266 | 1 | M |
| 133 | Scaffold-with-tools execution gate | D-267 | 1 | M |
| 138 | Hot-reload Go-source honesty | D-268 | 1 | M |
| 139 | Public-site honesty sweep | — (docs) | 1 | S |
| 134 | `sdk` `Example_` functions | — (examples) | 2 | M |
| 135 | TS wire-type generator + `event-viewer-ts` | D-269 | 2 | M |
| 136 | MCP agent-calls-tool integration test | — (test) | 2 | M |
| 137 | Conformance worked example (in-tree) | — (reaffirms D-210) | 2 | M |
| 140 | Wave E2E + v1.8.0 checkpoint audit | — (audit) | wave-end | M |

### Stage 1 — P0 + golden-path unblockers + honesty-now

#### 131a — Production identity setup guide (protocol, M) — **P0, leads the wave** (D-263)

Fixes: *No documented path to obtain a JWT for `harbor serve`.*

Ships `docs/site/protocol/production-identity-setup.md`: OIDC app
registration, claim mapping (`tenant`/`user`/`session`/`scopes` **+ matching
`iss`/`aud`**), the mint-and-test walkthrough, and Auth0 / Okta / Keycloak /
Cognito snippets. Lifts the JWT claim-shape from the authoritative parser
`internal/protocol/auth/auth.go:421-484` (`auth.go:40-52` is the illustrative
comment) into user docs. **Documents both on-ramps** (§3): the IdP path *and*
a forward pointer to the `harbor token` self-issuing path (131d). Links
`auth-and-identity.md:110-115` and `build-a-client.md:63-64` to it. **§18
(same PR):** updates `use-the-harbor-protocol/SKILL.md` §1 (the token step,
~line 37 — today it shows only the dev-token path) to forward-point to this
guide and the `harbor token` on-ramp. Adds the VitePress nav entry **and** the
`docs/site/` include stub (Phase 103 rule).

**Decision D-263:** `harbor serve` stays strictly IdP-/JWKS-config-driven
(D-220 preserved, verifier untouched); first-attach is solved by **two
documented on-ramps** — real IdP (131a/131c) and self-issuing via `harbor
token` (131d) — never by minting inside serve.

Gate: `scripts/smoke/phase-103.sh` (site page mapped, no dead site-internal
links); VitePress build.

#### 131b — `configure-production-identity` skill (protocol, S)

Fixes: *No worked production-deployment skill for identity setup.* Dep: 131a.

`docs/skills/configure-production-identity/SKILL.md` (`metadata.surface:
protocol`), with INDEX + CONFIG.md links. **Deliverables include the
`docs/site/skills/configure-production-identity/` include stub + the
`docs/site/.vitepress/config.ts` nav entry** (Phase 103 rule — mirror 131a so
its own `phase-103.sh` skill→page-mapping gate passes).

Gate: `make drift-audit` frontmatter check; `phase-103.sh` skill→page mapping.

#### 131c — Worked OIDC client + serve round-trip smoke (protocol, M) — **load-bearing P0 consumer**

Fixes: *No worked example of a custom client obtaining a JWT from OIDC.* Dep: 131a.

`examples/protocol-clients/oidc-client-example/` (SDK-free, stdlib only) does
the OAuth2 client-credentials flow, then `runtime.info` / start / subscribe
against `harbor serve`. (`examples/protocol-clients/` is an established dir —
`event-viewer` already lives there; no new top-level directory.)

Gate: `scripts/smoke/phase-131c.sh` — **the binding production leg**:
in-test ES256/JWKS mock-OIDC issuer → `harbor serve` (pointed at the issuer's
JWKS) → example obtains a token → `runtime.info` OK with expected scopes;
assert the example compiles SDK-free. The mock-OIDC issuer is **mandatory**
here (it proves the OAuth2/JWKS production path); it is not optional.

#### 131d — `harbor token` bring-your-own-issuer subcommand (cli/protocol, **L**) — **the no-IdP on-ramp** (D-264)

Fixes: *No on-ramp for an adopter with no IdP or issuing their own tokens.*
Dep: 131a (claim-shape docs).

> **Sizing note (L, not M).** This is **not** a thin wrapper over the existing
> dev signer. It shares only the **JWT claims shape** (`auth.go`'s claim
> struct) with the `harbor dev` path. It introduces **three net-new crypto
> pieces** the dev signer does not have: (1) an **RFC-7517 JWK Set emitter**
> (public key → `jwks.json`) — `internal/protocol/auth/jwks.go` is
> *consumer-only* today (it parses JWKs, no emitter, no JWK lib vendored), so
> this is hand-written stdlib; (2) **PEM private-key write/read** — net-new
> and a deliberate departure from the dev signer's "key never persisted to
> disk" posture (`devauth.go`); (3) an **RS256 branch** (the dev signer
> hardcodes ES256). Plus issuer/audience/kid/ttl **parameterization** the dev
> signer hardcodes. Build a small **persistable, issuer/audience-parameterized
> signer** that reuses the dev path's claim shaper only.

New `harbor token` subcommand with two verbs:

- `harbor token keygen --out <dir> [--alg ES256|RS256]` — generate an
  asymmetric keypair (ES256 default; RS256 opt-in — both on serve's §7
  allowlist), write `private.pem` (**mode `0600`, parent dir `0700`**; refuse
  to overwrite an existing key without `--force`; print a "keep this out of
  version control" stderr warning) and a `jwks.json` (public) file whose `kid`
  is the **JWK thumbprint** of the key (not a hardcoded constant).
- `harbor token mint --key <private.pem> --tenant T --user U --session S
  --issuer ISS --audience AUD [--kid …] [--scopes …] [--ttl 1h]` — mint a
  Harbor JWT with the claim shape serve's parser enforces (the authoritative
  source is the claim **parser** `internal/protocol/auth/auth.go:421-484`;
  `auth.go:40-52` is the illustrative comment), signed with the keypair.
  **`--issuer` and `--audience` are mandatory and MUST equal the operator's
  `identity.issuer` / `identity.audience`** — serve hard-rejects an `iss`/`aud`
  mismatch (`auth.go:439`/`:443`) and `Config.Validate` mandates both non-empty
  for the serve profile (`validate.go:194-198`), so a hardcoded `harbor-dev`/
  `harbor` mint (the dev signer's defaults) would 401 against any real
  `serve.yaml`. **Least-privilege defaults:** **no scopes** unless `--scopes`
  is passed (NOT the dev signer's / D-221's `admin` default), short `--ttl`
  default (1h) echoed to stderr.

**`cmd/harbor/cmd_serve.go` is NOT modified.** Attach flow:
`harbor token keygen` → point `serve`'s `identity.jwks_file` at `jwks.json`
→ `harbor token mint --issuer … --audience …` (matching `serve.yaml`) → attach
with the JWT. Threads the example into `examples/serve.yaml` (a `jwks_file`
stanza beside the existing OIDC stanza, with `issuer`/`audience` shown) and a
`harbor token` walkthrough into 131a's guide. **RFC §8 (binding):** a new
top-level CLI subcommand is an RFC update (RFC-001:1317) — the 131d PR updates
RFC §8's subcommand enumeration to include `harbor token keygen`/`mint`.

**Decision D-264:** `harbor token` is a *separate* subcommand that mints
operator-managed self-issued JWTs; serve's verifier is unchanged and trusts
the key only via operator-configured `jwks_file` (single keyset —
`validate.go:202-206` forbids a composite). §7 compliance: asymmetric
algorithms only; the private key is operator-managed, written `0600`, never
logged. The D-264 entry **explicitly cross-references D-220** and names the
tension: this reintroduces a self-issuing single-key posture, bounded to an
explicit operator opt-in (a subcommand the operator runs deliberately, not a
silent default), so §13's no-stub-default / dev-only-escape-hatch rules are
satisfied without a runtime banner. Honesty callout in the docs:
eval-/single-issuer-grade — graduate to a real IdP for multi-user SSO.

Gate: `scripts/smoke/phase-131d.sh` — `harbor token keygen` (assert
`private.pem` is `0600`) → `harbor serve` with `jwks_file` pointed at the
output → `harbor token mint` with matching `--issuer`/`--audience` →
`runtime.info` OK with the minted scopes; **a mismatched-`iss`/`aud` token is
rejected `401`** (not happy-path only); a default mint (no `--scopes`) yields a
**non-admin** token; assert `harbor serve` boot still prints "mints no token"
(D-220 invariant intact) and that serve itself mints nothing.

#### 132 — Embed production runner (`RunOnce`) + `NewRunContext` factory (embed, M) — **P1** (D-265)

Fixes: *No one-call runner* + *`RunContext` hand-assembled per run.*

- **Primitive:** `internal/runtime/runctx.NewRunContext(ctx, stack, quad,
  goal, opts...)` — auto-populates the production `RunContext`/`RunSpec`
  fields with stack-derived defaults. It MUST **compose the existing
  `internal/runtime/runctx` projection helpers** (the same ones the dev
  drivers use), not hand-roll a third construction site — and ship a
  **parity test** asserting it projects the same memory / skills / artifact /
  streaming surface as the established `cmd_dev_runloop.go` + `devstack.go`
  paths (D-094-mirrored pair). Do **not** rewrite the dev-driver bodies (their
  per-run projection differs by design).
- **Consumer (same phase, §13):** `Stack.RunOnce(ctx, goal, identity, opts
  ...RunOption) (planner.AnswerEnvelope, error)` on `internal/runtime/assemble`
  plus a `sdk/assemble` facade alias. **Blocking, no `Sync` suffix** — matches
  verified house style (`harbortest.RunOnce`, `RunLoop.Run` both block on the
  calling goroutine; "async" is the caller's `go`).
- N≥100 concurrent-reuse `-race` test against a single shared `Stack` (no
  context bleed, no cross-cancellation, no goroutine leak — §5 / D-025).
- Recipe (`docs/recipes/embed-harbor-headless.md`) gains step **4a** (the
  `RunOnce` blocking shorthand) beside the existing 4b (manual `RunLoop.Run`).
  Keep the manual recipe — it stays test-gated.

Gate: `scripts/smoke/phase-112b.sh` compiles **a checked-in
`examples/embed-runonce/` snippet** that runs a goal via `Stack.RunOnce`
(a real compiled file the smoke builds — *not* a gameable "<25 lines"
heredoc line-count) and the N≥100 `-race` + parity tests pass.

**Decision D-265:** the production one-call runner is a single blocking
`Stack.RunOnce` (no sync/async split); `NewRunContext` is the shared factory
composing the existing projection helpers.

#### 132-stream — `WithStream` sink on `RunOnce` (embed, M) — **P1** (D-266) · Dep 132

Fixes: *An agent framework without first-class streaming is a 2026 adoption
blocker.* Split from 132 so each is one reviewable worktree agent.

Adds a `WithStream(func(StreamEvent))` run option (one sink on the **same**
`RunOnce` method — `RunOnce` still blocks and returns the final envelope while
emitting token / tool / step chunks as they occur). No separate sync/async
method.

- **Seam (named explicitly):** wire the sink to the existing
  `planner.RunContext.OnChunk` (`planner.go:314-322`, `react.go:643-649`) +
  `steering.RunSpec.OnToolDispatched` (`internal/runtime/steering/runloop.go`,
  `RunSpec.OnToolDispatched` field ~:381, synchronous dispatch call ~:780)
  callbacks — both fired
  **synchronously on the run goroutine**, which makes "chunks arrive before the
  final envelope" ordering **deterministic** (no race to engineer around).
  There is **no** `internal/runtime/streaming` package on the `RunOnce` path —
  do not invent one.
- New public `StreamEvent` type (token / tool-dispatched / step kinds).
- §13 primitive-with-consumer: the sink ships with a test exercising it
  end-to-end.

Gate: `phase-112b.sh` streaming variant asserts ordered chunks arrive before
the final envelope; concurrent-reuse `-race` test extended to assert no
cross-run chunk bleed (run A's chunks never reach run B's sink).

#### 133 — Scaffold-with-tools execution gate (cross, M) — **P1** (D-267)

Fixes: *`phase-112b.sh` verifies scaffold + build but not execution* — the
scaffolded `agent_test.go.tmpl` never calls `RegisterTools`, so a
tools-declaring agent compiles and passes while no tool is invoked.

`agent_test.go.tmpl` gains a `{{if .CustomTools}}` / `{{if .BuiltIns}}` block
that calls `RegisterTools` and drives a goal invoking ≥1 tool **through the
catalog** when tools are declared. `phase-112b.sh` adds a `go test ./...` leg
on the scaffolded external module that **fails loud** if no tool is registered
and invoked (assert via an observable tool-dispatch signal, not merely that
`RegisterTools` is defined — Go does not flag an unused exported func).

**Decision D-267:** the scaffold's golden test exercises the
register-and-dispatch path, not just compilation, whenever the agent declares
tools.

Gate: `scripts/smoke/phase-112b.sh` scaffold → tidy → build → **test**
asserting a tool was registered **and invoked through the executor**.

#### 138 — Hot-reload Go-source honesty (cli, M) (D-268)

Fixes: *`.go` edits drive an in-process reboot that emits
`dev.hot_reload.completed{Success=true}` without recompiling the binary — a
loud false-success.*

- `shouldTrigger()` detects `.go` changes and routes them to a **WARN +
  `make build` / restart guidance** path — **not** a `bootDevStack` reboot
  that reports `Success=true`. (YAML / config / scaffold changes keep the
  existing in-process devStack rebuild.)
- Complete the dangling doc sentence at `cmd_dev_hot_reload.go:28`
  ("This is documented in." → name the recipe).
- The real doc gap is the **recipe**: `docs/recipes/run-harbor-dev.md` gains
  the `.go`-changes caveat. The skill (`run-the-dev-loop/SKILL.md:68`) is
  **already** honest — do not "fix" it. The README has **no** hot-reload claim
  — do not edit it for this (the marketing over-claim at `landingSpec.ts:236`
  is 139's target).
- **Open question (138) resolved:** the optional `policy: rebuild-binary`
  (wrap `bootDevStack` in `go build` + re-exec) is **deferred** — WARN +
  guidance only this phase.

**Decision D-268:** `harbor dev` is honest about `.go` changes — it warns and
guides a manual rebuild rather than reporting a successful hot-reload that did
not recompile.

Gate: `scripts/smoke/phase-65.sh` performs a **live `.go` edit** against the
running dev server and asserts the WARN/guidance log fires and **no**
`dev.hot_reload.completed{Success=true}` is emitted for that edit — a static
binary `strings` grep cannot distinguish this (the `completed` string remains
in the binary for the YAML path), so the assertion must be a live
edit-and-observe (or be delegated explicitly to the in-package
`cmd_dev_hot_reload_test.go` with the smoke asserting the YAML path only).

#### 139 — Public-site honesty sweep (cross, S) — **docs only, lands first**

Fixes: *Stale stats and a stale test comment on the public surface.*

- `landingSpec.ts:191` + `:271` — **109 → 110** canonical methods; **drop the
  "at v1.6" qualifier** on :191 (the count is current, not v1.6-pinned). (The
  +1 is `sessions.delete`, shipped in v1.7.0; the canonical count is pinned at
  110 by `methods_test.go` and `methods.md`.)
- `landingSpec.ts:236` — **retarget the hot-reload softening here** (NOT
  README:94, which has no hot-reload claim). "Local Runtime, Protocol server,
  and hot reload on your laptop" → qualify to config/YAML reload (the genuine
  capability), since `.go` reload is honest-WARN-only after 138.
- `landingSpec.ts:41` — remove the unprinted/cosmetic "3 drivers registered"
  from the dev banner string.
- **Drop** the original plan's "soften 'in under a second' → 'boots quickly'"
  directive — **that string does not exist** anywhere in `landingSpec.ts`
  (verified). No edit.
- **Drop** the original plan's "README.md:94 hot-reload qualified YAML-only"
  directive — **README has zero hot-reload mentions** (verified). No edit.
- `cmd_dev_hot_reload_test.go:20-24` — the stale reference to the
  non-existent `test/integration/phase65_hot_reload_test.go` → point at the
  in-package tests (pattern per `wave12_test.go:33-38`).
- README serve (`192-194`) — **no change needed**: it already says "mints no
  token" and points at `identity.jwks_url`/`jwks_file`. Once 131a/131d land,
  optionally add a one-line forward pointer to the production-identity guide.

Gate: VitePress build; `grep` confirms no stale `phase65_hot_reload_test.go`
reference and the `110` count on the landing surface.

### Stage 2 — discoverability + protocol tooling + verification

#### 134 — `sdk` `Example_` functions (embed, M) · Dep 132 (and 132-stream)

Fixes: *No runnable `Example_` in `sdk/assemble`, `sdk/planner`,
`sdk/steering`, `sdk/config`* (and `sdk/` has no `_test.go` files at all —
these are the first). Mock-LLM-backed, `sdk/`-only imports, prefer
`Stack.RunOnce` (incl. one `WithStream` example). Gate:
`go test ./sdk/... -run Example` + the existing no-internal-imports gate.

#### 135 — TS wire-type generator + `event-viewer-ts` (protocol, M) — **P1** (D-269)

Fixes: *TS generation deferred (D-132).*

- **Open question (135 scope) resolved: external-client types emitter under a
  DISTINCT name; the reserved slot stays reserved.** The name
  `cmd/harbor-gen-protocol-ts` is **reserved by §4.5(5) + D-223** for the FULL
  Console-`protocol.ts` generator (the deferred deliverable) — 135 must NOT
  consume it with a partial generator. Ship instead
  **`cmd/harbor-protocol-ts-types`**: it reflects over `CanonicalWireTypes` to
  emit a **vendorable external-client TS types module** (consumed by
  `event-viewer-ts` and copy-vendored by third-party clients). It does **not**
  touch the Console's hand-maintained `protocol.ts` or the **D-223
  manifest-verification gate** (`cmd/harbor-protocol-ts-lockstep` +
  `wire-manifest.gen.json` + `make protocol-ts-gen[-check]`) — those stay
  exactly as shipped. Add **new** make targets (`protocol-ts-types-gen` /
  `-check`). This **partially retires D-132** for external clients; the full
  Console-`protocol.ts` generation and the reserved `cmd/harbor-gen-protocol-ts`
  name remain deferred.
- **§4.5(5)/D-223 reconciliation (same PR):** add an amendment note to
  `docs/decisions.md` (under D-269) recording that the reserved
  `cmd/harbor-gen-protocol-ts` slot is **still reserved** for the full Console
  generator, and that `cmd/harbor-protocol-ts-types` is the distinct
  external-client emitter — so the reservation and the D-223 gate are not
  silently overridden.
- `examples/protocol-clients/event-viewer-ts/` consumes the generated types
  against the dev runtime.
- **§18 skill drift (same PR):** `use-the-harbor-protocol/SKILL.md` asserts
  the generator is deferred in **three** places — **lines 17, 286, 354** — all
  three must be updated to reflect the new external-client emitter (while
  noting the full Console generator stays deferred). Update
  `build-a-client.md:162-165` (the D-132 deferral note) too.

**Decision D-269:** `cmd/harbor-protocol-ts-types` (a distinct binary, NOT the
§4.5(5)/D-223-reserved `cmd/harbor-gen-protocol-ts`) generates a vendorable
external-client TS wire-type module under its own make targets; the D-223
manifest gate and the Console's hand-maintained `protocol.ts` are unchanged;
D-132 is partially retired for external clients; the full Console generator
stays deferred and its reserved name stays reserved.

Gate: smoke builds + runs `event-viewer-ts` against the dev runtime; the new
gen-check is green; `make protocol-ts-gen-check` (D-223) still green.

#### 136 — MCP agent-calls-tool integration test (cli, M)

Fixes: *MCP reaches the catalog but the agent-call leg (planner invoking an
MCP tool through the executor) is unverified* — the only executor-level
tool-invocation test today (`phase83l_real_bifrost_test.go`) calls an
in-process **builtin**, not an MCP-sourced tool.

Real stdio MCP server (the existing `cmd/harbor-mcptest-stdio` fixture),
devstack with the server in config, a goal driving `RunLoop.Run` to invoke
`mcptest_echo`, asserting dispatch **through the executor** (§17.8
spec-derived fixture).

Gate: a **net-new, explicitly named** test — `go test ./test/integration -run
'TestE2E_Phase83g_MCPAgentCallsTool' -race`. (The existing 83g test is
`…ReachTheCatalog`; the original `…Phase83g.*Call` regex matched **zero**
tests — a SKIP-that-should-be-OK / false-green hazard. Pin the exact name and
add a `-list`/no-match-fails guard so the gate cannot silently match nothing.)

#### 137 — Conformance worked example, in-tree (protocol, M)

Fixes: *No runnable worked example of wiring a custom `Factory` + `RunSuite`.*

**Scope correction:** the conformance suite stays under
`internal/protocol/conformance` — it is **deliberately not externally
importable** (D-210), and `RunSuite` is `*testing.T`-bound, so the "example"
is a **`_test.go` harness compiled via `go test`**, not a runnable client
binary like `event-viewer`. This phase ships an **in-tree** worked example
(`examples/protocol-clients/conformance-fork/` as a `_test.go` wiring a custom
`Factory` + `RunSuite`) **+ a cert-page pointer**. It reinforces the
already-documented in-tree Factory-seam posture (D-210 honest) — it does
**not** make the suite externally importable (that would be a §3-layout / RFC
change this wave does not propose). The exec-summary framing reflects this.

Gate: `scripts/smoke/phase-137.sh` — the example compiles + runs under
`go test`.

### Wave-end

#### 140 — Wave E2E + v1.8.0 checkpoint audit (cross, M) · Deps 131c, 131d, 132, 132-stream, 133, 135, 136, 137, 138

(Deps lists the *surface-consuming* phases the E2E imports; the audit-only /
docs-only phases — 131b, 134, 139 — are gated by §6 staging, not by 140's
import graph.)

`test/integration/wave_v18_test.go` (the v1.8 wave-end test, matching the
`wave_v17_test.go` precedent):

- **Embed leg:** a goal via `Stack.RunOnce` (+ a `WithStream` ordered-chunk
  assertion), real drivers.
- **Protocol leg:** the **binding** mock-OIDC → `serve` → `runtime.info` +
  scopes round-trip, **plus** the `harbor token` keygen → `jwks_file` →
  `serve` → `runtime.info` bring-your-own leg (both on-ramps proven), and an
  asserted `401` on a mismatched-`iss`/`aud` token.
- **MCP-tool-dispatch leg:** an in-process `RunLoop.Run` goal invoking an MCP
  tool (136 surface) reaching the executor. (The `harbor` **CLI-binary** path
  is gated by the 131d / 133 / 138 smokes against the preflight server, per
  §4.1/§17.4 — the Go E2E does not spawn the binary.)
- **≥1 failure mode** (§17.3): bad token rejected `401`; missing-identity
  rejected; `serve` still mints nothing under D-220.
- Identity propagation asserted end-to-end; `-race`; N≥10 concurrency stress.

Then the read-only §17.5 checkpoint audit → one
`chore(checkpoint): v1.8.0 audit fixes` PR. Flip `docs/plans/README.md` rows
(131a…139 → Shipped) + the root `README.md` Status table; author the
**`[1.8.0]`** CHANGELOG entry. **Gates the next band's scoping.**

---

## 5. Honesty actions

| Claim (source) | Reality | Action |
|---|---|---|
| "109 canonical methods at v1.6" (`landingSpec.ts:191,271`) | stale — 110, not v1.6-pinned | 139: 109 → 110, drop "at v1.6" |
| "hot reload on your laptop" (`landingSpec.ts:236`) | partial — config/YAML reload only; `.go` is honest-WARN after 138 | 139 soften + 138 fix-code |
| dev banner "3 drivers registered" (`landingSpec.ts:41`) | cosmetic/unprinted | 139: remove |
| `harbor serve` "mints no token … `jwks_url`/`jwks_file`" (`README:192-194`) | **honest** | none (optional forward pointer to 131a once it lands) |
| No TS generator (`build-a-client.md:153-171`; `use-the-harbor-protocol` SKILL:17,286,354) | deferred (D-132) | 135 fix-code, then update doc **+ all 3 SKILL lines** |
| serve attach path for no-IdP adopters | **missing** | 131a guide + 131d `harbor token` on-ramp |
| `harbortest` five-function surface (`README:216-224`) | honest | none |
| `assemble.Assemble` one call (`README:67-76`) | honest | none (running a goal is 132 friction, not a false claim) |
| recipe executed by test (`README:79-82`) | honest | none — keep true by test-gating the new 4a shorthand |
| Conformance not importable (`conformance-certification.md`) | honest (D-210) | none — 137 adds an in-tree example + pointer |

(The original plan's "soften 'in under a second'" and "README:94 hot-reload"
rows are **dropped** — neither string exists.)

---

## 6. Sequencing (§17.7 waves)

**Stage 1 (dispatch now):** 139 (fastest, docs-only) · 131a → then 131b +
131c + 131d (the production-identity cluster; 131b/c/d all dep 131a) · 132 →
132-stream · 133 · 138. 131c carries the **binding** mock-OIDC → serve
round-trip smoke; 131d carries the `harbor token` → `jwks_file` → serve smoke.

**Stage 2 (after Stage 1 merges):** 134 (hard-dep 132 + 132-stream) · 135 ·
136 · 137. Optional background `chore` sweep of audit WARN/NITs between stages.

**Wave-end:** 140 bundles `wave_v18_test.go` with the final Stage-2 merge, then
the §17.5 checkpoint audit PR. **Do not scope the next band until 140 merges.**

**Primitive-with-consumer (§13):** 132 ships `NewRunContext` + `RunOnce`
together; 132-stream ships the `WithStream` sink + its end-to-end test; 131a
ships with 131c's worked client + smoke and 131d's `harbor token` consumer
in-wave; 135 ships the generator with `event-viewer-ts`.

**Intra-stage dependency note:** 131b/131c/131d each dep 131a (claim-shape +
guide), and 132-stream deps 132 — so Stage 1 is really **131a first**, then
131b/c/d in parallel, with 132 → 132-stream as a 2-step chain alongside
133/138/139. Confirm staging with the coordinator before dispatch (§17.7 step 2).

---

## 7. Open questions — all resolved before dispatch

1. ~~Gated dev-token mint inside `harbor serve`?~~ **RESOLVED:** no — serve's
   verifier stays IdP-/JWKS-config-driven (D-220 preserved). Two on-ramps:
   real IdP (131a/131c) + `harbor token` self-issuing (131d, D-264). See §3.
2. **Mock-OIDC for 131c/140:** in-test ES256 JWKS issuer (hermetic, CGo-free,
   in `testfixtures`) — **confirmed in-test** (no external container; it is a
   fixture, not a user-facing quickstart).
3. ~~135 scope: full vs thin generator?~~ **RESOLVED: thin + non-clobbering**
   — new make targets, the D-223 manifest gate untouched, D-132 partially
   retired (D-269). See 135.
4. ~~132 scope: `RunOnce` only, or streaming too?~~ **RESOLVED:** streaming is
   REQUIRED, shipped as a **split** 132-stream phase (`WithStream` sink on the
   same blocking `RunOnce`; planner `OnChunk`/`OnToolDispatched` seam; D-266).
5. ~~Version label v1.7.1 vs v1.8.0?~~ **RESOLVED: v1.8.0** — v1.7.0 is tagged;
   additive public API ⇒ next minor. See "Version label" above.
6. ~~138: `policy: rebuild-binary` in scope?~~ **RESOLVED: deferred** — WARN +
   guidance only this phase (D-268).

---

## 8. Evidence anchors (verified in the live tree, 2026-06-26)

- `grep -r "func Example" sdk/` → empty; `find sdk -name '*_test.go'` → empty
  (134 still open).
- `ls cmd/` → `harbor`, `harbor-gen-protocol-docs`, `harbor-mcptest-stdio`,
  `harbor-protocol-ts-lockstep`; `harbor-gen-protocol-ts` is **reserved**
  (§4.5(5)/D-223, stays unused this wave) — 135 ships the **distinct**
  `harbor-protocol-ts-types`. `harbor-mcptest-stdio` **exists** (136's fixture
  is present).
- `ls examples/protocol-clients/` → only `event-viewer` (131c, 131d, 135, 137
  still open).
- `ls docs/skills/` → no `configure-production-identity` (131b open).
- `ls docs/site/protocol/` → no `production-identity-setup.md` (131a open).
- `grep "func (s *Stack)" internal/runtime/assemble/assemble.go` → only
  `Close` (132 open).
- `grep -ni "hot.reload" README.md` → empty (139: README is **not** a
  hot-reload edit target; `landingSpec.ts:236` is).
- `landingSpec.ts` → `:41` "3 drivers registered", `:191/:271` "109",
  `:236` "hot reload on your laptop"; **no** "in under a second" string.
- `methods_test.go` + `docs/site/protocol/methods.md` → canonical count
  **110** (139's 109 → 110 target).
- `Makefile` → `protocol-ts-gen` / `protocol-ts-gen-check` already exist
  (D-223 manifest gate); 135 must not clobber them.
- `cmd/harbor/cmd_dev_hot_reload.go:28` → dangling "This is documented in."
  (138 completes it).
- `README:194` → `harbor serve` verifies against `identity.jwks_url` /
  `jwks_file`, "mints no token" (131d's `jwks_file` attach path is
  already-supported config; no serve code change).

---

## 9. §16 placement ritual + dispatch checklist (§17.7 step 3)

Each dispatched worktree agent operates **only inside its own worktree**
(`pwd` first; STOP if a path resolves outside it). Per §16, each agent: copies
`docs/plans/_template.md` → `docs/plans/phase-NN-slug.md` and fills every
section; **creates a new `scripts/smoke/phase-NN.sh` ONLY when its Gate names a
new one (131c → `phase-131c.sh`, 131d → `phase-131d.sh`, 137 → `phase-137.sh`)
— otherwise it EXTENDS the existing smoke its Gate cites (132/132-stream/133 →
`phase-112b.sh`, 138 → `phase-65.sh`, 131a/131b → `phase-103.sh`), and the
non-smoke gates (134 `go test ./sdk/...`, 136 `go test ./test/integration`,
139 VitePress build) add no smoke** — using `scripts/smoke/common.sh` helpers
only; appends its `docs/plans/README.md` index row + detail block; appends its
pre-assigned `D-NNN` block to `docs/decisions.md` (**markdownlint-clean** —
blank lines around `---` and `## D-NNN`; run `markdownlint-cli2` repo-wide);
adds glossary terms; updates any §18 skill that quotes a changed surface in
the same PR; regenerates the relevant gates (135 → its new TS-types
generator + manifest check; docs phases → VitePress); and runs
`make drift-audit` + `make preflight` green before committing.

Each dispatch prompt MUST carry: the master-plan detail block; the mandatory
reading list (relevant CLAUDE.md §§, cited RFC sections, predecessor plans in
`Deps`, informing briefs); the §16 workflow; the validation gate; the
**pre-assigned `D-NNN`** (D-263…D-269 per the §4 table); the **workspace
warning**; and the **markdownlint hygiene reminder**.

**RFC-surface phases (binding).** A phase that adds a new top-level CLI
subcommand updates the RFC §8 subcommand enumeration in the same PR
(RFC-001:1317 — "CLI subcommand additions are an RFC update, not a casual
change"). In this wave that is **131d** (`harbor token keygen`/`mint`). The
pre-existing `serve`/`console`/`init`/`skill` §8 drift is noted as an optional
follow-up — this plan does not bless skipping the rule for new subcommands.

**Godoc-visible-source discipline (§13).** No `Phase NN` / `phase-NN`, inline
`D-NNN`, `brief NN`, or wave-band references in non-test Go source under
`internal/` or `cmd/`. Name the FEATURE, not the number. `_test.go` files,
plan prose, and decisions entries may reference numbers freely.
