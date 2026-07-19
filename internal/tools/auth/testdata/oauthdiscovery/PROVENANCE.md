# OAuth requirement discovery fixtures — provenance

These fixtures are committed VERBATIM from the canonical external-protocol
spec artifacts, per CLAUDE.md §17.8 (external-protocol conformance fixtures
derive from the real spec, not a hand-authored interpretation). A
wrong-field-name mutation of a fixture must FAIL the discovery test — the
right-field / wrong-field discriminator.

## `rfc9728_protected_resource.json`

The protected-resource-metadata response example document from
**RFC 9728 — OAuth 2.0 Protected Resource Metadata**, §3.2 ("Protected
Resource Metadata Response"). The load-bearing fields the discovery walker
reads are `resource` and `authorization_servers` (the RFC 9728 array of
authorization-server issuer identifiers).

## `rfc8414_authorization_server.json`

The authorization-server-metadata response example document from
**RFC 8414 — OAuth 2.0 Authorization Server Metadata**, §3.2 ("Authorization
Server Metadata Response"). The walker surfaces `issuer`,
`authorization_endpoint`, `token_endpoint`, `scopes_supported`, and the
optional `registration_endpoint` (RFC 7591 — reported, never invoked).

## `rfc8414_authorization_server_pkce.json`

The RFC 8414 §3.2 example extended with the IANA-registered
`code_challenge_methods_supported` metadata field (RFC 8414 §2 / RFC 7636 PKCE
— the field an AS advertises its PKCE posture on). Kept as a separate fixture
so the base RFC 8414 example stays verbatim while PKCE-posture parsing is
still covered against a spec-shaped document.

## `www_authenticate_challenge.txt`

A captured `WWW-Authenticate` response-header line per the **Model Context
Protocol authorization specification (2025-06-18)** — the `Bearer` challenge
carrying `resource_metadata` that points at the RFC 9728 document. This is the
step-up an unauthorized MCP HTTP call answers with.

## `www_authenticate_insufficient_scope.txt`

A `WWW-Authenticate` response-header line per **RFC 6750 — The OAuth 2.0
Authorization Framework: Bearer Token Usage**, §3.1 ("Error Codes") and the
§3 example, for the `insufficient_scope` error: a `403 Forbidden` whose
challenge carries `error="insufficient_scope"` plus the `scope` parameter
naming the scopes the request requires (RFC 6750 §3 states the `scope`
attribute "SHOULD" be included for this error). The load-bearing fields the
scope-shortfall capture reads are `error` (which MUST be literally
`insufficient_scope` to construct a structured shortfall) and `scope` (the
required-scopes list). A wrong-field mutation — e.g. renaming `scope` or
changing the `error` value — must FAIL the shortfall capture test.
