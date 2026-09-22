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
