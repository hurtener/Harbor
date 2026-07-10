---
name: configure-production-identity
description: "Get a real, verifiable JWT into a client and attach it to a production `harbor serve` — OIDC app registration, the (tenant, user, session) + scopes claim shape, the iss/aud exact-match contract, and the no-IdP `harbor token` self-issuing on-ramp. Use when moving off `harbor dev`'s ephemeral token to a production deployment, wiring an IdP (Auth0 / Okta / Keycloak / Cognito), or standing up your own issuer."
license: Apache-2.0
metadata:
  framework: harbor
  surface: protocol
  verbs: "serve, token"
---

# Configure production identity

`harbor dev` hands you an ephemeral `HARBOR_DEV_TOKEN` and you never think about
auth. `harbor serve` is the opposite: it boots the headless Runtime behind a
**production JWT verifier**, mints **no token of its own**, and rejects every
`/v1/*` request that does not carry a bearer JWT signed by a key you explicitly
told it to trust. This skill is the operator playbook for crossing that gap —
getting a real, verifiable token into a client's hands and attaching it.

The authority for every claim below is the published
[production identity setup guide](https://hurtener.github.io/Harbor/protocol/production-identity-setup);
this skill is the fast path through it. Read the guide when you need the full
per-provider detail.

There are **two honest on-ramps**. They differ only in *who issues the token* —
`serve`'s verifier is identical for both:

1. **You have an identity provider** (Auth0, Okta, Keycloak, Amazon Cognito, or
   any OIDC IdP) — the IdP signs tokens; you point `serve` at its published
   JWKS. This is the multi-user / SSO posture. → §A below.
2. **You have no IdP, or you issue your own tokens** — generate a keypair, point
   `serve` at the public half, mint tokens with the private half via
   `harbor token`. A legitimate single-issuer / self-hosting posture. → §B below.

Both converge on one fact: **a token Harbor accepts is one whose claims match
what `serve.yaml` declares, signed by a key in the JWKS `serve` loads.**

## 1. What `serve` verifies — the contract

The production `identity:` stanza (full example:
[`examples/serve.yaml`](../../../examples/serve.yaml)):

```yaml
identity:
  jwt_algorithms: [RS256, ES256]   # asymmetric only — HS* and `none` are rejected
  issuer:   https://your-issuer.example.com   # the `iss` every token MUST carry, verbatim
  audience: harbor                            # the `aud` every token MUST carry (or contain)
  jwks_url: https://your-issuer.example.com/.well-known/jwks.json   # set EXACTLY ONE
  # jwks_file: /etc/harbor/jwks.json
```

`serve` **refuses to boot** if `issuer` or `audience` is empty — they are
mandatory for the production profile. That has a direct consequence for your
tokens (the §3 exact-match contract).

### The same contract binds external serving binaries (`--with-server` / `sdk/server`)

The stock `harbor serve` binary is not the only production Protocol server. A
scaffolded agent built with `harbor scaffold --with-server` — whose
`cmd/<agent>/main.go` reaches the Protocol through the public `sdk/server`
facade — is **also** a production Protocol server, and it enforces the exact
same identity contract: `server.Open` always builds the JWKS verifier from this
`identity:` stanza and **fails loud** (naming the missing field) when the JWKS
source is absent. There is no dev-signer and no mock knob on that path — the
posture is production-only by construction. So everything below (the claim
shape, the iss/aud exact-match rule, the `harbor token` self-issuing on-ramp)
applies verbatim whether you deploy `harbor serve` or your own
`--with-server` binary. See
[`scaffold-a-harbor-agent`](../scaffold-a-harbor-agent/SKILL.md) for the
serving scaffold and its `harbor token` local-dev loop.

## 2. The claim shape

Harbor's verifier reads a flat set of claims off the JWT. The multi-isolation
triple is carried as **three top-level custom claims** — the single most
important thing to get right, because most IdPs do **not** emit them by default.

| Claim | Required | Role |
|---|---|---|
| `tenant` | **yes** (non-empty) | The outermost isolation scope. |
| `user` | **yes** (non-empty) | The connection's user. |
| `session` | **yes** (non-empty) | A **default** session id — a placeholder, not "the conversation" (see below). |
| `scopes` | no | Elevated scopes — a closed set: `admin`, `console:fleet`, `agent_config:user`. Absent = authenticated but unprivileged. |
| `iss` | **yes** | Must equal `identity.issuer` **exactly**. |
| `aud` | **yes** | Must equal (or, for an array, contain) `identity.audience`. |
| `exp` | **yes** | A token with no `exp` is rejected as expired. Keep it short. |

> **Why `session` is a placeholder.** The connection token authenticates *who*
> (`tenant` + `user` + scopes); the client picks *which conversation* per request
> with the `X-Harbor-Session` header (the per-request session selector). So mint
> a stable, non-empty placeholder (`"default"` is conventional) — the real
> per-conversation id rides the header. The triple must still be **complete** in
> the token: an empty `session` claim is rejected the same as an empty `tenant`.
> The two-part token/header model is covered in
> [`use-the-harbor-protocol`](../use-the-harbor-protocol/SKILL.md) §1.

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

## 3. The `iss` / `aud` exact-match contract (the most common production 401)

Because `serve` mandates a non-empty `issuer` and `audience`, the verifier
enforces both **exactly, fail-closed**:

- A token whose `iss` is anything other than your configured `identity.issuer` →
  `401 auth_rejected`.
- A token whose `aud` does not equal (or contain) your configured
  `identity.audience` → `401 auth_rejected`.

This is the failure operators hit most often: a token minted with a leftover
default issuer/audience signs fine but **401s against any real `serve.yaml`**,
because the strings don't match. When you configure the IdP **and** when you
mint, `iss`/`aud` in the token MUST equal `issuer`/`audience` in `serve.yaml`,
character for character.

## A. Real IdP — the OIDC on-ramp

The flow is the same for every provider; only the "add custom claims" step
differs.

1. **Register an application** for the backend that holds the Harbor connection.
   For server-side clients this is an OAuth2 **client-credentials** app.
2. **Set the audience / API identifier** to the value you will put in
   `identity.audience` (e.g. `harbor`). Many IdPs only emit `aud` when the token
   is requested for a registered API/resource — register one.
3. **Note the issuer.** The IdP's issuer URL becomes `identity.issuer`; its JWKS
   endpoint (usually `<issuer>/.well-known/jwks.json` or
   `<issuer>/protocol/openid-connect/certs`) becomes `identity.jwks_url`.
4. **Add the Harbor custom claims** — inject `tenant`, `user`, `session`, and
   `scopes` as **top-level** claims on the access token. This is the
   per-provider step the guide's snippets cover: an Auth0
   **Client-Credentials Action**, Okta **authorization-server Claims**, a
   Keycloak **client-scope Mapper**, or a Cognito **Pre-Token-Generation
   Lambda**. Auth0 namespaces claims by URL by default — Harbor reads
   **unprefixed** top-level claims, so emit them bare.
5. **Confirm the signing algorithm** is one Harbor allows — RS256 (the OIDC
   default) or any of RS384/RS512/ES256/ES384/ES512. `HS*` and `none` are
   rejected. List it in `identity.jwt_algorithms`.

The four worked provider snippets (Auth0 / Okta / Keycloak / Cognito) live in
the [setup guide](https://hurtener.github.io/Harbor/protocol/production-identity-setup#on-ramp-a-register-an-oidc-app).

## B. No IdP — the `harbor token` self-issuing on-ramp

Standing up an IdP is **not** a prerequisite for `harbor serve`. The cliff was
never "you must buy an IdP" — it was that the self-issuing path was undocumented.
`harbor token` closes it: generate a keypair, point `serve`'s
`identity.jwks_file` at the public half, mint Harbor JWTs with the private half.

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
```

`--issuer` / `--audience` are mandatory and **must equal** `identity.issuer` /
`identity.audience`. Mint with no `--scopes` for a least-privileged token.

> **Honesty note — know what grade this is.** Self-issued tokens are signed by a
> key **you** manage — protect the private key (it is the entire trust root),
> keep it out of version control, rotate it deliberately. This is legitimate for
> an eval, a single-tenant deployment, or a self-hosting operator who is their
> own issuer. It is **not** multi-user SSO — no central revocation, no login UI,
> no per-user lifecycle. When you need those, graduate to on-ramp A; the two
> share `serve`'s verifier exactly, so moving between them is a config change,
> not a re-architecture.

## 4. Mint-and-test — prove the loop before shipping a client

1. **Boot `serve`** against your identity config:

   ```bash
   harbor serve --config serve.yaml
   ```

   It prints that it mints no token and verifies against your JWKS. A
   misconfigured `issuer`/`audience`/JWKS exits non-zero with a named-field error
   — fix that first.

2. **Obtain a token** the way a backend client will (client-credentials grant
   for on-ramp A, or `harbor token mint` for on-ramp B).

3. **Decode and eyeball it** — catches the `iss`/`aud`/claim-shape mismatch
   before the network does:

   ```bash
   # JWT payloads are base64url; translate before decoding.
   echo "$TOKEN" | cut -d. -f2 | tr '_-' '/+' | base64 -d | jq .
   ```

   Confirm `iss`, `aud`, `tenant`, `user`, `session`, and `scopes` are present
   and exactly match `serve.yaml`. A self-consistent config that mints the wrong
   `aud` looks fine until the first `401`.

4. **Handshake with `runtime.info`** — the first call any client makes:

   ```bash
   curl -sS -X POST https://your-harbor-host:8080/v1/control/runtime.info \
     -H "Authorization: Bearer $TOKEN" \
     -H "X-Harbor-Session: default" \
     -H "Content-Type: application/json" \
     -d '{"identity": {}}'
   ```

   A `200` with `instance_id` / `protocol_version` / `capabilities` means the
   token verified and the triple resolved. A `401 auth_rejected` means
   verification failed (almost always `iss`/`aud`/`exp`/`alg` or a `kid` the JWKS
   doesn't hold); a `401 identity_required` means a triple claim was missing or
   empty.

5. **Attach a real client.** The same token + the `X-Harbor-Session` header drive
   every method — the same wire [`use-the-harbor-protocol`](../use-the-harbor-protocol/SKILL.md)
   walks. A complete, SDK-free worked OIDC client ships under
   `examples/protocol-clients/`, doing exactly this flow against `serve`.

## Common failure modes

- **Every call 401s with `auth_rejected`.** The `iss`/`aud` exact-match contract
  (§3) — your token's strings don't equal `serve.yaml`'s. Decode the token (step
  3) and compare character-for-character.
- **`serve` exits non-zero at boot.** `issuer` or `audience` is empty, or the
  JWKS is unreachable — the production profile mandates both. The error names the
  field.
- **`401 identity_required`.** A triple claim (`tenant` / `user` / `session`) is
  missing or empty in the token. Most often the IdP isn't emitting the custom
  claims — re-check the per-provider claim step in §A.4. An empty `session` is
  rejected the same as an empty `tenant`.
- **`alg` rejected.** Your IdP signs with `HS256` (or the token uses `none`).
  Harbor allows asymmetric only — switch the signing key to RS*/ES* and list it
  in `identity.jwt_algorithms`.
- **`kid` not found.** Your JWKS holds more than one key and the token's header
  `kid` doesn't select one — make sure the IdP stamps the `kid` and `serve`'s
  JWKS carries the matching key.

## See also

- [Production identity setup guide](https://hurtener.github.io/Harbor/protocol/production-identity-setup)
  — the full manual this skill operationalizes (per-provider snippets, the scope
  vocabulary, the complete `harbor token` walkthrough).
- [`use-the-harbor-protocol`](../use-the-harbor-protocol/SKILL.md) — once you
  have a token, this is the wire: `runtime.info`, `start`, the events stream.
- [`run-the-dev-loop`](../run-the-dev-loop/SKILL.md) — the `harbor dev`
  ephemeral-token path you're graduating from.
- [`validate-and-package`](../validate-and-package/SKILL.md) — the rest of the
  production checklist.
- [`examples/serve.yaml`](../../../examples/serve.yaml) — the annotated
  production `serve` config.
