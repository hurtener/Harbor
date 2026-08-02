# Phase 233b — Signed OAuth MCP capability registration

## Summary

Deliver HA-50 and D-401: a production-safe, boot-authorized, admin-only Protocol operation
that atomically prepares, persists, and publishes one OAuth provider plus one
MCP connection. A generic boot broker/trust anchor keeps all credential
custody; a signed, bounded authority envelope permits a new capability without
a runtime-config edit or redeploy. It requires the broker/trust anchor's
explicit signed-capability production opt-in; it is not enabled by default.

## RFC anchor

- RFC §4.
- RFC §5.5.
- RFC §6.4.
- RFC §6.11.
- RFC §6.16.

## Briefs informing this phase

- brief 03
- brief 05
- brief 09

## Brief findings incorporated

- brief 03 §5: tool transports use one catalog/egress path and fail loudly at
  their declared security boundary.
- brief 05 §4 and §10: durable conditional state and real-driver restart/race
  tests are part of the persistence contract.
- brief 09 §5 and §7: OAuth authority is identity-bound, token custody stays
  outside ordinary tool configuration, and missing authorization is explicit.

## Findings I'm departing from (if any)

- brief 09 discusses dynamic registration as an operator-convenience path.
  D-401 narrows that path: dynamic audience and endpoint binding are admitted
  only by a boot-authorized signed capability envelope, never administrator
  input alone. This is required by D-300's credential-sink invariant.

## Goals

- Add the one canonical admin registration/creation method
  `agent_config.register_oauth_mcp_capability`; no composition of
  `set_oauth_provider` and `add_mcp_connection` can create the same pair.
- Keep one generic boot broker/trust anchor responsible for the fixed exchange
  endpoint, credential pull, runtime broker credential, KEK, true scope
  ceiling, verifier issuer/key material, and an explicit production opt-in
  permitting signed capability authority.
- The trust anchor's bounded-expiry policy is an explicit, required positive
  boot `max_authority_lifetime` (not an inferred product-wide default). The
  envelope supplies only signed `iat`/`exp`; `exp - iat` may equal but may not
  exceed that configured ceiling.
- Accept only a signed envelope that exactly binds tenant, agent, broker,
  provider/capability identifier and revision, canonical connection URL digest,
  audience, normalized scope set, issuer/key ID, issue/expiry, and anti-replay
  ID. Request fields must byte-for-byte/canonically equal their claims.
- Share one canonical URL-byte helper across envelope matching, pair
  fingerprinting, transport enforcement, and restart/reconcile.
- Preserve the verified `(tenant, user, session)` subject through cache and
  exchange. Bind cache/exchange assertions additionally to agent, capability
  revision, audience, and URL digest; exchange independently validates the
  entitlement, exact audience, and binding.
- Repair first-ever `set_oauth_provider` uncertainty with a durable
  pending-activation/compensation fence, not a process-local repair guess.

## Non-goals

- No wire-carried token URL, credential URL, client secret, credential/env
  name, KEK, or downstream-host list.
- No use of `tools.allow_wire_oauth_descriptor` or its environment counterpart
  in the production path; D-340 remains development-only.
- No change to the process-global bare-name catalog rule. A cross-owner name
  collision fails loudly; this phase does not claim tenant-isolated dispatch
  in a shared runtime.
- No generic marketplace/catalog implementation, user OAuth interaction
  redesign, or free-form authority issuer discovered from the request.

## Acceptance criteria

- [x] The sole new production registration/creation write is admin-gated
  `agent_config.register_oauth_mcp_capability`. Its request contains provider
  name, boot broker name, audience, requested scopes, a dedicated closed
  `SignedOAuthMCPConnectionDescriptor`, `expected_content_hash`, and signed
  authority envelope only. It accepts no general MCP descriptor. The dedicated
  shape is exactly `{name, url, tool_allowlist, tool_denylist,
  connect_timeout_ms, request_timeout_ms}`; strict decode and reflection pin
  that it excludes `oauth`, `oauth_provider`, `token_url`, injection, discovery
  origins, stdio command/env/cwd, headers, credentials, secrets, and every
  host/sink list. Unknown fields reject before side effects.
- [x] A boot-declared generic broker/trust anchor validates its static exchange
  endpoint, credential-pull endpoint, broker authentication, KEK, true scope
  ceiling, trusted issuer/key set, and explicit signed-capability production
  opt-in. A missing/invalid opt-in, verifier, broker, or key fails closed.
