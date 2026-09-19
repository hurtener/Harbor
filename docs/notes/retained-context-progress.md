# Retained context implementation checkpoint

RFC 002 / phase 269, draft implementation in PR #779. Not a release candidate.

This checkpoint publishes the previously unreferenced phase-269 tree
`d6280f15e2a90d883106780942167040a9b68436` so terminal-retention work is not lost.
The first consumer is embedded `RunOnce`, with explicit `WithRetainedContext`.
The omitted/zero option does not enable richer persistence. Exact permitted
results are retained as inert evidence, not executable historical decisions.

The current pass reconstructed the implementation and reran the full race suites
for runctx, assemble and sdk/assemble successfully, including receipt integrity,
SQLite reopen, erasure/write failure and 128-session tests. The source recovery
job will supply the exact published tree for a second validation pass; equivalent
reconstruction is not represented as a verified byte-identical checkout.

Review is continuing before this becomes a finished phase. Required outstanding
work includes intent/settlement persistence, serve wiring, historical native
projection and checkpoint reuse, bounded artifact recovery, current tool authority,
TTL handling during active runs, and canonical RFC/index/decision synchronization.
The phase smoke's D-464 reference is not yet a completed documentation gate.
Do not mark the phase complete from terminal-only retention or these focused tests.

Long-term memory remains external. No native compaction, dependency update,
production deployment, merge, tag, or automatic replay of interrupted writes.
