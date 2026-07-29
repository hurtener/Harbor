# Phase 213 — heavy-content threshold rebalance + search-preview split

## Summary

`config.DefaultHeavyOutputThresholdBytes` is 32 KiB and is single-sourced into four
consumers with materially different jobs. At 32 KiB an ordinary tool result — a 40 KiB
JSON API response — is promoted to an artifact stub, so the model must spend a second
tool call reading back what it just produced. This phase raises the offload threshold to
128 KiB and splits the one consumer for which that value is wrong: the search-result
preview bound, where 128 KiB is not a preview.

## RFC anchor

- RFC §6.5
- RFC §6.10
- RFC §6.13

## Briefs informing this phase

- brief 05
- brief 15

## Brief findings incorporated

- brief 05 (state, tasks, artifacts, sessions): mandatory artifact routing exists to
  keep heavy content out of the context window, not to keep content small for its own
  sake. The threshold is a context-budget decision, so it should be sized against the
  context window it protects — 32 KiB is ~8k tokens, roughly 4% of a 200k window, which
  buys far less headroom than the round-trip it costs.
- brief 15 (native tool-calling + deferred loading): every avoidable round trip is a
  full planner turn — a provider call, a trajectory step, and latency the operator
  pays. A threshold that promotes typical results to stubs converts one tool call into
  three (call, stub, fetch) for content that would have fitted.
- brief 05: a preview is a discovery affordance, not a read. The search index's preview
  bound and the offload threshold answer different questions and only coincidentally
  shared a value.

## Findings I'm departing from (if any)

None. The single-sourcing comment on `DefaultHeavyOutputThresholdBytes` states that
"no other literal copy of the value is allowed"; this phase does not add a copy, it
gives one consumer its own named constant with its own documented rationale. That is a
split of the value, not a duplication of it, and the comment is amended to say so.

## Goals

- The offload threshold, the LLM-edge leak guard and the trajectory payload budget move
  together to 128 KiB, remaining one value with one source.
- The search-result preview bound stops tracking the offload threshold and gains its
  own constant, sized as a preview.
- The move is operator-visible and operator-reversible: the existing
  `artifacts.heavy_output_threshold_bytes` config field continues to override, and a
  configuration written before this phase resolves to the new default.

## Non-goals

- Raising the `artifact_fetch` read ceiling. `fetch_default_max_bytes` (64 KiB) and
  `fetch_hard_max_bytes` (1 MiB) are unchanged — they bound a deliberate read-back, not
  an automatic promotion, and they were already made operator policy in phase 209.
- Introducing a separate ceiling for pass-by-reference egress. Substituted bytes never
  enter the model's context and are therefore governed by a different budget entirely;
  phase 214 owns that knob and it is deliberately not derived from this one.
- Any change to D-241's conversation-text exemption or to which fields
  `findContextLeak` walks. Phase 210 widened the walk; this phase changes only the
  number it compares against.

## Acceptance criteria

- [ ] `DefaultHeavyOutputThresholdBytes` is 128 KiB and remains the ONE source for the
      offload threshold, the LLM-edge guard and the dispatch safety floor.
- [ ] `search.HeavyPreviewThreshold` no longer aliases it and carries its own constant
      with a godoc stating why a preview bound is not an offload bound.
- [ ] A tool result between 32 KiB and 128 KiB is returned inline rather than promoted
      to a stub — asserted at the dispatch projection, not only at the config layer.
- [ ] A tool result above 128 KiB is still promoted, and `ErrContextLeak` still fires
      above 128 KiB at the LLM edge.
- [ ] The trajectory payload budget tracks the new threshold
      (`threshold - trajectoryPayloadHeadroom`) with no separate edit.
- [ ] A search result whose preview would exceed the NEW preview bound is still
      truncated at the preview bound, not at 128 KiB.
- [ ] An existing `harbor.yaml` that sets `heavy_output_threshold_bytes: 32768`
      continues to resolve to 32 KiB — the operator's explicit value wins (§10).
- [ ] An existing `harbor.yaml` that does NOT set the field resolves to 128 KiB, and
      the example config documents the new default.
- [ ] Mutation-verified: reverting the split turns the preview-bound smoke assertion
      from `OK` to `FAIL`.

## Files added or changed

