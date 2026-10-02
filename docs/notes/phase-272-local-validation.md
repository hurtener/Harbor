# Phase 272 local qualification

The focused allocation gate passed on Go 1.27.1 with race detection, PostgreSQL 17.6, SQLite and in-memory StateStore. Provider work used a synthetic localhost OpenAI-compatible fixture; no paid inference or production credentials were used.

```sh
go test -race -p 1 ./internal/llm/allocation ./internal/llm/drivers/bifrost \
  ./internal/tasks/engine ./internal/tasks/protocol ./internal/runtime/serve \
  ./internal/protocol -run 'TestAllocation_|TestEngine_Allocation' -count=1 -v
```

The executed checks establish:

- 100 competing reservations admit exactly ten 10-token envelopes into a 100-token allocation on each of the three stores
- Distinct manager instances share the durable counter; committed-but-lost acknowledgments cannot refund or duplicate accounting
- SQLite close/reopen preserves outstanding crash liability
- 100 identity scopes sharing labels remain separate; cancelling half does not cancel or charge the other half, and the test detects leaked goroutines
- Known provider usage settles once; unknown usage and cancelled streams with partial usage retain their full liability
- Reported usage beyond the trusted envelope latches a visible breach and prevents new calls
- A real Bifrost fixture makes exactly two HTTP attempts for its configured retry; all capacity remains held after unknown failure and another call makes no third request
- Accepted task allocations are deep-copied, fingerprinted and inherited through child and grandchild tasks; descendants cannot replace the parent cap
- Task detail projects scoped content-free counters and bounded receipts, with foreign-user access refused
- The runtime run loop passes accepted funding to the actual provider-edge accounting path
- Start exact replay reuses its task, changed allocation revision conflicts, and requested hard monetary limits return the dedicated pricing-unavailable error

The first focused run exposed omitted ArtifactStore/Bus dependencies in three new test fixtures. The fixtures were wired to real in-memory dependencies; production constructors and assertions were preserved. The corrected full focused run passed all six packages.

This gate does not substitute for combined release preflight, generated Protocol/SDK checks or hosted whole-repository CI. Those remain integration gates. The reservation is intentionally conservative: it uses the configured model input window, explicit output cap and the transport’s maximum hidden attempts. It can reject work earlier than observed final usage would suggest. Hard monetary enforcement remains unavailable; the phase plan specifies the trusted tariff extension without fabricating prices or exporting an unused implementation.

## Monetary boundary reviewed during integration

The current provider catalog (`internal/llm/provider/catalog.go`) reports
`PricingKnown` and provider-reported provenance, not a versioned inclusive rate
manifest. `ModelProfile.CostOverrides` is a floating-point correction input;
`internal/llm/corrections/corrections.go` uses it after a response to estimate
cost. It does not establish an upper bound for cache, reasoning, per-request or
ancillary charges. External-grant usage receipts describe completed attempts and
cannot authorize or prove a pre-dispatch reservation.

Therefore token-only qualification is not monetary qualification. The proposed
trusted pricing extension still needs immutable operator provenance, exact
provider/model/version matching, inclusive integer charge ceilings, checked
round-up arithmetic, and atomic monetary reserve/settle coverage for every
physical attempt. This candidate rejects every hard monetary allocation request.
