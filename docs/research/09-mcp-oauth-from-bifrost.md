# Research Brief 09 — MCP OAuth, lessons from `bifrost`

**Date:** 2026-05-12
**Status:** research input for Phase 30. **Not a design decision.** This brief documents `bifrost`'s OAuth shape so Phase 30's plan can lift the pieces that fit Harbor's DNA and explicitly leave the rest behind. The settled adoption call is the RFC's / phase plan's, not this brief's.
**Bifrost version inspected:** `github.com/maximhq/bifrost/core v1.5.8` (the version Harbor's `go.mod` pins). Type signatures quoted here are verbatim from that version's `core/schemas` package and will drift; re-`go doc` at implementation time.

## Why this brief exists

Phase 30 (`docs/plans/README.md` row 30 — "Tool-side OAuth + HITL via pause/resume") is `Pending`. Its goal line is binding:

> `TokenStore` interface (InMem + SQLite + Postgres drivers). On `tool.auth_required`, pause via the unified pause/resume primitive (phase 50). Resume reattaches token; A2A `AUTH_REQUIRED` converges on the same primitive.

The MCP southbound (Phase 28, `Shipped`) intentionally left tool-side OAuth as a Phase 30 problem: `phase-28-tools-mcp.md` § "Non-goals" notes that `auth.OAuthHandler` is reachable through `StreamableClientConfig.OAuthHandler` but Phase 28 leaves the operator to wire it.

`bifrost`'s `core/schemas` package already implements a complete OAuth machinery for MCP, covering the two patterns Harbor needs to support:

1. **Per-user (`MCPAuthTypePerUserOauth`)** — each user authenticates individually; the upstream service sees the user as the actor. Used for personal-data integrations (Gmail, personal GitHub, Drive, Notion).
2. **Server-level / agent-bound (`MCPAuthTypeOauth`)** — admin authenticates once during agent setup via the Console; the upstream service sees the *agent* as the actor (or a shared service account). Used for shared-resource integrations (Outlook for a research agent, internal Snowflake via a service account, Slack/Teams bots, internal APIs). **This is the dominant production pattern** for assistant-style deployments — the agent has its own seat, its own quota, its own audit trail; users invoke it without ever connecting their own accounts.

Both are first-class for Harbor. Phase 30 can either re-derive that machinery from first principles or treat `bifrost`'s shapes as a reference implementation and lift the pieces that fit Harbor's DNA.

This brief documents the second path so Phase 30's plan author can do the lift with eyes open.

## What bifrost provides (verbatim Go surface)

### The four MCP auth types

```go
// Quoted verbatim from github.com/maximhq/bifrost/core/schemas
const (
    MCPAuthTypeNone         MCPAuthType = "none"           // No authentication
    MCPAuthTypeHeaders      MCPAuthType = "headers"        // Header-based authentication (API keys, etc.)
    MCPAuthTypeOauth        MCPAuthType = "oauth"          // OAuth 2.0 authentication (server-level, admin authenticates once)
    MCPAuthTypePerUserOauth MCPAuthType = "per_user_oauth" // Per-user OAuth 2.0 authentication (each user authenticates individually)
)
```

**Both modes are first-class for Harbor.** They model two distinct production patterns, and operators MUST be able to pick per MCP server (per HTTP tool, per A2A peer):

- **User-bound OAuth (`MCPAuthTypePerUserOauth`)** — *the agent acts as the human*. The token belongs to a specific Harbor user; only that user's invocations use it. Examples: "the assistant pulls *my* GitHub issues", "the assistant reads *my* Gmail", "the assistant searches *my* Notion workspace." Identity flows transparently: the upstream service sees `alice@example.com` doing the action and applies its own ACLs.

- **Agent-bound OAuth (`MCPAuthTypeOauth` — what bifrost calls "server-level")** — *the agent acts as itself*. The token belongs to an **agent identity** (a service-account-style principal owned by the admin), not to any individual user. The admin authenticates once via the Console; every invocation by any user of that agent uses the same token. Examples: "the Research Assistant agent has its own Outlook mailbox", "the Triage Bot posts in Teams channels under its own identity", "the agent queries an internal Snowflake warehouse via a service account", "the agent files Jira tickets as the bot user." Identity does not flow to the upstream — the upstream sees the agent doing the action; Harbor's audit trail captures `(originating user, agent-as-actor)`.

Mapping it to Harbor's Console design: the user's screenshot shows "Agent: Research Assistant" as a first-class field on the session. Admin-configured agents have their own MCP-server attachments and their own OAuth-account bindings; the admin clicks "Connect Outlook" once during agent setup, completes the OAuth flow once, and the token lives keyed by *the agent's identity*, not by any of the users who later chat with the agent. This is the most common production pattern for assistant-style deployments (Teams/Outlook/Slack/internal APIs/databases) — the agent has its own seat, its own quota, its own audit trail.

**The two modes are NOT exclusive.** A single agent may attach to GitHub via user-bound OAuth (so it can see each user's repos with their own ACLs) and to Outlook via agent-bound OAuth (so it can send mail from a shared mailbox). The policy is per-attachment, not per-agent.

The decision criterion is: *whose ACLs should the upstream service apply?* If the upstream's ACLs are user-shaped (Gmail, Drive, personal GitHub), use user-bound. If the upstream's ACLs are agent/service-shaped (shared mailbox, shared Slack bot, service-account DB role), use agent-bound. If both make sense, the operator picks per-attachment.

### The `OAuth2Provider` interface (the load-bearing contract)

```go
type OAuth2Provider interface {
    // Server-level (admin authenticates once) ---------------------------
    GetAccessToken(ctx context.Context, oauthConfigID string) (string, error)
    RefreshAccessToken(ctx context.Context, oauthConfigID string) error
    ValidateToken(ctx context.Context, oauthConfigID string) (bool, error)
    RevokeToken(ctx context.Context, oauthConfigID string) error

    // Per-user (each user authenticates individually) -------------------
    GetUserAccessToken(ctx context.Context, sessionToken string) (string, error)
    GetUserAccessTokenByIdentity(ctx context.Context,
        virtualKeyID, userID, sessionToken, mcpClientID string) (string, error)
    InitiateUserOAuthFlow(ctx context.Context,
        oauthConfigID string, mcpClientID string, redirectURI string,
    ) (*OAuth2FlowInitiation, string, error)
    CompleteUserOAuthFlow(ctx context.Context, state string, code string) (string, error)
    RefreshUserAccessToken(ctx context.Context, sessionToken string) error
    RevokeUserToken(ctx context.Context, sessionToken string) error
}
```

Ten methods, split into two halves: server-level (4) and per-user (6). **Both halves are relevant for Harbor**, mapping to the two binding scopes described above:

- Server-level (`GetAccessToken` / `RefreshAccessToken` / `ValidateToken` / `RevokeToken`) → Harbor's **agent-bound** scope. The `oauthConfigID` becomes Harbor's `(agent_id, source_id)` lookup key.
- Per-user (`GetUserAccessToken*` / `InitiateUserOAuthFlow` / `CompleteUserOAuthFlow` / `RefreshUserAccessToken` / `RevokeUserToken`) → Harbor's **user-bound** scope. The `sessionToken` becomes a Harbor-internal credential keyed by the identity triple.

Bifrost's interface treats the two halves as parallel surfaces; Harbor's design (below) folds them into a single `AccessToken(ctx, source) → Token` call where the binding scope is part of the *source's configuration*, not part of the API. The caller asks "give me a token for this source"; the provider knows the scope and routes internally.

### Supporting types

```go
type OAuth2Config struct {
    ID              string
    ClientID        string   // Optional: dynamic registration (RFC 7591) if not provided
    ClientSecret    string   // Optional: for public clients using PKCE, or via dynamic registration
    AuthorizeURL    string   // Optional: discovered from ServerURL if not provided
    TokenURL        string   // Optional: discovered from ServerURL if not provided
    RegistrationURL *string  // Optional: for dynamic client registration (RFC 7591)
    RedirectURI     string   // Required
    Scopes          []string
    ServerURL       string   // MCP server URL for OAuth discovery (required if URLs not provided)
    UseDiscovery    bool     // Deprecated: discovery now automatic when URLs are missing
}

type OAuth2Token struct {
    ID              string
    AccessToken     string
    RefreshToken    string
    TokenType       string
    ExpiresAt       time.Time
    Scopes          []string
    LastRefreshedAt *time.Time
}

type OAuth2FlowInitiation struct {
    OauthConfigID string
    AuthorizeURL  string  // the URL we need to hand the user
    State         string  // the state token bifrost generated for CSRF
    ExpiresAt     time.Time
}

type OAuth2TokenExchangeRequest struct {
    GrantType    string
    Code         string
    RedirectURI  string
    ClientID     string
    ClientSecret string
    RefreshToken string
    CodeVerifier string // PKCE verifier for authorization_code grant
}

type OAuth2TokenExchangeResponse struct {
    AccessToken  string
    RefreshToken string
    TokenType    string
    ExpiresIn    int
    Scope        string
}

// The pause signal — quoted verbatim
type MCPUserOAuthRequiredError struct {
    MCPClientID   string
    MCPClientName string
    AuthorizeURL  string
    SessionID     string
    Message       string
}
func (e *MCPUserOAuthRequiredError) Error() string
```

`MCPUserOAuthRequiredError` is the most useful single shape in this brief. It is what bifrost returns from a tool call when the upstream MCP server requires user auth. The `AuthorizeURL + SessionID + MCPClientID` triplet maps almost 1:1 onto Harbor's `tool.auth_required` event payload.

### `MCPClientConfig`'s OAuth fields

```go
type MCPClientConfig struct {
    // ... non-OAuth fields elided ...
    AuthType          MCPAuthType
    OauthConfigID     *string  // references oauth_configs table
    OauthClientID     *EnvVar  // Redacted on GET, not stored here
    OauthClientSecret *EnvVar  // Redacted on GET, not stored here
    // ... rest elided ...
}

// Convenience: build per-request headers, transparently refreshing via the provider.
func (c *MCPClientConfig) HttpHeaders(ctx context.Context, oauth2Provider OAuth2Provider) (map[string]string, error)
```

`HttpHeaders` is the seam where the runtime asks "what `Authorization: Bearer …` should I send on this MCP call?" and the provider either returns a fresh token or surfaces an `MCPUserOAuthRequiredError`. This is the single-call surface where the pause-vs-proceed decision crystallises.

### How bifrost identifies a user for per-user lookups

`GetUserAccessTokenByIdentity(ctx, virtualKeyID, userID, sessionToken, mcpClientID)` is the lookup path. bifrost's vocabulary is `virtualKeyID` (its multi-tenancy primitive — closest to Harbor's tenant), `userID`, `sessionToken` (the user-facing token bifrost issued at end of `CompleteUserOAuthFlow`), and `mcpClientID` (which MCP server). The lookup fallback is "by virtualKeyID + userID → by sessionToken." Harbor's identity triple maps cleanly: `tenant_id` ↔ `virtualKeyID`-equivalent (we have native identity, no virtual-key indirection); `user_id` ↔ `userID`; `session_id` is Harbor-only and is what Harbor uses to scope the pause record.

### Agent identity (Harbor's addition — bifrost has no concept)

bifrost has no first-class "agent" principal — its closest concept is the gateway-level OAuth config (`oauthConfigID`). Harbor's Console design treats agents as first-class addressable subjects: `agent_id` appears on every session, the admin manages agents (`Research Assistant`, `Triage Bot`, …), and an agent owns a configured set of tool attachments and OAuth bindings. **Agent-bound OAuth tokens are keyed by `agent_id`**, not by a user identifier. This is what allows the "Outlook for Research Assistant" pattern: the admin sets up the agent → connects Outlook once → the token persists keyed by `(agent_id, source_id="outlook")` → every user invoking that agent's Outlook tool reuses the agent's token.

**Open RFC-level question** (not in scope for this brief, but Phase 30 cannot ship cleanly without an answer): does Harbor's identity surface extend to a quadruple `(tenant, agent, user, session)`, with `agent_id` a peer of the existing three? Or does `agent_id` live separately on the session/run metadata? Recommendation: peer. Acting subject = `agent_id`; requesting principal = `user_id`. Provenance captures both; isolation predicates can key on either depending on the scope of the resource being protected.

### The wire-side context key

```go
const BifrostContextKeySessionToken BifrostContextKey = "bifrost-session-token"
```

bifrost reads the user's OAuth session token from `ctx` via this key, which means the calling layer is responsible for stashing it before invoking. In Harbor, the `tool.auth_required` → pause → resume cycle is what produces that session token; the runtime stashes it on the resumed-run `ctx` for any subsequent same-tool calls in the run.

## End-to-end flows in bifrost (reference)

### Per-user flow (`MCPAuthTypePerUserOauth`)

1. Operator declares an MCP client with `AuthType = "per_user_oauth"`, `OauthConfigID = "github-mcp"`, etc.
2. Operator stores an `OAuth2Config{ID: "github-mcp", ClientID: ..., RedirectURI: ..., ServerURL: ..., Scopes: ...}` in whatever persistence the `OAuth2Provider` implementation uses.
3. User initiates a chat. LLM emits a tool call to a tool exposed by the OAuth-gated MCP server.
4. `MCPClientConfig.HttpHeaders(ctx, provider)` is invoked. The provider's `GetUserAccessTokenByIdentity` returns either a token or `MCPUserOAuthRequiredError{AuthorizeURL, SessionID, MCPClientID}`.
5. On the error, bifrost surfaces it up through the LLM-call return. Caller (the gateway / SDK consumer) is responsible for handing `AuthorizeURL` to the user, polling `SessionID`, and re-invoking the call once auth completes.
6. The auth callback hits `CompleteUserOAuthFlow(ctx, state, code)` which exchanges the code for tokens, persists them keyed by the session, and returns a new `sessionToken` for the caller to send on future requests.
7. The next call carries `BifrostContextKeySessionToken` in `ctx`; `HttpHeaders` succeeds; the MCP tool call goes through with `Authorization: Bearer …`.

### Server-level / agent-bound flow (`MCPAuthTypeOauth`)

Structurally identical but admin-initiated *during agent setup*, not user-initiated *during chat*:

1. Operator (admin) declares an MCP client with `AuthType = "oauth"`, `OauthConfigID = "outlook-shared"`, etc., attached to a configured agent.
2. Operator stores the `OAuth2Config` once.
3. **During agent setup** (not during chat): admin clicks "Connect Outlook" in the Console. The Console calls `GetAccessToken(ctx, "outlook-shared")` → token miss → admin is shown the authorize URL → admin completes OAuth → token persists keyed by the `oauthConfigID` (in Harbor: keyed by `(tenant, agent_id, source)`).
4. Later, user initiates a chat with the agent. LLM emits a tool call to Outlook.
5. `MCPClientConfig.HttpHeaders(ctx, provider)` invokes `GetAccessToken(ctx, "outlook-shared")` → token hit → returns the shared agent-bound token.
6. The MCP tool call goes through. Upstream Outlook sees the agent's identity (a shared mailbox / service principal), applies its own ACLs, returns results. Harbor's audit captures `(user as requester, agent as actor)`.
7. Token expiry → `RefreshAccessToken` runs transparently using the stored refresh token. No user interaction; no chat-time pause. Refresh failures (revoked, scope changed, server-side credential invalidation) raise an admin-targeted pause via the unified pause/resume primitive.

The wire dance is RFC-6749-shaped + RFC-7591 (dynamic client registration) + PKCE for public clients. Token refresh is handled inside `GetUserAccessToken` / `GetAccessToken` transparently when the stored token is expired.

**The key difference between flows is *who* completes the OAuth dance, not *what* the dance looks like.** That's why Harbor folds them into one `AccessToken(ctx, source)` API; the binding scope on the source's config determines who gets the pause when re-auth is needed.

## Mapping bifrost → Harbor (Phase 30 design sketch)

This is the suggestion-level mapping. The phase plan author should treat it as a starting point, not a contract.

### Naming alignment

| bifrost | Harbor (proposed) | Reason |
|---|---|---|
| `OAuth2Provider` | `auth.OAuthProvider` (interface) | Lives in `internal/tools/auth/`; not part of `tools` directly |
| `OAuth2Config` | `auth.OAuthConfig` | Same shape, dropped `2` for noise; gains `BindingScope` field (user / agent) |
| `OAuth2Token` | `auth.Token` | Gains `Scope` field discriminating user-bound vs agent-bound |
| `OAuth2FlowInitiation` | `auth.FlowInitiation` | Same shape |
| `OAuth2TokenExchangeRequest/Response` | `auth.tokenExchangeReq/Resp` (unexported) | Wire-only; Harbor doesn't expose these as public types |
| `MCPUserOAuthRequiredError` | `auth.ErrAuthRequired` (typed sentinel + payload struct) | Wraps a `Payload` that becomes the `tool.auth_required` event body. **Note**: dropped "User" from the name — agent-bound flows also raise this sentinel (admin-driven, not user-driven, but same shape) |
| `BifrostContextKeySessionToken` | `identity` ctx key (existing) | Harbor's identity ctx already exists; reuse, don't add a new key |
| `virtualKeyID + userID + mcpClientID + sessionToken` lookup | `identity.MustFrom(ctx) + agent_id + ToolSourceID` | Harbor keys lookups by `(tenant, agent, source)` for agent-bound and `(tenant, user, source)` for user-bound. The provider routes internally based on `OAuthConfig.BindingScope` |
| server-level `GetAccessToken(oauthConfigID)` | `OAuthProvider.AccessToken(ctx, source)` where `OAuthConfig.BindingScope = ScopeAgent` | Resolves via `(tenant, agent_id, source)` lookup |
| per-user `GetUserAccessToken*` | `OAuthProvider.AccessToken(ctx, source)` where `OAuthConfig.BindingScope = ScopeUser` | Resolves via `(tenant, user_id, source)` lookup |

### Interface shape (proposed)

```go
// package auth

// BindingScope discriminates who the OAuth token belongs to.
// Configured per OAuth attachment (per MCP server / HTTP tool / A2A peer);
// the provider routes lookups accordingly. RFC-territory decision: this
// dimension may live on the agent/source config rather than OAuthConfig
// itself — Phase 30 plan author settles the placement.
type BindingScope string

const (
    // ScopeUser — token belongs to a Harbor user. Lookups key by
    // (tenant, user, source). Each user authenticates individually.
    // The upstream service applies the user's ACLs.
    ScopeUser BindingScope = "user"

    // ScopeAgent — token belongs to a Harbor agent (admin-configured
    // service-account-style principal). Lookups key by (tenant, agent,
    // source). The admin authenticates once during agent setup via the
    // Console; every user invoking that agent reuses the agent's token.
    // The upstream sees the agent doing the action; Harbor's audit
    // captures (originating user, agent-as-actor).
    ScopeAgent BindingScope = "agent"
)

// OAuthConfig is the per-source OAuth attachment. Stored inline on the
// MCPServerConfig / HTTPToolConfig / A2APeerConfig block (no separate
// oauth_configs table — bifrost has one, we don't need it).
type OAuthConfig struct {
    Source           tools.ToolSourceID // which MCP server / HTTP tool / A2A peer this binds to
    BindingScope     BindingScope       // user | agent
    ClientID         string             // optional: RFC 7591 dynamic registration if empty
    ClientSecret     string             // optional: PKCE-only if empty
    AuthorizeURL     string             // optional: discovered from ServerURL/.well-known/...
    TokenURL         string             // optional: discovered from ServerURL/.well-known/...
    RegistrationURL  string             // optional: RFC 7591 dynamic client registration
    RedirectURI      string             // required (Harbor Protocol-shaped callback URL)
    Scopes           []string
    ServerURL        string             // for discovery; required if AuthorizeURL/TokenURL empty
}

// OAuthProvider is the canonical contract for tool-side OAuth.
// Identity is mandatory: every method takes ctx and reads identity via
// identity.MustFrom. The provider routes between user-bound and
// agent-bound lookups based on the source's configured BindingScope.
type OAuthProvider interface {
    // Returns a fresh access token for (identity, source). The token's
    // actual ownership is determined by the source's OAuthConfig.BindingScope:
    //   - ScopeUser: lookup keyed by (tenant, user_id, source)
    //   - ScopeAgent: lookup keyed by (tenant, agent_id, source)
    // If no token exists or refresh fails, returns ErrAuthRequired carrying
    // the payload that becomes the tool.auth_required event.
    AccessToken(ctx context.Context, source tools.ToolSourceID) (Token, error)

    // Begins a flow. Returns the AuthorizeURL the user (or admin, for
    // agent-bound) must visit and the State token for CSRF. Internally
    // allocates a pause record via the unified pause/resume primitive
    // (Phase 50). For ScopeAgent flows, the pause is admin-targeted
    // (only callers with admin scope on the agent can complete the flow);
    // for ScopeUser flows, the pause is user-targeted (only the user
    // whose identity is on the pause record can complete).
    InitiateFlow(ctx context.Context, source tools.ToolSourceID, redirectURI string) (FlowInitiation, error)

    // Handles the callback. Exchanges (state, code) for tokens, persists
    // via TokenStore keyed by the binding scope, and resumes the paused
    // run. The returned ResumeToken is the protocol-level resume
    // credential — NOT the OAuth access token (which never leaves the
    // runtime).
    CompleteFlow(ctx context.Context, state, code string) (ResumeToken, error)

    // Idempotent revoke. For ScopeAgent, restricted to admin callers
    // on the agent's tenant. For ScopeUser, restricted to the token's
    // owning user or admin.
    Revoke(ctx context.Context, source tools.ToolSourceID) error
}

// Token is what TokenStore persists. The OAuth provider is the only
// component that ever sees the access/refresh tokens in plaintext.
// The Owner field discriminates who the token belongs to — exactly one
// of UserID / AgentID is non-empty depending on Scope.
type Token struct {
    Source          tools.ToolSourceID
    Scope           BindingScope       // user | agent — matches OAuthConfig.BindingScope
    TenantID        string             // always set
    UserID          string             // set when Scope == ScopeUser; empty otherwise
    AgentID         string             // set when Scope == ScopeAgent; empty otherwise
    AccessToken     string             // never logged, never emitted on bus
    RefreshToken    string             // never logged
    TokenType       string             // "Bearer" usually
    ExpiresAt       time.Time
    Scopes          []string
    LastRefreshedAt *time.Time
}

// ErrAuthRequired is the typed error returned by AccessToken when no
// usable token exists. The wrapped payload becomes the tool.auth_required
// event body (Phase 30 + Phase 50). Carries the BindingScope so the
// Console can render the right UX (per-user "Connect your account" vs
// admin-targeted "Configure agent credentials").
type ErrAuthRequired struct {
    Source       tools.ToolSourceID // which MCP server / HTTP tool / A2A peer
    SourceName   string
    Scope        BindingScope       // user → user-targeted prompt; agent → admin-targeted prompt
    AuthorizeURL string
    State        string             // CSRF token; the resume credential is derived from this
    Scopes       []string
    Message      string
}
func (e *ErrAuthRequired) Error() string
```

### `TokenStore` interface (proposed — what Phase 30's plan already calls for)

```go
// package auth/tokenstore (or wherever the Phase 30 plan settles it)

type TokenStore interface {
    // Identity-scoped get. The scope argument tells the store which key
    // shape to use: ScopeUser → (tenant, user, source); ScopeAgent →
    // (tenant, agent, source). Returns (Token, true) on hit, (zero, false)
    // on miss. ErrUnauthenticatedIdentity if ctx lacks the components the
    // scope requires (a ScopeAgent lookup needs (tenant, agent); a
    // ScopeUser lookup needs (tenant, user)).
    Get(ctx context.Context, scope auth.BindingScope, source tools.ToolSourceID) (auth.Token, bool, error)

    // Upsert. Persists or replaces a token for (scope-key, source).
    // The Token's TenantID / UserID / AgentID fields are read from t,
    // not from ctx — agent-bound tokens may be persisted by the admin's
    // session ctx but key on the agent_id encoded in t.
    Put(ctx context.Context, t auth.Token) error

    // Idempotent delete. Authorisation check (admin scope for ScopeAgent;
    // owning-user-or-admin for ScopeUser) happens above this layer — the
    // store enforces the key shape, not the authz.
    Delete(ctx context.Context, scope auth.BindingScope, source tools.ToolSourceID) error
}

// Three V1 drivers per §4.4 + §9: inmem, sqlite, postgres.
// All three pass a conformance suite that asserts:
//   - identity-scoped reads/writes for BOTH scope shapes (no cross-tenant
//     leak; no cross-user leak on ScopeUser; no cross-agent leak on ScopeAgent)
//   - mixed-scope coexistence: a ScopeUser token and a ScopeAgent token
//     for the same source coexist without collision
//   - audit redaction on stored tokens (never logged in plaintext)
//   - migration idempotency for sqlite + postgres
//   - concurrent reuse (D-025): N=100 concurrent reads/writes survive -race
```

**Storage schema sketch (informative; Phase 30 plan settles the schema).** One row per token. Composite key:

- `tenant_id` (always)
- `scope` (`user` | `agent`)
- `subject_id` (= `user_id` when scope=user; = `agent_id` when scope=agent)
- `source_id`

Indexed for `(tenant_id, scope, subject_id, source_id)` lookups. Tokens encrypted at rest using a tenant-scoped KEK from the keystore (Phase 91 covers KEK rotation; for Phase 30 a static config-supplied KEK is acceptable). Refresh tokens encrypted separately so a compromised access-token cache does not yield refresh capability.

### How the cycle composes with the unified pause/resume primitive (Phase 50)

The lift from bifrost is **the shape of the OAuth dance**. The composition with Harbor's runtime is the part bifrost does not have:

```
Tool invocation in flight
    └─ tools.RunWithPolicy (D-024)
        └─ provider.Invoke (Phase 28 MCP / Phase 27 HTTP / Phase 29 A2A)
            └─ auth.OAuthProvider.AccessToken(ctx, source)
                ├─ TokenStore hit → return Bearer token → call proceeds
                └─ TokenStore miss / expired-refresh-failed:
                    │
                    └─ auth.OAuthProvider.InitiateFlow(ctx, source, redirectURI)
                        ├─ allocates pause record via pauseresume.Coordinator (Phase 50)
                        ├─ persists pause state with identity scope
                        └─ returns ErrUserAuthRequired
                            │
                            └─ runtime catches ErrUserAuthRequired, emits
                                tool.auth_required event with the payload
                                (RFC §3.3 — the unified primitive)
                                    │
                                    └─ ┄┄┄ run is paused; user completes OAuth out-of-band ┄┄┄
                                          │
                                          └─ auth callback hits OAuthProvider.CompleteFlow
                                              ├─ exchanges (state, code) for tokens
                                              ├─ TokenStore.Put(ctx, Token{...})
                                              └─ pauseresume.Coordinator.Resume(resumeToken)
                                                  → runtime re-invokes the tool;
                                                    AccessToken now hits the store; call proceeds
```

The key insight: **bifrost returns `MCPUserOAuthRequiredError` synchronously and expects the caller to handle the pause itself**. Harbor's runtime handles the pause for *all* tools uniformly (RFC §3.3), so the OAuth provider's job is just to return the typed error with the right payload; the runtime emits the `tool.auth_required` event and parks the run. The OAuth callback is just a protocol method that internally calls `pauseresume.Coordinator.Resume`.

**For agent-bound flows**, the same diagram applies but with two differences:

1. The pause record is *admin-targeted*: only callers with admin scope on the agent's tenant can complete the flow (the JWT scope check in Phase 61 enforces this).
2. The pause typically does NOT happen at chat-time. Agent-bound tokens are set up during agent configuration (a dedicated Console flow), so the token already exists when chat-time invocations fire. The pause path is hit only on (a) initial setup, (b) refresh-failure / re-auth required (upstream revoked the credential), or (c) scope-expansion (admin extends the agent's permissions and needs to re-consent). These all happen outside the chat path and the Console handles them as administrative interruptions, not interactive prompts.

A user-bound source's pause feels like a chat-time prompt ("Connect your GitHub account to continue"). An agent-bound source's pause feels like a Console banner ("Outlook integration needs admin attention"). Same primitive; different UX targeting.

### A2A `AUTH_REQUIRED` convergence

Phase 30's master-plan line: *"Resume reattaches token; A2A `AUTH_REQUIRED` converges on the same primitive."* A2A returns `TaskState.AUTH_REQUIRED` with an auth-scheme descriptor. The translation layer (Phase 29 A2A driver) lifts the A2A descriptor into the same `ErrUserAuthRequired` shape, so the runtime sees one error type regardless of southbound transport. bifrost doesn't have A2A (it's MCP-only), so this is Harbor-specific work but reuses the same pause shape.

## What to lift from bifrost (concrete)

1. **Both halves of the `AuthType` enum** — `none | headers | oauth | per_user_oauth`. Harbor's `OAuthConfig.BindingScope` maps to bifrost's two OAuth modes 1:1 (`ScopeAgent` ↔ `MCPAuthTypeOauth`; `ScopeUser` ↔ `MCPAuthTypePerUserOauth`). Both ship in Phase 30.
2. **The dynamic-registration option** in `OAuth2Config.RegistrationURL` (RFC 7591). Many MCP servers support it. Implementing it once means operators don't have to hand-register a client app per server.
3. **The OAuth-discovery option** — `ServerURL` populates `AuthorizeURL` / `TokenURL` lazily via `.well-known/oauth-authorization-server`. Reduces operator config burden; reduces Console field count during agent setup.
4. **The PKCE field** on `OAuth2TokenExchangeRequest.CodeVerifier`. Required for public clients; we should support it from day one.
5. **The "transparent refresh inside `AccessToken`" pattern.** bifrost's `GetUserAccessToken` automatically refreshes if expired. Harbor should match. Means `TokenStore.Get` returns whatever's persisted; `OAuthProvider.AccessToken` does the freshness check + refresh dance. Applies to both user-bound and agent-bound tokens identically.
6. **The `MCPUserOAuthRequiredError` payload shape** — `(AuthorizeURL, State, MCPClientID/Name, Message)`. Maps onto Harbor's `ErrAuthRequired` event body verbatim, with the addition of `Scope` so the Console renders the right UX.
7. **The `State`-as-resume-key idea.** bifrost uses the OAuth state parameter as the lookup key for the pending flow. Harbor's pause/resume primitive can use the same `state` value as the resume token's external identifier — keeps the flow stateless on Harbor's side between initiate and complete.
8. **The two-method split for revocation** (`RevokeToken` for server-level / `RevokeUserToken` for per-user). Harbor folds these into one `Revoke(ctx, source)` call, but the authorisation gate differs: agent-bound revocation requires admin scope on the agent; user-bound revocation requires the owning user (or admin override). The Phase 30 plan should make the authz gate explicit.

## What to leave behind (explicitly)

1. **`virtualKeyID`-style multi-tenancy.** Harbor has native identity. Don't introduce a parallel "virtual key" layer.
2. **bifrost's `oauth_configs` table semantics.** bifrost references OAuth configs by ID and stores them in a separate table. Harbor can store them inline on the `MCPServerConfig` / `HTTPToolConfig` / `A2APeerConfig` blocks (with `secret:"true"` redaction). Simpler operator config; no extra join. (Note: the Phase 30 plan author may revisit this if real-world MCP configs frequently share OAuth credentials across many servers — see open question below.)
3. **bifrost's `Headers map[string]EnvVar` for non-OAuth header auth.** Harbor's Phase 28 already has its own `Headers` field with the same `secret:"true"` redaction. Don't refactor.
4. **The `OAuth2TokenExchangeRequest/Response` as public types.** Wire-only; keep unexported.
5. **bifrost's "session token" indirection** (`BifrostContextKeySessionToken`). Harbor's session is already part of the identity triple; there's no second session indirection. Per-user OAuth tokens are looked up by `(tenant, user, source)` directly, not by an intermediate session-token mapping.
6. **bifrost's gateway-facing callback URL conventions.** Harbor's callback is a Protocol method (Phase 60); the URL shape is Harbor-Protocol-shaped, not bifrost-gateway-shaped.
7. **bifrost's two-interface split (server-level methods + per-user methods on one interface).** Harbor's `OAuthProvider` has one `AccessToken(ctx, source)` method; the binding-scope decision is in the source's config, not in the API surface. Cleaner for callers (Phase 28 / Phase 27 / Phase 29 drivers all call the same method).

## What Harbor must add (that bifrost does NOT provide)

1. **First-class agent identity.** bifrost has no `agent_id` concept. Harbor's Console-shown agents (e.g. `Research Assistant`) need to be a runtime principal — addressable, ACL-bound, the owner of agent-scoped OAuth tokens, and captured in the audit trail as the *actor* (with the originating user captured as the *requester*). This is a cross-cutting addition that Phase 30 *cannot* punt on; it likely warrants an RFC stub before the Phase 30 plan PR.
2. **`BindingScope` dimension on `OAuthConfig`.** bifrost models user vs server as two separate halves of one interface; Harbor's `OAuthConfig.BindingScope` is the single discriminator that drives lookup keying, pause-record targeting, and Console UX. Settle this in Phase 30, not after.
3. **Admin-scope authz on agent-bound flows.** Only callers with admin scope on the agent's tenant can initiate / complete / revoke `ScopeAgent` flows. The `pauseresume.Coordinator` pause record records the admin's identity; the callback's JWT scope is verified against that.
4. **Identity-mandatory enforcement.** bifrost's `OAuth2Provider` takes optional fallback identifiers (`virtualKeyID, userID, sessionToken`). Harbor's `OAuthProvider` MUST require the components the binding-scope demands via `identity.MustFrom(ctx)` and fail closed on missing components (CLAUDE.md §6.9 / §7.4). `ScopeUser` requires `(tenant, user)`; `ScopeAgent` requires `(tenant, agent)`.
5. **Audit redaction on persistence + emission.** Tokens MUST flow through `audit.Redactor` before any persistence or event emission (CLAUDE.md §3 / §7.6). bifrost has its own redactor (`RedactSensitiveString`) but Harbor uses its own.
6. **Encryption at rest for stored tokens.** Tenant-scoped KEK; refresh tokens encrypted separately from access tokens. Phase 30 ships with a config-supplied KEK; KEK rotation lives post-V1 (Phase 91-ish).
7. **Concurrent-reuse contract (D-025).** A single `OAuthProvider` instance must support N concurrent flows / lookups under `-race` with no cross-identity bleed, no cross-agent bleed, no scope confusion (a `ScopeUser` lookup must never return a `ScopeAgent` token even for the same source). Test mandatory.
8. **Cross-tenant + cross-user + cross-agent isolation conformance.** Phase 76's isolation harness exercises the provider with N tenants × M users × K agents; each token resolves only for its owning principal.
9. **Mixed-scope coexistence test.** A single MCP server may have both an agent-bound *and* a user-bound OAuth attachment (rare but legal — e.g. an agent's primary Outlook is shared, but a user opted to connect their personal Outlook to the same agent for a specific session). Tokens for these two coexist without collision.
10. **Pause/resume integration.** `InitiateFlow` allocates a pause record; `CompleteFlow` resumes via `pauseresume.Coordinator`. The OAuth flow is one *use case* of the unified primitive (RFC §3.3), not a parallel pause mechanism (CLAUDE.md §7.4). Pause-record targeting differs by scope: user-bound parks pending the user; agent-bound parks pending an admin.
11. **`tool.auth_required` event emission** with the canonical payload (Phase 30 + Phase 50). Bifrost surfaces an error; Harbor emits an event so the Console can render it (per-user "Connect your account" prompt vs admin-targeted "Configure agent credentials" prompt — driven by `ErrAuthRequired.Scope`).
12. **Identity-scoped JWT enforcement on resume.** The resume request carries a JWT (Phase 61); the OAuth callback handler verifies the JWT's identity scope matches the pause record's identity scope before resuming. Bifrost has no such layer.
13. **`TokenStore` driver triad** — InMem + SQLite + Postgres with the same conformance suite as every other persistence subsystem (CLAUDE.md §9). The conformance suite covers both scopes.
14. **Forward-only migrations for SQLite + Postgres** with `pg_advisory_lock`-gated boot (Postgres) and `WAL` journal mode (SQLite). Same as Phase 15 / 16.
15. **A2A `AUTH_REQUIRED` translation in Phase 29's driver.** bifrost has no A2A; this is Harbor-only. A2A peers may be agent-bound or user-bound following the same `BindingScope` rule.
16. **HTTP-tool OAuth.** bifrost's OAuth is MCP-only. Harbor's `OAuthProvider` should be transport-agnostic — Phase 27 (HTTP tools) should be able to ask for tokens too. The `ToolSourceID` discriminator is the source's identity; the provider doesn't care about transport.
17. **Tool-side approval gates (Phase 31)** layer on top — they are a *different* pause shape (synchronous approve/reject, not a token-exchange dance) but use the same `pauseresume.Coordinator`. bifrost has no approval-gate concept; Phase 31 is Harbor-only.
18. **Console agent-setup flow.** Phase 73 (Console state inspection surface) gains an Agent management view: admin creates an agent, attaches MCP servers / HTTP tools / A2A peers, configures `BindingScope` per attachment, completes the OAuth flow (for agent-bound) once during setup. Status (token expired? refresh failed? scope mismatch?) is visible. This is Console-side work that Phase 30 *enables* via the Protocol but doesn't itself ship.

## Cross-impact on the phase plan

- **Pre-Phase-30 work: agent identity RFC stub.** Harbor's identity surface (`Identity{TenantID, UserID, SessionID}`) does not currently model agents. Phase 30 *needs* agent identity to ship cleanly (agent-bound OAuth keys on `agent_id`). Recommendation: a small RFC PR adds `agent_id` as a peer of the existing identity components and documents the actor/requester distinction (acting subject = agent; requesting principal = user). This is RFC-territory, not Phase 30 scope, but Phase 30 is blocked without it.
- **Phase 30 acceptance criteria refinement.** The current goal line ("`TokenStore` interface (InMem + SQLite + Postgres drivers)") undersells the full surface. Recommended additions when the per-phase file is authored:
  - `OAuthProvider` interface as a peer of `TokenStore` (the provider owns the dance; the store owns persistence).
  - `auth.ErrAuthRequired` typed sentinel + payload struct carrying `BindingScope`.
  - `BindingScope` dimension on `OAuthConfig`; both `ScopeUser` and `ScopeAgent` ship.
  - Per-source OAuth config in `MCPServerConfig` / `HTTPToolConfig` / `A2APeerConfig` (not a separate config table).
  - PKCE support (`CodeVerifier` in the exchange flow).
  - RFC 7591 dynamic registration support (`RegistrationURL` optional).
  - Discovery support (auto-populate `AuthorizeURL`/`TokenURL` from `ServerURL/.well-known/oauth-authorization-server`).
  - Encryption at rest for stored tokens (tenant-scoped KEK).
  - Admin-scope authz gate on `ScopeAgent` flows; user-or-admin gate on `ScopeUser` flows.
  - Cross-tenant + cross-user + cross-agent isolation conformance tests.
  - Mixed-scope coexistence test (same source, both ScopeUser and ScopeAgent tokens).
  - Goroutine-leak test on initiate-flow-then-cancel (no leaked pause records on cancel).
  - Audit-redaction test on `Token` (never appears in events / logs in plaintext).
- **Phase 28 (MCP southbound)** retroactively gains tool-side OAuth via Phase 30. No Phase 28 change.
- **Phase 27 (HTTP tools)** gains tool-side OAuth from the same `OAuthProvider` — the `ToolSourceID` abstraction means the provider is transport-agnostic. Phase 27 plan should explicitly reference Phase 30 for the auth layer.
- **Phase 29 (A2A southbound, Shipped)** — Phase 30 adds the `AUTH_REQUIRED → ErrAuthRequired` translation; small additive change. A2A peers may also be agent-bound (e.g. "the Triage agent talks to a remote Compliance agent under the Triage agent's service identity").
- **Phase 50 (pause/resume coordinator)** must support the OAuth use case in its acceptance criteria (carry the `state` token, scope by identity, support both user-targeted and admin-targeted pauses, time out gracefully if no one returns).
- **Phase 61 (Protocol auth + identity-scope enforcement)** is the gate for resume calls — the OAuth callback handler is a Protocol method; the JWT scope check varies by binding scope (admin scope for agent-bound; user scope for user-bound).
- **Phase 72–74 (Console subscription / state / topology)** must surface the new agent-management view (see "What Harbor must add" §18). The Console renders agent-bound credentials as admin-only configuration; user-bound credentials appear in the user's own "Connected accounts" view.

## Glossary additions (pre-stage; land in `docs/glossary.md` at Phase 30 PR)

- **`auth.OAuthProvider`** — the contract for tool-side OAuth. Transport-agnostic (MCP / HTTP / A2A). Composes with the unified pause/resume primitive: missing/expired tokens emit `tool.auth_required` and park the run.
- **`auth.TokenStore`** — three-driver persistence (InMem / SQLite / Postgres) for OAuth tokens. Identity-scoped reads/writes (keyed by `(tenant, user, source)` or `(tenant, agent, source)` per `BindingScope`); tokens never emitted unredacted; encrypted at rest.
- **`auth.BindingScope`** — discriminator on `OAuthConfig` selecting `ScopeUser` (token belongs to a Harbor user; upstream sees the user) or `ScopeAgent` (token belongs to a Harbor agent; upstream sees the agent / service account). Per-attachment, not per-agent.
- **`auth.ErrAuthRequired`** — typed sentinel returned by `OAuthProvider.AccessToken` when no usable token exists. Payload becomes the `tool.auth_required` event body. Carries `BindingScope` so the Console targets the right principal (user vs admin).
- **`tool.auth_required`** — canonical event type emitted by the runtime when a tool invocation requires OAuth. Carries `(AuthorizeURL, State, Source, Scopes, BindingScope)`. Phase 30.
- **agent identity (`agent_id`)** — admin-configured service-account-style principal. Owns agent-bound OAuth tokens, agent-bound MCP attachments, and is the *acting subject* in audit when running an agent session (with `user_id` captured as the *requester*). RFC stub required before Phase 30; entry placeholder here so the term is documented.

## Risks / open questions

- **Agent identity placement (RFC-level).** Does `agent_id` join the identity triple as a fourth peer, or does it live as session/run metadata addressed indirectly? The "fourth peer" framing makes provenance + isolation easier (`identity.MustFrom(ctx)` returns it natively); the "metadata" framing avoids touching every existing identity-bearing API surface. Recommendation: **fourth peer**, with a one-PR RFC stub before Phase 30. This is the biggest blocking decision.
- **Acting subject vs requesting principal in audit.** When an agent-bound MCP call fires, the audit event captures *both* `agent_id` (actor) and `user_id` (requester). Existing event payloads carry only the identity triple; adding `agent_id` is non-breaking but changes the payload shape. Phase 30 plan needs to either bump the event-payload version or document the field as optional with a back-compat strategy.
- **Cross-session OAuth sharing for user-bound tokens.** Should a user's GitHub token from session A be reused in session B? bifrost says yes (per-user, not per-session). Harbor's CLAUDE.md §6.4 ("Memory is session-scoped by default. Cross-session promotion (user-level, tenant-level) requires an explicit declared policy with audit") suggests we should make the cross-session policy explicit. **Open question**: default to user-scoped tokens (reused across that user's sessions), or default to session-scoped with an explicit "user-scope" policy knob? **Recommendation**: default to user-scoped reuse (matches operator expectations: "I connected my GitHub once, why would I have to do it every session?"); document explicitly so the audit story is clear.
- **Cross-session OAuth sharing for agent-bound tokens.** Agent-bound tokens are user-orthogonal — they're shared by definition across every user of the agent. The question becomes: are they shared across every *session* of the agent (yes, by definition) and across every *agent of that agent's tenant* (no — `agent_id` is the key). One agent, one set of agent-bound tokens, used by every session of every user that runs that agent.
- **Token revocation on session close.** Phase 8 GC sweeps idle sessions. Should that revoke OAuth tokens? For session-scoped (if introduced as opt-in): yes. For user-scoped: no — survive across sessions. For agent-scoped: never via session GC — only via admin action.
- **Multiple identity scopes (impersonation).** If a planner runs as `system:admin` impersonating `user:alice`, which token does the MCP call use for a user-bound source? Recommendation: **the impersonated identity's token** (`alice`), not the admin's. For an agent-bound source, the agent's token (impersonation is irrelevant). Document the rule explicitly.
- **`OauthConfigID` placement.** bifrost references configs by ID and stores them in a separate table. Inlining on each attachment block (Harbor's proposal) is simpler but doesn't share a config across multiple sources. **Open question**: do operators ever want one OAuth config across N MCP servers (e.g. multiple GitHub-shaped servers all using the same OAuth app)? If yes, Harbor needs a config-by-ID indirection like bifrost's. **Recommendation**: start inline; add config-by-ID if real-world configs demand it.
- **PKCE-only vs PKCE+secret.** Phase 30 should default to PKCE for public clients and PKCE+secret for confidential. Easy to get wrong; tests must cover both paths.
- **Token expiry race.** Between `TokenStore.Get` and the actual MCP call, a token can expire. bifrost handles this by refreshing on miss inside `GetUserAccessToken`; Harbor should match. Test: forced near-expiry token + concurrent call.
- **Concurrent refresh storm on agent-bound tokens.** Agent-bound tokens are shared across N concurrent sessions. When an agent-bound token expires, N concurrent sessions all try to refresh simultaneously. **Mitigation**: single-flight refresh per `(tenant, agent, source)` key — only one goroutine performs the refresh; others wait on its result. Test mandatory.
- **Admin who set up an agent leaves the org.** Agent-bound tokens persist; the admin's identity that initiated the original flow no longer exists. The flow's audit record is anchored on `agent_id`, not on the admin's identity — refresh works fine, but revocation / re-auth needs *some* admin on the agent's tenant to action it. **Recommendation**: agent-bound flows record `initiated_by_user_id` for audit-trail completeness but don't depend on it for refresh / revocation; any admin on the tenant can re-auth.
- **Bifrost drift.** Bifrost is on a fast cadence (we're already at v1.5.8). The shapes quoted here will change. Phase 30 implementation should re-`go doc` and diff before lifting.

## What to verify before lifting (homework for Phase 30 plan author)

1. Re-run `go doc github.com/maximhq/bifrost/core/schemas OAuth2Provider` against the then-current bifrost version. Diff against this brief.
2. Read `MCPClientConfig.HttpHeaders` source in bifrost to understand exactly when it returns `MCPUserOAuthRequiredError` vs when it refreshes silently.
3. Read bifrost's reference `OAuth2Provider` implementation (likely under `bifrost/framework/oauth` or similar) for the wire-level state-token handling AND for how it handles concurrent refresh.
4. Confirm RFC 8693 (token exchange) is not in scope — bifrost-mcp uses standard authorization-code grant + refresh-token grant + dynamic client registration; we should match unless an MCP server specifically requires 8693.
5. **Survey 3-5 real-world MCP server configs** (GitHub, Notion, Linear, Outlook, Slack, …) to confirm: (a) which support per-user vs service-account binding, (b) whether OAuth-config sharing across multiple instances is a real pattern, (c) whether any require RFC 8693.
6. **Look at how Microsoft / Google / Slack** structure their service-account ("bot user") OAuth flows. Their patterns dominate agent-bound deployment; Harbor's `ScopeAgent` UX needs to match operator muscle memory.
7. **Land the agent-identity RFC stub** before Phase 30 work starts. This brief flags it as a blocker; don't draft Phase 30 acceptance criteria around `agent_id` until the RFC settles its placement.

## Findings summary

- ✓ bifrost has a complete OAuth2 implementation for MCP, both server-level (admin-once) and per-user, with PKCE + RFC 7591 dynamic registration + discovery.
- ✓ **Both binding scopes are first-class for Harbor.** User-bound OAuth covers the "agent acts as the human" case (Gmail, personal GitHub); agent-bound OAuth covers the "agent acts as itself / shared service account" case (Outlook for the Research Assistant, internal Snowflake service account, shared Slack bot). The Console-driven admin-setup flow targets agent-bound; user-bound is the planner-time interactive prompt.
- ✓ The `MCPUserOAuthRequiredError` shape maps directly onto Harbor's `tool.auth_required` event body, with the addition of `BindingScope` so the Console targets the right principal (user vs admin).
- ✓ The 10-method `OAuth2Provider` interface gives a clean reference for the contract Harbor should implement; Harbor folds both halves into a single `AccessToken(ctx, source)` call where the binding scope lives on the source's config.
- ✓ The `Token` shape, `OAuth2Config` shape, and `OAuth2FlowInitiation` shape are usable as-is with renames; `Token` gains `Scope` + `AgentID` fields.
- ⚠ bifrost's OAuth is MCP-only. Harbor's `OAuthProvider` must be transport-agnostic (MCP + HTTP + A2A).
- ⚠ bifrost has no first-class agent identity. Harbor needs one (RFC-territory; recommendation: `agent_id` joins the identity triple as a fourth peer). This is the largest blocking decision; Phase 30 cannot ship without it.
- ⚠ bifrost surfaces the OAuth-required error synchronously and lets the caller handle the pause. Harbor's runtime handles pauses uniformly via Phase 50 — the OAuth provider's job is just to return the typed error and let the runtime emit the event.
- ⚠ Identity-mandatory + audit redaction + cross-tenant isolation + concurrent-reuse contract are Harbor-only requirements that don't come for free from bifrost.
- ⚠ Concurrent refresh storms on agent-bound tokens need single-flight protection — N sessions racing the same expired token. bifrost has its own handling; Harbor needs its own (test mandatory).
- ✗ Virtual-key tenancy and the OAuth-config-table indirection are not needed (start inline; revisit if shared configs prove common).

## Source artifacts referenced

- bifrost package: `github.com/maximhq/bifrost/core/schemas` @ v1.5.8.
- Bifrost docs: <https://docs.getbifrost.ai/mcp/overview> (Gateway-shaped; the SDK surface is the authoritative reference for our purposes).
- Phase 28 plan: `docs/plans/phase-28-tools-mcp.md` — covers the MCP southbound this brief layers OAuth onto.
- Phase 30 detail block: `docs/plans/README.md` § "30 — Tool-side OAuth + HITL via pause/resume."
- Master plan row: `docs/plans/README.md` line 49.
- RFC anchors: §3.3 (unified pause/resume primitive), §6.4 (tool catalog), §3.4 (fail-loudly).
- CLAUDE.md anchors: §6 (multi-isolation), §7 (security), §17 (E2E integration testing).

## Re-discussion checklist

When the Phase 30 plan is authored, return to this brief and confirm:

- [ ] **Agent-identity RFC stub landed.** `agent_id` placement (fourth identity peer vs metadata) is settled. Provenance shape (actor + requester) is documented. Phase 30 acceptance criteria can reference it.
- [ ] Re-`go doc` the bifrost OAuth surface at then-current version; diff against §"What bifrost provides."
- [ ] Confirm the `BindingScope` discriminator lives on `OAuthConfig` (per-attachment), not on the agent or the source itself.
- [ ] Decide the cross-session OAuth-sharing default for user-bound (user-scoped reuse recommended; session-scoped is opt-in).
- [ ] Decide `OauthConfigID` placement (inlined vs separate config table — recommendation: inline; revisit if shared configs prove common).
- [ ] Decide impersonation rule: which token does an admin-impersonating-user call use? (Recommendation: impersonated identity's token for user-bound; agent's token for agent-bound.)
- [ ] Confirm OAuth callback is a Protocol method (Phase 60+) and the JWT-scope check happens at the Protocol edge (Phase 61). Verify the scope check varies by `BindingScope`.
- [ ] Verify the `OAuthProvider` interface is transport-agnostic (MCP + HTTP + A2A all share it).
- [ ] Single-flight refresh design landed (`(tenant, agent_or_user, source)`-keyed mutex).
- [ ] Encryption-at-rest design landed (tenant-scoped KEK; refresh tokens encrypted separately).
- [ ] Console agent-management view (Phase 73) acceptance criteria reference Phase 30's surface.
- [ ] Document explicitly under "Findings I'm departing from" any place Phase 30 *intentionally* diverges from bifrost's shape OR from this brief's recommendations.
