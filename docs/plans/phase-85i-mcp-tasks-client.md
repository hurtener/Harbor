# Phase 85i — mcp-tasks-client

## Summary

Build the MCP Tasks *client* (requestor) on the Phase 85h wire types: Harbor task-augments `tools/call` requests against servers that support it, polls `tasks/get` honouring `pollInterval` / `ttl`, retrieves results via `tasks/result`, cancels via `tasks/cancel`, and lists via `tasks/list`. Honours per-tool `execution.taskSupport` (`required` / `optional` / `forbidden`), threads the related-task `_meta` through every task-lifecycle message, and enforces identity-bound task isolation — a task created under one `(tenant, user, session)` is invisible to another.

## RFC anchor

- RFC §6.4

## Briefs informing this phase

- brief 14
- brief 03

## Brief findings incorporated

- brief 14 §5: the full requestor behaviour — two-phase response (`CreateTaskResult` then `tasks/result`), polling until terminal-or-`input_required`, the tool-level negotiation precedence, the related-task `_meta` obligation, the error-code contract — is the spec for this phase.
- brief 14 §5 (security): "when an authorization context exists, receivers MUST bind tasks to it … a task created under one identity is invisible to another. This is non-negotiable for Harbor and lands as a hard acceptance criterion." — enforced here.
- brief 14 §5: "`tasks/result` blocks until terminal; `tasks/get` polls … requestors SHOULD continue polling even after invoking `tasks/result`." — the polling/result interaction is implemented per the spec's concurrency note.
- brief 14 §5: once 85c/85d ship, servers may task-augment Harbor's sampling/elicitation (Harbor as *receiver*). This phase ships the *requestor* side; receiver-side task state machines are flagged as a follow-up.

## Findings I'm departing from (if any)

- None.

## Goals

- Harbor advertises the `tasks` client capability (`list`, `cancel`, and `requests.tools.call` — the requestor-relevant subset) when task support is enabled.
- A `tools/call` against a server advertising `tasks.requests.tools.call` is task-augmented per the tool's `execution.taskSupport`: `forbidden`/absent → never; `optional` → operator-policy choice; `required` → always (a non-task call would get `-32601`).
- After a `CreateTaskResult`, Harbor polls `tasks/get` at the server-suggested `pollInterval`, transitions through the lifecycle, retrieves the result with `tasks/result` once terminal, and surfaces it to the planner as an ordinary tool result.
- `tasks/cancel` is wired to Harbor's cancellation path — cancelling a run cancels its in-flight MCP tasks.
- `tasks/list` is consumed (paginated) for observability — the Console can list a server's tasks.
- Every task-lifecycle message carries the related-task `_meta`; `tasks/get|list|cancel` correctly omit it (taskId is already a param).
- Task isolation: `tasks/get|result|cancel` for a task not belonging to the current identity are rejected; `tasks/list` returns only the current identity's tasks.

## Non-goals

- Task *receiver* behaviour — Harbor running task state machines for server-task-augmented sampling/elicitation. Flagged as a follow-up phase once 85c/85d/85h/85i are all in.
- Durable task storage surviving a Harbor restart — in-process task tracking is V1-of-this-band; durability overlaps Phase 87 (durable TaskService) and is deferred.
- The `notifications/tasks/status` *server* side — Harbor consumes the notification opportunistically but never relies on it (the spec says requestors MUST NOT); polling is the source of truth.

## Acceptance criteria

- [ ] Harbor advertises the `tasks` capability with `list`, `cancel`, `requests.tools.call`.
- [ ] A `required`-taskSupport tool is always invoked task-augmented; a `forbidden`/absent one never is; an `optional` one follows operator policy. A capability-absent server is never task-augmented regardless of per-tool metadata. All four cases are tested.
- [ ] The poll loop honours `pollInterval` and stops at a terminal status or `input_required`; `tasks/result` is called once terminal; the result maps to a normal Harbor tool result.
- [ ] `input_required` triggers a `tasks/result` call (per the spec: requestors SHOULD preemptively call `tasks/result` on `input_required`) — the elicitation/sampling the task depends on flows through Phase 85c/85d.
- [ ] Cancelling a Harbor run issues `tasks/cancel` for that run's in-flight tasks; a cancelled task stays cancelled.
- [ ] Related-task `_meta` is present on every task-augmented request + on `tasks/result` responses; absent on `tasks/get|list|cancel`. Verified by a wire-capture test.
- [ ] **Identity isolation:** a `tasks/get|result|cancel` for a task owned by identity B, issued under identity A, is rejected before the wire call; `tasks/list` under A returns only A's tasks. A two-identity concurrent test asserts this.
- [ ] Error handling: `-32602` (bad/expired taskId, terminal-cancel), `-32603`, `-32600` (task-augmentation-required) are surfaced as typed Harbor errors; an expired task is not retried indefinitely.
- [ ] Concurrent-reuse: N≥100 concurrent task-augmented invokes against a shared provider pass under `-race` with no task-tracking corruption or cross-task bleed.

