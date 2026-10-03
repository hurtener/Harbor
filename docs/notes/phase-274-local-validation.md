# Phase 274 — Local monetary contract qualification

This is local candidate evidence, not a release certification. No paid provider
calls, production credentials, installed production tariffs, configuration
changes or external publication were part of these checks. All provider HTTP
fixtures used synthetic localhost services.

## Passed evidence

Go 1.27.1, race detection and serial package execution established:

- 37 pricing/accounting test events passed with no skips or failures, including
  in-memory, SQLite and real PostgreSQL 17 storage
- Pricing coverage: 87.4%; allocation accounting coverage: 87.3%
- Five integer token-rate categories round upward independently, plus fixed
  request/ancillary ceilings, checked against arbitrary-precision arithmetic
- Incomplete/negative/ambiguous tariffs, unknown selectors, wrong endpoint
  bindings, changed reference hashes and unsupported currencies refuse
- Caller mutations cannot alter the catalog's detached contents
- 100 competing manager instances cannot exceed the shared monetary cap;
  separate owners remain isolated and token/money reservations commit atomically
- SQLite close/reopen retains crash liability and rejects changed catalog
  content under the same ID/revision; a new revision cannot reprice an old task
- A committed-but-lost reserve/settle acknowledgment cannot refund monetary
  liability or duplicate its terminal receipt
- Real Bifrost HTTP retries retain the entire inclusive money envelope and
  cannot dispatch again after its cap is exhausted
- Externally resolved routes, multimedia/native files, arbitrary passthrough,
  OpenRouter, and custom providers disguised as native IDs refuse monetary bounds
- Native Anthropic thinking has a separate conservative output allowance
- Accepted monetary caps and tariff references are copied, fingerprinted,
  inherited and reconstructed through the durable TaskRegistry
- Production assembly wires both acceptance and provider pricing; real SQLite
  restart enforces immutable catalog pins and task detail exposes scoped counters
- The served run loop carries accepted monetary funding into the provider edge
- Protocol pricing failures keep the dedicated pricing-unavailable error code

The first endpoint-binding integration run found two fixture compile errors:
endpoint fields had also been inserted into ordinary LLM configuration literals.
Those fixture fields were corrected. The affected Bifrost and assembly packages,
plus the new acknowledgment-loss and Protocol error checks, then passed under
race detection: 13 test events, no failures or skips. The SDK facade compiled
and has no package-local test cases. The earlier unaffected config, engine,
durable tasks, served run-loop and Protocol checks passed separately.

The repository drift audit passed 1,608 checks with zero warnings/failures;
changed Markdown passed the pinned 0.22.1 linter. AGENTS.md and CLAUDE.md match.

## Commands

The accounting gate ran with a real PostgreSQL server started, exercised and
stopped inside the same invocation; `HARBOR_PG_DSN` selected a synthetic database.
No database endpoint or authentication data belongs in this note.

```sh
go test -race -p 1 -count=1 -coverprofile=accounting.cover \
  ./internal/llm/pricing ./internal/llm/allocation

go test -race -p 1 -count=1 \
  ./internal/config ./internal/llm/drivers/bifrost ./internal/tasks/engine \
  ./internal/tasks/drivers/durable ./internal/tasks/drivers/inprocess \
  ./internal/tasks/protocol ./internal/runtime/assemble \
  ./internal/runtime/serve ./internal/protocol ./sdk/llm \
  -run 'TestMonetary|TestEngine_Monetary|TestAllocation_|TestEngine_Allocation'

go test -race -p 1 -count=1 ./internal/llm/allocation \
  ./internal/llm/drivers/bifrost ./internal/runtime/assemble ./internal/protocol \
  -run 'TestMonetaryAllocation_(LostAcknowledgments|Bifrost|Assembly)|TestMapTaskError_AllBranches'
```

## Guarantee and remaining gates

No provider-returned cost float is used for monetary admission or settlement.
The initial real consumer is bounded static OpenAI/Anthropic text. A correct
inclusive operator tariff and truthful provider bounds are external assumptions;
this code cannot establish actual billed spend or universally price every route.
A successful final response does not prove hidden retries were free. Their
entire monetary ceiling stays unknown; complete single-attempt work consumes a
conservative charged ceiling. Only proven cancellation before driver entry can
refund money. Accounting holds never expire.

The generated Protocol/Console/TypeScript joins must be regenerated after the
parallel candidate extensions are integrated. Combined preflight, broad hosted
CI, independent adversarial review with every finding fixed, and documentation
site build/deployment verification remain release-owner gates. This worktree did
not run the full release preflight, publish, tag, deploy or claim stable readiness.
