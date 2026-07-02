# Phase 142 — External tool-credential provisioning: the `tokenexchange` OAuth driver

## Summary

Ships the pull-based external-credential acquisition strategy: a new `tokenexchange` driver on the D-095 OAuth flow-strategy registry that obtains a downstream tool credential (a user's Microsoft 365 token, a Google Workspace token — foreign-IdP tokens used to call third-party tools) from an operator-configured external credential broker (a fleet orchestrator, an enterprise token vault, an STS) via an RFC-8693-shaped token exchange, instead of Harbor's interactive authorization-code flow. Brokered tokens are TTL-cached in memory only — `TokenStore.Put` is never called — so one central grant serves N runtimes without N consents and N encrypted copies. Push-style credential injection over the Protocol is explicitly REJECTED, not deferred (D-271).

## RFC anchor

- RFC §6.4 (tool catalog and transports; tool-side OAuth; the external-provisioning paragraph added alongside this plan)
- RFC §3.3 (unified pause/resume — the broker-declined-consent park path)

## Briefs informing this phase

- brief 09

## Brief findings incorporated

- brief 09 §"Why this brief exists" (the two binding patterns): per-user (`ScopeUser`) and agent-bound (`ScopeAgent`) are first-class peers, and the per-user pattern is the one where each user's own account is the upstream actor. The fleet central-custody use case is precisely the per-user pattern, so the V1 driver mirrors the `oauth2` driver's `ScopeUser` construction posture (`internal/tools/auth/drivers/oauth2/oauth2.go:141`) with the per-tool `tools.entries[].oauth.binding_scope` field steering the catalog wrapper, exactly as today.
- brief 09 §"Open questions" item 4: RFC 8693 was confirmed out of scope for Phase 30 ("bifrost-mcp uses standard authorization-code grant + refresh-token grant + dynamic client registration; we should match unless an MCP server specifically requires 8693") — and Phase 30 shipped 8693-free. This phase is where 8693 lands: as its OWN acquisition strategy behind the D-095 registry, never as a mutation of the interactive `oauth2` driver.
- brief 09's convergence lesson (one primitive family, no parallel auth path — the same lesson the 92k–92q band applies): the new driver reuses TokenStore's identity rules, the `OAuthProvider` interface, the typed `*ErrAuthRequired`, and the unified pause primitive rather than growing a second credential path. The broker-declined-consent case parks on the SAME pause primitive the interactive flow uses (§7 rule 4).

## Findings I'm departing from (if any)

- brief 09 open-question 4 recommended matching plain authorization-code + refresh "unless an MCP server specifically requires 8693". The fleet central-custody requirement (one user grant serving N runtimes: today it is N consents and N encrypted copies) IS that requirement, arriving from the orchestrator side rather than the MCP-server side. The departure is recorded in D-271 and scoped to a NEW driver so the Phase-30 interactive machinery is untouched.

## Goals

- An external authority can supply per-user downstream tool credentials to a runtime non-interactively. The runtime PULLS at token-miss time, keyed on the VERIFIED ctx identity triple — never on request-body or tool-arg identity (the D-219 posture).
- Zero new interfaces and zero config-schema surgery: the driver registers on the existing D-095 registry (`tools.oauth_providers[].driver: tokenexchange`); broker-specific knobs ride the reserved `Extra` map (`internal/tools/auth/registry.go` — "Reserved for future drivers' per-flow knobs"); `ToolOAuthProviderConfig` gains no new fields.
- Brokered tokens never persist: in-memory TTL cache bounded by the broker-advertised expiry, single-flight per `(scope, subject, source)` mirroring `Provider.refreshLocked` (`internal/tools/auth/provider.go:251`); `TokenStore.Put` is never called. One central copy stays the broker's; Harbor holds no shadow store of the broker's truth (the D-061 rule read southbound: revocation at the broker must not leave a live sealed copy in N runtimes).
- Fail-loud end-to-end: broker unreachable or refusing → typed error to the run; NO silent fallback to the interactive flow. A fallback would silently void the central-custody policy (N consent prompts reappear) — the exact silent-degradation shape CLAUDE.md §13 forbids.
- A broker-declined "consent required" answer surfaces the EXISTING typed `*auth.ErrAuthRequired` (AuthorizeURL = the broker-supplied consent URL when present) so the run parks on the unified pause/resume primitive; after the user consents centrally, a resume re-drives `Token()` against the now-granting broker. One pause path, no fork.
- Every actual broker exchange is audited: a new canonical `tool.credential_exchanged` event (SafePayload by construction — zero token bytes), satisfying §7's "requires explicit configuration AND emits audit events" for the external-credential posture.

## Non-goals

- **Push injection (candidate shape B) — REJECTED, not deferred.** No Protocol method, tool-arg channel, or per-run request field ever carries a downstream credential into the runtime. A credential arriving in-band from a northbound client is the §7 "credential passthrough" pattern (unverifiable provenance/audience, secrets riding channels the codebase is engineered to keep secrets out of — the `ErrAuthRequired` SafePayload discipline, the HTTP driver's `ErrTemplateSecretLeak` gate) and the D-219 anti-pattern (authority from request body). D-271 records the rejection so it is not re-litigated.
- Agent-bound (`ScopeAgent`) brokered credentials. The fleet use case is per-user; agent-bound custody already has the interactive path plus the planned 92k–92q runtime-OAuth band. Follow-up phase if a real demand arrives.
- The broker itself — consent capture, central encrypted custody, cross-runtime grant dedup, refresh against the foreign IdP. All orchestrator-side, outside Harbor. Harbor ships only the exchange CLIENT contract.
- Per-call `Authorization` injection into the MCP/HTTP transports — that is 92l's Pending surface. This phase's §13 consumer is the catalog `WrapWithOAuth` pre-check path (`internal/tools/catalog/catalog.go:475`) exercised end-to-end by the integration test. The driver slots under 92l unchanged when it lands.
- Broker-push revocation propagation. Revocation lag is bounded by the TTL cap; `Revoke` clears the local cache only.
- Forwarding the northbound Protocol JWT as the 8693 `subject_token`. Architecturally wrong for Harbor's durable-run model: a resumed or background run outlives the initiating request's JWT (a task resumed hours later has no live northbound token). See Risks for the trust-model consequence and the post-V1 upgrade path.

## Acceptance criteria

- [ ] `internal/tools/auth/drivers/tokenexchange` self-registers as driver name `tokenexchange` on the D-095 registry via `init()`; the blank import lands in `internal/drivers/prod` (D-196) — NOT in `cmd/harbor`; `ErrDriverUnknown` messages now list it.
- [ ] `Token(ctx, source)` fails closed with `ErrIdentityRequired` on a missing/partial triple. On cache miss it POSTs an RFC-8693 token-exchange request to the configured `token_url`: `grant_type=urn:ietf:params:oauth:grant-type:token-exchange`, `subject_token_type=urn:harbor:oauth:token-type:identity-triple`, `subject_token` = base64url JSON of the verified triple, `audience` (default: the source ID; overridable via `extra.audience`), `scope` = the configured scopes — with the runtime authenticated to the broker by the env-indirected `client_id_env` / `client_secret_env` credential (the existing §7 rule-2 indirection).
- [ ] Issued tokens live in an in-memory TTL cache only. Serve horizon = min(broker `expires_in`, `extra.cache_ttl_cap` default 5m) — a token is NEVER served past the broker-advertised expiry; the ≥30s floor rate-limits RE-EXCHANGE (within 30s of the previous exchange a re-exchange fails loudly naming the floor; once the window elapses the next call re-exchanges normally — the floor only bites when the broker advertises a sub-30s expiry), and a `cache_ttl_cap` below the floor is rejected at construction. (Reviewed semantics — the original wording floored the serve TTL itself, which would have served tokens past broker expiry; adjusted per the phase-PR adversarial review.) `TokenStore.Put` is NEVER invoked — asserted by test, not just by review. Refresh is re-exchange, collapsed under a single-flight gate keyed `(scope, tenant, user, source)` — tenant included, components length-prefixed against separator-byte collisions.
- [ ] Broker network failure, 5xx, or a non-consent OAuth error → wrapped `ErrExchangeFailed` propagated to the run. NEVER a fallback to the interactive flow; NEVER a stale cache entry served past TTL.
- [ ] A broker `consent_required`-class refusal → typed `*auth.ErrAuthRequired` (AuthorizeURL = the broker-supplied consent URL when the error response carries one; `Message` names the broker host); the runtime's existing catch parks the run on the unified primitive; a resume after the broker flips to granting re-drives `Token()` to success (E2E-asserted, including the "bare resume against a still-declining broker re-parks" leg).
- [ ] `InitiateFlow` / `CompleteFlow` / `DenyFlow` return the new typed sentinel `auth.ErrNonInteractive` — never a silent no-op (§13). `PendingFlow` reports no flow. `Revoke` clears the cache entry and returns nil (idempotent).
- [ ] New canonical event `tool.credential_exchanged` registered beside the existing two in `internal/tools/auth/events.go`, payload SafePayload by construction (source, binding scope, subject kind, broker host, granted scopes, expiry — zero token bytes), emitted once per ACTUAL exchange (cache hits emit nothing). `make protocol-docs-gen` run and the regenerated pages committed; the generator lockstep test passes (§18).
- [ ] D-025 concurrent-reuse test: N≥100 concurrent `Token()` calls (mixed identity triples) against one shared provider instance under `-race` — no data races, no identity bleed (two triples never observe each other's token), no cancellation cross-talk, goroutine count restored to baseline.
- [ ] Integration test with an httptest broker fixture derived from RFC 8693's wire format (§17.8: the fixture asserts the exact `grant_type` / `subject_token_type` / `audience` / client-auth params ON THE BROKER SIDE, so a driver wired to the wrong field fails the test), run through the real catalog `WrapWithOAuth` path with a real events bus + audit redactor.
- [ ] An `examples/` config gains a documented `tokenexchange` provider stanza; `config` validation accepts it — `token_url` mandatory for this driver, `auth_url` / `redirect_url` NOT required (they are interactive-flow fields).
- [ ] The RFC §6.4 external-provisioning paragraph (added with this plan as "planned — D-271") flips to shipped wording in the phase PR. §18 sweep: any `docs/skills/` playbook that enumerates the OAuth driver set or documents `tools.oauth_providers[]` updates in the same PR.

## Files added or changed

- `internal/tools/auth/drivers/tokenexchange/tokenexchange.go` (+ `tokenexchange_test.go`, `concurrent_test.go`)
- `internal/tools/auth/auth.go` — `ErrNonInteractive` sentinel + godoc
- `internal/tools/auth/events.go` — `EventTypeToolCredentialExchanged` + `ToolCredentialExchangedPayload`
- `internal/drivers/prod/prod.go` — blank import of the new driver
- `internal/config/validate.go` — per-driver validation branch (`token_url` mandatory; `auth_url` / `redirect_url` waived for `tokenexchange`)
- `examples/` — a `tokenexchange` provider stanza in the tools example config
- `test/integration/phase142_token_exchange_test.go`
- `scripts/smoke/phase-142.sh`
- `docs/site/protocol/events.md` — regenerated (`make protocol-docs-gen`)
- `docs/glossary.md`, `docs/decisions.md` (D-271 flip to shipped), `docs/plans/README.md` (status flip), `RFC-001-Harbor.md` (§6.4 paragraph wording flip)

## Public API surface

(Operator-facing surface is YAML only; Go surface is internal but consumed by later phases.)

- Driver name `tokenexchange` on the D-095 registry.
- `auth.ErrNonInteractive` — typed sentinel returned by interactive-flow methods of non-interactive acquisition drivers (92-band phases and future drivers branch on it via `errors.Is`).
- `auth.EventTypeToolCredentialExchanged events.EventType = "tool.credential_exchanged"` + `auth.ToolCredentialExchangedPayload`.
- Recognized `extra` keys on `tools.oauth_providers[]`: `audience`, `cache_ttl_cap`.

## Test plan

- **Unit:** exchange-request construction (table-driven over the 8693 params); error mapping (network / 4xx / 5xx / OAuth error body → `ErrExchangeFailed`; `consent_required` → `*ErrAuthRequired` with/without broker consent URL); TTL cache semantics + the never-`Put` assertion (a `TokenStore` spy that fails the test on `Put`); `ErrNonInteractive` on the three interactive methods + `PendingFlow` false + idempotent `Revoke`; identity fail-closed on partial triples.
- **Integration:** the httptest RFC-8693 broker (spec-derived fixture per §17.8) through the real catalog wrapper + real inmem bus + real redactor: happy path; two-identity isolation (distinct triples → distinct tokens, asserted broker-side); `consent_required` park → broker flips → resume → success; broker outage fail-loud with no interactive fallback and no event-with-secret; `-race` throughout.
- **Conformance:** ~~join `internal/tools/auth/conformancetest` for the `Token` identity rules~~ — §4.3 deviation: that suite is a TokenStore/Sealer persistence conformance kit (Put/Get round-trips, encryption at rest), inapplicable to a driver that never persists; the `Token` identity rules and the `ErrNonInteractive` interactive-flow legs are covered directly by the driver's own unit suite instead.
- **Concurrency / leak:** the D-025 N≥100 mixed-identity stress (above), plus a single-flight collapse assertion (a burst of M concurrent misses for one subject produces exactly 1 broker request) and a goroutine-baseline check after provider `Close`.

## Smoke script additions

- `scripts/smoke/phase-142.sh` (`PREFLIGHT_REQUIRES: unit-tests`): runs `go test -race` for `internal/tools/auth/drivers/tokenexchange` + the phase-142 integration test; static greps asserting (a) the `internal/drivers/prod` blank import exists and (b) no non-test file in the driver package references `TokenStore.Put`. Skeleton parks with `skip` until the surface lands (the 92-band precedent).

## Coverage target

- `internal/tools/auth/drivers/tokenexchange`: 85%
- `internal/tools/auth` (touched lines): no regression below the package's current coverage

## Dependencies

- 30 (tool-side OAuth subsystem: `OAuthProvider` / `TokenStore` / `ErrAuthRequired` / D-083)
- 64a (catalog `WrapWithOAuth` — the §13 consumer path; D-090)
- the D-095 driver registry (shipped with Wave 11.5 Stage A, PR #119)
- 50 (unified pause/resume — the `consent_required` park path)

## Risks / open questions

- **Impersonation trust model.** The broker trusts the runtime's client credential to assert the subject triple — RFC 8693's impersonation semantics (client-authenticated actor, no cryptographic proof-of-user in V1). A compromised runtime broker-credential can request any subject's token the broker's policy allows. Mitigations are broker-side (per-runtime credentials, per-tenant scoping, anomaly detection) plus Harbor-side per-exchange audit. The plan deliberately does NOT forward the northbound JWT as `subject_token` — durable runs outlive the initiating request's JWT, so request-token forwarding breaks exactly the durable-run cases Harbor exists for. A signed runtime-side subject assertion (a private-key actor token, symmetric with the `harbor token` keypair machinery from D-264) is the named post-V1 upgrade path; V1 documents the trust model honestly instead of faking a stronger one.
- **Availability coupling.** Broker down = the source's tool calls fail, loudly. Accepted: it is the same posture as `identity.jwks_url`, and the alternative (silent interactive fallback) is worse on both security (surprise consent prompts) and policy (decentralized copies reappear).
- **92l composition.** When 92l lands per-call MCP `Authorization` injection reading `provider.Token`, this driver slots under it unchanged. One thing to verify in 92l review: its park path must key off the typed `*ErrAuthRequired` (which this driver also produces for `consent_required`) and never assume every provider can `InitiateFlow` — flag `ErrNonInteractive` handling there.
- **Event cardinality.** Per-exchange emission is bounded by TTL + single-flight; a pathological zero-TTL broker could spam the bus — the `cache_ttl_cap` floor guards it. Exact floor value resolved at implementation.
- **`subject_token_type` URN.** `urn:harbor:oauth:token-type:identity-triple` is a Harbor-defined URN (8693 permits impersonation-style deployments to define their own subject-token types); brokers that insist on `…:token-type:jwt` need the post-V1 signed-assertion upgrade. Named here so it is a documented limitation, not a surprise.

## Glossary additions

- **Credential broker** — the external authority a `tokenexchange` provider pulls downstream tool credentials from.
- **`tokenexchange` driver** — the non-interactive, RFC-8693-shaped acquisition strategy on the D-095 OAuth driver registry.

(Both added to `docs/glossary.md` in the same PR as this plan.)

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. (This phase builds a reusable driver — the test is mandatory.)
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. (It consumes 30 + 64a + 50 — the integration test is mandatory.)
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (D-271)