## Files added or changed

- `internal/tools/drivers/mcp/` — new `tasks_client.go`: task-augmentation decision, the poll loop, result retrieval, cancel wiring, `tasks/list` consumption.
- `internal/tools/drivers/mcp/mcp.go` — advertise the `tasks` capability; route task-augmentable `tools/call` through the task path.
- `internal/tools/drivers/mcp/tasks/` (Phase 85h package) — consumed; small additions if the transcription missed a requestor-side helper.
- `internal/tools/drivers/mcp/events.go` — `mcp.task_created` / `mcp.task_status_changed` / `mcp.task_completed` events.
- `internal/config/config.go` — operator policy for `optional`-taskSupport tools (task-augment by default or not).
- `internal/protocol/` — a read surface for `tasks/list` so the Console can show tasks (finalised against the Console wave).
- Test files — mock MCP server implementing the Tasks state machine; two-identity isolation fixtures.
- `examples/harbor.yaml` — document the task-augmentation policy knob.
- `scripts/smoke/phase-85i.sh`.
- `docs/decisions.md` — decision entry (filed at implementation time): task isolation bound to the identity triple; in-process (non-durable) task tracking for this band.
- `docs/plans/README.md` — Status flip on merge.

## Public API surface

No new exported MCP-driver types beyond what Phase 85h established. Three new event types. A Protocol read method for `tasks/list` (single-source rule applies).

## Test plan

- **Unit:** task-augmentation decision across the four capability/`taskSupport` combinations; poll-loop termination (terminal, `input_required`, expiry); related-task `_meta` placement; error-code mapping.
- **Integration:** mock MCP server with a working Tasks state machine — task-augmented `tools/call` → poll → result; an `input_required` task that drives an elicitation (composes with Phase 85d); cancellation; `tasks/list`. Real driver, identity propagation, `-race`.
- **Conformance:** N/A — Phase 85j (which adds a Tasks conformance pass).
- **Concurrency / leak:** N≥100 concurrent task-augmented invokes; two-identity isolation; poll-loop goroutines joined on Close (leak baseline).
- **Failure modes:** expired task; `-32600` task-required server; cancel of a terminal task; server that stops responding mid-poll.

## Smoke script additions

- `scripts/smoke/phase-85i.sh` (classification: `static-only`):
  - Assert `internal/tools/drivers/mcp/tasks_client.go` exists.
  - Assert the events taxonomy contains `mcp.task_created` / `mcp.task_status_changed` / `mcp.task_completed`.
  - Assert the MCP driver advertises a `tasks` capability.

## Coverage target

- `internal/tools/drivers/mcp`: 85%.

## Dependencies

- 85h (the Tasks wire types).
- 28 (MCP driver).

## Risks / open questions

- **Poll-loop goroutine discipline.** Each in-flight task runs a poll loop; these must be cancellable via the run's `ctx` and joined on Close — a leak here is a D-025 violation. The leak-baseline test is a hard gate.
- **`input_required` ↔ elicitation composition.** A task that hits `input_required` because the server needs elicitation input depends on Phase 85d being landed. If 85i lands before 85d, the `input_required` path is exercised by a mock and the real composition is a fast-follow — documented.
- **Non-durable tracking.** A Harbor restart loses in-flight task tracking; the server's task survives (it has the durable ID) but Harbor forgets it. This band accepts that; durable task tracking is deferred (overlaps Phase 87). The limitation is documented in `examples/harbor.yaml`.
- **`ttl` honesty.** Harbor must not poll a task past its `ttl`; an expired-task `-32602` must terminate the loop, not retry.

## Glossary additions

- (Tasks glossary terms land with Phase 85h.) This phase adds no new vocabulary.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage ≥ target
- [ ] **Cross-isolation test passes** — identity-bound task isolation is the headline guarantee.
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent task-augmented invokes; poll-loop leak baseline restored after Close.
- [ ] **Integration test passes** — mock MCP server with a real Tasks state machine; poll → result → cancel → list.
- [ ] Glossary updated — N/A (terms landed with 85h); marked N/A with this reason.
- [ ] No brief departures.
