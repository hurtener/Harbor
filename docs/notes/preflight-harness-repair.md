# Preflight harness repair

This repair follows the failed preflight at `0a1a7539` without removing test
cases, relaxing runtime deadlines, or replacing real transports with mocks.
This note distinguishes measured correctness gates from remaining integration
and production-resource qualification.

## Source guards

- MCP invocation preparation owns the tool-definition App binding after the
  invocation-boundary refactor. Pin that method's actual binding parameter.
- Execution memory has one cumulative session owner under D-477. Pin admission,
  application, terminal persistence, and the devstack's shared driver/config;
  do not require the removed pair-only projection or duplicate writeback.
- Require the renamed no-parallel-projection, truncated-summary preservation,
  and caller-memory/conversation composition tests to run by their exact names.
- Native Batch reconstruction now dispatches from `react/historical.go` into
  the renderer in `prompt.go`; guard both halves.
- Production import guards exclude external test fixtures. A production import
  of the forbidden dependency still fails the guard.
- Preserve the exact closed error-code set with the eight subsequently added
  receipt, transfer, allocation, and retained-context codes.
- Count the three redactor admission-gate comments without also counting the
  unrelated session-admission import and calls.
- OAuth removal's public method delegates to its authorized implementation;
  verify delegation and the trimmed required hash/conflict check together.
- Put phase 275 in the canonical master index with Status as the final column.
  The reversed detail table previously made the Plan link look like a status.

The landing statistic and prose now both use the docs build's canonical
manifest count (160), replacing the stale literal 110. Guards pin the build
injection and both consumers. Evaluating the TypeScript confirms both display
160 from the checked-in manifest.

## Aggregate memory-suite budget

The phase-25a smoke retained its 120-second timeout when the old
`internal/memory/strategy` package was replaced by the cumulative
`internal/memory/session` owner. The new owner has 79 top-level tests, including
260-turn, 300-step and large-evidence regressions across three stores. The
original budget timed out with 272 passing test/subtest events while still
making progress through the storage-pressure tests.

With the same race detector, cases and operation deadlines, the complete
registry/owner invocation passed 400 test/subtest events with no skips or
failures against in-memory, SQLite and PostgreSQL. The owner package took
207.19 seconds. Its aggregate harness budget is now Go's normal bounded
10-minute default. This is correctness qualification, not a performance SLA
or a claim that runtime memory/latency meets a performance target.

## Assembly fixture resource diagnosis

The original full assembly suite was killed by signal in cumulative rollover,
while that unchanged rollover case passed alone in 19.10 seconds. An exclusive
repeat reached 3,760,968 KiB maximum child RSS before being killed. A diagnostic
profile at rollover entry showed about 65.6 MB Go heap and 12 live goroutines;
there was no comparable live-goroutine leak at that boundary. Cgroup OOM
counters are not exposed in this environment, so the signal's OS cause is not
claimed as proven.

Bifrost v1.9.0 defaults to 1,000 workers and a 5,000-entry buffer per provider
(`schemas/provider.go`). Harbor's routed account lists 24 curated providers;
Bifrost initialization prepares each configured provider and starts the worker
count (`bifrost.go`). Three assembly fixtures exercise serial real requests,
not throughput. They now explicitly request two workers and four buffer slots,
retaining their real provider, grant, routing, truncation, monetary and receipt
assertions. Production defaults are unchanged. The fixed full assembly command reached
921,736 KiB maximum child RSS
without a signal kill. This comparison includes Go compilation/linking. It
then reached the separate aggregate timeout described below; it did not pass
the original preflight gate.

The same omitted network configuration affects production routed runtimes.
A separate ordinary CGo-free diagnostic binary constructed one routed Bifrost
**driver** without sending model requests. Defaults produced 25,001 total
live goroutines (25,000 workers plus the probe), with observed idle RSS of
251,620 KiB after GC. Explicit two-worker/four-buffer configuration produced
51 total goroutines and 23,624 KiB RSS. Both returned to the one-goroutine
baseline after Close; the default process still retained allocated RSS
immediately afterward. This is a cold/idle component observation, not whole
Runtime, throughput, latency, or production-load qualification.

No production default change is included. Operators can already configure
pool sizes appropriate to their workload. A separate design review could
consider lazy initialization of actually selected curated providers while
preserving their configured concurrency and bounded lifecycle, instead of
allocating every supported provider at startup.

## Separate assembly-suite budget correction

After limiting only the serial fixtures' provider pools, the unchanged
120-second cap still failed the aggregate assembly package. Five 100-turn
rollover cases completed; the timeout interrupted the sixth case,
SQLite/budget-100000, with no assertion failure beforehand. The fixed source
therefore exposed a second, distinct harness-budget failure. The original
signal-kill and the fixed-source timeout logs are both preserved.

The assembly package grew from 51 to 86 top-level tests, including the full
three-store/three-budget 100-turn matrix, since the structured-output smoke's
July 2 aggregate cap. The phase-143 broad package invocation now gets the same
bounded 10-minute correctness-suite budget as `go test` normally supplies.
Its separately filtered structured-output integration invocation remains at
120 seconds. All test cases and runtime operation deadlines are unchanged.
The corrected assembly/runctx/planner invocation passed 444 test/subtest
events across in-memory, SQLite and PostgreSQL, with no skips or failures.
Assembly took 206.82 seconds; the full command took 215.28 seconds and peaked
at 922,940 KiB maximum child RSS including compilation/linking. This change
does not claim that the original 120-second gate passed or qualify runtime
performance.

## Additional checked witnesses

- Static smoke scripts 109e, 139, 223, 83f and 83i passed 69 assertions without
  skips. Thirteen broken-contract/wiring mutations were correctly refused.
- Phase 123 passed all three smoke checks, including exact execution of its
  three named cumulative-memory tests under the race detector.
- Phase 219's exact six integration and three served-runner named witnesses
  passed without skips; its external live HTTP leg is not claimed here.
- The separate phase-143 structured-output E2E invocation passed six
  test/subtest events without skips, at its unchanged 120-second budget.
- Repository drift audit passed 1,616 checks without warnings or failures.

## Remaining qualification

Full final-head live preflight, hosted aggregate CI and runtime performance
qualification remain integration gates. These local results do not assert
that the entire preflight or the original 120-second gates passed.
