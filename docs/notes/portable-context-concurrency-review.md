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
