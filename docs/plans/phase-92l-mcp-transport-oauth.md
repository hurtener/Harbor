# Phase 92l — MCP transport agent-bound OAuth + typed ErrAuthRequired

## Summary

Teach the MCP driver to authenticate via Harbor's existing tool-side OAuth provider. `mcpdrv.Config` / `AttachDeps` gain an optional `auth.OAuthProvider` plus the source's binding info; the streamable-HTTP and SSE transports, before dialing, resolve an agent-bound token via `provider.Token(ctx, source)` and inject `Authorization: Bearer <token>` when one is returned. A missing token surfaces a TYPED `*auth.ErrAuthRequired` verbatim out of `mcpdrv.Attach`, letting the runtime-add boundary replace the `looksLikeAuthRequired` string heuristic with `errors.As`. Static operator headers stay supported and take precedence when present. See `docs/plans/wave-mcp-oauth-decomposition.md` (§2, §3, §7) for the cross-phase design.

## RFC anchor

- RFC §6.4 — Tool catalog and transports (the MCP southbound driver + the transport-level auth injection this phase adds).
- RFC §3.3 — The unified pause/resume primitive (the substrate the typed `ErrAuthRequired` ultimately parks on, via the add-connection service in 92m — out of scope here, but the typed error is what unblocks it).

## Briefs informing this phase

- brief 09
- brief 14

## Brief findings incorporated

- **brief 09 (MCP OAuth from bifrost):** the transport is the right injection point for the bearer token — the driver resolves an agent-bound token immediately before composing the upstream request, never caching a copy on the compiled artifact. This phase calls `provider.Token` per-dial and injects `Authorization` on the request, mirroring bifrost's token-lifecycle placement.
- **brief 14 (MCP client/host compliance, spec 2025-11-25):** a missing/invalid credential is a 401-shaped condition the client must surface as a distinct, typed auth state — not fold into a generic dial failure. This phase propagates `*auth.ErrAuthRequired` verbatim so the runtime-add boundary can branch on the typed sentinel (`errors.As`) instead of pattern-matching the error string.

## Findings I'm departing from (if any)

None.

## Goals

- `mcpdrv.Config` / `AttachDeps` carry an optional `auth.OAuthProvider` + the source's binding info (the `tools.ToolSourceID` the provider keys on; agent-bound by construction for a runtime-added connection).
- The streamable-HTTP and SSE transports call `provider.Token(ctx, source)` before dialing and inject `Authorization: Bearer <token>` when a token is returned.
- A `*auth.ErrAuthRequired` from `Token` propagates VERBATIM (typed) out of `mcpdrv.Attach`, so a caller can branch with `errors.As(err, &authErr)`.
- Replace the runtime-add boundary's `looksLikeAuthRequired` string heuristic with `errors.As` against the now-typed error.
- Static operator headers remain supported and take precedence over the provider token when present (back-compat).

## Non-goals

- Runtime config registration on the provider (the `RegisterConfig` / `UnregisterConfig` seam) — that is 92k.
- Wiring the typed error into `add_mcp_connection` (`InitiateFlow` parking, the authorize-URL response) — that is 92m.
- The spec-faithful 401 → RFC 9728 → AS discovery dance — that is 92p.
- The `pause.resumed` resume-completes-attach bridge — that is 92n.
- stdio OAuth (stdio is not an OAuth transport pattern); A2A `AUTH_REQUIRED` already converges on the same `ErrAuthRequired`.

## Acceptance criteria

- [ ] When the provider returns a token for the source, the transport injects `Authorization: Bearer <token>` on every streamable-HTTP / SSE request (pinned by a transport-level test asserting the header on the wire).
- [ ] When `provider.Token` returns a `*auth.ErrAuthRequired`, the error propagates verbatim out of `mcpdrv.Attach` and a caller recovers it via `errors.As(err, &authErr)` (the typed payload — source, binding scope, authorize URL — is intact).
- [ ] The runtime-add boundary's `looksLikeAuthRequired` string heuristic is removed and replaced by `errors.As`; a non-auth dial failure still surfaces as a loud `failed`, never a silent drop (§13).
- [ ] Static operator headers take precedence: when a `Authorization` (or operator-supplied) header is present, the provider token is NOT consulted/injected (back-compat, pinned by a test).
- [ ] Identity is threaded through `provider.Token` via ctx; the agent-bound token keys by `agent_id` (a registration identity, NOT an isolation filter — CLAUDE.md §6). A missing triple fails closed via the provider's `ErrIdentityRequired`.
- [ ] The provider is optional: a nil `OAuthProvider` leaves the transport on the existing static-header-only path (concurrent-reuse + boot behaviour unaffected; pinned by the existing attach tests staying green).

## Files added or changed

