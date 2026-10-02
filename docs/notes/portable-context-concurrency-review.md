# Portable context concurrency review

PR #779 release validation; this note does not declare an RC ready.

## Task burst fixture

The shared served-driver fixture used a 64-event subscriber buffer while the
settings and isolation workloads issue 128 accepted tasks. The production
configuration defaults to 256. An administrative spawn subscription deliberately
left undrained during the burst deterministically lost 64 events with the former
fixture, before any planner or retention work. This explains why the settings
fixture could time out without reaching the provider; it is separate from the
required persistence deadline failures.

The fixture now takes its queue capacity from the production default. Existing
idle/drop timing, the 128-task workload and all assertions remain unchanged.
The regression checks zero drops and every accepted task's order and identity.
The real shared-driver settings test continues checking all model/effort/output
choices and the unconsumed legacy override. Its race run and the burst regression
passed ten consecutive runs with Go 1.26.4. The broader per-task/override fixture
suite also passed three race repetitions after improved failure diagnostics.
No production buffer, persistence deadline, dispatch policy or dependency changed.

This is not a guarantee of lossless delivery under arbitrary production overload.
The event-bus overflow contract remains unchanged and has its own conformance
suite. Whole hosted macOS and release acceptance remain required.

## Opaque evidence decoding

The retained decoder's host-envelope pass copied opaque leaves into a
`json.RawMessage` only to discard them. It also scanned opaque root evidence
before the final typed decoder scanned it again. The envelope pass now consumes
opaque leaves without retaining a copy and avoids that redundant root pass when
no typed host container exists. The final decode still validates JSON syntax,
UTF-8, trailing content, scalar/custom-decoder types and exact numbers. Typed
containers, including root slices of retained turns, keep the duplicate/case-alias
checks. Tool-result keys do not become host metadata.

New regressions reject malformed nested opaque values, malformed and trailing
root evidence, invalid encoding, invalid custom scalars and duplicate host fields
inside root slices. Existing source-bound checkpoint, recovery, erasure and
exact-receipt tests remain unchanged. Full Go 1.26.4 runctx, assembly and SDK
assembly race suites pass.

A same-host current/baseline/current benchmark alternation (three samples per
variant) measured 95,495 versus 47,968 allocated bytes for a 14,660-byte opaque
receipt, with 35 versus 23 allocations. Median opaque-root decode time was
206,854 ns/op for the baseline and 93,535 / 91,408 ns/op for the two corrected
batches. A 32-turn typed window used 2,186,646 versus about 1,655,420 bytes per
operation. Whole-window timing was close and variable; no corresponding runtime
latency improvement or resolution of hosted persistence deadlines is claimed.

No dependency, persisted format, authority cache, persistence timeout or workload
limit changed. Full hosted platform tests remain a separate release requirement.

## Validate prepared intent and reuse settled action

The settlement path decoded its own newly constructed frame and then decoded the
same receipt again only to compare actions. Frame preparation now returns the
validated, detached action encoding alongside the serialized frame. Settlement
still loads the stored intent, verifies its exact generation and typed host
metadata, compares the redacted actions, and performs the same conditional
multi-record commit. No authority is cached between calls.

Review of that reuse exposed a pre-existing earlier-boundary gap: a custom
redactor could return ambiguous nested failure metadata that passed intent
preparation and was rejected only after tool execution. The new in-memory and
SQLite intent regressions fail against the exact published `2d430ac` source.
The real embedded RunOnce regression likewise executes the tool once before
rejection on that source; after the correction it executes no tool. Strict nested
host validation now runs during preparation, before dispatch. Invalid settlement
metadata and changed actions leave both committed head and intent unchanged;
opaque result keys, full source strings and exact large numeric IDs stay data.

Full affected core race suites and the targeted embedded regression pass on
Go 1.26.4. Phase 269 with the disposable PostgreSQL service passes 14 checks,
zero skips and zero failures. The existing journal smoke group includes the
new tests. Local lint uses the pinned binary with vendor mode because the module
proxy is unavailable; repository dependencies and CI lint configuration are not
changed. Final hosted platform and preflight acceptance remain open.

The settlement benchmark includes the real in-memory driver and default redactor,
with an exact 14,660-byte receipt. Three 100-iteration samples measured median
allocated bytes of 509,044 before and 379,422 after the change. These scoped
allocation measurements are not a claim that hosted deadline failures are fixed;
wall-clock samples were variable. The production five-second deadlines, all
128-session workloads, erasure predicates and persisted formats remain unchanged.

The broader coverage run also exposed an existing measurement race in
`TestFetchMemoryBlocks_ConcurrentReuse`: it sampled process-wide goroutine counts
while running in parallel with other tests. Its observed baseline of 112 rose to
184 as sibling tests started workers. That test now runs outside the inter-test
parallel group while preserving its own 100 concurrent calls and unchanged leak
threshold. No runtime concurrency or assertion is reduced.

## Terminal preparation and persistence budgets

The macOS failures in PR #781 / run 36946956253 occur during exact journal-frame
or head cleanup, after the atomic terminal publication has succeeded. The next
served turn still sees the correct scoped source. A cleanup error does not undo
that sealed evidence or authorize replay; it remains an explicit failure that
the existing reconciliation operation can resolve.