- `internal/config/config.go` — the constant, its godoc, and the amended
  single-sourcing note.
- `internal/search/search.go` — the split constant + rationale godoc.
- `examples/harbor.yaml`, `examples/dev.yaml`, `examples/serve.yaml` — documented
  default.
- `internal/llm/registry_test.go`, `internal/runtime/dispatch/dispatch_test.go`,
  `internal/search/search_test.go` — threshold-sensitive assertions retargeted.
- `internal/llm/summarizer/trajectory_test.go` — budget arithmetic.
- `test/integration/heavy_threshold_test.go` — the band between old and new default,
  end-to-end.
- `scripts/smoke/phase-213.sh`
- `docs/decisions.md` — D-358.

## Public API surface

```go
// internal/config
// DefaultHeavyOutputThresholdBytes is the ONE source of the heavy-output
// threshold default (128 KiB).
const DefaultHeavyOutputThresholdBytes = 128 * 1024

// internal/search
// HeavyPreviewThreshold bounds a SearchResultRow preview. It is
// deliberately NOT the offload threshold: a preview is a discovery
// affordance sized to be scanned in a result list, and a result list of
// ten previews at the offload threshold would exceed most context
// windows on its own.
const HeavyPreviewThreshold = 32 * 1024
```

## Test plan

- **Unit:** threshold resolution from config (unset → new default; set → operator
  value; zero/negative → default). The preview bound is independent of the offload
  threshold — asserted by a test that would fail if the alias were restored.
  Trajectory budget arithmetic at the new value.
- **Integration:** `test/integration/heavy_threshold_test.go` — a real dispatch
  executor over a real artifact store. A 64 KiB tool result (in the newly-inlined band)
  reaches the planner observation inline with no stub written to the store; a 256 KiB
  result is promoted and the stub resolves. Identity propagation asserted; failure mode
  = a store that rejects the promotion write, which must fail loud rather than silently
  inline the heavy value.
- **Conformance:** N/A — no driver interface changes.
- **Concurrency / leak:** N/A for new artifacts; this phase builds none. The existing
  dispatch executor's D-025 test is re-run at the new threshold to confirm the
  projection stays race-free when more results take the inline branch.

## Smoke script additions

- Assert the running server's resolved heavy threshold is 128 KiB on a default config
  (via the config-reporting surface, skip-if-404).
- Drive a tool producing a ~64 KiB result and assert the observation is inline (no
  `ArtifactStub` in the response shape).
- Drive a tool producing a ~256 KiB result and assert a stub IS produced.
- Assert a search query whose row preview would exceed 32 KiB reports truncation at the
  preview bound.

## Coverage target

- `internal/config`: 90% (no regression)
- `internal/search`: 85%
- `internal/runtime/dispatch`: 85% (no regression)

## Dependencies

- 210 (the `findContextLeak` widening onto `ToolCalls[].Args`, whose comparison this
  phase re-targets)
- 209 (the fetch-ceiling config fields this phase deliberately leaves alone)

## Risks / open questions

- **A 128 KiB inline result is ~32k tokens, roughly 16% of a 200k window.** That is the
  deliberate trade: the cost is paid only by results in the 32–128 KiB band, which
  would otherwise have cost a full extra planner turn each. Results above the threshold
  are still promoted, so the tail is unchanged.
- **Weakening the leak guard is the objection, and it does not hold on inspection.**
  `materializeRequest` runs first at the same threshold and offloads; `findContextLeak`
  catches what materialization could not. They are a designed pair, so moving them
  together preserves the relationship. The guard's job is "something got past the
  offload", not "nothing is ever this large" — this is recorded in D-358 because it is
  the argument a future reader is most likely to re-derive incorrectly.
- **Operators who tuned around 32 KiB.** Any explicit value still wins. The risk is
  confined to deployments relying on the default, and the example configs document the
  change.

## Glossary additions

- **Preview bound** — the byte ceiling on a search-result row preview. Distinct from
  the heavy-output threshold: the first sizes a scannable list entry, the second sizes
  what may enter a model's context as a whole tool result.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] N/A — this phase builds no new reusable artifact; the existing dispatch D-025
      test is re-run at the new threshold
- [ ] Integration test wires real drivers, asserts identity propagation, covers ≥1
      failure mode, runs under `-race`
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
