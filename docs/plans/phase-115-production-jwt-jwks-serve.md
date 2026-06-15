# Phase 115 — production JWT verification (JWKS) + `harbor serve`

## Summary

Give the Protocol's production authentication path a real implementation.
The `auth.JWKSURL` / `auth.JWKSFile` config fields exist but have no
consumer — the only token verifier wired today is the dev signer. This
phase builds a JWKS-backed `auth.Validator` (RS/ES asymmetric allowlist,
keyset fetch + cache + refresh) and a `harbor serve` subcommand that boots
the headless Runtime behind that production verifier, so an operator can
run Harbor with their own IdP instead of the dev bootstrap.

## RFC anchor

- RFC §5.5 — Authentication (asymmetric-algorithm allowlist; the Protocol
  never accepts a request without a verified identity scope).
- RFC §5.4 — Wire transport (the surface `harbor serve` exposes).

## Briefs informing this phase

- brief 09

## Brief findings incorporated

- brief 09 §564: *"the JWT-scope check happens at the Protocol edge … the
  scope check varies by `BindingScope`."* — JWKS verification populates
  the verified identity + scope set at the edge that every surface
  (steering, artifacts, search) already reads via `identity.From(ctx)` /
  `auth.ScopesFrom(ctx)`.
- brief 09 §565: *"the `OAuthProvider` interface is transport-agnostic
  (MCP + HTTP + A2A all share it)."* — the production verifier is wired
  once at the server boundary; no transport re-implements verification.

## Findings I'm departing from (if any)

None.

## Goals

- A JWKS-backed `auth.Validator`: fetch the keyset from `JWKSURL` (or load
  from `JWKSFile`), cache it, refresh on `kid` miss / TTL, verify with the
  existing RS/ES asymmetric allowlist (never HS\* / `none`).
- A `harbor serve` subcommand that boots the headless Runtime + Protocol
  server behind the production verifier, failing loud at boot when no
  verifier is configured (CLAUDE.md §13 — no silent fallback to the dev
  signer or a mock).
- The dev bootstrap path stays a clearly-gated dev-only escape hatch.

## Non-goals

- Token *exchange* / credential passthrough (RFC 8693) — separate work.
- Non-admin token issuance — Phase 116.
- KEK rotation / encrypted token storage (brief 09 §458 — post-V1).

## Acceptance criteria

- [x] `auth.Validator` verifies a token against a JWKS keyset; rejects
      HS\*/`none`; rejects an unknown `kid` after a bounded refresh.
- [x] Keyset fetch is cached and refreshed; a `-race` concurrent-reuse
      test covers N≥100 concurrent verifications against one Validator.
      (N=150 against one shared keyset+validator.)
- [x] `harbor serve` boots behind the production verifier and exits
      non-zero with a config-key-naming error when none is configured.
- [x] A request with a JWKS-verified token reaches a surface carrying the
      verified identity + scopes on ctx. (`test/integration/jwks_serve_test.go`.)

## Files added or changed

- `internal/protocol/auth/` — JWKS validator driver + keyset cache.
- `cmd/harbor/` — `harbor serve` subcommand.
- `internal/config/config.go` / `loader.go` — validate the JWKS fields.
- `examples/` — a production-auth example config.
- `scripts/smoke/phase-115.sh`.

## Public API surface

- `auth.NewJWKSValidator(cfg) (auth.Validator, error)` — the production
  verifier behind the existing `auth.Validator` interface (no new
  interface; the seam already exists).

## Test plan

- **Unit:** keyset fetch/cache/refresh; `kid` miss → bounded refresh;
  HS\*/`none` rejection; expired/nbf handling against a controllable clock.
- **Integration:** `test/integration/` — a JWKS-verified request flows
  through the wire transport to a surface with verified identity on ctx
  (real validator + real surface, ≥1 failure mode, `-race`).
- **Conformance:** N/A — no new Protocol method.
- **Concurrency / leak:** Validator concurrent-reuse (N≥100, `-race`); no
  keyset-cache data race; goroutine baseline restored after server stop.

## Smoke script additions

- `scripts/smoke/phase-115.sh`: `harbor serve --help` exists; a boot with
  no verifier config fails non-zero with the config-key-naming error;
  skips cleanly on builds without the subcommand.

## Coverage target

- `internal/protocol/auth`: ≥ 85%.

## Dependencies

- Phase 114 (verified-identity authority — the consumer of what JWKS
  populates), Phase 55/56 (auth scopes + `identity.From`).

## Risks / open questions

- Keyset refresh policy under a hostile / flapping IdP (bounded refresh +
  cache TTL; never unbounded fetch on every request).
- `harbor serve` vs the existing `harbor dev` boundary — `serve` is
  headless production; `dev` keeps its bootstrap escape hatch. No Console
  embedding in either (D-091 — only `harbor console` serves the Console).
- Full §16 brief pass (re-read brief 09 + RFC §5.5) when dispatched.

## Glossary additions

- `JWKS validator` — add to `docs/glossary.md` if not present.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [x] `make check-mirror` passes (no rule-file edits; AGENTS.md ↔ CLAUDE.md unchanged)
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes (integration no-identity-bleed run)
- [x] Concurrent-reuse test passes (Validator, N≥100, `-race`)
- [x] Integration test exists, real drivers, identity propagation, ≥1 failure mode, `-race`
- [x] If new vocabulary: glossary updated (`JWKS validator`)
- [ ] If a brief finding was departed from: justified + decisions.md entry (none departed)
