# Phase 212 — artifact read-path byte correctness + loud reference-resolution failure

## Summary

Two defects on the agent-facing artifact read path, both of which turn a recoverable
situation into a silent-or-fatal one. `artifact_fetch` returns its window as
`Content: string(window)` over a `Content string` field, so any byte sequence that is
not valid UTF-8 is silently replaced with `U+FFFD` — a PDF, an image or a zip comes
back corrupted with no error and no signal. Separately, an artifact-reference parameter
that fails to resolve terminates the dispatch step instead of becoming an observation
the model can act on, so a model that names a wrong id gets a dead run rather than a
correction. This phase makes the first fail loud and the second recoverable.

## RFC anchor

- RFC §6.10
- RFC §6.4
- RFC §6.5

## Briefs informing this phase

- brief 05
- brief 07

## Brief findings incorporated

- brief 05 (state, tasks, artifacts, sessions): artifact routing is mandatory rather
  than advisory — the store is the one place heavy content lives, and every read path
  is obliged to be honest about what it returns. A read that silently substitutes
  replacement characters for stored bytes breaks the routing guarantee at the last
  hop, where it is least visible.
- brief 07 (code-level tool calling): the runtime owns dispatch and the model is a
  decision-maker, not a runner. A decision the model got wrong — naming an artifact id
  that does not resolve — is a decision to be corrected on the next turn, not a runtime
  fault. The failure belongs in the observation channel, which is the channel the model
  reads.
- brief 05: content-addressed identity means a mis-stamped MIME is permanent for that
  content. The UTF-8 admissibility check therefore cannot be MIME-driven alone —
  `application/json` is stamped unconditionally on heavy tool results whatever the tool
  produced, so a MIME-only gate would both admit corrupt binary and refuse valid text.

## Findings I'm departing from (if any)

None.

## Goals

- A read of a non-UTF-8 artifact through `artifact_fetch` fails loudly, naming the MIME
  and the byte offset at which admissibility failed, and points the model at the
  by-reference route rather than returning damaged text.
- A read of a UTF-8 artifact is byte-identical to today, including at a window boundary
  that splits a multi-byte rune.
- An artifact-reference parameter that fails to resolve becomes a tool-result
  observation naming the unresolvable id, not a step-terminating error.
- The single-call dispatch path and the parallel dispatch path agree on what a
  resolution failure is.

## Non-goals

- Base64-encoding binary content into the model's context. A 1 MiB PDF is ~350–470k
  tokens of base64 and the model can do nothing with it; the correct route for binary
  is pass-by-reference (phase 210's in-process arm, phase 214's MCP arm), and this
  phase points at that route rather than widening this one.
- Changing `artifacts.get`'s Protocol response. `types.ArtifactsGetResponse.Content` is
  already `[]byte` and already correct — it is the twin this phase stops contradicting,
  not a surface this phase touches.
- Any change to the heavy-content threshold or the fetch ceiling. Phase 213 owns those.
- Any change to which identity may read which artifact. The scope check at
  `artifact_fetch.go:240` and the resolver at `dispatch.go:353` are unchanged.

## Acceptance criteria

- [ ] `artifact_fetch` over an artifact whose window is not valid UTF-8 returns a
      populated `Error` naming the stored MIME and the failing byte offset, and an
      empty `Content` — never a `U+FFFD`-substituted string.
- [ ] The refusal message names the by-reference route explicitly, so the model's next
      decision has somewhere to go.
- [ ] `artifact_fetch` over a valid-UTF-8 artifact returns byte-identical `Content` to
      the pre-phase build, asserted over a fixture set that includes multi-byte runes.
- [ ] A window whose trailing bytes are a truncated multi-byte rune is ADMITTED, not
      refused: the truncation is an artefact of windowing, not of the stored content.
      The window is trimmed to the last complete rune and `returned_bytes` reports the
      trimmed length, so the reported count and the returned content agree.
- [ ] A window whose LEADING bytes are a rune continuation (an `offset` landing
      mid-rune) is admitted on the same rule, trimmed from the front.
- [ ] An `artifactref.Ref` whose id does not resolve produces a tool-result observation
      carrying the unresolvable id and the reason, and the run continues to the next
      planner turn.
- [ ] The observation does NOT carry the session's full artifact list — the
      `<session_artifacts>` block already carries it every turn, and duplicating it into
      a failure payload would be a second source for the same fact.
- [ ] A resolution failure is distinguishable in the observation from a tool's own
      error, so a planner cannot mistake one for the other.
- [ ] `ErrNoResolver` (no resolver seated at all) stays a step-terminating error — it
      is a runtime misconfiguration, not a model mistake, and the model has no recovery
      for it.
- [ ] Mutation-verified: reverting each guard turns a smoke `OK` into a `FAIL`, never
      into a `SKIP`.

## Files added or changed