- [x] The envelope uses an approved asymmetric signature and has exact claims:
  tenant ID, agent ID, broker, capability/provider ID and immutable capability
  revision, canonical URL digest, audience, normalized unique scope set,
  issuer, key ID, issued-at, bounded expiry, and unique replay ID (JTI). The
  durable operation key is tenant-scoped `(tenant_id, trust_anchor_name, issuer,
  kid, jti)`; tenant is signed and Harbor isolation stays tenant-local. A
  reserved tenant control-scope record uses a collision-safe deterministic Kind
  from the canonical length-prefixed tuple hash. Its bounded payload repeats
  tuple hashes/fields, exact pair fingerprint, expiry, phase, and revision
  identity. One pair-lifetime record has exactly one normal graph:
  `claimed -> revision_committed -> published -> removal_revision_committed -> catalog_unpublished -> teardown_receipted -> removed`.
  Every transition `SaveIf`-compares its exact operation EventID. There is no
  generic `aborted` phase: a prepared-but-incomplete claim retries its recorded
  phase. Only `claimed` or `revision_committed` may terminally enter
  `expired_incomplete`, after safe close/compensation and a preserved
  prior/no-active activation fence; that tombstone cleans only after expiry plus
  bounded skew. No claim+revision cross-record ACID is asserted: this durable
  state machine is recovery. Exact tuple+fingerprint retries resume phase; the
  same key with a different fingerprint rejects. A `claimed` retry re-prepares;
  uncertain revision write exact-rereads active revision/fingerprint to advance,
  retry, or conflict; `revision_committed` re-prepares/re-publishes after
  restart; publish-then-checkpoint error verifies the exact live pair before
  advancing; `published` returns the original response; `removed` never
  recreates. A `published` record remains durable for the full immutable
  pair-history lifetime despite registration-authority expiry or verifier-key
  revocation, so removal/retirement resumes from the frozen fingerprint. It is
  a recovery/replay constraint rather than a bearer grant: exchange still
  validates current entitlement and exact binding. `removed` remains an
  anti-replay tombstone with pair history and never less than the authority
  expiry-plus-skew horizon, preventing recreation/replay.
- [x] A signed provider is pair-owned and outside the general `ProviderSet`.
  Private MCP preparation binds directly to that exact provider instance; a
  catalog source swap is the sole data-plane dispatch linearization point.
  Protocol projections derive only from the immutable signed-pair revision,
  never a live provider map, and generic provider resolution cannot bind it. A
  pair-owned live registry may retain close/reconcile receipts only: it is never
  an authority, projection, or dispatch surface. Name reservation still
  collision-checks the general bare namespace. Prepare is never durable and
  closes on refusal/failure/restart; teardown closes transport and provider as
  one receipt.
- [x] The provider token endpoint stays boot-pinned. One named shared
  canonical-URL helper supplies signer/verifier request matching, fingerprinting,
  transport enforcement, and restart/reconcile. It requires absolute HTTPS;
  applies IDNA2008 ASCII lower-case host with trailing root dot removed and
  RFC5952 compressed lower-case IPv6 in brackets; rejects IP zone, userinfo,
  fragment, and a leading-zero explicit port (omitted is `443`). It uppercases
  percent hex and decodes unreserved bytes *before* RFC3986 remove-dot-segments,
  so `%2e` participates in the dot rule; empty path is `/`. Query preserves
  original pair order and duplicates while canonicalizing percent encoding (no
  sorting; `+` remains literal plus); absent query omits `?`, while explicit
  empty query retains a terminal `?`. Golden fixtures pin each case.
  Canonical URL bytes are `https://host:port/path[?query]`; the only sink is
  origin `https://host:port`. The pair stores canonical URL digest/sink, never a
  host list; every bearer send rechecks it and refuses redirects.
- [x] Requested scopes are normalized before signing/comparison. A requested
  scope outside the boot true ceiling rejects loudly with a typed invalid-scope
  result; the production path never silently intersects/drops scopes.
- [x] Token and cache keys/assertions include verified tenant/user/session plus
  agent, capability revision, audience, and URL digest. The exchange refuses
  absent, malformed, unentitled, or mismatched audience/binding independently
  of Harbor's signature check; audience never substitutes for subject identity.
- [x] Signed-pair representation is server-owned/read-only. Whole
  `agent_config.set_revision`, rollback, every generic section setter, and
  legacy `remove_oauth_provider` / `remove_mcp_connection` must carry it
  forward byte-identically or reject addition, mutation, omission, deletion,
  and pair-half changes. A closed-door census covers all those writers. Only
  paired removal may remove it; it is owner-scoped and atomic in desired state.
  Rollback activation performs full binding verification, while paired removal
  and retirement use the frozen durable pair fingerprint to close/revoke even
  if envelope verification would now fail.
