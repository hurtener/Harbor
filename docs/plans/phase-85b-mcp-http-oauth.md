# Phase 85b — mcp-http-oauth

## Summary

Wire Harbor's existing `auth.Provider` (Phase 30 — PKCE, RFC 7591 dynamic client registration, refresh, encrypted token store) into the MCP southbound driver, and extend it with the 2025-11-25 spec's authorization requirements the current provider lacks: RFC 9728 protected-resource-metadata discovery, `WWW-Authenticate`-driven 401 step-up, and RFC 8707 resource indicators for token audience binding. Today MCP HTTP servers get static `Headers` only — a `401 + WWW-Authenticate` triggers no flow. The interactive authorization flow runs through the unified pause/resume primitive, exactly as Phase 30 does for HTTP tools.

## RFC anchor

- RFC §6.4
- RFC §3.3

## Briefs informing this phase

- brief 14
- brief 09
- brief 03

## Brief findings incorporated

- brief 14 §2 (#9): "`auth.Provider` exists but is not wired into the MCP driver. MCP HTTP auth = static `Headers` only." — wiring it is the core goal.
- brief 14 §3 / §2 (#10): "no RFC 9728 … no RFC 8707 `resource` indicator / audience binding." — both are added here.
- brief 09 (MCP OAuth — lessons from bifrost): the bifrost OAuth shapes (`OAuth2Provider`, `OAuth2FlowInitiation`, `MCPUserOAuthRequiredError`) are the Go-shaped reference for the discovery + step-up surface; brief 09's "Re-discussion checklist" is binding input for this plan.
- brief 14 §4: MCP HTTP OAuth is "the single largest spec gap for HTTP servers" — this phase is the highest-value item in the band after 85a.

## Findings I'm departing from (if any)

- None. This phase extends Phase 30's `auth.Provider`; it does not fork it. If the RFC 9728 / 8707 additions reshape `auth.Provider`'s public surface, that reshape is in-scope and documented in the PR.

## Goals

- An MCP HTTP/Streamable server that returns `401 + WWW-Authenticate` triggers the OAuth flow instead of failing.
- Harbor discovers the protected-resource metadata (RFC 9728) and the authorization-server metadata, runs the OAuth 2.1 Authorization Code + PKCE flow, and retries the MCP request with a bearer token.
- The access token is bound to the MCP server's resource (RFC 8707 `resource` indicator) so a token minted for server A cannot be replayed against server B.
- The interactive leg (user consent at the authorization server) pauses the run via the unified pause/resume primitive and resumes on callback — no bespoke coordination.
- Tokens are stored in the Phase 30 encrypted store, keyed by the identity triple + the MCP server resource.

## Non-goals

- The OAuth Client-Credentials extension and Enterprise-Managed Authorization extension (matrix rows 32) — enterprise-tier, separate future work.
- Token passthrough — forbidden by AGENTS.md §7.3; this phase uses token exchange / fresh acquisition only.
- Non-HTTP transports — stdio servers have no OAuth surface.

## Acceptance criteria

- [ ] A `401 + WWW-Authenticate: Bearer resource_metadata="..."` from a mock MCP HTTP server triggers RFC 9728 protected-resource-metadata discovery, then authorization-server metadata discovery, then the PKCE Authorization Code flow.
- [ ] The token request includes an RFC 8707 `resource` parameter naming the MCP server; a test asserts the parameter is present and correct.
- [ ] The interactive consent leg emits a `RequestPause` through the unified pause/resume primitive; resume on the OAuth callback continues the MCP request. No code path coordinates pause outside the primitive (AGENTS.md §7.4).
- [ ] A successful flow retries the original MCP request with `Authorization: Bearer <token>`; tokens never appear in a query string (AGENTS.md §7 + brief 14 §2 #10).
- [ ] Tokens persist in the Phase 30 encrypted store keyed by `(tenant, user, session, mcp_resource)`; a second request to the same server reuses the stored token; expiry triggers refresh.
- [ ] A step-up scope request (server returns 403 + an `insufficient_scope` challenge) re-runs the flow for the additional scope.
- [ ] All OAuth flow transitions emit audit events through `audit.Redactor`; no token value is logged.
- [ ] Cross-isolation: a token acquired by identity A is never served to identity B — verified by a concurrent two-identity test.

## Files added or changed

- `internal/tools/drivers/mcp/` — new `auth.go` (or extend `transport_streamable.go` / `transport_sse.go`) wiring `auth.Provider` into the HTTP transports; `WWW-Authenticate` parsing; 401/403 retry path.
- `internal/tools/auth/` — RFC 9728 protected-resource-metadata discovery; RFC 8707 `resource` parameter on token requests; `WWW-Authenticate` challenge parser.
- `internal/tools/auth/` token store — key extended with the MCP server resource.
- Test files — mock MCP HTTP server with an OAuth-protected endpoint + a mock authorization server.
- `examples/harbor.yaml` — document MCP-server OAuth config (reuse the `tools.oauth_providers[]` shape from D-095 where possible).
- `scripts/smoke/phase-85b.sh`.
- `docs/decisions.md` — decision entry (filed at implementation time) recording the RFC 9728 / 8707 additions to `auth.Provider`.
- `docs/plans/README.md` — Status flip on merge.

## Public API surface

```go
// internal/tools/auth (delta — illustrative; finalised at implementation)

// ProtectedResourceMetadata is the RFC 9728 document discovered from a
// WWW-Authenticate challenge's resource_metadata URL.
type ProtectedResourceMetadata struct {
    Resource             string
    AuthorizationServers []string
    // ...
}

// The token-request path gains an RFC 8707 resource indicator.
```

The MCP driver's HTTP transports gain an internal dependency on `auth.Provider`; no exported MCP-driver surface changes.

## Test plan

- **Unit:** `WWW-Authenticate` challenge parsing (valid, malformed, missing `resource_metadata`); RFC 9728 metadata fetch; `resource` parameter construction.
- **Integration:** mock MCP HTTP server (OAuth-protected) + mock authorization server; full flow: 401 → discovery → PKCE → pause → resume → token → retry → 200. Real `auth.Provider`, real pause/resume coordinator, identity propagation, `-race`.
- **Conformance:** N/A — Phase 85j.
- **Concurrency / leak:** two-identity concurrent flow asserting token isolation; goroutine-leak baseline after Close.
- **Failure modes:** authorization-server unreachable; token endpoint returns error; refresh fails; 403 step-up.

## Smoke script additions

- `scripts/smoke/phase-85b.sh` (classification: `static-only`):
  - Assert `internal/tools/drivers/mcp/` imports `internal/tools/auth` (the wiring exists).
  - Assert the auth package references RFC 9728 / `resource_metadata` and an RFC 8707 `resource` parameter.

## Coverage target

- `internal/tools/drivers/mcp`: 85%.
- `internal/tools/auth`: 85% (security-critical; conformance-grade).

## Dependencies

- 28 (MCP driver — the HTTP transports being augmented).
- 30 (Tool-side OAuth — the `auth.Provider` being wired + extended).
- 50 (Pause/Resume Coordinator — the interactive flow's coordination primitive).

## Risks / open questions

- **`auth.Provider` reshape blast radius.** Adding RFC 9728 discovery may change `auth.Provider`'s construction surface; Phase 30's HTTP-tool callers must not regress. The PR runs Phase 30's full test set.
- **Authorization-server quirks.** Real authorization servers diverge from the RFCs; the discovery path must fail loudly with actionable errors (which metadata field was missing) rather than a generic failure.
- **Resource-indicator support.** Not every authorization server honours RFC 8707; when unsupported, the token is unbound — the phase documents this and audits it as a reduced-assurance condition rather than failing the flow.

## Glossary additions

- **Protected-resource metadata** — the RFC 9728 document an MCP HTTP server points to via a `WWW-Authenticate` challenge; it names the resource and its authorization servers, bootstrapping OAuth discovery.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage ≥ target
- [ ] **Cross-isolation test passes** — token isolation across identities is the headline security guarantee.
- [ ] **Concurrent-reuse test passes** — concurrent two-identity flows under `-race`.
- [ ] **Integration test passes** — mock MCP HTTP server + mock auth server, real `auth.Provider` + real pause/resume.
- [ ] Glossary updated.
- [ ] No brief departures.
