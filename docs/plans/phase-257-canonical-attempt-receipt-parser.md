# Phase 257 — strict public canonical attempt-receipt parser (HA-72, D-437)

## Summary

Harbor exposes the strict public inverse of its canonical content-free attempt
receipt encoder. External receivers can reconstruct the public receipt without
importing `internal/` or copying the private snake-case wire, while every
alternate or malformed JSON representation fails closed.

This is the parser-only slice of HA-72. It adds no coordinator transport,
Protocol method, runtime readiness claim, persistence, goroutine, timer,
network call, or idle database work.

## RFC anchor

- RFC §6.5 — the provider-neutral LLM edge owns attempt usage facts.
- RFC §6.11 — governance accounting remains bounded and fail closed.
- RFC §6.15 — usage and cost facts remain content-free and explicit.

## Briefs informing this phase

- brief 03 — one provider-neutral LLM contract rather than parallel modes.
- brief 06 — external consumers use canonical projections, not private state.
- brief 08 — usage/cost facts and cancellation stay explicit at the LLM edge.

## Brief findings incorporated

- brief 03 §5: do not create a parallel LLM mode; the parser is the inverse of
  Harbor's existing canonical receipt encoder.
- brief 06 §1: external consumers depend on a canonical projection rather than
  private runtime state; the SDK returns the public receipt and keeps the wire
  type private.
- brief 06 §6: telemetry consumers need stable token, cost, latency, and
  identity facts; the existing receipt validator remains the semantic gate.
- brief 08 §"What bifrost provides": provider usage and cost are normalized at
  the LLM edge; the parser does not reinterpret or estimate them.

## Findings I'm departing from (if any)

None.

## Goals

- Export exactly
  `UnmarshalCanonicalAttemptUsageReceipt([]byte) (AttemptUsageReceipt, error)`
  beside the canonical marshal helper.
- Decode through the private current snake-case wire or the exact v1.30.0
  legacy wire already selected by the encoder, then project either back to the
  public receipt without exporting a wire DTO.
- Reject unknown, duplicate, missing, reordered, alternatively encoded,
  malformed, or trailing content under `ErrInvalidUsageReceipt`.
- Validate identity, route, interval, usage, cost, status, and canonical body
  hash before returning a receipt.
- Require `MarshalCanonicalAttemptUsageReceipt(parsed)` to reproduce the input
  byte-for-byte.
- Preserve a legacy blank public `RouteMode` when its historical body hash is
  valid. The exact v1.30.0 legacy wire omits that later field; parsing returns
  the blank public value so the preserved hash remains verifiable, and
  re-marshal reproduces the identical historical wire bytes.
- Keep parser errors and all tests content-free; log nothing.

## Non-goals

- Stock `harbor serve` coordinator delivery, authentication, ACK/replay, lease
  top-up, or readiness projection.
- A new Protocol method/type/version, HTTP endpoint, config field, database
  record, provider policy, billing, catalog, or user interface.
- Accepting arbitrary persistence JSON, later legacy shapes, or merely
  equivalent noncanonical JSON. Only the exact v1.30.0 wire emitted by the
  canonical encoder is compatible.
- Changing receipt production, outbox delivery, grant verification, provider
  execution, or disabled-mode behavior.

## Acceptance criteria

- [x] The exact public function signature is available from `sdk/llm` without
  an internal import.
- [x] Canonical marshal → unmarshal → marshal is byte-identical and returns all
  public receipt fields unchanged.
- [x] Unknown, duplicate, missing, casing-drifted, reordered, whitespace,
  alternate-escape, alternate-number, explicit-omitted-zero, and trailing-value
  forms are rejected with `errors.Is(err, ErrInvalidUsageReceipt)`.
- [x] Missing identity, negative usage/cost, invalid interval/status, mixed
  route claims, wrong hash, and uppercase hash are rejected.