- `internal/tools/builtin/artifact_fetch.go` — UTF-8 admissibility gate; rune-boundary
  trimming in `artifactWindow`; refusal shape.
- `internal/tools/builtin/artifact_fetch_test.go` — binary refusal, UTF-8 parity,
  boundary-split fixtures.
- `internal/tools/artifactref/artifactref.go` — a typed resolution-failure error the
  dispatch layer can classify (distinct from `ErrNoResolver`).
- `internal/runtime/dispatch/dispatch.go` — `callTool` classifies a resolution failure
  as an observation rather than a step error, matching `callParallel`'s existing
  per-branch posture.
- `internal/runtime/dispatch/dispatch_test.go` — the classification, both paths.
- `test/integration/artifact_readpath_test.go` — real store + real dispatch, both
  defects end-to-end.
- `scripts/smoke/phase-212.sh`
- `docs/decisions.md` — D-357.
- `docs/glossary.md` — "admissible window".

## Public API surface

```go
// internal/tools/artifactref
// ErrResolveFailed classifies a reference whose id did not resolve — the
// model named something that is not there. Distinct from ErrNoResolver
// (a runtime misconfiguration) because only the first is recoverable by
// the model on its next turn.
var ErrResolveFailed = errors.New("artifactref: reference did not resolve")
```

No change to `Ref`, `Substitute`, `Resolver`, or `WithResolver` signatures.

## Test plan

- **Unit:** UTF-8 admissibility over a fixture matrix (pure ASCII; multi-byte runes;
  a rune split at the window head; a rune split at the window tail; a PDF header; a
  zip header; an empty artifact). `artifactWindow` trimming arithmetic, including the
  case where trimming empties a window entirely. The resolution-failure classification
  in isolation.
- **Integration:** `test/integration/artifact_readpath_test.go` — real `inmem` +
  `sqlite` artifact stores behind a real dispatch executor. Store a PDF fixture, fetch
  it, assert refusal-not-corruption. Store UTF-8, fetch it, assert parity. Dispatch a
  tool with a bad `Ref`, assert the run reaches the next turn with an observation.
  Identity propagation asserted on every leg; cross-tenant read still answers
  not-found. Failure mode: a store that errors mid-read.
- **Conformance:** N/A — no interface gains a method, so the five-driver conformance
  suite is untouched.
- **Concurrency / leak:** the builtin and the dispatch executor are both compiled
  artifacts under D-025. N≥128 concurrent `artifact_fetch` invocations against one
  registered instance and one shared store, mixing binary and text artifacts across
  two tenants, under `-race`: no data race, no cross-tenant bleed, no goroutine growth
  past baseline after teardown.

## Smoke script additions

- `put` a non-UTF-8 artifact, `artifact_fetch` it, assert the response carries a
  populated error naming the MIME and an empty content field.
- `put` a UTF-8 artifact containing multi-byte runes, `artifact_fetch` it with a
  `max_bytes` that splits a rune, assert the returned content is valid UTF-8 and
  `returned_bytes` equals its length.
- Assert the refusal message contains the by-reference pointer.
- Skip-if-404 on the `artifact_fetch` surface so the script is a no-op on builds that
  predate it.

## Coverage target

- `internal/tools/builtin`: 85%
- `internal/tools/artifactref`: 90%
- `internal/runtime/dispatch`: 85% (no regression from current)

## Dependencies

- 210 (the in-process pass-by-reference arm and the substitution invariant this phase's
  second half makes recoverable)
- 209 (the `artifacts.get` byte read whose `Content []byte` shape is the correct twin)

## Risks / open questions

- **The UTF-8 gate could refuse valid text that happens to be windowed badly.** This is
  the main correctness risk and it is why boundary trimming is an acceptance criterion
  rather than an implementation note: without it, paging a UTF-8 CSV at a fixed
  `max_bytes` would intermittently refuse. The fixture matrix exists to pin exactly
  this.
- **A binary artifact currently "works" in the sense that it returns something.** Making
  it refuse is a behaviour change for any caller that was tolerating corruption. It is
  taken deliberately: the returned content was never usable, and §13 names silent
  degradation as the forbidden shape. The refusal message carries the alternative route,
  so a caller has somewhere to go rather than only a wall.
- **Classifying a resolution failure as an observation gives a model a retry loop it
  can spin in.** Bounded by the existing per-run step cap and the governance layer's
  cost ceilings; this phase adds no new bound and claims none (D-351's bar).

## Glossary additions

- **Admissible window** — the byte range `artifact_fetch` will return as text: the
  requested window trimmed to whole UTF-8 runes at both ends. A window that is not
  valid UTF-8 after trimming is inadmissible and refused rather than substituted.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] Concurrent-reuse test passes — N≥128 concurrent invocations against a single
      shared instance under `-race`
- [ ] Integration test wires real drivers, asserts identity propagation, covers ≥1
      failure mode, runs under `-race`
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
