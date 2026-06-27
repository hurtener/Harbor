# Phase 131c — Worked OIDC client + serve round-trip smoke

## Summary

Ships `examples/protocol-clients/oidc-client-example/` — a SDK-free,
stdlib-only worked Harbor Protocol client that runs the OAuth2
client-credentials grant against an identity provider, obtains a JWT, and
attaches it to a production `harbor serve` instance (`runtime.info`). It is
the load-bearing P0 **consumer** of the v1.8.0 Adopter-Path wave: the
runnable proof that 131a's documented IdP on-ramp works end to end. The
binding gate `scripts/smoke/phase-131c.sh` drives a hermetic ES256/JWKS
mock-OIDC issuer → `harbor serve` → `runtime.info` round-trip, proving the
production OAuth2/JWKS path against the REAL parser.

## RFC anchor

- RFC §5.5 (Authentication — the Protocol's JWT verification surface; the
  identity triple lives in the JWT claims; the Protocol rejects any
  request without an identity scope).
- RFC §5.4 (Wire transport — the `POST /v1/control/{method}` REST surface
  the worked client calls `runtime.info` over).
- RFC §8 (CLI layer — `harbor serve` is the production JWKS verifier this
  example attaches to).

## Briefs informing this phase

- brief 07
- brief 09

## Brief findings incorporated

- brief 07 (the Harbor Protocol as the integration surface): the Console —
  and any third-party client — is a Protocol client that talks to the
  Runtime over the canonical wire, never via an internal Go import. This
  phase's example is the literal demonstration: a client in pure stdlib,
  no Harbor import, attaching over HTTP + a JWT. The phase smoke
  compile-gates the SDK-free guarantee so it cannot silently rot.