- [x] Parser errors do not reflect untrusted field names or values and no
  prompt, response, tool argument, reasoning trace, credential, or raw body is
  logged.
- [x] Fuzz seeds prove every accepted byte slice validates and is exactly its
  own canonical re-encoding.
- [x] An external-package test compiles and executes the public parser.
- [x] Existing callers and runtime behavior are unchanged; no Protocol or
  persistence surface changes.

## Files added or changed

- `internal/llm/external_grant.go` — strict inverse using the existing private
  canonical wire.
- `internal/llm/external_grant_receipt_parse_test.go` — round-trip,
  adversarial, semantic/hash, and fuzz-seed coverage.
- `sdk/llm/llm.go` — public alias for the strict inverse.
- `sdk/assemble/external_grant_sdk_test.go` — external-package first consumer.
- `docs/plans/phase-257-canonical-attempt-receipt-parser.md`, the master plan,
  D-437, HA-72 register truth, changelog, and Phase-257 smoke.

## Public API surface

```go
func UnmarshalCanonicalAttemptUsageReceipt(data []byte) (AttemptUsageReceipt, error)
```

The function returns `ErrInvalidUsageReceipt` for every rejected document.
`AttemptUsageReceipt` and `MarshalCanonicalAttemptUsageReceipt` remain the
already-public D-436 aliases/helpers.

The legacy v1.30.0 public receipt shape has a blank `RouteMode` and a preserved
pre-extension body hash. Its exact historical wire predates and omits
`route_mode`. When that exact hash validates, this parser returns the blank
public value; marshaling it again emits the same legacy bytes. New explicit
coordinator-bound receipts use the current snake-case wire and keep the
explicit public value.

## Test plan

- **Unit:** exact canonical round-trip; closed-field, duplicate, missing,
  ordering, encoding, number, whitespace, trailing, identity, route,
  interval, status, usage, cost, and hash cases.
- **Integration:** the `sdk/assemble` external-package test creates a public
  receipt, hashes/marshals/parses/remarshals it without importing `internal/`.
  No runtime driver is needed because this phase is a pure codec over the
  already-produced canonical receipt.
- **Conformance:** N/A — one implementation selects either the current
  snake-case encoding or the exact preserved v1.30.0 encoding; this is not a
  driver family.
- **Concurrency / leak:** N/A — the parser is a pure function with no shared
  mutable state, goroutine, I/O handle, timer, or lifecycle.
- **Fuzz:** seed canonical and adversarial documents; any accepted input must
  validate and byte-match its own canonical re-encoding.

## Smoke script additions

`scripts/smoke/phase-257.sh` statically asserts the plan/decision/register,
exact public signature, current and legacy private wire projections,
adversarial/fuzz tests,
external-package consumer, and unchanged Protocol boundary.

## Coverage target

No repository-wide threshold changes. Every new parser branch is covered by
focused unit/adversarial/fuzz seeds; `internal/llm` and `sdk/assemble` run under
`-race`.

## Dependencies

- Phase 254 / D-434: canonical content-free attempt receipt and validation.
- Phase 256 / D-436: public receipt/marshal/hash surface and private snake-case
  canonical wire.

## Risks / open questions

- JSON's ordinary decoder accepts duplicate keys and equivalent encodings; the
  byte-identical canonical re-marshal is therefore load-bearing and has direct
  mutation-shaped tests.
- The parser validates integrity, not sender authentication. The remaining
  HA-72 stock transport must authenticate delivery and bind ACKs separately.
- No RFC §11 question is opened by this additive pure-codec surface.

## Glossary additions

None. The existing **External execution usage receipt** entry remains the
canonical term.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [ ] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] N/A — no multi-isolation path changes
- [x] N/A — this phase adds a pure function, not a reusable stateful artifact
- [x] External-package integration test exercises the public parser and a
  malformed-input failure is covered by the in-package adversarial suite
- [x] N/A — no new vocabulary
- [x] N/A — no brief finding departure
