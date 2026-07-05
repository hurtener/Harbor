# Phase 154 — OAuth provider credential source: env or coordinator-served pull

## Summary

A `tools.oauth_providers[]` entry's client credential is resolved from the process env exactly once at boot (`BuildProviders` fails loud on an empty env var), so a broker credential minted by a coordinator AFTER the runtime booted can never reach it — the operator must export the envs and reboot once. This phase adds a `credential_source` seam to the provider entry: `env` (today's behavior, the default) or `remote` (the runtime PULLS its client `client_id`/`client_secret` from a coordinator-served endpoint, authenticated by the runtime's own service token, at first need — TTL-cached in memory, single-flight, fail-loud). This turns one-reboot provisioning into zero-touch, removes the broker secret from the runtime's environment entirely (defense-in-depth: the secret lives only in the coordinator's vault), and mirrors D-271's own PULL model — a push of the credential over the Protocol stays rejected as credential passthrough.

## RFC anchor

- RFC §6.4
- RFC §3.3

## Briefs informing this phase

- brief 09
- brief 03

## Brief findings incorporated

- brief 09 §3: OAuth client credentials never ride application-level channels — provisioning is either operator-static (env) or an authenticated fetch from the credential authority; the bifrost lessons show in-band credential delivery is where confused-deputy bugs breed.
- brief 09 §6: token/credential caches are memory-only with TTL + single-flight; a persisted copy of an externally-owned credential creates a revocation hole (the same reasoning D-271 records for brokered tokens applies one level up to the broker client credential itself).
- brief 03 §4: tool-auth failures surface as typed errors to the run — never a silent unauthenticated fallback.

## Findings I'm departing from (if any)

- None.

## Goals

- A provider entry can declare `credential_source: remote` with a `remote:` block (`url`, `auth_token_env`, optional `cache_ttl`, `timeout`); `env` remains the default — existing configs are byte-compatible (§10 backward compatibility).
- With `remote`, boot validates the block's SHAPE (url well-formed, auth-token env var non-empty) but resolves the client credential lazily at first use — a coordinator-minted credential reaches an already-running runtime with zero touch.
- The fetch contract is documented as a stable Harbor-defined shape (request: authenticated GET; response: `client_id`, `client_secret`, optional `expires_in`) so any coordinator can serve it.
- Every remote fetch outcome is observable: success emits a new canonical SafePayload audit event (zero secret bytes); failure fails the tool call loud with a typed sentinel — never a silent fallback to env or to an unauthenticated call.

## Non-goals

- No hot-reload of the `tools.oauth_providers[]` LIST itself (adding/removing provider entries still requires a restart; the entry is declared up front, only its credential resolves late). A config-reload surface is out of scope and, for the secret leg, incoherent (a running process's env is fixed at exec).
- No Protocol verb that carries a credential (rejected by D-271 — recorded, not re-litigated).
- No change to the interactive `oauth2` driver's flow, the TokenStore, the KEK/Sealer chain, or `WrapWithOAuth` (the provider instance is still constructed at boot and captured by the catalog wrapper — only cred resolution inside it is late).
- No coordinator implementation — Harbor ships the client side + a test fixture server; the coordinator (the second consumer) implements the serve side against the documented contract.

## Acceptance criteria

- [x] `config.ToolOAuthProviderConfig` gains `credential_source` (`env` default | `remote`) + the `remote` block; `config.Validate` requires exactly the fields the declared source needs (env source: today's rules unchanged; remote source: url + auth_token_env, and rejects a populated `client_id_env`/`client_secret_env` alongside `remote` — one source, no dual path, §13).
- [x] A §4.4 seam: credential-source interface + `env` and `remote` drivers + factory dispatch by name; drivers self-register; blank imports live in `internal/drivers/prod` (D-196); factory error lists registered sources.
- [x] `BuildProviders` threads the source into provider construction; the `env` source resolves at build time with today's exact fail-loud messages (behavioral no-op for existing configs, asserted by test).
- [x] The `remote` source: authenticated fetch (Authorization: Bearer from `auth_token_env`) with timeout; response parsed strictly; credential held memory-only with TTL (default derived from `expires_in`, capped by `cache_ttl`); single-flight per provider; refetch on expiry.
- [x] Failure legs are typed + loud: unreachable/non-200/malformed → a new sentinel wrapping the provider + url (no secret bytes in the error), the tool call fails, and a SafePayload failure event is emitted; there is NO fallback to env or unauthenticated operation.
- [x] The `tokenexchange` driver (and `oauth2`) consume the resolved credential through the source seam with zero behavior change when `env` is declared.
- [x] Fixture per §17.8: a local fixture credential server (the contract's reference implementation) drives an end-to-end integration test — boot with `remote` declared and NO broker envs → first tool-auth need pulls the credential → a token exchange against the local fixture broker succeeds; rotation leg: fixture swaps the credential, post-TTL fetch picks it up without restart.
- [x] The credential-fetch contract documented (docs site config/reference page) + `examples/` config gains a commented `credential_source: remote` block; CONFIG enumerations updated.
- [x] New canonical event type registered; D-209 docs regen in the same PR. No Protocol method/wire-type change → no D-223 impact (asserted in PR body).
- [x] `scripts/smoke/phase-154.sh` shows OK ≥ 1, FAIL = 0; prior smokes pass.

## Files added or changed

- `internal/config/config.go` + `internal/config/validate.go` — schema + validation.
- `internal/tools/auth/credsource/` — interface + factory (new).
- `internal/tools/auth/credsource/drivers/env/`, `.../drivers/remote/` — the two drivers (new).
- `internal/drivers/prod/` — blank imports.
- `internal/tools/auth/build_providers.go` — thread the source.
- `internal/tools/auth/events.go` — the fetch events + sentinel.
- `examples/` + docs site config reference — the documented contract.
- `RFC-001-Harbor.md` §6.4 — additive sentence on the D-271 paragraph (landed in the plans PR).
- `scripts/smoke/phase-154.sh`.

## Public API surface

- The credential-source interface (shape: `Resolve(ctx) (ClientCredential, error)`) — consumed by `internal/tools/auth` only; drivers are concrete per §4.4 rules.
- New typed sentinel (shape: `ErrCredentialSourceUnavailable`) callers can `errors.Is` against.

## Test plan

- **Unit:** validation matrix (source×fields), TTL/expiry math, strict response parsing, error redaction (no secret bytes in error strings — asserted), factory unknown-source message.
- **Integration:** the §17.8 fixture-server E2E above (real StateStore, real bus, real tokenexchange driver, identity triple asserted at the exchange); failure mode = fixture down → typed loud failure + failure event; rotation leg.
- **Conformance:** both credential-source drivers pass one shared source-conformance suite (resolve, TTL, concurrent single-flight).
- **Concurrency / leak:** N≥100 concurrent `Token()` calls against ONE provider with a slow fixture — exactly one in-flight fetch (single-flight asserted), no races under `-race`, goroutines return to baseline.

## Smoke script additions

- unit-tests: run the credsource package tests. live-server additions are N/A (the dev server does not declare a remote source); the E2E lives in the integration suite. The smoke asserts the example config parses (`harbor validate` on the example with the remote block).

## Coverage target

- `internal/tools/auth/credsource/...`: 85%; `internal/tools/auth`: keep ≥ current; `internal/config`: keep ≥ current.

## Dependencies

- 142 (`tokenexchange` — the motivating consumer), 148 (per-identity southbound OAuth binding), 30 (tool-side OAuth subsystem), 149 (config-declared manifest tools — the black-box OAuth vehicle for the E2E).

## Risks / open questions

- Misconfigured remote source now surfaces at first use instead of boot. Mitigated: boot-time shape validation + the loud typed sentinel + failure event; the plan deliberately trades boot-time certainty for zero-touch provisioning and says so in godoc.
- The fetch contract is a new Harbor-defined wire shape — versioned by a `format_version` field in the response from day one so it can evolve.

## Glossary additions

- Credential source — the term landed in `docs/glossary.md` with the
  plans PR (#458), ahead of this implementation PR; no glossary change
  rides the implementation.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] Concurrent-reuse: the remote source + wrapped provider are compiled artifacts — the N≥100 single-flight stress above under `-race`.
- [x] Integration test wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, runs under `-race`
- [x] If new vocabulary: glossary updated (landed with the plans PR #458, not this PR)
- [x] If a brief finding was departed from: justified above + decisions.md entry filed
