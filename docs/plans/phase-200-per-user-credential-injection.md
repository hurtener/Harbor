# Phase 200 — Per-user credential injection for receiver-style MCP servers (HA-34)

## Summary

The southbound MCP driver gains a per-user credential-INJECTION mode: on an outbound tool call for a connection configured with it, the driver SOURCES the acting principal's credential from the configured broker (the same broker-pull the `tokenexchange` provider already performs — per-user, fetched-not-held, memory-only TTL) and INJECTS it into the outbound request per a declared, non-secret INJECTION MAPPING — arbitrary headers, `Authorization: Basic`, or MCP `_meta.<vendor>` keys. This reaches a server that RECEIVES its credential directly instead of pulling it via RFC 8693. It is a controlled pull-then-inject exception to the pull-only posture (D-271) that extends the shipped per-identity injection seam (D-278); the audit redactor is extended so the non-Bearer forms are held to the same no-log bar.

## RFC anchor

- RFC §6.4

## Briefs informing this phase

- brief 09
- brief 03

## Brief findings incorporated

- brief 09 §"broker custody": the credential still originates from the broker and is fetched per-user at call time — injection changes only the LAST hop (the runtime delivers because the receiver cannot pull), never the custody model; no secret is held or pushed by a client.
- brief 09 §"MCP OAuth — lessons from bifrost": one injection mechanism, not two — the new declared forms ride the SAME per-identity outbound seam as the bearer path (`bearerInjectingTransport` / `_meta` stamp), so injection is mutually exclusive with the bearer/oauth mode (one auth mode per connection).
- brief 03 §"static auth (API key, bearer, cookie)": a credential on an outbound request is a secret that must never be logged — the redactor's Bearer-only coverage is EXTENDED to the Basic scheme, the declared injection header keys, and the declared `_meta` credential keys.

## Findings I'm departing from (if any)

None. D-271's pull-only posture is revisited (a controlled exception) but not departed from silently — D-341 authorizes the pull-then-inject last hop and forbids client-pushed credentials.

## Goals

- A per-user injection call sources a per-user credential from the broker and injects it in each declared form (header / `Authorization: Basic` / `_meta.<vendor>`).
- Two different acting users get two different injected values on the same connection (per-user isolation).
- A broker error fails the call loudly (no silent skip / no fallback to an unauthenticated call).
- The audit redactor holds every new injected form to the same `***` bar as the Bearer path.
- Injection is mutually exclusive with the existing bearer/oauth mode and static `Authorization` header (one auth mode per connection).

## Non-goals

- A server capability-advertisement discovery protocol for the injection mapping — the mapping is operator-declared config this phase; discovery is a future enhancement noted in Risks.
- Carrying the injection mapping over the wire — even though the HA-32 wire descriptor (phase 199) is its natural future carrier, this phase declares the mapping in connection config only.
- Any change to the pull path itself (`tokenexchange` `Token`) — it is reused unchanged.

## Acceptance criteria

- [ ] A connection configured with an injection mapping sources the acting principal's credential via the broker-pull (`OAuthProvider.Token(ctx, source)`, reading `identity.From(ctx)`) per outbound call and injects it per the declared mapping (header / Basic / `_meta`).
- [ ] Per-user isolation: two acting users on the same connection produce two distinct injected values; a concurrent-reuse test (N≥100 interleaved per-user calls, one shared driver, `-race`) shows no cross-user value bleed.
- [ ] A broker error fails the call loudly (surfaces the typed error; the call is not sent unauthenticated).
- [ ] The audit redactor redacts `Authorization: Basic <b64>`, the declared injection header keys, and the declared `_meta` credential keys to `***` (asserted against a captured outbound request payload).
- [ ] The attach-time one-auth-mode guard rejects an injection mapping declared alongside an `oauth_provider` bearer binding or a static `Authorization` header.
- [ ] `scripts/smoke/phase-200.sh` asserts the redaction discipline / mapping validation (degrades to SKIP where no dev receiver fixture is reachable).

## Files added or changed

