# Phase 273 — Native output provenance

## Summary

Record immutable task-owned output membership only for successful, verified native binary tool materialization. Persist bounded pre-invocation fences before I/O, seal the manifest before task completion, and project metadata through existing canonical session turns.

## RFC anchor

- RFC §6.4
- RFC §6.8
- RFC §6.10

## Briefs informing this phase

- brief 03
- brief 05
- brief 07

## Brief findings incorporated

- brief 03 §4: resolve and invoke through the unified descriptor, with transport-neutral result normalization.
- brief 05 §4: idempotency and at-least-once delivery require persisted identity-bound state rather than an exactly-once claim.
- brief 05 §5: mandatory interfaces and shared conformance avoid driver-specific optional capability gaps.
- brief 07 §8: runtime dispatch owns concurrent execution and ordered result handling.

## Findings I'm departing from (if any)

None.

## Goals

- An artifact first written by another task can still be proven as a current task output only after its bytes were natively materialized in a successful current invocation.
- JSON-shaped references, helper results, sibling session artifacts and failed tool results never create membership.
- Headless `RunOnce` retains ordinary tool behavior but owns no registry-backed task output manifest. Its correlation ID and an inherited caller context cannot create task-output authority; the served task driver binds its exact accepted task.
- Pending or settled invocation slots cannot be blindly reissued after a missing observation.
- Legacy unknown and new sealed empty output sets remain distinct. A modern completed task with an explicitly empty result projects a definite empty answer, permitting outputs-only completion without inventing legacy data.

## Non-goals

- No authoring, preview or download tools; no helper/sibling output inheritance.
- No publication authority, runtime production grants, byte proxy or knowledge ingestion.
- No automatic repair of an admitted invocation with missing observation; operator attention is required.
- No multi-writer task scheduling or automatic re-drive of interrupted durable tasks. Task-row EventID CAS prevents a stale registry from overwriting newer fences or recovery outcomes.

## Acceptance criteria

- [ ] Stable invocation identity binds owner triple, engine TaskID, append-only trajectory position and branch index; exact request hash rejects drift.
- [ ] Admission persistence follows approval/OAuth/local argument checks and precedes tool I/O; approval/auth pause/resume leaves no false pending fence. Uncertain admission/capture terminates planning via required-cleanup semantics.
- [ ] Opaque runtime witnesses arise only after verified materialization and successful tool return.
- [ ] At most 32 output refs and 50 current-decision branch slots; terminal manifest is immutable.
- [ ] In-memory, SQLite and PostgreSQL durable restart preserve exact evidence.
- [ ] Existing session-turn Protocol exposes version/hash/input-revision and deterministic exact refs.
- [ ] Real synthetic MCP binary result reaches canonical projection and a narrowly authorized direct transfer consumer.
- [ ] 100-way shared-instance race tests pass; known ordinary tool errors retain existing semantics.

## Files added or changed

- `internal/tasks/{output_artifacts.go,engine/,conformancetest/,drivers/durable/}`
- `internal/tools/artifactcontent/`, `internal/tools/drivers/mcp/`
- `internal/runtime/{dispatch,parallel,steering,serve}/`
- `internal/sessions/turns/`, `internal/protocol/`, `sdk/protocolclient/`
- Generated Protocol docs and TypeScript wire declarations; this phase plan and D-489.

## Public API surface

- Existing `sessions.turns.get` / `sessions.turns.list` consumer rows gain `output_manifest: {version, sha256?, input_revision}`.
- `version: 0` is legacy/unknown; `version: 1` identifies a sealed direct-native output set, including known empty.
- `outputs` are deduplicated exact references ordered by artifact ID; callers require a currently visible completed sealed task/turn, matching effective agent and input revision, plus current artifact authorization.
- Public SDK aliases expose the existing session-turn request/response and metadata types. No new Protocol method.

## Test plan

- **Unit:** opaque witness binding, forged JSON, exact scope, immutable manifest digest, known empty vs unknown, late projection refusal, known error handling.
- **Integration:** native dispatcher/MCP materialization with real artifact/task stores; fresh canonical turn projection and direct transfer; persisted uncertainty stops the next planner step.
- **Conformance:** shared TaskRegistry admission, drift, completion and defensive-copy tests; durable restart across state-store triad.
- **Concurrency / leak:** 100-way exact slot admission and 100 concurrent native tasks sharing one executor, including parallel branches.
- **Failure:** admission/capture persistence failure, restart before capture and after capture/before observation, failed tool binary content, helper witness mismatch.

## Smoke script additions

- `scripts/smoke/phase-273.sh` runs the deterministic native MCP, restart, uncertainty, shared-executor and immutable projection gates against real local stores; PostgreSQL remains a required qualification environment.

## Coverage target

- Touched task engine, dispatch, artifact-content and turn projection paths: 80% branch-oriented focused coverage; full package measurements reported during qualification.

## Dependencies

- Phase 269 (canonical durable trajectory checkpoint).
- Phase 270 (recipient-admitted direct transfer).
- Phase 271 (consumed input revision).

## Risks / open questions

- A settled capture without a persisted observation is deliberately terminal attention, not an automatic replay or fabricated observation.
- Only the direct native invocation result is certified. Nested helpers, resource links and model-authored refs remain unsupported.
- The task registry is single-active-runtime; stored metadata proves historical provenance, never current visibility or a publication grant.

## Glossary additions

- **Native output manifest:** immutable bounded task-owned metadata for successful verified direct-native binary tool outputs.
- **Output invocation fence:** persisted exact decision/branch admission preventing blind replay while provenance persistence is uncertain.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. If this phase does NOT build a reusable artifact, mark this checkbox N/A with a one-line reason.
- [ ] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists (in-package adapter test OR `test/integration/<topic>_test.go`), wires real drivers end-to-end, asserts identity propagation, covers ≥1 failure mode, and runs under `-race`.** See AGENTS.md §17. If `Dependencies` above is `00` only, mark this checkbox N/A with a one-line reason.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
