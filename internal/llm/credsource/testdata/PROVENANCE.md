# Inference-plane broker-pull fixture provenance

The broker fixture used by this package's tests is the executable reference
server `internal/llm/credsource/inferencebrokertest`, NOT a hand-authored,
self-consistent JSON blob (the D-216 rubber-stamp failure mode CLAUDE.md
§17.8 closes).

## The canonical contract it derives from

The inference-plane credential-pull contract mirrors, one plane over, the
shipped tool-plane `remote` credential-source contract
(`internal/tools/auth/credsource/drivers/remote`, D-285) — the same
authenticated-GET / strict-JSON / `format_version` shape, differing only in
the resolved field:

```text
GET <credential_url>
Authorization: Bearer <runtime service token from auth_token_env>
Accept: application/json
→ 200 {"format_version":1,"api_key":"<provider key>","expires_in":<seconds>}
```

- `format_version` — REQUIRED; the runtime accepts version `1`. An
  unsupported version fails loud (contract-drift guard).
- `api_key` — REQUIRED; the LLM provider API key. NEVER logged; held in
  memory only, rides the atomic `LiveKey` swap.
- `expires_in` — OPTIONAL seconds; omitted / 0 means "no expiry advertised"
  (the serve horizon is then the operator `cache_ttl` cap alone).

Unknown top-level fields are rejected (`DisallowUnknownFields`) so a
mis-served field fails loud instead of being silently ignored.

## Why a reference server, not a static fixture

A self-consistent hand fixture passes even when the source is wired to the
wrong field or placement — the test goes green while the feature is inert
against a real coordinator. The `inferencebrokertest.FixtureBroker` is the
same artifact a coordinator author builds against: it asserts the request
method (GET), the Bearer credential (401 on mismatch), and drives every
failure leg (down / malformed / unauthorized / bad-version / redirect) plus
the rotation leg — so the tests exercise the code against the contract's
real edges, not a rubber stamp.

All credential values in tests are documented dummy fixtures (§7 rule 2),
never real secrets.
