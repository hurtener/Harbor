# Phase 258 — stock coordinator receipt transport and readiness (D-438)

## Summary

Phase 258 closes HA-72's stock-runtime composition gap. An operator can enable
authenticated delivery of canonical content-free usage receipts directly in
`harbor serve` configuration, without writing a host binary. The stock
transport does not apply lease successors. The durable outbox owns lossless
replay; the HTTP client owns bounded authenticated exchange and exact ACK
parsing; the existing grant wrapper owns all authority verification.

The entire path is opt-in. With the coordinator block absent, Harbor performs
no environment lookup, creates no client, starts no timer or goroutine, scans
no outbox, reads no StateStore record, and makes no network request.
`runtime_default` grants continue to use the runtime's configured provider and
model and never require coordinator provider credentials or a model catalog.

## RFC anchors and dependencies

- RFC §5.3: additive `runtime.info` compatibility and truthful readiness.
- RFC §6.5: provider-neutral LLM edge and runtime-default routing.
- RFC §6.11: durable StateStore-backed receipt outbox.
- RFC §6.15: content-free usage accounting and local governance.
- D-025: immutable-after-construction concurrent reuse.
- D-434 / Phase 254: grants, reservations, receipts, and durable outbox.
- D-436 / Phase 256: public canonical receipt and route-mode contract.
- D-437 / Phase 257: strict public canonical receipt parser.

## Briefs informing this phase

- brief 03 — one provider-neutral LLM boundary for retries and usage facts.
- brief 06 — canonical content-free projections for external consumers.
- brief 08 — pure-Go LLM clients with explicit cancellation and validation.

## Brief findings incorporated

- brief 03 §5: delivery stays attached to the existing LLM receipt path rather
  than introducing a parallel provider or accounting mode.
- brief 06 §1: the coordinator receives a canonical content-free projection,
  never runtime-private state or provider payloads.
- brief 06 §6: stable token, cost, latency, route, and attempt identity facts
  are delivered without prompts, responses, or reasoning traces.
- brief 08 §2: the stock client is pure Go, bounded, cancellation-aware, and
  reusable without mutable request state.

## Transport contract

The configured receipt endpoint receives one versioned JSON object containing a
bounded array of Harbor's canonical receipt JSON objects. It returns a
versioned array of acknowledgements, each containing the exact receipt id and
canonical body hash it accepted. The response parser rejects unknown fields,
duplicate JSON keys, trailing values, duplicate ACKs, unknown receipt ids, and
hash mismatches. Partial ACK is valid transport evidence but degraded
readiness: only the acknowledged durable facts are removed; every omitted fact
is backed off and replayed.

The client sends a bearer service credential read once from the configured
environment variable at boot. It requires HTTPS except for loopback HTTP,
refuses redirects, and bounds timeout, request bytes, response bytes, and batch
count. Error text carries only fixed classes/status codes, never the endpoint,
token, response body, receipt content, or identity. The client is immutable and
safe for at least 100 concurrent calls.

Stock lease top-up is explicitly unsupported. A later phase may enable it only
after a verified successor grant can advance the already-materialized durable
lease epoch/capacity idempotently, including response-loss replay. A transport
method alone is not enough and is not advertised as readiness.

## Durable replay

The existing outbox gains an optional batch-delivery interface while retaining
the single-receipt interface for compatibility. It loads at most the configured
batch from the durable due index, sends once, and conditionally removes only
exact ACKed facts. A request error or response loss retains every fact. An
invalid ACK fails the whole batch closed. Missing ACKs retain only their own
facts. Backoff remains bounded and gains stable receipt-derived jitter; the
existing circuit breaker prevents an unavailable coordinator from turning into
a query or request storm.

Retry deadlines and enqueue wakes run delivery replay only. The slow
maintenance path has its own deadline. The old whole receipt-prefix scan runs
only until a versioned durable legacy-reconciliation marker is committed.
Startup legacy recovery requests one row beyond the configured work batch and
refuses overflow before adopting a page; this hotfix does not claim keyset
continuation. Stock serve performs that check synchronously and fails boot.
After the marker, ordinary ACKed lifetime history does not re-enter the scan or
its bound.

Lease settlement atomically writes a separate removable pending-receipt
handoff for success, error, and cancellation. Maintenance scans only that
bounded prefix, durably enqueues each exact receipt, and removes the handoff
only after enqueue succeeds. A crash between enqueue and removal replays the
same canonical receipt and converges through outbox idempotency. Retained
attempt history is never a recovery queue and cannot hide a later pending
handoff behind its first page.

## Runtime posture

`runtime.info.external_grant` reports:

- compile-time support, an explicit configured fact, and disabled/optional/required mode;
- accepted and independently ready `runtime_default` and/or
  `coordinator_bound` route shapes;
- verifier, durable-reservation, and credential-resolver wiring;
- strict canonical receipt-parser readiness;
- receipt transport kind as stock authenticated HTTP or host injection, with
  disabled, absent, wired, or degraded state;
- top-up transport as `host_injected` when an embedder supplies the existing
  seam, otherwise `unsupported` (stock remains unsupported);
- `strict_ready` only when the chosen route shapes are fully enforceable.

