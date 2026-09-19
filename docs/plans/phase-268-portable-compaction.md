# Phase 268 — Portable compaction and request budgets

## Summary

Implement RFC 002's first slice by evolving the trajectory, compression runner,
ReAct builder, and existing governed LLM client. Preserve fresh results after
compaction and bound both ordinary requests and maintenance calls. This phase
is in progress; no release, durable cross-turn continuity, or RC is claimed.

## RFC anchor

- RFC §6.2
- RFC §6.3
- RFC §6.5

## Briefs informing this phase

- brief 02
- brief 05
- brief 08

## Brief findings incorporated

- brief 02 §1: the runtime owns mechanism; swappable planners own reasoning.
- brief 05 §1: reuse identity-scoped persistence and artifact abstractions.
- brief 08: the composed LLM client and pinned Bifrost integration remain the
  single provider-call path, including maintenance calls.

## Findings I'm departing from (if any)

D-462 supersedes the single-compression and summary-only replay assumptions of
D-055/D-202. Historical research sketches are not current API contracts. Raw
reasoning, tool handles, and duplicate raw observations are not summary input.

## Goals

- Repeatable, source-bound compaction without hiding newer observations.
- Protected recent and not-yet-presented exchanges, including queued results.
- Honest capacity enforcement over model-visible input with output headroom.
- Bounded chronological summarization through the existing governed client.

## Non-goals

- No long-term memory, provider-native compaction, separate SDK, new service,
  resource registry, or additional public transcript.
- No automatic cold execution relaunch or replay of ambiguous side effects.
- Durable cross-turn retention and release validation belong to RFC 002's
  later slices; they are not claimed by this increment.

## Acceptance criteria

- [x] Runtime stamps version, generation, exact boundary, and source digest;
      model-generated coverage is ignored.
- [x] ReAct replays uncovered native call/result groups after a checkpoint,
      including a fresh complete receipt and later corrective error.
- [x] A growing tail supports repeated compaction with the previous narrative;
      already-covered evidence is not supplied again as raw steps.
- [x] Latest exchanges and not-yet-presented queued outcomes remain outside the
      compacted prefix; standalone callers always retain the latest exchange.
- [x] Failed, vacuous, cancelled, or stale candidates preserve previous state;
      an extension cannot mutate live result trees through its summary input.
- [x] Inspection proceeds during generation; checkpoint publication is guarded.
- [x] Serialization preserves validated coverage; legacy summaries never hide
      history through a guessed coverage boundary.
- [x] Summarization visits all selected steps chronologically under bounded
      byte-input/output/call limits; no silent step elision or metadata clipping.
- [x] Production compaction measures the assembled request, including tools,
      arguments, instructions, output schema, context, and media estimates.
- [ ] Effective model/route capacity, output headroom, and fallback attempts are
      validated before sending; maintenance has a distinct governed call identity.
- [x] Real Bifrost scripted-HTTP integration covers chunked success, known
      truncation failure, and compatible model switching with fresh native
      receipts intact; no native compaction endpoint or paid model is used.
- [ ] Route/grant maintenance admission and differing provider wire formats
      have full integration coverage before completing this phase.
- [ ] Full phase smoke, existing regressions, drift, coverage, and preflight
      gates pass before declaring the phase finished.

## Files added or changed

- `internal/planner/trajectory/`, `internal/planner/compression.go`
- `internal/planner/react/`, `internal/runtime/steering/`
- `internal/llm/`, `internal/llm/summarizer/`, `sdk/planner/`
- Corresponding unit/integration tests, RFCs, glossary, decisions, operator
  skill, master plan, and `scripts/smoke/phase-268.sh`.

## Public API surface

Additive `SummaryCoverage` alias on the existing planner facade. No Protocol
method, public conversation content, configuration default, or dependency change
in the replay increment. `Summariser` and `MaybeCompress` remain source-compatible.

## Test plan

- **Unit:** coverage boundaries/digests, legacy behavior, detached input,
  cancellation/failure, repeated compaction, token-accounting contributions.
- **Integration:** real ReAct builder/client boundary and steering loop; fresh
  receipt and error projection, native pairing, identity, and failure. Add the
  real Bifrost HTTP seam before completing the phase.
- **Conformance:** trajectory JSON round-trip; existing planner/summarizer suites.
- **Concurrency / leak:** N>=128 shared-runner invocations under `-race`,
  cancellation isolation, and inspection while the summarizer blocks.

## Smoke script additions

Run portable-compaction and inspection regressions with `go test -race` against
actual components. Assert the phase/RFC/decision links. No fake endpoint or
implementation-success assertion for pending acceptance criteria.

## Coverage target

