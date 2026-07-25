# Choreography 1 — Auth & identity

How a Protocol client authenticates, which scopes exist, and how the
multi-isolation triple `(tenant, user, session)` flows through every request.

> Methods demonstrated: `start`, `sessions.list`, `auth.rotate_token`

## The two-part model: the token says WHO, the header says WHICH conversation

The connection token is a **per-backend credential, like an API key**. It
authenticates `(tenant, user)` plus the connection's scopes. It does **not**
pin a session. The **session is dynamic — chosen per-request** via the
`X-Harbor-Session` header, always scoped under the token's verified
`(tenant, user)`:

```text
Authorization: Bearer <connection-token>
X-Harbor-Session: <conversation-session-id>
```

Rules the Runtime enforces at the auth middleware, fail-closed:

- `X-Harbor-Session` present and non-empty → it **replaces** the token's
  `session` claim. Tenant and user stay token-verified.
- Header absent → the token's `session` claim is used as a **back-compat
  default** (the dev token carries `session: dev` for exactly this).
- Neither a header nor a default claim → `401 identity_required`. The session
  is mandatory; identity fails closed — there is no silent default.
- The header can only ever set the *session*. A client can never widen its
  tenant or user — there is no header override for those on the
  authenticated path.

**New conversation = new session id = create-on-first-use.** Pick a fresh id
(a UUID is recommended; any non-empty string works) and issue a `start` with
it — the Runtime materialises the session row on the first turn. There is no
explicit "open session" call. A `start` on an already-open session is the
normal second-and-later turn (not an error); a `start` on a **closed** session
(explicitly closed or GC-reaped) **reopens** it — the conversation resumes with
its history intact. The one exception is a session that was permanently deleted
by `sessions.delete` (right-to-erasure): a `start` on an **erased** session id
is rejected `session_erased` — the conversation is gone; start a new one with a
fresh session id.

Many sessions coexist under one token, fully isolated: every storage read and
event delivery filters by the complete `(tenant, user, session)` triple. One
user driving five concurrent conversations gets five disjoint worlds.

## The JWT

Asymmetric algorithms **only**: RS256 / RS384 / RS512 / ES256 / ES384 / ES512.
`HS*` and `none` are rejected at the algorithm allowlist before the keyfunc is
ever consulted. The verified claims:

| Claim | Role |
|---|---|
| `tenant` | verified — the connection's tenant |
| `user` | verified — the connection's user |
| `session` | default only — used iff `X-Harbor-Session` is absent |
| `scopes` | the connection's elevated scopes (may be empty) |
| `iss` / `aud` / `exp` / `nbf` / `kid` | standard JWT validation against the Runtime's `identity:` config |

