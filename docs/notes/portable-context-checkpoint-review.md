# Portable context checkpoint review

Phase 268 / RFC 002, incremental implementation; not an RC acceptance report.

## Candidate publication

The stock runtime validates the rebuilt request before publishing a summary.
A syntactically valid narrative that expands input or still exceeds the resolved
physical input allowance is not a usable replacement. Rejection restores the
previous checkpoint before releasing the inspection mutex and emits a
content-free failure rather than a successful compression event. The same rule
applies when the caller does not configure an inspection mutex.

If a non-shrinking replacement is rejected and the original request still fits
its physical allowance, continue using the unchanged checkpoint and evidence.
Otherwise fail explicitly. This is one bounded attempt at the request boundary,
not an immediate re-compaction loop. No remote call runs during validation.
Custom compaction callbacks retain their existing interface; only the stock
runtime guarantees transactional checkpoint publication through this check.

## Regression evidence

The new expanding-summary and protected-overflow assertions failed before the
fix: the previous checkpoint had already been replaced. They now pass against
the real run loop, ReAct, summarizer, composed LLM client and pinned Bifrost HTTP
adapter with synthetic localhost responses. Coverage includes an existing
checkpoint, an initially empty checkpoint, and no inspection mutex. Rejection
retains the exact fresh result and does not publish a compression-success event.

Full race suites for `internal/llm`, `internal/llm/drivers/bifrost`, and
`internal/runtime/steering` pass with the supplied Go 1.27.1 toolchain and pinned
vendored dependencies. This is harness evidence, not a paid model-quality test.
Full repository preflight and durable cross-turn continuity remain separate
acceptance work. No merge, release or deployment is claimed here.
