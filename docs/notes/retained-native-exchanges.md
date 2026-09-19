# Retained native exchange checkpoint

RFC 002 / phase 269; incremental implementation, not RC acceptance.

Retained steps now have a versioned, non-executable envelope carrying their
source run, original ordinal, concrete action kind, and permitted exchange.
The outer trajectory step has no dispatchable action or completion-ingestion
preamble. Only ReAct's request renderer interprets the closed native kind set,
using the same rendering functions as live exchanges. No historical action is
placed on a pending-call queue or sent to an executor.

Single tools, parallel tools, mixed batches, progress and task-control outcomes
retain native assistant-call/result structure. Parallel/batch results preserve
branch ordering and exact values through JSON restoration. The renderer assigns
stable source-run/ordinal/branch identifiers using a provider-safe alphabet;
reused original provider IDs from separate turns cannot collide in a request.
Original IDs remain in the retained evidence, not rewritten in storage.

Source strings, numeric identifier lexemes and completeness values are preserved.
Audit/storage may canonicalize JSON envelope whitespace and key ordering; this
is not a promise of byte-identical JSON formatting. Failed action arguments are
removed before retention using the shared failure predicate, with per-branch
handling that leaves successful branch arguments intact. Historical compaction
therefore cannot reintroduce arguments that native replay suppresses. Private
reasoning, diagnostic duplicates and live handles remain excluded.

The retained-window format is now version 2. Version 1 records are read as inert
evidence without guessing their erased action type, then upgraded on a write.
Old version-1 readers reject the new window. The dispatch journal remains at
version 1; its independent action-boundary contract is unchanged. Unknown
historical versions/kinds, nested history, private fields and ambiguous outer
execution fields fail explicitly, rather than producing partial native calls.

Tests cover the actual embedded request, SQLite restoration, legacy migration,
native-renderer parity, 128-way stable ID isolation, failed arguments, malformed
history and compaction coverage. Real pinned Bifrost adapters exercise ordinary
OpenAI and Anthropic endpoints in both directions, including JSON-restored
history, exact source/completeness/version values, and truncated summaries.
These are local scripted-provider tests, not real-model quality measurements.

A monolithic full served-runtime race run exceeded this container's 4 GiB memory
limit. The unchanged 128-run retained stress test passes in isolation. Full
served testing in bounded-process shards also encountered a timeout; neither
that run, the monolithic hosted gate, nor repository preflight is claimed green.

Cross-turn checkpoint reuse, explicit interrupted-prefix reconciliation,
authorized large-result recovery and catalog refresh remain pending phase work.
No new provider client, native compaction endpoint, public transcript, long-term
memory subsystem, release tag or automatic cold execution is introduced.

## Shared historical-envelope validation

Admission, ordinary rendering, and compaction now use one host-envelope
validator. It rejects private/raw fields, nested history, ambiguous outer
execution fields, unsupported versions/kinds, duplicate or noncanonical host
field names, invalid encodings and oversized envelopes before inference.
Tool-result objects remain opaque data; a tool's own field names are not
reinterpreted as runtime metadata. Exact numeric lexemes and source strings
survive the detached decode, including concurrent readers.

The regression reproduces the prior gap: a malformed historical envelope with
private reasoning reached the summarization client even though native rendering
would reject it. The corrected path returns a content-free typed error without
calling the model. Both in-memory and SQLite admission tests reject the same
corruption before creating a new retained run. This tightens D-467's existing
contract; it does not add a new format, provider requirement or recovery mode.
