# Portable context diagnostics review

RFC 002 / phases 268–269. This is incremental evidence, not RC acceptance.

## Failure-event privacy

Compaction previously copied the beginning of arbitrary estimator/summarizer
errors into a `SafePayload` event. Limiting the message to 256 bytes did not
prevent source fragments from escaping. The estimator and summarizer regression
cases both failed against the published attachment checkpoint before this fix.

Failure events now contain the existing category, run identity, counts and a
fixed description. Original causes still propagate through the returned error
chain; callers must apply their own error-reporting policy. The previous
checkpoint remains unchanged. No new event type, wire field, API or dependency
is introduced. Shared-component tests cover 128 concurrent isolated failures.

## Outstanding diagnostics acceptance

Request-section estimates, checkpoint-range inspection and explicit usage
availability still need full acceptance under the existing telemetry/Protocol
ownership. Cache-prefix tests must distinguish an append-only live tail from
intentional rewrites at compaction, model/policy change and retained-turn setup.
No cache-hit ratio, paid cost saving or model-proficiency claim is made here.