```text
internal/tools/drivers/mcp/mcp.go                 # injection-mode config on the driver; per-call source-and-inject at the outbound seam (alongside resolveBearerCtx)
internal/tools/drivers/mcp/transport_sse.go       # extend the injecting transport: Basic scheme + arbitrary declared header keys
internal/tools/drivers/mcp/attach.go              # one-auth-mode guard extended to reject injection + bearer/static-Authorization
internal/config/config.go                         # connection-descriptor injection-mapping field (NON-secret) + IsReservedMCPMetaKey guard for _meta form
internal/audit/rules.go                           # Basic-scheme + declared-key + _meta-credential redaction rules
internal/tools/drivers/mcp/inject_test.go         # NEW — per-form inject, per-user isolation, broker-error-loud, concurrent-reuse (N≥100, -race)
internal/audit/rules_test.go                      # new redaction forms held to ***
scripts/smoke/phase-200.sh                        # NEW — mapping validation + redaction discipline (degrades to SKIP)
examples/harbor.yaml                              # documented injection-mapping example (non-secret)
docs/skills/connect-an-mcp-server/SKILL.md        # note the per-user injection mode (surface: mcp) — grep docs/skills for the matching surface
docs/plans/phase-200-per-user-credential-injection.md # this plan
docs/glossary.md                                  # "Receiver-style MCP server", "Credential injection mapping"
```

## Public API surface

- Config: a non-secret `injection` mapping on the MCP connection descriptor (which credential field → which header / `Basic` / `_meta.<vendor>` key). No new exported Go interface — injection rides the existing outbound seam.

## Test plan

- **Unit:** each declared form injects the pulled value; per-user isolation (two users → two values); broker error → loud failure (call not sent); one-auth-mode guard rejects injection + bearer/static-Authorization; `_meta` form respects `IsReservedMCPMetaKey`.
- **Integration:** through the driver's outbound path with a real broker-pull provider and the real `audit.Redactor` on the seam — a per-user injection call's audit payload shows `***` for every injected form; identity propagation asserted (the per-user value derives from `identity.From(ctx)`).
- **Conformance:** N/A — single MCP driver.
- **Concurrency / leak:** N≥100 interleaved per-user injection calls against one shared driver under `-race` (D-025) — no cross-user value bleed, no goroutine leak.

## Smoke script additions

- `scripts/smoke/phase-200.sh` (`PREFLIGHT_REQUIRES: live-server`): validate that a connection declaring both an injection mapping and a bearer `oauth_provider` is rejected (one-auth-mode); assert the audit surface never emits an injected credential in cleartext (static grep guard on the redaction rules + a live probe where a dev receiver fixture is reachable); SKIP cleanly on 404/405/501 or absent fixture.

## Coverage target

- `internal/tools/drivers/mcp`: ≥ 80% on the injection path. `internal/audit`: the new redaction rules covered.

## Dependencies

- Gate-0 (D-341). Builds on the shipped southbound per-identity injection seam (D-278), the `tokenexchange` broker-pull (D-271/D-285), and the audit redactor — all on `dev-experimental`. Composes with phase 199 (the wire descriptor is the future carrier of the mapping) but does not require it to land first.

## Risks / open questions

- A misconfigured mapping could target a reserved `_meta` key or collide with the identity stamp — the `IsReservedMCPMetaKey` guard rejects that at attach time (fail-loud), asserted by test.
- Discovery of the injection contract (a server advertising its accepted credential forms) is deferred; the mapping is operator-declared this phase. Noted, not blocking — the receiver server already declares its forms in its own error text, so onboarding is config, not code.
- §17.8: the receiver-form fixture derives from a real receiver-style server's declared credential forms, not a hand-authored shape.

## Glossary additions

- **Receiver-style MCP server** — an MCP server that authenticates by RECEIVING a credential on each call (headers / `Authorization: Basic` / `_meta`) rather than PULLING it via RFC 8693. D-341.
- **Credential injection mapping** — non-secret connection config mapping a broker-pulled per-user credential's field(s) to outbound request header(s) / a `Basic` value / `_meta.<vendor>` keys. D-341.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — the per-user value derives from `identity.From(ctx)`; a cross-user isolation test asserts no value bleed.
- [ ] **Concurrent-reuse test passes** — the MCP driver is a reusable artifact; N≥100 interleaved per-user injection calls under `-race`. See §5 + D-025.
- [ ] **Integration test exists** — the injection path with a real broker-pull provider + real `audit.Redactor` on the seam (Deps names shipped D-278/D-271 phases).
- [ ] If config schema changed: `examples/harbor.yaml` updated; backward compatible (new optional mapping).
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — the D-271 revisit is authorized by D-341 (not a silent departure).
