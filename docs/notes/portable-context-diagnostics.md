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
ownership. The cache-prefix boundary tests below cover the portable live-request path;
retained-turn setup is intentionally a separately constructed context generation.
No cache-hit ratio, paid cost saving or model-proficiency claim is made here.

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