Preserve existing package floors. Exercise every new checkpoint rejection and
publication branch; record measured coverage in the PR before phase completion.
The incremental race suite is not a claim that coverage/preflight already pass.

## Dependencies

- 43, 45, 46 — trajectory, ReAct, compaction.
- 111e — production summarizer and steering consumer.
- 33 — Bifrost driver.

## Risks / open questions

A valid JSON summary is not proof of semantic completeness. Legacy history can
be incomplete; no guessed cursor is allowed. Large protected results can exceed
physical capacity and must fail or remain behind authorized references, never
silently disappear. Full request budgets and maintenance grant identity remain pending.
Raw JSON trajectory deserialization yields action maps, not executable Decision
values; cold native-call replay needs an explicit representation in the durable
context slice. The model-switch test here uses the retained live trajectory and
does not claim cold-run replay.

## Glossary additions

Summary coverage; updated compression-budget and runner semantics.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage on touched packages meets the stated target
- [ ] Cross-session isolation and shared-component race tests pass
- [ ] Real-driver integration includes identity and failure paths
- [x] New vocabulary is in the glossary
- [x] Departures are recorded in D-462

## Chronological summarizer increment

D-463 removes pre-summary step elision and fragment clipping. Every selected
exchange enters one bounded chronological request; the prior narrative carries
forward. Defaults cap generation at 16 completion calls, 2,048 output tokens per
call, and 16 KiB returned narrative (or half the smaller payload allowance).
An indivisible oversized exchange requires a bounded reference or raises an
explicit capacity error. The effective composed-client model window is still
an additional guard, not interchangeable with this byte budget.

Local validation rejects unknown/duplicate/missing fields, null/non-string
array values, note-only output, invalid encoding, extra content, known length or
non-stop termination, and unexpected tools. Bifrost unary/streaming adapters now
preserve choice-zero finish reasons; absent reasons remain unknown. Exact JSON
integers survive the runner's detached summary-input copy. No partial candidate
is installed after failure, cancellation, or bounded-maintenance exhaustion.

## Physical request capacity increment

The mandatory final safety guard subtracts explicit or profile-default output
allowances and the existing conservative margin from the effective model window.
It counts native declarations and historical arguments through the shared request
estimator. The check applies again on every leaf attempt; changing to a smaller
model cannot reuse the larger model's capacity. An absent output bound remains
unknown for legacy callers, not an invented provider default. Production automatic
compaction over the assembled request and strict maintenance grants remain pending.

Regression tests cover exclusive boundaries, defaults/overrides, invalid bounds,
integer extremes, native input, smaller-model rejection, and 128 concurrent calls.
The same increment repairs baseline task error handling and the canonical
`run_llm_settings_v1` capability registry/conformance mismatch found by CI. Wire
digests are regenerated with the repository's Go 1.26 toolchain; Go 1.27-only
reflected type spelling is not introduced into generated documentation.

## Assembled-request compaction increment

The reference ReAct planner exposes a message rebuild callback, and the runtime
binds its existing compactor through a run-local request preparation context.
The composed LLM client invokes preparation after route selection and before
governance/provider execution. The effective request estimate includes native
declarations and arguments, instructions, response schema and the existing media
estimate. Its target is bounded by the model window minus output and margin;
physical admission remains mandatory after all downstream transformations.

`MaybeCompressRequest` reuses the same selection/generation/publication algorithm
as `MaybeCompress`. Non-request-aware planners retain standalone compression;
there is no second compaction implementation. A maintenance request has no
message rebuild callback, so it cannot recursively start context preparation.
Each prepared decision performs at most one compaction and then rebuilds messages
without duplicating guidance events or changing tools, grants or sampling controls.
A protected tail may exceed the soft target; physical overflow is an explicit
error, not another compaction loop or silent evidence drop.

The run-loop/Bifrost HTTP regression proves that guidance can trigger compaction
while the trajectory alone remains below target. It asserts exact fresh result
replay, matching call identity, a single compression event and failure isolation.
Unit tests cover declaration/argument/schema accounting, output-default capacity,
invalid input, cancellation, failures, bypasses and 128 concurrent scoped calls.
Strict maintenance-grant identity/accounting and model-aware summary chunk sizing
remain outstanding; this increment does not declare phase or RC completion.

## Disabled-compaction compatibility correction

Fresh-result exposure tracking is installed and advanced only when a compactor
and positive working-input budget are configured. Disabled compaction leaves
legacy pause-checkpoint bytes unchanged; it does not silently add execution
retention state. The no-runner and zero-budget regressions first failed with the
unconditional tracking and now pass with the steering and pause/resume race
suites. This is a compatibility correction, not durable cross-turn delivery.