A focused Go 1.27.1 race benchmark uses the same 14,660-byte synthetic receipt
and the production in-memory store and redactor. With `GOMAXPROCS=2`, three
100-iteration baseline samples measured median terminal preparation of 25.82 ms,
publication of 0.705 ms and cleanup of 0.014 ms. About 97% of the elapsed budget
was spent before the first store read. A call-local snapshot of the freshly
built historical envelope and content-stripped action avoids decoding the
original receipt again solely for identity comparison. Median terminal time
fell from 26.54 to 23.37 ms, and allocation from 1,115,310 to 920,657 bytes.
Custom-redactor output still receives the same host and action validation.
These measurements isolate finalization; they do not measure contention across
the full runtime or prove that the hosted failures are resolved.

D-492 gives preparation its own bounded five-second context before starting the
unchanged five-second publication/cleanup budget, matching RFC 002's existing
boundary. Earlier caller deadlines and cancellation remain binding. Served and
embedded finalization still detach from completed execution cancellation; their
total finalization allowance can now reach ten seconds. Preparation failure has
no store effects. There are no background workers or unbounded cleanup retries,
and the atomic seal, exact-generation deletion, expiry and erasure rules remain.

The regressions exercise fresh stage budgets, earlier parent deadlines, context
values, cancellation before/during preparation, redaction refusal, malformed
custom JSON, and failed-argument scrubbing. Cancellation immediately after the
atomic seal or after frame deletion keeps the error explicit and preserves
unchanged terminal evidence across SQLite reopen. Reconciliation removes only
transient records; reacquisition and new dispatch from the sealed source fail.
The 128-scope runtime tests and full hosted platform gates remain required.

The new stage-boundary regressions fail against the published `3c6307cf` source:
preparation and persistence share a context, and cancelled preparation still
reaches the store. On the corrected source, the complete session package's
`TestRetained*` group passes with Go 1.27.1 and `-race` in 128.364 seconds,
covering the in-memory/SQLite paths without an external PostgreSQL service.
The original `TestRunOnce_RetainedContextConcurrentReuse` and
`TestRetainedServer_ConcurrentReuse` then pass three race repetitions each
(assembly 21.154 seconds; serving 20.607 seconds), with their 128 scopes,
receipt bytes, assertions and test timeouts unchanged. The drift audit passes
1,604 checks with no warnings or failures, including repository Markdown lint.
This qualification is not a full repository or hosted macOS pass.

## Immutable payload copies outside the state lock

The later hosted macOS job 110735369460 (run 36974526980, `b81d1691`)
still failed the unchanged served 128-scope test at dispatch-head cleanup.
Terminal evidence was sealed and correctly available to the next turn. The
separate D-492 budgets did not remove contention inside the state driver.

The fixture uses an in-memory StateStore, not a SQL pool. Its global map mutex
covered defensive payload allocation and copying, including ordinary missing-
record error formatting. Those operations do not need the map lock, but their
race-instrumented work and scheduling could hold up unrelated identity scopes.
Point reads now capture one record generation under the lock and copy its
immutable payload afterward. Writes detach payloads before acquiring the lock;
batch preparation also detaches the caller's slice headers. Stored payloads are
never changed in place. Predicate, EventID/idempotency checks, atomic multi-slot
publication, timestamps and deletion still use the existing critical section.
No StateStore API, storage format, deadline, peer count or cleanup policy changes.

Paired local Go 1.27.1 Linux runs use the original served test with 128 scopes,
`-race`, `GOMAXPROCS=2`, and CPU, mutex, blocking and scheduler traces. Completed
lock waits in the in-memory write paths change as follows:

| Operation | Total blocked time, baseline → corrected | Maximum wait, baseline → corrected |
| --- | --- | --- |
| `DeleteIf` | 53.317 s → 0.0176 s | 881.0 ms → 8.9 ms |
| `SaveBatchIf` | 154.630 s → 0.0208 s | 885.2 ms → 17.2 ms |
| `SaveIf` | 41.956 s → 0.0104 s | 871.5 ms → 10.4 ms |

Totals sum waits across goroutines and can exceed wall time. Overall package
times remain close (7.899 and 7.966 seconds): CPU work and the existing task
registry queues remain. These measurements support removing the state-lock
convoy from required persistence, not a general runtime speedup or a claim that
hosted macOS acceptance has passed.

The in-memory conformance suite and new 128-scope snapshot/batch-ownership race
checks pass three repetitions (2.024 seconds). They preserve input and returned-byte ownership, exact generations,
conditional cleanup and immutable snapshots across replacement. The predecessor
retained preparation, deadline, erasure, cleanup-failure and recovery contracts
remain unchanged: the complete `TestRetained*` session race group passes in
131.414 seconds. The original 128-scope assembly and served tests pass three
race repetitions each, in 20.251 and 19.881 seconds respectively. Counts, receipt
bytes, assertions and deadlines are unchanged; hosted macOS is still a separate
acceptance gate.

Three 3,000-operation samples of the journal-shaped state benchmark measure
median 66.1 microseconds per operation before and 53.8 after. Each batch now
allocates one small detached record-header slice (about 320 bytes for the
two-record fixture) so it cannot mutate the caller's headers. Payloads are
copied once per write as before; no extra payload cache or persistent state is
introduced. This modest allocation tradeoff keeps payload work outside the
shared critical section.