- [x] Paired removal continues that same pair-lifetime JTI operation; it is not
  a second operation or handoff. It is a durable operation, never a best-effort
  close. Its exact EventID `SaveIf` subphases are `removal_revision_committed` (desired
  pair absent by revision CAS), `catalog_unpublished`, `teardown_receipted`
  (transport/provider close+revoke from frozen fingerprint), then terminal
  `removed` checkpoint. Each commit-then-error or unknown outcome exact-rereads
  the operation EventID/phase, desired revision, catalog source, and close/revoke
  receipt; retry resumes the missing phase idempotently. Expiry, key revocation,
  or lost verifier never block this teardown. It cannot report `removed` while
  authority remains live, and retirement's terminal manifest invokes this same
  state machine rather than a second teardown path.
- [x] Reconcile/restart/rollback activate only a stored immutable pair whose
  operation record is exactly `published`, whose fingerprint/bindings verify,
  and whose activation fence is committed; they never classify that stored JTI
  as a fresh replay. They fail closed for an incomplete operation, absent current
  exchange entitlement, JTI bound to another fingerprint, unknown broker, scope
  widening, URL mismatch, provider collision, pending fence, or pair half-state.
  Registration-authority expiry/key revocation neither deletes nor replays a
  published record nor blocks its frozen-fingerprint paired removal/retirement.
- [x] Before a first-install candidate can become semantically active,
  `set_oauth_provider` creates a durable pending-activation/compensation fence
  under the agent scope. The fence binds exact operation/content fingerprint,
  attempted revision identity, and prior active revision/EventID (or no-active),
  and has its own phase/EventID. `Registry.Active`, every generic section writer
  and production registration/creation write, `set_revision`, rollback, pair
  removal, retirement, and reconcile consult the exact fence and physical active
  revision/EventID: while pending they return
  exactly prior/no-active and never authorize the candidate. A foreign operation
  rejects with typed pending/conflict; only the same operation may serialize and
  resume. Success `SaveIf`-commits the fence; failure `SaveIf`-aborts it; every
  unknown transition stays safely pending across runtimes until exact reread
  proves committed or aborted. Immutable candidate history remains.
  Under D-403, `DeactivateIfActive` may afterwards remove a truly first-write
  physical pointer only through mandatory StateStore `DeleteIf` against its
  exact non-empty EventID. A prior boot/config revision is restored by rollback
  only while the candidate content hash remains active. Wrong-generation and
  absent deletes mutate nothing; terminal/corrupt lifecycle bytes fail closed.
  This never loosens D-400's closed lifecycle decoder, and the physical repair
  remains post-fence convergence rather than the security fence.
- [x] All canonical method/type/error/event/Console manifest/docs lockstep
  gates cover the new surface. Events and audit carry only redacted identity,
  provider/capability names or hashes, revision, audience hash, and URL digest;
  no authority envelope, secret, URL, credential, or raw replay ID is emitted.

## Files added or changed

- `internal/config/{config,validate}.go` and tests for broker trust-anchor
  posture; no configuration migration/docs are shipped in this planning phase.
- `internal/agentcfg/`, `internal/runtime/agentcfg/protocol/`, projection, and
  `internal/runtime/serve/` provider/connection preparation and reconcile.
- `internal/state/` plus the in-memory, SQLite, and Postgres drivers for the
  mandatory exact-generation `DeleteIf` primitive and shared conformance.
- `internal/tools/auth/` and MCP driver binding/cache/exchange checks.
- `internal/protocol/{types,methods,errors,singlesource}/`, stream transport,
  generated Protocol docs and Console typed lockstep artifacts.
- `test/integration/` and focused unit/conformance tests.
- `scripts/smoke/phase-233b.sh`, RFC, decisions, glossary, and phase index.

## Public API surface

- `agent_config.register_oauth_mcp_capability` request/response, signed
  authority envelope view, immutable signed capability-pair revision binding,
  and typed errors for authority, replay, scope ceiling, broker, and binding
  failures.
- `agent_config.remove_oauth_mcp_capability` is the only paired removal verb;
  it is admin-only and owner-scoped.
- `agentcfg` operation-record and pending-activation-fence types plus their
  tenant/agent control-scope identities, phases, exact EventID `SaveIf`
  transitions, and typed conflict/pending errors.
- `CanonicalOAuthMCPURL(raw string) (canonicalURL, sink string, err error)` is
  the shared canonical-byte helper; cross-language golden fixtures are public
  testdata for signers.
- `agentcfg.Registry.DeactivateIfActive` remains a post-fence physical-pointer
  convergence seam. It uses exact-generation StateStore deletion for a truly
  absent prestate and never substitutes for the cross-runtime security fence.
