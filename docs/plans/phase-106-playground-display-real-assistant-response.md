# Phase 106 — Playground displays the real assistant response

## Summary

The Playground today renders the literal string `"Message accepted by the Runtime."` as the agent's response to every chat message, regardless of what the LLM actually answered. Live-validation of the YouTube agent (2026-05-26) proved the LLM IS being called and IS returning a real answer — but the answer travels into `memory.AddTurn` only, never reaches the `TaskResult` the wire surface projects, and the Playground UI hardcodes a placeholder bubble instead of consuming the real event payload.

The bug is exclusively in the answer-plumbing path between `planner.Finish.Payload` and the Console chat bubble. Three single-pointer fixes restore the chain:

1. **Runtime side (`cmd/harbor/cmd_dev_runloop.go:684`)**: `d.tasks.MarkComplete(taskCtx, taskID, tasks.TaskResult{})` is called with an EMPTY result. Populate `TaskResult.Value` with the JSON-encoded answer that `extractAssistantAnswer(fin)` already produces (the helper lives at lines 960-985 of the same file and is currently called only for the memory writeback at line 673).
2. **Wire (`internal/protocol/types/tasks.go`)**: `TaskDetail.ResultInline` is already populated by the projector (`internal/tasks/protocol/registry_projector.go:205-207` reads `task.Result.Value`). NO new wire field is needed — the wire is fully built; the projector simply has nothing to project today because `TaskResult.Value` is empty.
3. **Console side (`web/console/src/routes/(console)/playground/[session_id]/+page.svelte:523`)**: remove the hardcoded `'Message accepted by the Runtime.'` placeholder. Subscribe to `task.completed` events. On completion, fetch the task via `tasks.get` (already part of the typed client), JSON-parse `task.result_inline`, render the answer as the agent bubble's text.

This phase is intentionally scoped to **single-shot completion** (the default LLM call path). Per-token streaming via `llm.completion.chunk` events is NOT in scope — see "Non-goals" and "Risks / open questions" for why. The current bifrost driver HAS a streaming path (`internal/llm/drivers/bifrost/bifrost.go::streamComplete`) but the RunLoop driver only invokes the single-shot `complete` path; wiring streaming through would touch the planner contract + add new event types, which is a Phase 107 concern.

## RFC anchor

- RFC §7 — Console as Protocol client (the Playground is the load-bearing first-touch operator surface).
- RFC §6.5 — LLM client + completion surface.
- RFC §6.8 — task lifecycle + TaskResult contract.
- RFC §1 — first-five-minutes adoption guarantee.

## Briefs informing this phase

