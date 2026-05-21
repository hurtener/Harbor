# Phase 85h — mcp-tasks-wire-types

## Summary

The pre-phase for MCP Tasks support. The official Go SDK (`go-sdk v1.6.0`) exposes no `tasks/*` surface, so Harbor hand-transcribes the Tasks wire types and capability shapes from the 2025-11-25 spec (`basic/utilities/tasks`) into Go — the same pattern Harbor used for the A2A v1 wire shapes (`internal/distributed/a2a/`, "hand-transcribed from proto"). The Dockyard MCP-server framework, which already retrofitted Tasks compatibility in Go, is the reference implementation for the transcription. This phase ships **types + capability negotiation surface only** — no client polling logic (that is Phase 85i).

## RFC anchor

- RFC §6.4

## Briefs informing this phase

- brief 14
- brief 03

## Brief findings incorporated

- brief 14 §5: "go-sdk v1.6.0 exposes **no** `tasks/*` surface. This does **not** block Harbor: Tasks is hand-transcribed from the 2025-11-25 spec, the same pattern Harbor already used for the A2A v1 Go shapes." — this phase is that transcription.
- brief 14 §5: the full Tasks surface — capability shape, `Task` object fields (`taskId`, `status`, `statusMessage?`, `createdAt`, `lastUpdatedAt`, `ttl`, `pollInterval?`), the `working → input_required ⇄ working → terminal` lifecycle, `CreateTaskResult`, `execution.taskSupport ∈ {required, optional, forbidden}`, related-task `_meta`, the `-32602/-32603/-32600` error codes — is the transcription spec.
- brief 14 §5: "Tasks is **experimental** in 2025-11-25." — the Go types carry an experimental-stability godoc marker.
- brief 14 §5 (security): "a task created under one identity is invisible to another … the wire types must carry the binding field." — the transcribed types include an identity-binding field.

## Findings I'm departing from (if any)

- None. Hand-transcription of a spec into Go shapes is established Harbor practice (A2A precedent, AGENTS.md §3).

## Goals

- A Go package carrying the complete MCP Tasks wire surface: the `tasks` capability struct (with the `requests.{tools.call, sampling.createMessage, elicitation.create}` sub-structure), the `Task` object, `TaskStatus` enum, `CreateTaskResult`, the `task` request-param, `execution.taskSupport` tool metadata, the related-task `_meta` shape, and the four method names + the `notifications/tasks/status` notification name.
- The `TaskStatus` enum is a sealed set: `working`, `input_required`, `completed`, `failed`, `cancelled`, with the valid-transition table encoded as a validator.
- The capability negotiation surface: a function that, given a server's advertised `tasks` capability, answers "can request type X be task-augmented against this server?".
- Every type carries a godoc note that Tasks is experimental in spec 2025-11-25 and the shapes may change.
- The types carry the identity-binding field the spec's security section mandates (mapped to Harbor's identity triple).

## Non-goals

- Any client logic — no polling, no `tasks/get` calls, no task-augmented `tools/call`. That is Phase 85i.
- Any task *receiver* logic — Harbor running task state machines when servers task-augment its sampling/elicitation. Receiver behaviour is a later concern; this phase ships types both directions can use.
- Persistence of tasks — durable task storage is Phase 85i's concern (and overlaps Phase 87's durable TaskService).

## Acceptance criteria

