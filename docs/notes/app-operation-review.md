# App operation candidate review — 2026-10-06

Base: `6305c88fce1fcacc6ff2fa34440ac037c5de63f3`. This branch implements the
runtime portion of [App operation v1](../contracts/app-operation-v1.md).
The external broker/host consumer has now completed an isolated real-service
qualification using this candidate; no production deployment is included.

## Direct review

Reviewed the exact Protocol admission, source resolution, argument serialization,
credential exchange, shared cache and singleflight, current-use revalidation,
cancellation/shutdown, audit and MCP retry paths without delegated reviewers.
The selector is not a credential. Existing signed identity/provider destination
and fresh render admission remain separate required checks.

The real transport test exposed the zero-policy retry default: an empty retry
list alone still restored the default policy and repeated a simulated write.
The corrected call-local policy uses the existing single-attempt budget and an
empty retry set. The regression now observes exactly one execution. Ordinary
provider behavior is unchanged.

No additional concrete P0/P1 was found in this bounded runtime review. This is
a bounded runtime review; downstream policy/UI has separate local evidence.

## Local verification

- The Protocol, auth, credential-source, MCP driver and App accessor race suites
  passed except an existing exact wire-field-set assertion. Its expected set was
  updated for the new optional selector; the full types package then passed.
- Final focused race checks pass for the new operation binding, Protocol
  admission, broker exchange and real MCP transport, including N=128 concurrent
  pulls, warm-cache isolation, cancellation, withdrawal and unknown effects.
- New binding package coverage: 91.3%. Parser fuzzing: 31,768 executions in a
  bounded 15-second run; passed.
- Console Protocol/host-client suites: 42 tests passed. Svelte: zero errors and
  warnings. Changed TypeScript lint and Protocol lockstep passed.
- Generated Console manifest, external TypeScript types and Protocol docs passed
  regeneration checks. Go vet passed for the changed runtime packages.
- Existing production signed-provider preparation and render-admission focused
  race tests passed.
- Initial audit found extra blank lines in new prose, now corrected, plus an
  existing main-branch scaffold fallback at v1.31.9. The published module ledger
  is v1.32.1; the pin and two generated goldens were updated and their test passed.
- The full preflight's drift audit passed (1,592 checks, no failures). Its
  static batch exposed four old memory smoke scripts still asserting removed
  pre-RFC-002 APIs. Those guards now pin the existing retained-context
  admission/application/writeback and current budget projection; all four
  corrected scripts pass, including their compression runtime tests.
- The initial full preflight failed on stale smoke expectations. The corrected
  final full gate now passes; the final result is recorded below.

Hosted CI was not used because account billing prevents it. No production
deployment, merge or paid model call occurred. External App activation was limited
to the isolated local qualification below.

## External consumer qualification — 2026-10-07

The existing Protocol resource/tool path was exercised by a real no-chat App host
and canonical broker. The 261,609-byte resource used `artifact_ref` with bounded,
identity-scoped retrieval and exact digest verification. Actual callback execution,
fresh sealed admission and operation-specific broker pulls passed. The consumer
qualified creation/save/reopen/preview/publication and a separate read-only user
through both registered HTTP and MCP modes, including dependency withdrawal,
close/reopen, expiry, changed admission/generation, digest mismatch and logout.
Four central catalogs intentionally remain host-owned native HTTP composition;
this is not a claim that all catalog traffic uses MCP. No model conversation or
paid provider call was opened. Production and merges remain unchanged.

## Host-generation integration follow-up

The downstream host cannot derive an exact callback intent from the sealed
admission alone. Operation-bound opt-in resource reads now also return the exact
catalog generation used by that mint, outside the token. A real sealed-authority
regression verifies the returned value; ordinary, unavailable and non-opt-in
responses omit it. The new read/admission race selection passes. Generated wire
artifacts were refreshed; final lockstep checks remain part of the final gate.

The initial full unit batch additionally exposed three stale guards after the
accepted retained-context refactor: phase112b's renamed projection test,
phase123's renamed failed-summary preservation test, and phase186's moved batch
renderer call site. The guards now follow the existing implementation; corrected
phase123/186 scripts pass with their actual runtime tests. Phase143 limited the entire assemble suite to120seconds. The captured full
package rerun passes (assemble160.211s, runctx3.454s, planner4.236s). Its gate now
keeps the same complete race suites with a five-minute bound and prints captured
failure output. No runtime change or test removal was needed.

The first full preflight also exposed phase 72b's stale canonical error-code inventory: RFC-002 added `retained_context_unsettled` and `retained_context_unavailable`. Its exact identifier list now includes those two existing codes; the cardinality and declaration checks remain enforced. The live phase 72b rerun passes in the final preflight.

The final retry also corrected phase132/219 smoke delegates to their current
existing test names (`TestNewRunContext_NoParallelMemoryProjection` and
`TestE2E_CallerMemory_ComposesWithConversation`). The stale names selected no
tests and correctly failed smoke validation. Tests were not removed or weakened.

## Final before-commit gates — 2026-10-07

`make drift-audit` passed 1,592 checks with zero warnings/failures after the final
contract/status updates. The complete `make preflight` passed: build, live boot,
133 static and 143 unit smoke scripts, then live Protocol/runtime surfaces. Its
395 phase summaries contain 187 expected skipped assertions, including 21 wholly
unshipped surfaces under the existing convention; this is not a zero-skip release
claim. The changed phase238 smoke passed 69 assertions, zero skips and zero
failures. No failure marker remains. The final source was reviewed directly;
no additional unresolved P0/P1 is known in the bounded operation integration.

The real-service binaries were built from the final implementation working trees
before these qualification documentation commits. No post-commit deployment or
published runtime release is claimed. External local fixtures completed normally
with zero model calls, and their task-owned services were stopped after evidence
capture. Hosted CI remains unrun because of billing.