Coordinator-bound acceptance requires a credential resolver. Runtime-default
acceptance does not. A failed or partial stock exchange or transient
reconciliation failure changes observed transport state to degraded without
leaking operational material. Maintenance keeps retrying with bounded delay
and returns to wired only after a successful reconciliation; startup
reconciliation still fails boot. The whole additive readiness object is
optional so clients can render an older runtime that omitted it as
pre-projection/unknown rather than ready.

## Acceptance criteria

- Disabled/default stock wiring performs zero coordinator work and needs no
  coordinator environment variable.
- Enabled config fails loudly for missing auth, unsafe URLs, invalid bounds, or
  duplicate injected/stock transports.
- Canonical batches authenticate, honor timeout/cancellation, refuse redirects,
  and parse ACKs strictly within byte/count bounds.
- Partial ACK, response loss, duplicate ACK, unknown ACK, and hash mismatch
  preserve exactly the unacknowledged durable facts for replay.
- Stock top-up is not configured or wired; host injection is reported
  truthfully when present and absence reports `unsupported`.
- Startup refuses a retained legacy backlog larger than the configured bound
  instead of silently starving later pending facts.
- More than one configured batch of ordinary lifetime ACKs does not re-enter
  legacy reconciliation after the durable marker, including after restart.
- Success, error, and cancellation settlements atomically create removable
  pending handoffs; crash-before-removal replay converges in-memory and SQLite.
- A transient cadence failure degrades readiness, retries, and recovers; an
  explicit disabled YAML mode conflicts with an injected enabled mode.
- A v1.30.0 blank-route receipt is delivered in its original legacy canonical
  wire; the strict public parser accepts it and re-emits byte-identical JSON
  with the same body hash.
- `runtime.info` distinguishes support, configured mode, route-specific wiring,
  observed transport/parser readiness, and strict readiness truthfully.
- In-memory and SQLite outbox behavior, N=100 concurrent reuse, focused race,
  vet, Protocol generators, config documentation, Phase-258 smoke, and drift
  gates pass.

## Files added or changed

- `internal/llm/receipts/httptransport/` — bounded authenticated batch client.
- `internal/llm/receipts/outbox.go` — optional batch delivery and durable
  retry/reconciliation scheduling.
- `internal/runtime/serve/` — stock composition and truthful readiness.
- `internal/config/`, `sdk/config/`, `examples/harbor.yaml`, and `docs/CONFIG.md`
  — opt-in coordinator transport configuration.
- `internal/protocol/`, generated Protocol references/TypeScript projections,
  and `sdk/protocolclient/` — additive content-free readiness projection.
- `scripts/smoke/phase-258.sh` and this plan — static acceptance truth.

## Test plan

- **Unit:** strict request/ACK bounds, redirects, malformed/duplicate/unknown
  ACKs, partial ACKs, stable jitter, and retained-fact reconciliation.
- **Integration:** assemble stock serve with the real in-memory and SQLite
  StateStore drivers, authenticate against a local HTTP server, and prove
  response-loss replay plus route-specific readiness and disabled zero work.
- **Conformance:** existing single-receipt delivery remains valid; the optional
  batch extension has identical durable removal semantics for acknowledged
  facts.
- **Concurrency / leak:** reuse one immutable HTTP client for at least 100
  concurrent deliveries under `-race`; cancellation does not cross requests,
  and shutdown joins the outbox worker.
- **Fuzz:** strict canonical receipt parsing remains Phase 257's fuzz surface;
  this phase adds bounded adversarial ACK documents rather than a second JSON
  canonicalizer.

## Smoke script additions

`scripts/smoke/phase-258.sh` asserts D-438/HA-72/plan truth, the stock client,
batch/ACK/replay tests, disabled and readiness integration tests, Console
Protocol projection, operator configuration, and runtime-default independence.

## Coverage target

- `internal/llm/receipts/httptransport`: at least 85% branch coverage across
  request construction, transport refusal, response bounds, and ACK parsing.
- `internal/llm/receipts`: every new batch, retry, response-loss, partial-ACK,
  startup reconciliation, and cancellation branch covered.
- `internal/runtime/serve` and `internal/config`: every new stock wiring,
  disabled/default, invalid configuration, and readiness branch covered.

## Dependencies

- Phase 254 / D-434 — context-bound grants, durable reservations, and outbox.
- Phase 256 / D-436 — public receipt/delivery and explicit route modes.
- Phase 257 / D-437 — strict canonical receipt parser.
- D-025 — immutable concurrent reuse for the stock client.

## Compatibility and evidence boundary

Protocol remains `0.1.0`; `runtime.info.external_grant` is additive and
optional. Its absence means the runtime predates or does not project this
readiness surface; clients must not infer strict readiness. Existing
host-injected transports and single-receipt delivery implementations remain
valid. No product-specific endpoint, catalog, policy, credential-custody model,
or presentation layer is introduced.

Two contained follow-ups remain explicit rather than hidden in this phase: the
due index is one bounded internal JSON record rather than a keyset-addressable
queue, and the stock outbox has no DSN-gated real-PostgreSQL acceptance test.
The in-memory and SQLite behavior is binding here; a later phase may replace
the index or add hosted PostgreSQL evidence without changing the public
transport contract.

This candidate claims local implementation and focused evidence only. Hosted
CI, release/tag/assets/module provenance, an external coordinator deployment,
and downstream acceptance are separate gates and remain unclaimed.