- brief 09 (MCP OAuth lessons — asymmetric verification, the token is
  obtained from the issuer and verified against its published keys, never
  minted by the resource server): the worked client obtains its JWT from
  the IdP's token endpoint via the client-credentials grant; `harbor
  serve` verifies it against the IdP's published JWK Set and mints
  nothing. The mock-OIDC fixture signs with the same `golang-jwt`
  library + JWK shape the production verifier consumes (CLAUDE.md §17.8),
  so a wrong field fails the verifier rather than rubber-stamping.

## Findings I'm departing from (if any)

None.

## Goals

- A genuine, real-framework-quality worked example an adopter copies to
  obtain a JWT from their OIDC provider and attach a custom client to
  `harbor serve`.
- A binding production-leg smoke proving the OAuth2/JWKS path end to end
  against the real parser, with the mock-OIDC issuer mandatory.
- Mechanically enforce the SDK-free guarantee (no `sdk/` or `internal/`
  Harbor import in the example's dependency graph).

## Non-goals

- A user-facing mock-OIDC quickstart. The mock-OIDC issuer is an in-test
  fixture only; the production self-issuing on-ramp for an operator with
  no IdP is the `harbor token` subcommand (131d), and a real deployment
  fronts a real IdP (Auth0 / Okta / Keycloak / Cognito).
- Any change to `harbor serve`'s verifier. D-220 is preserved — serve
  mints nothing; it verifies whatever JWKS the operator configures.
- Exercising the planner / LLM. The example's headline is the attach
  handshake (`runtime.info`), which never invokes the LLM.

## Acceptance criteria

- [ ] `examples/protocol-clients/oidc-client-example/` exists, compiles,
  and is SDK-free (no `github.com/hurtener/Harbor/sdk` and no
  `github.com/hurtener/Harbor/internal` package in its dep graph).
- [ ] The example runs the OAuth2 client-credentials grant
  (`client_secret_basic`), obtains a JWT, and calls `runtime.info` with a
  `Bearer` token.
- [ ] `scripts/smoke/phase-131c.sh` (new) drives the binding round-trip:
  an in-test ES256/JWKS mock-OIDC issuer → `harbor serve` pointed at its
  JWKS → the example obtains a token → `runtime.info` OK with the granted
  scope surfaced.
- [ ] The mock-OIDC fixture mints tokens the REAL parser accepts (signs
  with `golang-jwt/jwt/v5` ES256, publishes a JWK in the shape
  `internal/protocol/auth/jwks.go` parses).
- [ ] A failure-mode leg: a token signed by a key absent from the JWKS is
  rejected by the verifier and the example exits non-zero.
- [ ] The 404/405/501 → SKIP convention holds: on a pre-`harbor serve`
  build the round-trip test self-skips and the smoke still passes.

## Files added or changed

- `examples/protocol-clients/oidc-client-example/main.go` (new) — the
  SDK-free worked client.
- `test/integration/mockoidc_test.go` (new) — the hermetic ES256/JWKS
  mock-OIDC issuer fixture (reused by the wave-end E2E).
- `test/integration/oidc_serve_roundtrip_test.go` (new) — the binding
  round-trip + failure-mode integration test.
- `scripts/smoke/phase-131c.sh` (new) — the gate.
- `docs/plans/phase-131c-oidc-client-example.md` (this file).
- `docs/plans/README.md` — index row + detail block.
- `docs/glossary.md` — "client-credentials flow".

## Public API surface

None. This phase ships an example program + tests + a smoke; it adds no
Go package other phases depend on, no Protocol surface, and no config
schema.

## Test plan

- **Unit:** N/A — the example is exercised end-to-end (no isolated unit
  surface worth a unit test beyond the integration leg).
- **Integration:** `test/integration/oidc_serve_roundtrip_test.go` —
  real production JWKS validator + real control transport + real
  `bin/harbor serve` subprocess + the built example binary. Identity
  propagates as the `(tenant, user, session)` JWT claim triple verified
  at the Protocol edge. ≥1 failure mode: an unpublished-key token is
  rejected. Runs under `-race`.
- **Conformance:** N/A — no new driver.
- **Concurrency / leak:** N/A — the example is a one-shot client, not a
  reusable compiled artifact.

## Smoke script additions

`scripts/smoke/phase-131c.sh` (unit-tests class) asserts:

- The example exists.
- The example is SDK-free (`go list -deps` carries no `sdk/` or
  `internal/` Harbor package).
- The example compiles.
- The binding round-trip Go test passes (mock-OIDC → serve →
  `runtime.info` OK + unpublished-key rejection); self-SKIPs cleanly on a
  pre-`harbor serve` build.

## Coverage target

N/A — example program + integration test + smoke; no production package
gains or loses coverage. The integration test is the binding gate.

## Dependencies

- 131a (the production-identity setup guide whose IdP on-ramp this example
  realizes). Code-wise the example depends only on the already-shipped
  `harbor serve` JWKS verifier (Phase 115) and the control transport.

## Risks / open questions

- The integration test builds `bin/harbor` + the example and boots a
  `harbor serve` subprocess — heavier than a static smoke. Mitigated by
  the `unit-tests` preflight class (parallel batch) and a build-once
  `sync.Once`.
- The LLM provider boots with a literal dummy key so `harbor serve`
  constructs without a real provider; `runtime.info` never invokes the
  LLM, so no real completion is attempted. If a future change makes the
  LLM driver probe connectivity at boot, this test's config needs a stub
  endpoint.

## Glossary additions

- **client-credentials flow** — added to `docs/glossary.md`.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target (N/A — example + test)
- [x] If multi-isolation paths changed: cross-session isolation test passes (N/A — no isolation code changed; identity flows as JWT claims through the existing verifier)
- [x] If this phase builds a reusable artifact: concurrent-reuse test passes — N/A: the example is a one-shot client, not a reusable compiled artifact.
- [x] If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists — `test/integration/oidc_serve_roundtrip_test.go` wires the real JWKS verifier + control transport + `harbor serve` end-to-end, asserts identity propagation via the JWT triple, covers the unpublished-key failure mode, and runs under `-race`.
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry filed (N/A — none departed; reaffirms D-263, no new decision)