- [ ] A new package (`internal/tools/drivers/mcp/tasks/` or `internal/mcptasks/` — implementer's call, documented) carries: `Capability`, `Task`, `TaskStatus`, `CreateTaskResult`, `TaskParams`, `TaskSupport` (`required`/`optional`/`forbidden`), `RelatedTaskMeta`, and the method-name constants (`tasks/get`, `tasks/result`, `tasks/cancel`, `tasks/list`, `notifications/tasks/status`).
- [ ] `TaskStatus` is a sealed enum; a `TaskStatus.CanTransitionTo(next)` validator encodes the spec's transition table (`working → {input_required, completed, failed, cancelled}`; `input_required → {working, completed, failed, cancelled}`; terminal → nothing).
- [ ] All types JSON-round-trip against the spec's example payloads (from brief 14 §5 / the spec page) — golden tests with the spec's literal JSON.
- [ ] A `CanTaskAugment(serverCapability, requestType)` helper answers the capability question; a `ToolTaskSupport(tool)` helper reads `execution.taskSupport` and applies the spec's precedence rules (capability gate first, then per-tool metadata).
- [ ] The related-task `_meta` key constant is exactly `io.modelcontextprotocol/related-task`.
- [ ] Every exported type has a godoc note: experimental, spec 2025-11-25, may change.
- [ ] The `Task` type carries an identity-binding field; godoc explains it maps to the `(tenant, user, session)` triple and is the basis for the cross-identity isolation Phase 85i enforces.
- [ ] No client behaviour ships — a test asserts the package has no network/IO dependency (pure types + validators).

## Files added or changed

- `internal/tools/drivers/mcp/tasks/` (new package) — `types.go` (wire types), `status.go` (enum + transition validator), `capability.go` (negotiation helpers), `method_names.go` (constants).
- `internal/tools/drivers/mcp/tasks/*_test.go` — golden JSON round-trip tests against the spec examples; transition-table tests; capability-helper tests.
- `scripts/smoke/phase-85h.sh`.
- `docs/decisions.md` — decision entry (filed at implementation time): hand-transcription of MCP Tasks from spec, Dockyard as reference, A2A precedent cited.
- `docs/plans/README.md` — Status flip on merge.

## Public API surface

```go
// internal/tools/drivers/mcp/tasks (illustrative — finalised at implementation
// against the 2025-11-25 spec and Dockyard's Go retrofit)

type TaskStatus string

const (
    TaskWorking       TaskStatus = "working"
    TaskInputRequired TaskStatus = "input_required"
    TaskCompleted     TaskStatus = "completed"
    TaskFailed        TaskStatus = "failed"
    TaskCancelled     TaskStatus = "cancelled"
)

func (s TaskStatus) IsTerminal() bool
func (s TaskStatus) CanTransitionTo(next TaskStatus) bool

type Task struct {
    TaskID        string
    Status        TaskStatus
    StatusMessage string // optional
    CreatedAt     string // ISO 8601
    LastUpdatedAt string // ISO 8601
    TTL           *int64 // ms; nil = unlimited
    PollInterval  *int64 // ms; optional
    // identity-binding field — maps to Harbor's (tenant, user, session)
}

type CreateTaskResult struct{ Task Task /* + _meta */ }

const RelatedTaskMetaKey = "io.modelcontextprotocol/related-task"
```

## Test plan

- **Unit:** JSON round-trip against every spec example payload; `TaskStatus` transition table (all valid + a sample of invalid transitions); `CanTaskAugment` across capability shapes; `ToolTaskSupport` precedence (capability-absent overrides per-tool `optional`/`required`).
- **Integration:** N/A — this phase ships pure types; the integration test belongs to Phase 85i (the consumer). A test asserts the package imports nothing from `net/*` or an MCP transport (purity).
- **Conformance:** N/A — Phase 85j.
- **Concurrency / leak:** N/A — immutable value types; marked N/A with this reason.

## Smoke script additions

- `scripts/smoke/phase-85h.sh` (classification: `static-only`):
  - Assert the `tasks` package exists.
  - Assert `RelatedTaskMetaKey` equals the exact spec string `io.modelcontextprotocol/related-task`.
  - Assert the five `TaskStatus` constants are present.

## Coverage target

- `internal/tools/drivers/mcp/tasks`: 90% (pure types + validators — high coverage is cheap and the transcription must be exact).

## Dependencies

- 28 (MCP driver — the package sits in its tree and shares its conventions).

## Risks / open questions

- **Spec drift.** Tasks is experimental; the 2025-11-25 shapes may change in the next spec version. The godoc markers + the golden tests against literal spec JSON make a future spec bump a visible, contained diff.
- **Dockyard divergence.** Dockyard's Go retrofit is a *reference*, not authoritative — the spec is authoritative. Where Dockyard and the spec differ, the spec wins; the transcription documents any such divergence.
- **Package placement.** `internal/tools/drivers/mcp/tasks/` keeps Tasks with the MCP driver; an alternative is a top-level `internal/mcptasks/`. The §3 layout rule means a new top-level dir needs an RFC update — so the sub-package placement is preferred unless a strong reason emerges.

## Glossary additions

- **MCP Tasks** / **Task-augmented request** / **Related-task metadata** — see brief 14 §9; the canonical Go shapes for all three land in this phase's package.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage ≥ target
- [ ] Cross-isolation test — N/A (pure types; the isolation *field* ships here, its *enforcement* is Phase 85i). Marked N/A with this reason.
- [ ] Concurrent-reuse test — N/A (immutable value types). Marked N/A with this reason.
- [ ] Integration test — N/A (pure types; consumer is Phase 85i). Marked N/A with this reason.
- [ ] Glossary updated.
- [ ] No brief departures.
