# Phase 131d — harbor token (bring-your-own-issuer subcommand)

## Summary

Ship `harbor token`, a new CLI subcommand that lets an operator with **no
external identity provider** self-issue the JWTs `harbor serve` verifies.
`harbor token keygen` generates an asymmetric keypair and emits a public JWK
Set; `harbor token mint` self-issues a Harbor JWT signed with that key. The
operator points `identity.jwks_file` at the emitted JWK Set and attaches —
`harbor serve` is unchanged and still mints nothing (D-220 preserved; D-264).

## RFC anchor

- RFC §5.5
- RFC §8

<!-- Security posture (asymmetric algorithms only, no hardcoded secrets, never
log the private key/token) follows CLAUDE.md §7; the RFC anchor cites the
Authentication (§5.5) and CLI-layer (§8) sections this phase implements. -->

## Briefs informing this phase

- brief 06
- brief 09

## Brief findings incorporated

- brief 09 (OAuth/identity edge): *"AccessToken … never logged, never emitted on
  bus; RefreshToken … never logged"* — token / key secret material never
  reaches a log or an event. `harbor token` honours this: the private key is
  never logged or printed (only its path is surfaced); the minted token reaches
  stdout (it is the command's product) but is never slog'd.
- brief 09: *"Identity-scoped JWT enforcement … the JWT's identity scope"* and
  the asymmetric-only signing posture — the minted token carries the mandatory
  `(tenant, user, session)` triple the verifier rejects a token without, and is
  signed with an asymmetric algorithm (ES256/RS256) on the §5.5 allowlist
  (never HS\*/`none`).
- brief 06 (events/observability/devx): *"unlocks remote attach … IDE/TUI
  integrations"* — first-attach is the operator-DX moment this subcommand
  serves: a no-IdP operator can obtain a verifiable token and attach a client
  without writing JWT-signing code.

## Findings I'm departing from (if any)

None.

## Goals

- Give a no-IdP / self-issuing operator a first-class path to obtain a token
  `harbor serve` accepts, without minting inside serve and without an external
  provider.
- Keep `harbor serve`'s auth edge unchanged: it trusts the self-issued key only
  via operator-configured `identity.jwks_file` (a single keyset; no composite).
- Enforce a least-privilege, fail-loud posture: asymmetric keys only, private
  key 0600 under a 0700 dir, mandatory `--issuer`/`--audience`, no scopes by
  default, short TTL by default.

## Non-goals

- No change to `cmd/harbor/cmd_serve.go` or the verifier (`internal/protocol/auth`).
- No multi-key / key-rotation tooling, no token revocation list, no OAuth
  token-exchange (RFC 8693) flow — those belong to the real-IdP path.
- No runtime/Protocol surface: `harbor token` is a CLI-only, offline subcommand.

## Acceptance criteria

- [ ] `harbor token keygen --out <dir>` writes `private.pem` (mode 0600, parent
  dir 0700; refuses overwrite without `--force`; stderr "keep this out of
  version control" warning) and a valid RFC 7517 `jwks.json` whose `kid` is the
  RFC 7638 JWK thumbprint.
- [ ] `--alg` selects ES256 (default) or RS256; both emit a verifier-parseable
  JWK (EC `crv`/`x`/`y`, RSA `n`/`e`, base64url).
- [ ] `harbor token mint --key … --tenant … --user … --session … --issuer …
  --audience …` mints a JWT with the claim shape the parser
  (`internal/protocol/auth/auth.go`) enforces; `--issuer`/`--audience` are
  mandatory; default mint carries **no** scopes; `--ttl` defaults to 1h and is
  echoed to stderr.
- [ ] The REAL production validator (`auth.NewJWKSKeySet` file source +
  `auth.NewValidator` with the matching issuer/audience) **accepts** a matching
  minted token and **rejects** a mismatched-`iss` and a mismatched-`aud` token
  (the 401 contract at the edge).
- [ ] `cmd/harbor/cmd_serve.go` is unmodified; `harbor serve --help` still
  states it "mints no token".
- [ ] No `Phase NN` / `D-NNN` / `brief NN` references in the new non-test Go.

## Files added or changed

- `cmd/harbor/cmd_token.go` (new) — the `token` command + `keygen`/`mint` verbs.
- `cmd/harbor/token_crypto.go` (new) — keypair gen, PEM I/O, JWK Set emitter +
  RFC 7638 thumbprint, signing-method dispatch.
- `cmd/harbor/tokenclaims.go` (new) — the shared `harborClaims` claim shaper
  (the ONLY piece shared with the dev signer).
- `cmd/harbor/devauth.go` (changed) — `SignDevToken` now calls `harborClaims`.
- `cmd/harbor/root.go` (changed) — register `newTokenCmd`; help group line.
- `cmd/harbor/cmd_token_test.go` (new) — keygen/mint + real-parser round-trip.
- `cmd/harbor/testdata/golden/help.txt` (regenerated) — adds the `token` row.
- `RFC-001-Harbor.md` (changed) — §8 subcommand enumeration + Settled bullet.
- `examples/serve.yaml` (changed) — `jwks_file` + `harbor token` walkthrough.
- `docs/site/protocol/production-identity-setup.md` (changed) — On-ramp B body.
- `docs/decisions.md` (changed) — D-264.
- `docs/glossary.md` (changed) — JWK Set / JWK thumbprint / bring-your-own-issuer.
- `docs/skills/use-the-harbor-protocol/SKILL.md` (changed if it shows the step).
- `scripts/smoke/phase-131d.sh` (new).

## Public API surface

N/A — `harbor token` is an operator-facing CLI surface, not a Go API other
phases import. The only cross-package dependency is the existing
`internal/protocol/auth` verifier, consumed unchanged by the round-trip test.

## Test plan

- **Unit:** keygen perms (0600/0700), overwrite-without-`--force` refusal,
  thumbprint determinism + kid match, default-mint-has-no-scopes,
  scopes-granted-when-requested, ES256 + RS256 branches.
- **Integration:** §17.8 real-parser round-trip — a keygen-produced JWK Set
  loaded by the REAL `auth.JWKSKeySet` (file source) + `auth.NewValidator`;
  a mint-produced token is **accepted** when iss/aud match and **rejected**
  when iss or aud mismatch (the edge's 401). Real verifier, real audit
  redactor on the seam, identity propagation asserted (the triple round-trips).
- **Conformance:** N/A — no new driver-backed interface.
- **Concurrency / leak:** N/A — keygen/mint are one-shot CLI invocations that
  build no shared reusable artifact; each call owns its signer on the stack.

## Smoke script additions

`scripts/smoke/phase-131d.sh` (`live-server` class, to guarantee the binary is
built; it boots no shared server):

- `harbor token keygen` → assert `private.pem` is 0600 and `jwks.json` is valid
  RFC 7517 (jq: `.keys[0].kty` present).
- `harbor token mint` (matching iss/aud) → decode the JWT, assert the claim
  shape (`tenant`/`user`/`session`/`iss`/`aud`/`exp`).
- default mint (no `--scopes`) → assert the decoded claims carry **no** `scopes`
  (non-admin / least privilege).
- the real-parser accept + mismatched-`iss`/`aud` **401-equivalent** rejection
  via `go test ./cmd/harbor -run TestTokenRoundTrip` (the always-on proof).
- `harbor serve --help` still contains "mints no token" (D-220 intact) and
  exposes no mint flag; `cmd/harbor/cmd_serve.go` carries no mint call.
- the optional live `harbor serve` + `runtime.info` round-trip is env-gated
  (`HARBOR_LIVE_*`) because serve demands a real LLM provider at boot.
- degradation: a build without the `token` subcommand SKIPs (the 404/405/501 →
  SKIP convention applied to a CLI surface).

## Coverage target

- `cmd/harbor` (the new token files): ≥ 80% on the keygen/mint/crypto paths.

## Dependencies

- 131a (production-identity setup guide — the claim-shape docs + On-ramp B
  placeholder this phase fleshes out).

## Risks / open questions

- A self-issued single signing key is a small-production / eval posture: a
  leaked private key forges any identity. Mitigated by 0600/0700, the stderr
  warning, and the explicit "graduate to a real IdP for multi-user SSO" callout
  in the docs + `harbor token --help`. The verifier's max-stale ceiling
  (D-261) and short default TTL bound exposure.
- The full live `harbor serve` HTTP 401 round-trip needs a real LLM provider at
  boot (serve does not honour the mock escape hatch); CI proves the accept/401
  contract through the real verifier in a Go test instead, with the HTTP leg
  env-gated.

## Glossary additions

- JWK Set
- JWK thumbprint
- bring-your-own-issuer

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A, no storage path touched; the minted triple is asserted to round-trip through the real verifier.
- [ ] **If this phase builds a reusable artifact … concurrent-reuse test passes.** N/A — keygen/mint build no shared reusable artifact; each invocation owns its signer on the stack.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists …** The real-parser round-trip test consumes the unchanged `internal/protocol/auth` verifier end-to-end, asserts identity propagation, covers the iss/aud-mismatch failure mode, and runs under `-race`.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — N/A (no departures).
