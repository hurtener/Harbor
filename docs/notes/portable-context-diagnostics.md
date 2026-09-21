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

## Bounded request preparation

`llm.context.prepared` is emitted through the existing asynchronous event path
at the mandatory leaf input-capacity check, after request transformations and
before provider execution. A structurally valid but oversized request also
emits with `CapacityExceeded=true`; identity, authorization, structure and
heavy-content failures that occur before this boundary do not emit it.
Candidate validation during compaction is not a provider attempt and emits no
prepared event. A failed candidate never appears as installed coverage.

`Sections` partitions the existing admission estimate into text, declarations,
call arguments/correlation, output schema, media, framing and other input.
`EstimatedTokens` equals their sum, not a second counting algorithm. Semantic
sections such as skills versus instructions are deliberately not inferred from
message text. Media remains the existing coarse estimate, not proof that a model
can inspect that media. `InputLimitExclusive` and `OutputReserved` use the same
resolved model profile as admission. `OutputLimitKnown=false` means unspecified,
not an explicit zero-token allowance or an assumed provider default.

The runtime supplies detached checkpoint version/generation, replay start/end
(exclusive), and known/unavailable fresh-exposure coordinates. These are runtime
selection coordinates, not a guarantee about a custom planner's renderer or
provider receipt. Legacy/invalid coverage is not given invented coordinates.
Maintenance uses its own counts and ordinal and omits parent history and target.
Numeric attempt coordinates identify planner step, retry, downgrade and fallback;
unknown attempts are explicit. No prompt, result, summary, source digest, call
identifier/nonce, arbitrary model string, credential, provider extra or error
excerpt enters this payload. The identity quadruple retains normal event access
controls. Existing model/route events remain the routing authority.

The payload is fixed-shape metadata, with no list proportional to history length,
new store, debug endpoint or logging mode. Publication uses the existing bounded
best-effort telemetry lane. It is neither admission authority nor a cost/usage
receipt; consumers must not count it as a successful provider request. Actual
usage/cost remains on the existing completion/receipt path, including its explicit
availability flags. Preparation telemetry can be lost on bus pressure/cancellation;
durable execution state, not observability, remains authoritative.

Regression tests cover unchanged estimate arithmetic, capacity rejection, unknown
output bounds, detached snapshots, maintenance isolation and 128 concurrent calls.
The real run-loop/ReAct/summarizer/Bifrost fixture checks successful compaction,
rejected expanding candidates, prior checkpoints, and absent inspection mutexes.
Cache-prefix tests below cover the portable live-request path; retained-turn setup
is a separately constructed generation. These are deterministic tests, not a
claim of real cache hits, billing savings or model proficiency.

## Cache-prefix boundaries

`TestContextCache_LivePrefixesAndExplicitRewriteBoundaries` captures HTTP requests
from the pinned OpenAI and Anthropic Bifrost adapters. With stable instructions,
tool definitions and text-based live history, appending a complete native
exchange leaves preceding message bytes and tool declarations unchanged.
Rebuilding unchanged evidence produces the same full request. These properties
hold again after portable compaction starts a new checkpoint generation.

Compaction deliberately changes the covered history while retaining the latest
exchange. Tool exclusion changes the declared tools even when that breaks cache
reuse, and switching models changes the outbound model. The fixtures use ordinary
completion endpoints with no native compaction or required cache hints. They run
no historical tools. Provider output is scripted; this proves request behavior,
not a cache hit, cache retention, provider billing or a performance ranking.

The guarantee is conditional on unchanged prompt inputs. Changing caller memory,
skills, date-sensitive default instructions, repair guidance, tool discovery,
first-turn attachment materialization or trailing retrieval/outcome notices may
change the corresponding request section. A new user turn is reconstructed from
its retained window; it is not claimed to be a byte-for-byte append of the prior
turn. Correct authority and source lifetime take precedence over cache reuse.

## Usage availability and sparse streaming reports

Normalized `Usage` and `Cost` carry additive, omitted-when-false `ReportPresent`
flags. `Usage` also records `PromptDetailsPresent` and `CompletionDetailsPresent`.
These indicate the normalized SDK objects that actually arrived, not the presence
of every raw provider field. Missing/legacy flags mean unknown availability, never
proof of a free request or a measured cache miss. A present all-zero usage object
is not replaced by the optional corrections-layer backfill. That backfill marks
its token/price-table values with `Estimated`; it never changes them into driver
reports. Existing cost events forward these flags with the numeric values, and
older readers may continue ignoring the additive metadata.

The pinned Bifrost SDK collapses absent individual cache fields and explicit zero
cache fields into the same integer. A prompt-details object may contain only
audio/text counts. Accordingly `PromptDetailsPresent` is not a per-cache-category
presence claim, and a zero read/write cache count remains inconclusive. No provider
SDK replacement or raw-response capture is added to recover that lost distinction.
SDK-calculated costs are also not proof of an actual billed invoice.

Streaming counts are cumulative, not summed across chunks. Cost-only or empty
updates preserve earlier totals; missing detail objects preserve cache/reasoning
counts. Present detail/cost objects update their own category, including zeros.
All state is per completion. Regression tests exercise actual SDK decoding,
sparse updates, estimate labeling, cost-event forwarding and 128 isolated calls.
