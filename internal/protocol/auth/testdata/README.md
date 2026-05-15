# `internal/protocol/auth/testdata` — dummy JWT keypairs

These four PEM files are **test-only dummy keypairs** generated for the
Phase 61 unit + security + integration test suite. They are documented
here as such because CLAUDE.md §7 rule 2 forbids hardcoded secrets,
including in tests, except as documented `testdata/` fixtures.

| File | Purpose | Algorithm |
|------|---------|-----------|
| `rs256_private.pem` | Signs test JWTs for `RS256` parser-acceptance tests | RSA 2048 |
| `rs256_public.pem`  | Verifies the above | RSA 2048 |
| `es256_private.pem` | Signs test JWTs for `ES256` parser-acceptance tests | ECDSA P-256 |
| `es256_public.pem`  | Verifies the above | ECDSA P-256 |

**These keys are public knowledge.** They are committed to the
repository, visible in every clone, and were generated locally with
`openssl` for the sole purpose of the Phase 61 test suite. They MUST
NEVER be used to sign a real JWT for any deployed Harbor Runtime — a
fresh keypair is the responsibility of every operator at deploy time.

## Regeneration

The tests do not depend on the byte-exact content of these PEMs — they
only require:

- A working RSA 2048 keypair under `rs256_*.pem`.
- A working ECDSA P-256 keypair under `es256_*.pem`.

Regenerate with:

```bash
cd internal/protocol/auth/testdata
openssl genrsa -out rs256_private.pem 2048
openssl rsa -in rs256_private.pem -pubout -out rs256_public.pem
openssl ecparam -name prime256v1 -genkey -noout -out es256_private.pem
openssl ec -in es256_private.pem -pubout -out es256_public.pem
```

If the regeneration is committed, the tests will continue to pass
without changes — the test loader reads the PEMs at runtime via
`crypto/x509`.

## Why two algorithm families

The asymmetric-algorithm allowlist Harbor enforces (CLAUDE.md §7 rule 1)
covers two families: RSA-PKCS#1v1.5 (`RS256`/`RS384`/`RS512`) and
ECDSA (`ES256`/`ES384`/`ES512`). The security suite needs at least one
keypair from each family to exercise both `Validator` parser paths and
the algorithm-confusion attack shape (an `HS256` token signed with an
asymmetric public key as the HMAC secret — the classical JWT CVE).
`RS256` and `ES256` are the most widely deployed members of each
family, so testdata stops there; the longer-key variants share the
same parser code path.
