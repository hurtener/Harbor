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
