# Production identity setup

`harbor serve` boots the headless Runtime behind a **production JWT verifier**.
Unlike `harbor dev`, it mints no token of its own — it only *verifies*. Every
`/v1/*` request must carry a bearer JWT signed by a key the operator has
explicitly told `serve` to trust, via `identity.jwks_url` or
`identity.jwks_file`. This guide is the missing manual for getting a real,
verifiable token into a client's hands and attaching it.

There are **two honest on-ramps**, and they differ only in *who issues the
token*. `serve`'s verifier is identical for both — it does not care whether the
JWK Set it fetches belongs to Auth0 or to a key you generated this morning:

- **You have an identity provider** (Auth0, Okta, Keycloak, Amazon Cognito, or
  any OIDC-compliant IdP) → follow this guide. The IdP signs tokens; you point
  `serve` at the IdP's published JWKS. This is the multi-user / SSO posture.
- **You have no IdP, or you issue your own tokens** → use the `harbor token`
  self-issuing on-ramp (the [last section](#on-ramp-b-no-idp-issue-your-own-tokens)
  of this guide). You generate an asymmetric keypair, point `serve` at the public
  half, and mint tokens with the private half. This is a legitimate
  single-issuer / self-hosting posture — see the honesty note in that section
  before you reach for it.

Both on-ramps converge on the same fact: **a token Harbor accepts is one whose
claims match what `serve.yaml` declares, signed by a key in the JWKS `serve`
loads.** The rest of this guide is the precise shape of those claims and how to
make your issuer produce them.

## What `serve` verifies — the contract in one block

The relevant `serve.yaml` stanza (full example:
[`examples/serve.yaml`](../../../examples/serve.yaml)):

```yaml
identity:
  # Asymmetric algorithms only — HS* and `none` are rejected at the
  # allowlist before the signing key is ever consulted.
  jwt_algorithms: [RS256, ES256]
  # The `iss` claim every accepted token MUST carry, verbatim.
  issuer:   https://your-issuer.example.com
  # The `aud` claim every accepted token MUST carry (or contain).
  audience: harbor
  # Where serve fetches the signing keys. Set EXACTLY ONE of these.
  jwks_url:  https://your-issuer.example.com/.well-known/jwks.json
  # jwks_file: /etc/harbor/jwks.json
```

`serve` refuses to boot if `issuer` **or** `audience` is empty — they are
mandatory for the production profile, not optional niceties. That has a direct
consequence for your tokens, covered next.

## The claim shape Harbor verifies

Harbor's verifier reads a flat set of claims off the JWT. The
multi-isolation triple `(tenant, user, session)` is carried as **three
top-level custom claims** — this is the single most important thing to get
right, because most IdPs do **not** emit them by default. You configure your
IdP to add them (the per-provider sections below show how).

| Claim | Required | Type | Role |
|---|---|---|---|
| `tenant` | **yes** | string, non-empty | The connection's tenant — the outermost isolation scope. |
| `user` | **yes** | string, non-empty | The connection's user. |
| `session` | **yes** | string, non-empty | A **default** session id. The live conversation is normally chosen per-request via the `X-Harbor-Session` header; the claim is the fallback used when that header is absent (see the note below). It must still be a non-empty string in the token. |
| `scopes` | no | array of strings | Elevated scopes from the closed set of three — `admin`, `console:fleet`, `agent_config:user` (see [the scope vocabulary](#the-scope-vocabulary)). Absent or empty = an authenticated but unprivileged connection. |
| `iss` | **yes** in production | string | Must equal `identity.issuer` **exactly**. |
| `aud` | **yes** in production | string or array | Must equal (or contain) `identity.audience`. |
| `exp` | **yes** | numeric date | A token with no `exp` is rejected as expired. Keep it short. |
| `nbf` | no | numeric date | Not-before. Absent = valid immediately. |
| `sub` | no | string | Standard subject — audited, not used for isolation. |
| `kid` (header) | recommended | string | Selects which JWKS key verifies the signature. Required once your JWKS holds more than one key. |

A worked token payload:

```json
{
  "iss": "https://your-issuer.example.com",
  "aud": "harbor",
  "sub": "auth0|6630f1c0a1b2c3d4e5f6a7b8",
  "exp": 1799999999,
  "tenant": "tenant-acme",
  "user": "user-12345",
  "session": "default",
  "scopes": ["console:fleet"]
}
```

> **Why `session` is a placeholder, not "the conversation."** Harbor's session
> is dynamic: the connection token authenticates *who* (`tenant` + `user` +
> scopes), and the client picks *which conversation* per request with the
> `X-Harbor-Session` header. So your IdP should mint a stable, non-empty
> placeholder (`"default"` is conventional) — the real per-conversation id
> comes from the header at request time. The full two-part model is in the
> [auth & identity choreography](./auth-and-identity.md). The triple must
> nonetheless be complete in the token: an empty `session` claim is rejected
> the same as an empty `tenant`.

### The `iss` / `aud` contract (the most common production 401)

Because `serve` mandates a non-empty `issuer` and `audience`, the verifier
enforces both **exactly**, fail-closed:

- A token whose `iss` is anything other than your configured `identity.issuer`
  is rejected — `401 auth_rejected`.
- A token whose `aud` does not equal (or, for an array `aud`, contain) your
  configured `identity.audience` is rejected — `401 auth_rejected`.

This is the failure operators hit most often: a token minted with a default
issuer/audience (for example a leftover dev value) sails through signing but
**401s against any real `serve.yaml`**, because the strings don't match. When
you configure the IdP and when you mint, the `iss` and `aud` in the token MUST
equal the `issuer` and `audience` in your `serve.yaml`, character for
character.

### The scope vocabulary

Harbor's scope universe is a **closed set of three** — an unknown scope on a
JWT is silently dropped, so an attacker cannot grant themselves an
undocumented privilege by inventing a name. Map your IdP's roles/groups onto
these in the `scopes` claim:

| Scope | Grants |
|---|---|
| `admin` | Cross-tenant fan-in on every read surface; the admin-gated mutations; admin impersonation. |
| `console:fleet` | Fleet observation — cross-tenant reads, posture snapshots, `sessions.list` — observation-only, satisfies no mutation gate. |
| `agent_config:user` | The per-user agent-config tier: a non-admin caller owns a durable, versioned safe-subset config variant across their own sessions (the `agent_config.user.*` verbs gate on it). Strictly below `admin` and orthogonal to it — an admin token does *not* implicitly own per-user variants. |

A connection with no `scopes` is still fully authenticated; it just acts only
within its own `(tenant, user)` and cannot reach elevated surfaces. The
authoritative, per-method scope map is the
[methods reference](./methods.md) Auth column.

## On-ramp A — register an OIDC app

The flow is the same for every provider; only the "add custom claims" step
differs.

1. **Register an application** with your IdP for the machine-to-machine /
   backend that will hold the Harbor connection. For server-side clients this
   is an OAuth2 **client-credentials** application (a confidential client with a
   client id + secret). For user-facing logins it is whatever flow your app
   already uses — what matters to Harbor is the *resulting* access token's
   claims.
2. **Set the audience / API identifier** to the value you will put in
   `identity.audience` (e.g. `harbor`). Many IdPs only include an `aud` claim
   when the token is requested for a registered API/resource — register one.
3. **Note the issuer.** Your IdP's issuer URL (e.g.
   `https://your-tenant.us.auth0.com/` or
   `https://login.example.com/realms/harbor`) becomes `identity.issuer`. Its
   JWKS endpoint (usually `<issuer>/.well-known/jwks.json` or
   `<issuer>/protocol/openid-connect/certs`) becomes `identity.jwks_url`.
4. **Add the Harbor custom claims.** Configure the IdP to inject `tenant`,
   `user`, `session`, and `scopes` as **top-level** claims on the access token.
   This is the step the per-provider snippets below cover.
5. **Confirm the signing algorithm** is one Harbor allows — RS256 (the OIDC
   default) or any of RS384/RS512/ES256/ES384/ES512. `HS*` and `none` are
   rejected. List the one(s) your IdP uses in `identity.jwt_algorithms`.

### Auth0

Add the claims with a **Login / Client-Credentials Action**. Auth0 namespaces
custom claims by URL, but Harbor reads **unprefixed** top-level claims, so emit
them bare:

```js
// Auth0 Action — "Add Harbor identity claims"
exports.onExecuteCredentialsExchange = async (event, api) => {
  // Source these from the client's app_metadata, the requested
  // organization, or your own mapping — shown inline for clarity.
  api.accessToken.setCustomClaim("tenant", event.client.metadata.tenant);
  api.accessToken.setCustomClaim("user", event.client.client_id);
  api.accessToken.setCustomClaim("session", "default");
  api.accessToken.setCustomClaim("scopes", ["console:fleet"]);
};
```

- `identity.issuer`: `https://YOUR_TENANT.us.auth0.com/` (note the trailing
  slash Auth0 uses).
- `identity.audience`: the API Identifier you registered (request the token
  with `audience=<that value>`).
- `identity.jwks_url`: `https://YOUR_TENANT.us.auth0.com/.well-known/jwks.json`.
- `identity.jwt_algorithms`: `[RS256]`.

### Okta

Add the claims under **Security → API → Authorization Servers → your server →
Claims**. Create one claim per Harbor field, included in the **access token**:

| Claim name | Include in | Value (expression) |
|---|---|---|
| `tenant` | Access Token | `appuser.tenant` (or a static/group expression) |
| `user` | Access Token | `user.id` |
| `session` | Access Token | `"default"` |
| `scopes` | Access Token | `Arrays.flatten("console:fleet")` |

- `identity.issuer`: your Okta authorization-server issuer, e.g.
  `https://YOUR_ORG.okta.com/oauth2/aus...`.
- `identity.audience`: the authorization server's **Audience** value.
- `identity.jwks_url`: `<issuer>/v1/keys`.
- `identity.jwt_algorithms`: `[RS256]`.

### Keycloak

Use **Client scopes → (a dedicated scope) → Mappers** to add hardcoded or
user-attribute mappers, each with *Add to access token* enabled:

- A **User Attribute** mapper → token claim `tenant` (claim JSON type
  `String`).
- A **User Property** mapper (`id`) → token claim `user`.
- A **Hardcoded claim** mapper → `session` = `default`.
- A **Hardcoded claim** (or a Group/Role mapper) → `scopes` (claim JSON type
  `JSON` so it serialises as an array).

- `identity.issuer`:
  `https://KEYCLOAK_HOST/realms/YOUR_REALM`.
- `identity.audience`: set a dedicated **Audience** mapper to `harbor` (or your
  chosen value) — Keycloak does not add a usable `aud` unless you map one.
- `identity.jwks_url`:
  `https://KEYCLOAK_HOST/realms/YOUR_REALM/protocol/openid-connect/certs`.
- `identity.jwt_algorithms`: `[RS256]` (or `[ES256]` if you set the realm key
  to ECDSA).

### Amazon Cognito

Cognito access tokens do not carry arbitrary custom claims out of the box — add
them with a **Pre-Token-Generation Lambda** (the "V2" / access-token trigger):

```js
// Cognito Pre-Token-Generation (V2_0) — add Harbor claims to the access token
exports.handler = async (event) => {
  event.response = {
    claimsAndScopeOverrideDetails: {
      accessTokenGeneration: {
        claimsToAddOrOverride: {
          tenant: event.request.userAttributes["custom:tenant"],
          user: event.request.userAttributes.sub,
          session: "default",
          scopes: ["console:fleet"], // a JSON array claim
        },
      },
    },
  };
  return event;
};
```

- `identity.issuer`:
  `https://cognito-idp.REGION.amazonaws.com/USER_POOL_ID`.
- `identity.audience`: the app-client id (Cognito access tokens place it in
  `client_id`; if your tokens use `aud`, match that — confirm against an actual
  token, see below).
- `identity.jwks_url`: `<issuer>/.well-known/jwks.json`.
- `identity.jwt_algorithms`: `[RS256]`.

> Always **decode a real token** (paste it into any JWT decoder, or
> `jq -R 'split(".")[1] | @base64d | fromjson'`) and confirm `iss`, `aud`,
> `tenant`, `user`, `session`, and `scopes` are present and exactly match your
> `serve.yaml` before you wire a client. A self-consistent config that mints
> the wrong `aud` looks fine until the first `401`.

## Mint-and-test walkthrough

Prove the loop end-to-end before you ship a client.

1. **Boot `serve`** against your identity config:

   ```bash
   harbor serve --config serve.yaml
   ```

   It boots behind the JWKS verifier and mints no token of its own (unlike
   `harbor dev`). If `issuer`/`audience`/JWKS are misconfigured it exits
   non-zero with a named-field error — fix that first.

2. **Obtain a token** the way a backend client will. For client-credentials:

   ```bash
   TOKEN=$(curl -sS https://your-issuer.example.com/oauth/token \
     -H 'Content-Type: application/json' \
     -d '{
       "grant_type": "client_credentials",
       "client_id": "YOUR_CLIENT_ID",
       "client_secret": "YOUR_CLIENT_SECRET",
       "audience": "harbor"
     }' | jq -r .access_token)
   ```

3. **Decode and eyeball it** (catches the `iss`/`aud`/claim-shape mismatch
   before the network does):

   ```bash
   # JWT payloads are base64url (- and _, no padding); translate to standard
   # base64 before decoding so GNU/BSD `base64 -d` does not choke silently.
   echo "$TOKEN" | cut -d. -f2 | tr '_-' '/+' | base64 -d | jq .
   ```

4. **Handshake with `runtime.info`** — the first call any client makes:

   ```bash
   curl -sS -X POST https://your-harbor-host:8080/v1/control/runtime.info \
     -H "Authorization: Bearer $TOKEN" \
     -H "X-Harbor-Session: default" \
     -H "Content-Type: application/json" \
     -d '{"identity": {}}'
   ```

   A `200` with an `instance_id` / `protocol_version` / `capabilities` body
   means the token verified and the triple resolved. A `401 auth_rejected`
   means the token failed verification (almost always `iss`/`aud`/`exp`/`alg`
   or a `kid` the JWKS doesn't hold); a `401 identity_required` means a claim
   in the triple was missing or empty. The full rejection table — which `code`
   means what, and the fix for each — is in the
   [auth & identity choreography](./auth-and-identity.md#what-401-403-mean-branch-on-code-not-on-the-status-alone).

5. **Attach a real client.** The same token + the `X-Harbor-Session` header
   drive every method. The end-to-end client recipe is
   [build a client](./build-a-client.md); a complete, SDK-free worked OIDC
   client lives at `examples/protocol-clients/oidc-client-example/`, doing
   exactly this flow against `serve`.

## On-ramp B: no IdP, issue your own tokens

Standing up an IdP is not a prerequisite for `harbor serve`. The cliff was
never "you must buy an IdP" — it was that the self-issuing path was
undocumented. The **`harbor token`** subcommand closes it: you generate an
asymmetric keypair, point `serve`'s `identity.jwks_file` at the public half,
and mint Harbor JWTs with the private half.

```bash
# 1. Generate a keypair + the matching public JWK Set.
harbor token keygen --out ./identity --alg ES256

# 2. Point serve at the public JWKS (instead of jwks_url):
#    identity.jwks_file: ./identity/jwks.json
#    identity.issuer:    https://harbor.internal     (your chosen values)
#    identity.audience:  harbor

# 3. Mint a token whose iss/aud MATCH serve.yaml exactly.
harbor token mint --key ./identity/private.pem \
  --tenant tenant-acme --user user-1 --session default \
  --issuer https://harbor.internal --audience harbor \
  --scopes console:fleet --ttl 1h

# 4. Attach with the minted JWT exactly as in the walkthrough above.
```

`--issuer` and `--audience` are mandatory and **must equal** your
`identity.issuer` / `identity.audience` — the same exact-match contract every
token is held to. Mint with no `--scopes` for a least-privileged token (the
default); add `console:fleet` or `admin` only when the connection needs them.

### `harbor token` reference

`keygen` writes two files under `--out`:

| File | Mode | Purpose |
| --- | --- | --- |
| `private.pem` | `0600` (parent dir `0700`) | Your signing key — **keep it private**. keygen refuses to overwrite it without `--force`. |
| `jwks.json` | `0644` | The public JWK Set (RFC 7517) you point `identity.jwks_file` at. |

- `--alg` selects the algorithm: **`ES256`** (ECDSA P-256, the default — fast
  keygen) or **`RS256`** (RSA-2048, opt-in). Both are on `serve`'s asymmetric
  allowlist (`HS*` / `none` are never accepted).
- The JWK Set's key id (`kid`) is the **RFC 7638 JWK thumbprint** of the key —
  content-derived, not a constant. `mint` stamps the same `kid` into the token
  header by default, so the verifier resolves the key automatically. Pass
  `--kid` only if you manage a multi-key set by hand.

`mint` flags: `--key` (the `private.pem`), the identity triple
(`--tenant` / `--user` / `--session`), `--issuer` / `--audience` (mandatory,
exact-match), and the optional `--kid`, `--scopes` (comma-separated; default
none), and `--ttl` (default `1h`, echoed to stderr). The signed token is the
only thing written to stdout — capture it with `TOKEN=$(harbor token mint …)`.
The private key is never logged or printed.

The attach flow is identical to On-ramp A; the only difference is *who holds the
signing key.*

> **Honesty note — know what grade this is.** Self-issued tokens are signed by
> a key **you** manage. Protect the private key (it is the entire trust root —
> anyone who holds it can mint any identity), keep it out of version control,
> and rotate it deliberately. This is a legitimate posture for an eval, a
> single-tenant deployment, or a self-hosting operator who is their own
> issuer. It is **not** multi-user SSO: there is no central revocation, no
> login UI, no per-user lifecycle. When you need those — multiple human users,
> federated login, centralized revocation — graduate to a real IdP and follow
> on-ramp A above. The two on-ramps share `serve`'s verifier exactly; moving
> between them is a config change, not a re-architecture.

## See also

- [Auth & identity choreography](./auth-and-identity.md) — the two-part
  token/header session model, the scope vocabulary, and the full `401`/`403`
  rejection table.
- [Build a client](./build-a-client.md) — the three moves every Protocol
  client makes once it has a token.
- [Protocol quickstart](./quickstart.md) — speak Protocol in 15 minutes
  against `harbor dev`.
- [`examples/serve.yaml`](../../../examples/serve.yaml) — the annotated
  production `serve` config.