The body of a request may also carry an `identity` object
([`IdentityScope`](./types.md#identityscope)). The simplest correct client
sends `"identity": {}` and relies on the header — the Runtime backfills the
body from the verified request identity, and rejects any non-empty body
component that contradicts it (`401 identity_required`). The body's `run` and
`scope` fields are independent of the token and survive the backfill (they
parameterise steering — see [task control](./task-control.md)).

## The scope vocabulary (closed set)

Two elevated scopes exist. There is no third; per-surface scopes
(`tools.admin`, `events.crosstenant`, …) are deliberately not minted:

| Scope | Grants |
|---|---|
| `admin` | Cross-tenant fan-in on every read surface that has one; the admin-gated mutations (`artifacts.delete`, `memory.put` / `memory.delete`, the `mcp.servers.*` and `tools.*` admin verbs, the `agents.*` fleet-control verbs, `flows.run`, `auth.rotate_token`); admin impersonation. |
| `console:fleet` | Fleet observation: cross-tenant fan-in on `events.subscribe` / `events.aggregate`, the five `search.*` methods, `sessions.list`, `artifacts.list`, `memory.list`, and the seven posture reads (`runtime.*`, `metrics.snapshot`, `governance.posture`, `llm.posture`). It is observation-only — it satisfies **no** mutation gate, and the admin-only fan-ins (`tasks.list`, `pause.list`, `topology.snapshot`, `flows.list` / `flows.runs.list`) do not consult it. The per-method Auth column in the [methods reference](./methods.md) is the authoritative row-level map. |

Distinct from both: the **steering scope** (`session_user` / `owner_user` /
`admin`) — the privilege tier each control is checked against per its RFC §6.3
minimum. It is **derived from your verified token**, never read from a request
body: the Runtime compares your token's `(tenant, user)` and `admin` claim
against the target run (`admin` → cross-tenant + every control; you own the run
→ `owner_user`, which covers `inject_context` / `user_message` / `cancel` /
`pause` / `resume` / `redirect` / `approve` / `reject`; otherwise no authority).
A `prioritize` or any cross-tenant control needs `admin`. The body's `scope`
field is ignored. See [task control](./task-control.md).

## Dev bootstrap vs production posture

**Dev** — `harbor dev` mints an ephemeral ES256 keypair at boot and serves
`POST /v1/dev/bootstrap.json` (loopback-only — non-loopback peers get a flat
403 regardless of headers; the request `Host` must also name a local
authority such as `localhost` or `127.0.0.1`, with an optional port). The
minted token carries
`tenant=dev / user=dev / session=dev (default)` and the full scope set
`["admin", "console:fleet"]`. It is printed at boot as `HARBOR_DEV_TOKEN=` and
returned by the bootstrap envelope. The [quickstart](./quickstart.md) uses it.

To mint a **lesser-privileged token** for testing the steering authorization
contract — a token for a chosen identity with no `admin` scope — POST an
optional override body to the same loopback endpoint:

```bash
curl -sS -X POST http://127.0.0.1:18080/v1/dev/bootstrap.json \
  -d '{"tenant":"dev","user":"dev","session":"dev","scopes":[]}'
```

An explicit empty `scopes` array mints a token with no scopes (a non-admin
token); a full `(tenant, user, session)` triple overrides the identity. An
empty `{}` body keeps the default admin token, so the one-click attach flow is
unchanged. This override is dev-only — `harbor serve` does not mint tokens.

**Production** — the Runtime validates tokens against your OIDC provider via
the `identity:` config block (`issuer`, `audience`, `jwks_url`,
`jwt_algorithms`). Your identity provider mints the tokens; the bootstrap
endpoint does not exist on `harbor serve`. An `admin`-scoped operator can
rotate their own token via `auth.rotate_token` (one-time reveal; every
rotation emits a redacted audit event). The end-to-end setup — registering an
OIDC app, mapping the `(tenant, user, session)` + `scopes` claims, the
`iss`/`aud` exact-match contract, the mint-and-test loop, and the no-IdP
`harbor token` self-issuing on-ramp — is the
[production identity setup guide](./production-identity-setup.md).

## What 401 / 403 mean (branch on `code`, not on the status alone)

Four distinct rejections, one error envelope each
([errors reference](./errors.md)):

| HTTP | `code` | Meaning | Fix |
|---|---|---|---|
| 401 | `identity_required` | No identity resolved: missing bearer, missing session, or a body identity contradicting the token. | Attach the token / the `X-Harbor-Session` header; send `"identity": {}`. |
| 401 | `auth_rejected` | A token was present but failed verification (bad alg / signature / expiry / `kid` / audience / issuer). | Obtain a fresh, correctly-issued token. |
| 403 | `identity_scope_required` | Authenticated and identified, but the requested cross-tenant fan-in or admin verb needs `admin` / `console:fleet`. | Re-authenticate with a scope-bearing token. |
| 403 | `scope_mismatch` | A steering control's body `scope` claim is below the control's RFC §6.3 minimum, or cross-tenant steering without `admin`. | Use a sufficient steering scope. |

A useful diagnostic habit: `401` means "the wire doesn't know who you are";
`403` means "it knows exactly who you are, and that's the problem."

## Listing what a connection can see

`sessions.list` returns every session under the connection's
`(tenant, user)` — all conversations, open and closed, surviving Runtime
restarts (the session catalog persists in the StateStore):

```bash
curl -sS -X POST "$HARBOR_BASE_URL/v1/sessions/list" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Harbor-Session: any" \
  -H "Content-Type: application/json" \
  -d '{"identity": {}, "filter": {}, "limit": 50}'
```

A cross-tenant `filter.tenant_ids` requires `admin`; without it the call is
rejected with `scope_mismatch`. Existence is never revealed across tenants —
a foreign session id behaves exactly like a missing one (`not_found`).
