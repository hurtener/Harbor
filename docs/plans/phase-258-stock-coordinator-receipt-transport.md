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

## Runtime posture

`runtime.info.external_grant` reports:

- compile-time support, an explicit configured fact, and disabled/optional/required mode;
- accepted and independently ready `runtime_default` and/or
  `coordinator_bound` route shapes;
- verifier, durable-reservation, and credential-resolver wiring;
- strict canonical receipt-parser readiness;
- receipt transport kind as stock authenticated HTTP or host injection, with
  disabled, absent, wired, or degraded state;
- stock top-up transport as `unsupported`;
- `strict_ready` only when the chosen route shapes are fully enforceable.

Coordinator-bound acceptance requires a credential resolver. Runtime-default
acceptance does not. A failed or partial stock exchange changes observed
transport state to degraded without leaking operational material.

## Acceptance criteria

- Disabled/default stock wiring performs zero coordinator work and needs no
  coordinator environment variable.
- Enabled config fails loudly for missing auth, unsafe URLs, invalid bounds, or
  duplicate injected/stock transports.
- Canonical batches authenticate, honor timeout/cancellation, refuse redirects,
  and parse ACKs strictly within byte/count bounds.
- Partial ACK, response loss, duplicate ACK, unknown ACK, and hash mismatch
  preserve exactly the unacknowledged durable facts for replay.
- Stock top-up is not configured or wired and reports `unsupported`.
- `runtime.info` distinguishes support, configured mode, route-specific wiring,
  observed transport/parser readiness, and strict readiness truthfully.
- In-memory and SQLite outbox behavior, N=100 concurrent reuse, focused race,
  vet, Protocol generators, config documentation, Phase-258 smoke, and drift
  gates pass.

## Compatibility and evidence boundary

Protocol remains `0.1.0`; `runtime.info.external_grant` is additive. Existing
host-injected transports and single-receipt delivery implementations remain
valid. No product-specific endpoint, catalog, policy, credential-custody model,
or presentation layer is introduced.

This candidate claims local implementation and focused evidence only. Hosted
CI, release/tag/assets/module provenance, an external coordinator deployment,
and downstream acceptance are separate gates and remain unclaimed.