- A boot-only `ToolOAuthCredentialBrokerConfig` signed-capability authority
  block; none of its secrets, endpoints, host lists, or verifier material are
  Protocol-writable.

## Test plan

- **Unit:** strict wire decoding; claim matching; JTI operation Kind/payload
  construction; every operation/fence phase, EventID CAS, unknown-outcome reread,
  full legal JTI graph, expiry/skew incomplete-only cleanup, published
  pair-lifetime retention through authority expiry/key revocation, removed
  anti-replay tombstone retention, remove terminality, and
  same-key/different-fingerprint refusal; scope-ceiling loud refusal; canonical URL golden bytes/digest/sink
  equality including IDNA, RFC5952 IPv6, encoded dot, leading-zero port,
  absent-versus-empty query, and percent/query edge cases; dedicated descriptor
  reflection/strict-decode rejection; pair fingerprint and writer/removal census;
  removal subphase/receipt commit-then-error recovery; pair-owned-provider/
  catalog-only dispatch; and first-write pending-fence commit/abort/uncertain
  cross-runtime reader and foreign-mutator conflict cases.
- **Integration:** real SQLite/Postgres operation-state recovery through every
  phase and restart/fault point; token exchange assertion capture; exact
  pair-owned catalog dispatch/no generic-provider binding; cross-language URL
  signer fixtures; no-redirect enforcement; paired removal/retirement after
  authority expiry/key rotation/revocation and unknown outcomes; and
  cross-tenant/user/session/agent cache and bearer-bleed denials.
- **Conformance:** all StateStore drivers run the same JTI operation and
  pending-activation fence phase/EventID/fault suite, including two-runtime
  readers and recovery. Their mandatory `DeleteIf` suite covers exact-once,
  wrong-generation/absent zero mutation, invalid/cancelled/closed failure,
  conditional replacement races, and durable reopen; Protocol/Console/
  generated-doc lockstep covers every new canonical type, method, error, and
  event.
- **Concurrency / leak:** N>=100 shared broker verifier/provider-set and MCP
  preparer invocations under `-race`; competing registration/removal/rollback/
  retirement and commit-then-error cases prove one winner, no incorrect
  compensation, cancellation cross-talk, identity bleed, or goroutine leak.

## Smoke script additions

- Static source assertions pin the canonical method name, D-401 RFC anchor,
  no-use production-path rule for the development-only descriptor opt-in, and
  the Phase 233 `SaveIf` no-active neutralization requirement.
- Once implemented, replace the static checks with named unit/integration test
  assertions for authority rejection, atomic pair registration, derived sink,
  scope-ceiling refusal, restart/reconcile, removal/retirement, and first-write
  failure. Absent live endpoint remains `404`/`405`/`501` SKIP-conformant.

## Coverage target

- `internal/runtime/agentcfg/protocol`, `internal/runtime/serve`,
  `internal/tools/auth`, `internal/tools/drivers/mcp`, and Protocol types/
  transport: 85%; config/agentcfg StateStore paths: 90%.

## Dependencies

- Phase 233 is the prerequisite. Phase 233a and 233b may proceed independently
  after it; both gate Phase 234. Phase 235 gates release completion after 232,
  233, 233a, 233b, and 234.

## Risks / open questions

- Signature format and verifier key rotation must use an existing approved
  asymmetric validator or receive an RFC ruling before implementation; request
  parsing must not select algorithms or issuers.
- Expiry+skew cleanup/maintenance applies only to `claimed`, `revision_committed`,
  and `expired_incomplete` records, without weakening exact tuple lookup.
  `published` persists for the full immutable pair-history lifetime; `removed`
  remains a durable anti-replay tombstone with that pair history. Maintenance
  must distinguish exact phase-resume from a fresh mutation; neither may be a
  best-effort cache.
- A previously registered capability can expire before rollback/reconcile. The
  safe behavior is inactive/unavailable with loud diagnostics, not an implicit
  renewal or acceptance of administrator input; removal/retirement remains
  available from its frozen durable pair fingerprint.

## Glossary additions

- Signed OAuth MCP capability.
- Signed capability authority envelope.
- Capability-pair binding.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages >= stated target
- [x] Cross-session and cross-tenant bearer/cache isolation test passes
- [x] Reusable verifier/provider/preparer N>=100 concurrent-reuse test passes
  under `-race` with no race, bleed, cancellation cross-talk, or leak
- [x] Real-driver integration covers identity, restart, failure, and cleanup
- [x] Protocol/error/event/Console/generated-doc lockstep gates pass
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry
  filed