- brief 11 — Console feature surface (Playground is RFC §7's load-bearing operator entry).
- brief 13 — operator UX (the chat-completion surface is the first thing an operator sees after attach).
- brief 06 — events / observability / devx (task-completion events + their payloads).

## Brief findings incorporated

- **brief 11 (Console feature surface).** "The Playground is the most-trafficked operator surface; treat it as a production product." A hardcoded placeholder agent message is the same anti-pattern caught in CLAUDE.md §13 (stub-as-default-driver) applied to UI.
- **brief 13 (operator UX).** "If the operator chats and gets no model output, the project reads as broken regardless of what the runtime actually did." The runtime's silent answer-drop is the LAST-mile failure that erases every other success.
- **brief 06 (devx surface).** "Every task's terminal state must carry the answer the operator (and the Console) needs to render the run." Empty TaskResult is the silent-drop bug pattern CLAUDE.md §13 explicitly forbids.

## Findings I'm departing from (if any)

None.

## Goals

- An operator who sends a message in the Playground sees the agent's actual answer text — the same text that today lands in `memory.AddTurn`.
- `tasks.MarkComplete` carries the planner's answer in `TaskResult.Value`. Any Protocol consumer (Console, third-party UI per `use-the-harbor-protocol`, CLI inspect commands) reads it via `tasks.get` → `TaskDetail.ResultInline`.
- The hardcoded `'Message accepted by the Runtime.'` is REMOVED from `playground/[session_id]/+page.svelte` (current line 523). No placeholder text injected by the chat flow.
- The Playground's SSE subscriber registers a `task.completed` handler that fetches `tasks.get` for the just-completed task and populates the agent bubble's text from `result_inline`.
- The `TaskResult.Value` JSON format is stable + documented in `docs/glossary.md`: a planner-shaped envelope `{"answer": "<text>", "tool_calls_seen": <int>, "finish_reason": "<string>"}`. The shape is forward-compatible for richer answers (markdown, multimodal) in future phases.
- A regression-proofing test asserts that `MarkComplete` is NEVER called with an empty `TaskResult` on a `FinishGoal` Finish.

## Non-goals

- **Per-token streaming.** The LLM provider (bifrost) supports streaming; the RunLoop driver does not consume the streaming path. Phase 106 leaves this untouched. A follow-up (Phase 107 or V1.3) can:
  1. Switch the RunLoop to `streamComplete`.
  2. Emit `llm.completion.chunk` events with the run/task ID + chunk bytes.
  3. Have the Playground subscribe to the chunk stream and append to the in-flight bubble.
  Phase 106 explicitly does not block on this — the single-shot path is what 100% of operators see today and what V1.2's first-five-minutes guarantee depends on.
- **Cost rollup population.** The probe showed `cost.total_tokens=0` because no Enricher implementation is wired in `cmd/harbor/cmd_dev*.go`. Wiring a cost-aggregating subscriber to `llm.cost.recorded` events is a separate gap; Phase 106 does not touch it. Tracked as a separate V1.2 follow-up (filed in this PR's CHANGELOG `[Unreleased]` if not already there).
- **Rendering markdown / tool-call traces / artifact previews inline.** Phase 106 renders plain text only. Markdown + tool-call cards already work in the existing Playground bubble shape (`web/console/src/lib/chat/`); Phase 106 just feeds them the correct text payload.
- **Changing the planner contract.** `planner.Finish.Payload` stays exactly as it is today.
- **Changing `task.completed` event shape.** The event is intentionally minimal (TaskID only, SafeSealed). Phase 106 does NOT add the answer to the event payload — consumers fetch via `tasks.get` (the existing two-step pattern documented in `internal/tasks/events.go:136-145`).

## Acceptance criteria

- [ ] **AC-1** `cmd/harbor/cmd_dev_runloop.go:684` no longer passes `tasks.TaskResult{}`. The exact new shape:
  ```go
  payload := map[string]any{
      "answer":         extractAssistantAnswer(fin),
      "finish_reason":  string(fin.Reason),
      "tool_calls_seen": len(traj.Steps),
  }
  raw, err := json.Marshal(payload)
  if err != nil {
      // Fail loudly per §13 — a marshal failure of a simple map is a coding bug.
      d.logger.Error("perTaskRunLoopDriver: marshal TaskResult.Value failed",
          slog.String("task_id", string(taskID)),
          slog.String("err", err.Error()))
      // Fall through to MarkComplete with empty Value — better than dropping the
      // run on the floor. The operator sees the empty result + the error log
      // (no silent degradation per §13).
      raw = []byte("{}")
  }
  if mErr := d.tasks.MarkComplete(taskCtx, taskID, tasks.TaskResult{Value: raw}); mErr != nil {
      ...
  }
  ```
- [ ] **AC-2** `tasks.TaskResult.Value` carries a JSON object of EXACTLY the shape `{ "answer": string, "finish_reason": string, "tool_calls_seen": int }`. The shape is documented in:
  - `docs/glossary.md` under `TaskResult.Value (answer envelope)` (new entry).
  - `internal/tasks/tasks.go` godoc on the `TaskResult` struct (extending the existing comment with one paragraph documenting the envelope contract).
- [ ] **AC-3** No regression to the memory writeback path. The existing `memory.AddTurn(taskCtx, sessionQ, turn)` call at line 676 still runs with the same `AssistantResponse: extractAssistantAnswer(fin)`. Both writebacks use the same source.
- [ ] **AC-4** `internal/tasks/protocol/registry_projector.go:205-207` is unchanged. The projector already populates `TaskDetail.ResultInline` from `task.Result.Value` when non-empty; no edit needed.
- [ ] **AC-5** New regression test `cmd/harbor/cmd_dev_runloop_test.go::TestPerTaskRunLoop_FinishGoal_PopulatesTaskResult`: drive a fake RunLoop whose `Run` returns `planner.Finish{Reason: planner.FinishGoal, Payload: map[string]any{"answer": "hello"}}`; assert the captured `MarkComplete` call's third argument has `len(TaskResult.Value) > 0` AND `JSON-parse(Value).answer == "hello"` AND `JSON-parse(Value).finish_reason == "goal"`.
- [ ] **AC-6** Negative test `cmd/harbor/cmd_dev_runloop_test.go::TestPerTaskRunLoop_FinishGoal_EmptyAnswer_StillPopulatesShape`: drive a RunLoop returning `Finish{Reason: FinishGoal, Payload: nil}`; assert `MarkComplete` still gets a populated `TaskResult.Value` with `JSON-parse(Value).answer == ""` and the other fields present.
- [ ] **AC-7** `web/console/src/routes/(console)/playground/[session_id]/+page.svelte` deletes the hardcoded placeholder line (current line 523). The agent bubble is created EMPTY when `sendMessage` resolves; populated by the `task.completed` handler.
- [ ] **AC-8** The same file's SSE subscriber (currently registers `task.completed`, `task.failed`, `task.cancelled` handlers for the running-flag clear) gains a NEW responsibility: on `task.completed`, it invokes `client.tasks.get(taskID)` and reads `result.result_inline`. The handler parses the JSON envelope, extracts `.answer`, and replaces the agent bubble text. Pseudocode:
  ```ts
  async function onTaskCompleted(taskID: string) {
    const detail = await client.tasks.get({
      identity: { tenant, user, session },
      id: taskID,
    });
    let answer = '';
    if (detail.task.result_inline) {
      try {
        const envelope = JSON.parse(detail.task.result_inline);
        answer = envelope.answer ?? '';
      } catch {
        answer = '(failed to parse answer payload)';
      }
    }
    // Replace the in-flight agent bubble for this taskID.
    messages = messages.map(m =>
      m.taskID === taskID && m.role === 'agent'
        ? { ...m, text: answer }
        : m
    );
    activeTaskID = null;
  }
  ```
- [ ] **AC-9** When `sendMessage` succeeds, the Playground IMMEDIATELY appends an empty agent bubble with `role: 'agent'`, `text: ''`, `taskID: <returned-id>`, `pending: true`. The bubble renders a loading indicator (existing CSS class `streaming-indicator` from the chat module). On `task.completed`, the bubble's `text` is replaced and `pending: false`.
- [ ] **AC-10** `task.failed` and `task.cancelled` SSE handlers populate the agent bubble with an error-shaped message: `text: "Task ${state} — see Tasks page for details."`, `role: 'system'`, `pending: false`. The bubble is visually distinct (existing CSS variant `system` already supports this).
- [ ] **AC-11** Reading `task.result_inline` from `tasks.get` works against the running YouTube validation agent end-to-end: a manual Playground send produces a non-empty agent bubble within 10 seconds (Phase 106's smoke verifies this).
- [ ] **AC-12** No SSE / chunk / streaming event handlers are added. The Playground stays single-shot. (Streaming is explicitly Phase 107 / future work — see Non-goals.)
- [ ] **AC-13** The generated TypeScript client at `web/console/src/lib/protocol.ts` regenerates via `make protocol-ts-gen` and shows a non-empty `result_inline` field on the `TaskRow` / `TaskDetail` type. (`TaskDetail.result_inline` is ALREADY in the wire shape per `internal/protocol/types/tasks.go:430`; the generator just needs a re-run if it was generated when the field was added.)
- [ ] **AC-14** All existing tests in `cmd/harbor/cmd_dev_runloop_test.go` + `internal/tasks/protocol/get_test.go` + `internal/tasks/protocol/registry_projector_test.go` still pass.
- [ ] **AC-15** Smoke `scripts/smoke/phase-106.sh` exercises every wire assertion listed below.
- [ ] **AC-16** A regression-proofing lint rule: a grep added to `scripts/drift-audit.sh` that fails the audit on any file under `web/console/src/routes/(console)/playground/` containing the literal `'Message accepted by the Runtime.'`. The literal is forbidden going forward.

## Files added or changed

### Runtime (Go)

- `cmd/harbor/cmd_dev_runloop.go` — apply AC-1 edit at line 684. ~15 LOC change. Imports `encoding/json` if not already present (it likely is).
- `cmd/harbor/cmd_dev_runloop_test.go` — add tests AC-5, AC-6. ~80 LOC. Use the existing fake-RunLoop fixture (already present in this file's test suite).
- `internal/tasks/tasks.go` — extend the godoc on `TaskResult` (line 263-268) with the envelope contract:
  ```go
  // TaskResult carries the successful-completion payload. `Value` is
  // pre-redacted by the caller (D-020); the registry stores it
  // verbatim.
  //
  // Phase 106 (V1.2) pins the answer-envelope contract: when the
  // run-loop driver (cmd/harbor/cmd_dev_runloop.go::handleSpawn)
  // produces TaskResult from a planner.Finish, `Value` is the JSON
  // encoding of:
  //
  //   {
  //     "answer":          string,  // the LLM's natural-language answer
  //     "finish_reason":   string,  // planner.FinishReason as string
  //     "tool_calls_seen": int      // len(traj.Steps) at finish
  //   }
  //
  // Consumers (Console Playground, CLI, third-party UIs) MAY rely on
  // this shape. Future planners that return richer answers (markdown
  // structure, multimodal) will EXTEND the shape with new keys, never
  // break existing ones (forward-compatible additive evolution).
  type TaskResult struct {
      Value json.RawMessage
  }
  ```

### Console (Svelte)

- `web/console/src/routes/(console)/playground/[session_id]/+page.svelte` — three edits to the same file:
  - Delete the hardcoded placeholder push at current line 523 (AC-7).
  - In `sendMessage`'s success path, append an empty agent bubble carrying `taskID` (AC-9).
  - Extend the SSE handler for `task.completed` to fetch `tasks.get` and populate the bubble (AC-8). Keep existing `task.failed` / `task.cancelled` handlers but add per-handler bubble population (AC-10).
- `web/console/src/lib/chat/` — verify the message-bubble component supports a `pending: true` flag that renders the existing loading indicator. If not, add it (likely a `<MessageBubble>` prop). Tag the loading indicator with `data-testid="agent-bubble-pending"` if not already.

### Smoke + drift

- `scripts/smoke/phase-106.sh` — **NEW** (replaces the existing skeleton from PR #243). Detailed below.
- `scripts/drift-audit.sh` — extend with the AC-16 grep. Add this block (after the existing skill frontmatter audit):
  ```bash
  # Phase 106 regression guard: the Playground placeholder bubble must not
  # come back. The literal text was load-bearing for the V1.1 bug where
  # operators saw no model output.
  if grep -rq "Message accepted by the Runtime" web/console/src/routes/\(console\)/playground/ 2>/dev/null; then
      fail "playground placeholder text 'Message accepted by the Runtime.' is forbidden — see phase 106"
  else
      ok 'Phase 106 regression guard: no playground placeholder text'
  fi
  ```

### Docs

- `docs/glossary.md` — new entry alphabetised under "T":
  ```markdown
  **TaskResult.Value (answer envelope)** — the JSON shape `TaskResult.Value` carries when the run-loop driver completes a task from a `planner.Finish`. Pinned by Phase 106 (V1.2):
  `{"answer": "<llm text>", "finish_reason": "<planner.FinishReason>", "tool_calls_seen": <int>}`.
  Forward-compatible — future phases extend with new keys; never break existing ones. Consumed via `tasks.get` → `TaskDetail.ResultInline` (string). Distinct from `TaskCompletedPayload` (the bus event), which is intentionally minimal (TaskID only) and routes consumers to `tasks.get` per D-020 redaction posture.
  ```

## Public API surface

- **No new Protocol method.** Phase 106 uses the existing `tasks.get` (`MethodTasksGet` at `internal/protocol/methods/methods.go:479`).
- **No new wire field.** `TaskDetail.ResultInline` already exists at `internal/protocol/types/tasks.go:430` and is already projected at `internal/tasks/protocol/registry_projector.go:205-207`. The fix is purely populating it upstream.
- **TaskResult.Value envelope shape** is a new informal contract documented in glossary + the `TaskResult` godoc. A semver-compatible additive evolution.
- **TypeScript `protocol.ts` regeneration**: run `make protocol-ts-gen` to refresh the generated client; the diff should be small (only if `TaskDetail.ResultInline` wasn't already exposed).

## Test plan

### Unit (Go)

- `cmd/harbor/cmd_dev_runloop_test.go::TestPerTaskRunLoop_FinishGoal_PopulatesTaskResult` — AC-5.
- `cmd/harbor/cmd_dev_runloop_test.go::TestPerTaskRunLoop_FinishGoal_EmptyAnswer_StillPopulatesShape` — AC-6.
- `cmd/harbor/cmd_dev_runloop_test.go::TestPerTaskRunLoop_FinishNonGoal_DoesNotPopulateResult` — verify `MarkFailed` is called (existing test; just confirm it still passes).
- `internal/tasks/protocol/registry_projector_test.go::TestProjectDetail_ResultInline_PopulatedWhenNonEmpty` — verify the projector reads `task.Result.Value` correctly. (Likely an existing test; if not, add one.)

### Unit (Svelte)

- `web/console/src/routes/(console)/playground/[session_id]/+page.test.ts` (new or extend):
  - `Playground: sendMessage appends an empty pending agent bubble with the returned taskID` — mock client.sendMessage to return a task_id; assert the new bubble has `pending: true`, `text: ''`, `taskID: <id>`.
  - `Playground: task.completed event populates the bubble from result_inline` — mock the SSE event + a `client.tasks.get` returning `result_inline: '{"answer":"Hi!"}'`; assert the bubble's text becomes `"Hi!"` and `pending: false`.
  - `Playground: task.failed shows error bubble` — assert role flips to 'system' + text mentions failure.
  - `Playground: malformed result_inline JSON renders graceful fallback` — mock `result_inline: 'not-json'`; assert bubble text is `(failed to parse answer payload)`.
  - **Negative**: `Playground: never injects "Message accepted by the Runtime."` — render the page, send a message, query the DOM for the placeholder text, assert it is NEVER present.

### Integration (Playwright)

- `test/integration/console_e2e_playground_test.go::TestE2E_Playground_HelloAnswered` — boot `harbor dev` against the YouTube validation agent, boot `harbor console`, attach (per Phase 105's one-click), open Playground, send "Hello! how are you?", assert within 15s that the agent bubble contains a non-empty text NOT equal to `"Message accepted by the Runtime."`. Real LLM + real bifrost.
- `test/integration/console_e2e_playground_test.go::TestE2E_Playground_TaskFailed_ShowsErrorBubble` — break the LLM key, send a message, assert the agent bubble shows the error variant.

### Conformance

- `internal/tasks/conformancetest/` — extend the existing MarkComplete conformance with: after MarkComplete with a non-empty TaskResult.Value, `Get(id)` returns a Task whose `Result.Value` matches byte-for-byte. Already covered by existing conformance; verify the touch.

### Concurrency / leak

- N/A — Phase 106 doesn't build a new reusable artifact. The TaskResult shape is per-call data, not shared state.

## Smoke script additions

`scripts/smoke/phase-106.sh` — `PREFLIGHT_REQUIRES: live-server`. Assertions:

1. **Send a question via Protocol** against the dev runtime (a fresh `harbor dev` boot the preflight gate provides):
   ```bash
   TOKEN=$(grep HARBOR_DEV_TOKEN "${HARBOR_DATA_DIR}/server.log" | head -1 | cut -d= -f2)
   RESP=$(curl -sS -X POST "$(api_url /v1/control/start)" \
     -H "Authorization: Bearer ${TOKEN}" \
     -H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev" \
     -H "Content-Type: application/json" \
     -d '{"query":"Reply with the single word OK","description":"phase-106 smoke"}')
   TASK_ID=$(echo "$RESP" | jq -r '.task_id')
   assert_truthy "$TASK_ID" "start returned a task_id"
   ```
2. **Poll until complete** (bounded 30s; fail on timeout):
   ```bash
   for i in $(seq 1 30); do
     STATUS=$(curl -sS -X POST "$(api_url /v1/tasks/get)" \
       -H "Authorization: Bearer ${TOKEN}" -H "X-Harbor-Tenant: dev" -H "X-Harbor-User: dev" -H "X-Harbor-Session: dev" \
       -H "Content-Type: application/json" \
       -d "{\"identity\":{\"tenant\":\"dev\",\"user\":\"dev\",\"session\":\"dev\"},\"id\":\"${TASK_ID}\"}" \
       | jq -r '.task.status')
     if [ "$STATUS" = "complete" ]; then break; fi
     sleep 1
   done
   assert_eq "$STATUS" "complete" "task reached complete within 30s"
   ```
3. **Read `result_inline` + parse the envelope**:
   ```bash
   DETAIL=$(curl -sS -X POST "$(api_url /v1/tasks/get)" -H "Authorization: Bearer ${TOKEN}" ... -d "{...}")
   INLINE=$(echo "$DETAIL" | jq -r '.result_inline')
   assert_truthy "$INLINE" "result_inline non-empty (phase 106 plumbed the answer)"
   ANSWER=$(echo "$INLINE" | jq -r '.answer')
   assert_truthy "$ANSWER" "answer field in envelope non-empty"
   FINISH=$(echo "$INLINE" | jq -r '.finish_reason')
   assert_eq "$FINISH" "goal" "finish_reason is goal for a normal completion"
   ```
4. **Static**: `grep -q "Message accepted by the Runtime" web/console/src/routes/(console)/playground/` returns NONZERO (zero hits — drift guard).
5. **Static**: `grep -q "result_inline" web/console/src/routes/(console)/playground/\[session_id\]/+page.svelte` — the Playground reads the new field.

## Coverage target

- `cmd/harbor/`: 80% (existing target). The single-line fix is trivially covered; the new tests bring the coverage delta.
- `internal/tasks/`: 80% (existing target). No coverage delta — existing tests already cover the projector path.
- `web/console/src/routes/(console)/playground/`: 80% (existing target).

## Dependencies

- 73 (Console state inspection surface — Playground is its evolution).
- 73d (Phase 73d: `tasks.get` + `TaskDetail` + the `Enricher` interface — already shipped).
- 83i (D-152 memory writeback — Phase 106 uses the SAME `extractAssistantAnswer` helper).
- **Soft dependency on 105**: a less-experienced operator following the first-five-minutes chain needs to attach FIRST before they see Playground answers. Both phases land in V1.2.

## Risks / open questions

- **Streaming vs single-shot drift.** Phase 106 ships single-shot. A future operator who hears about "streaming responses" from LangGraph / eino + finds Harbor non-streaming will be disappointed. Mitigation: document the V1.2 posture explicitly in `docs/skills/drive-the-playground/SKILL.md` ("V1.2 ships single-shot completion; streaming is post-V1") + file Phase 107 as the streaming follow-up.
- **Heavy answers.** A multi-page LLM answer JSON-encodes to several KB. The `TaskResult.Value json.RawMessage` field accepts arbitrary size, but the projector inlines it into `result_inline string`. RFC §6.10's D-026 heavy-content threshold (32KB default) is the cutoff: above that, the projector SHOULD switch to `result_ref ArtifactRef` instead of `result_inline`. Verify in the projector test that the existing threshold logic kicks in. (Existing logic; Phase 106 does not extend the threshold.)
- **JSON-marshal failure of the envelope.** `extractAssistantAnswer` returns a string; the envelope marshals deterministically. The fallback at AC-1 logs Error and writes `{}` — this is "fail loudly + degrade gracefully" rather than crashing the run. A future contract-validation pass (Phase 107?) could promote this to Marshal-failures-as-fatal.
- **Envelope shape evolution.** If a future planner returns markdown / multimodal, the envelope gains keys. Backward compatibility: existing consumers (Console) read only `.answer`, future consumers can read `.markdown` / `.attachments` etc. Document the rule in glossary.

## Glossary additions

- **TaskResult.Value (answer envelope)** — see "Docs" section above for the exact text. Alphabetised under "T".

## Pre-merge checklist

- [ ] `make drift-audit` passes (with the new Phase-106 regression guard active)
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Multi-isolation: no identity surface changes — N/A
- [ ] Concurrent-reuse test — N/A (no new reusable artifact)
- [ ] Integration test — `TestE2E_Playground_HelloAnswered` against real `harbor dev` + real LLM
- [ ] Glossary updated for `TaskResult.Value (answer envelope)`

## Implementation order (suggested)

1. **Apply the AC-1 runtime fix** (`cmd_dev_runloop.go:684`). Verify with `go test -race ./cmd/harbor/...`.
2. **Add the Go regression tests AC-5, AC-6**.
3. **Extend `TaskResult` godoc** (AC-2 in `internal/tasks/tasks.go`).
4. **Verify the projector path end-to-end** with a quick `harbor dev` + `curl /v1/control/start` + `curl /v1/tasks/get` round-trip; the new `result_inline` should be populated.
5. **Edit `playground/[session_id]/+page.svelte`**:
   1. Remove the placeholder bubble injection (AC-7).
   2. Append an empty pending bubble on `sendMessage` success (AC-9).
   3. Extend the `task.completed` handler to fetch + populate (AC-8).
   4. Extend `task.failed` / `task.cancelled` handlers to populate error variants (AC-10).
6. **Add Svelte unit tests** for the Playground edits.
7. **Add the Playwright e2e test** (`TestE2E_Playground_HelloAnswered`).
8. **Add the drift-audit regression guard** (AC-16 grep).
9. **Add the new glossary entry**.
10. **Write `scripts/smoke/phase-106.sh`** + verify it passes locally against `harbor dev` + the YouTube agent.
11. **Run `make protocol-ts-gen-check`** — should be a no-op (TaskDetail.result_inline already in the wire shape).
12. **Run `make drift-audit && make preflight`** — both green.
13. **Open PR.**