- `internal/tools/drivers/mcp/mcp.go` — `Config` gains an optional `auth.OAuthProvider` + the source binding info (the `tools.ToolSourceID` + agent binding).
- `internal/tools/drivers/mcp/attach.go` — `AttachDeps` gains the optional provider + binding; threaded onto `Config` at `New`; a typed `*auth.ErrAuthRequired` propagates verbatim (no wrap that hides the type).
- `internal/tools/drivers/mcp/transport_streamable.go` + `transport_sse.go` — before dialing, resolve `provider.Token(ctx, source)`; inject `Authorization: Bearer <token>`; static operator header wins when present.
- `cmd/harbor/cmd_dev_mcp_attacher.go` — replace `looksLikeAuthRequired` (+ `status401Pattern`) with `errors.As(err, &authErr)` against `*auth.ErrAuthRequired`.
- `harbortest/devstack/devstack.go` — mirror the attacher change (D-094 twin) so the production + test attach paths cannot drift.
- `internal/tools/drivers/mcp/transport_oauth_test.go` (new) — the §17.8 fixture-driven transport tests.
- `scripts/smoke/phase-92l.sh`.

## Public API surface

```go
// mcpdrv.Config / AttachDeps gain (all optional):
//   OAuthProvider auth.OAuthProvider // nil → static-header-only path (back-compat)
//   Source        tools.ToolSourceID // the source the provider keys the token on
// Behaviour: before dialing, when OAuthProvider != nil and no static
// Authorization header is set, the transport calls Token(ctx, Source);
// a token injects "Authorization: Bearer <token>"; a typed auth-required
// error propagates verbatim out of Attach for the caller to errors.As on.
```

## Test plan

- **Unit:** token injection when the provider returns a token (header asserted on the request); static-header precedence (provider not consulted when an operator `Authorization` header is present); nil provider → static-only path unchanged.
- **Integration:** in-package transport test against a REAL local MCP server fixture (or a committed transcript — §17.8) that returns 401 with no token, asserting the typed `*auth.ErrAuthRequired` propagates verbatim through `mcpdrv.Attach`; then a provider stubbed to return a token lets the same attach reach `online`. Real provider on the seam (`auth.OAuthProvider`), identity propagation through ctx, the 401 failure mode, under `-race`.
- **Conformance:** reuses the existing MCP driver conformance; the OAuth path is additive (nil provider preserves prior behaviour).
- **Concurrency / leak:** the provider is consulted per-dial from ctx; no token cached on the compiled artifact. A concurrent-reuse assertion (N≥100 dials against one shared provider+driver under `-race`) confirms no context bleed and no goroutine leak.

## Smoke script additions

- `scripts/smoke/phase-92l.sh` (`unit-tests`): runs `go test` for `internal/tools/drivers/mcp` exercising the transport OAuth injection + typed-error propagation + static-header precedence; greps `cmd/harbor` / `harbortest/devstack` to confirm `looksLikeAuthRequired` is gone and `errors.As` against the typed auth error is the branch. Skeleton ships as a single `skip` until the surface lands.

## Coverage target

- `internal/tools/drivers/mcp`: 85%

## Dependencies

- 28 (the MCP southbound driver + attach lifecycle).
- 30 (tool-side OAuth: `auth.OAuthProvider`, `*auth.ErrAuthRequired`, `ScopeAgent`).
- 92k (the provider's runtime config-registration seam — a runtime-added source has a registered config for `Token` to resolve against).

## Risks / open questions

- **Token-injection ordering.** Static operator header vs provider token must be unambiguous: the operator header wins when present, otherwise the agent-bound token. Pinned by a transport-level precedence test so the rule is mechanically enforced, not just documented.
- **Verbatim typed-error propagation.** `Attach` must not wrap the `*auth.ErrAuthRequired` in a way that defeats `errors.As` (a `fmt.Errorf("...: %w", err)` preserves the chain, but an opaque string conversion would not). The attach test asserts `errors.As` recovers the typed payload through the full call stack.
- **Fixture realism (§17.8).** The 401 fixture MUST be a real local MCP server (or a captured transcript), never a hand-authored self-consistent JSON blob — a fixture that can't tell a real 401 from a wrong-shaped one is a rubber stamp.

## Glossary additions

None — this phase reuses existing vocabulary (tool-side OAuth, agent-bound token, `ErrAuthRequired`). The runtime-add control-plane terms land with 92f / the wave doc.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **Concurrent-reuse test passes (N≥100 dials against one shared provider+driver under `-race`) — the provider is read from ctx per-dial, never cached on the artifact.**
- [ ] **Integration test exists: real MCP server fixture (§17.8) + real `auth.OAuthProvider` on the seam, identity propagation, the 401 failure mode (typed error verbatim + token-recovered attach), `-race`.**
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
