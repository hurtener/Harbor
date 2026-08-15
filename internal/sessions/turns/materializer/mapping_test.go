package materializer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestMaterialize_TerminalCauseMapping pins the three terminal causes
// and the closed error-class derivation from task error codes.
func TestMaterialize_TerminalCauseMapping(t *testing.T) {
	t.Run("cancelled", func(t *testing.T) {
		h := newHarness(t, "")
		defer h.closeStore()
		m := h.newMaterializer(t)
		quad := testQuad(h.id, "run-cx")
		h.src.publish(t, spawnEv(h.id, quad.RunID, "task-cx", tasks.KindForeground, ""))
		h.src.publish(t, startedEv(h.id, "task-cx"))
		h.src.publish(t, cancelledEv(h.id, "task-cx"))
		if _, err := m.Materialize(context.Background()); err != nil {
			t.Fatalf("materialize: %v", err)
		}
		row := mustGetRow(t, h, "task-cx")
		if row.Status != turns.StatusCancelled || !row.Sealed || row.FinishReason != turns.FinishCancelled {
			t.Errorf("cancelled row = status %q sealed %v finish %q", row.Status, row.Sealed, row.FinishReason)
		}
	})
	t.Run("error class derivation", func(t *testing.T) {
		cases := []struct{ code, want string }{
			{"timeout", "timeout"},
			{"deadline_exceeded", "timeout"},
			{"5xx", "5xx"},
			{"output_invalid", "permanent"},
			{"something-else", "unclassified"},
		}
		for _, tc := range cases {
			t.Run(tc.code, func(t *testing.T) {
				h := newHarness(t, "")
				defer h.closeStore()
				m := h.newMaterializer(t)
				quad := testQuad(h.id, "run-ec")
				h.src.publish(t, spawnEv(h.id, quad.RunID, "task-ec", tasks.KindForeground, ""))
				h.src.publish(t, failedEv(h.id, "task-ec", tc.code))
				if _, err := m.Materialize(context.Background()); err != nil {
					t.Fatalf("materialize: %v", err)
				}
				row := mustGetRow(t, h, "task-ec")
				if string(row.ErrorClass) != tc.want {
					t.Errorf("error class = %q, want %q", row.ErrorClass, tc.want)
				}
			})
		}
	})
}

// TestMaterialize_ActivityPolicyExhausted pins the policy_exhausted
// activity status and its derived terminal class + summary.
func TestMaterialize_ActivityPolicyExhausted(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)
	quad := testQuad(h.id, "run-px")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-px", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-px"))
	h.src.publish(t, toolInvokedEv(h.id, quad.RunID, "flaky.tool", time.Now()))
	h.src.publish(t, toolPolicyExhaustedEv(h.id, quad.RunID, "flaky.tool", "transient"))
	h.src.publish(t, failedEv(h.id, "task-px", "timeout"))
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	row := mustGetRow(t, h, "task-px")
	if len(row.Activity.Rows) != 1 {
		t.Fatalf("activity rows = %d", len(row.Activity.Rows))
	}
	act := row.Activity.Rows[0]
	if act.Status != turns.ActivityPolicyExhausted || !act.PolicyExhausted ||
		act.TerminalClass != turns.ActivityTerminalPolicyExhausted ||
		act.AttemptCount != 3 {
		t.Errorf("activity = %+v", act)
	}
	if row.Activity.Totals.PolicyExhausted != 1 {
		t.Errorf("totals = %+v", row.Activity.Totals)
	}
}

// TestMaterialize_ToolCompletionWithoutInvocationIsOmitted pins the
// honest unmatched-completion contract: a tool.completed with no
// tracked in-flight invocation (its invoked event was evicted from the
// source, surfaced by a retention gap) is omitted — never a fabricated
// success row, never an error.
func TestMaterialize_ToolCompletionWithoutInvocationIsOmitted(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)
	quad := testQuad(h.id, "run-uc")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-uc", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-uc"))
	h.src.publish(t, toolCompletedEv(h.id, quad.RunID, "ghost.tool", 5))
	h.src.publish(t, failedEv(h.id, "task-uc", "timeout"))
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	row := mustGetRow(t, h, "task-uc")
	if len(row.Activity.Rows) != 0 || row.Activity.Totals.Succeeded != 0 {
		t.Errorf("activity = %+v, want empty (unmatched completion omitted honestly)", row.Activity)
	}
}

// TestMaterialize_ReasoningKindsAndOmission pins the derived reasoning
// mapping: CallTool / SpawnTask / AwaitTask derive closed steps with
// strictly increasing chronological indices, and shapes without a safe
// derivative (CallParallel, Finish, RequestPause) are omitted honestly.
func TestMaterialize_ReasoningKindsAndOmission(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)
	quad := testQuad(h.id, "run-rk")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-rk", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-rk"))
	h.src.publish(t, decisionEv(h.id, quad.RunID, "CallTool"))
	h.src.publish(t, decisionEv(h.id, quad.RunID, "SpawnTask"))
	h.src.publish(t, decisionEv(h.id, quad.RunID, "CallParallel")) // unclassifiable → omitted
	h.src.publish(t, decisionEv(h.id, quad.RunID, "AwaitTask"))
	h.src.publish(t, decisionEv(h.id, quad.RunID, "Finish")) // unclassifiable → omitted
	h.src.publish(t, failedEv(h.id, "task-rk", "timeout"))
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	row := mustGetRow(t, h, "task-rk")
	want := []turns.ReasoningStep{
		{Index: 0, Kind: turns.ReasoningKindToolCall},
		{Index: 1, Kind: turns.ReasoningKindSpawn},
		{Index: 2, Kind: turns.ReasoningKindAwait},
	}
	if len(row.Reasoning.Steps) != len(want) {
		t.Fatalf("reasoning steps = %+v, want %+v", row.Reasoning.Steps, want)
	}
	for i, step := range row.Reasoning.Steps {
		if step != want[i] {
			t.Errorf("reasoning step %d = %+v, want %+v", i, step, want[i])
		}
	}
}

// TestMaterialize_CumulativeUsageAccumulates pins the cumulative
// per-measure rollup across multiple llm.cost.recorded events: tokens
// and latency sum as exact integers, cost sums as integer micro-dollars
// (honestly estimated), and a measure no call reported stays
// unavailable (never a fabricated zero).
func TestMaterialize_CumulativeUsageAccumulates(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)
	quad := testQuad(h.id, "run-u")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-u", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-u"))
	h.src.publish(t, costRecordedEv(h.id, quad.RunID, "model-a", llm.Usage{
		PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, LatencyMS: 100,
	}, 0.10))
	h.src.publish(t, costRecordedEv(h.id, quad.RunID, "model-b", llm.Usage{
		PromptTokens: 200, CompletionTokens: 0, TotalTokens: 200, LatencyMS: 250,
	}, 0.05))
	h.src.publish(t, failedEv(h.id, "task-u", "timeout"))
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	row := mustGetRow(t, h, "task-u")
	if row.Usage.PromptTokens.State != turns.UsageExact || usageValue(t, row.Usage.PromptTokens) != 300 {
		t.Errorf("prompt = %+v, want 300", row.Usage.PromptTokens)
	}
	if row.Usage.TotalTokens.State != turns.UsageExact || usageValue(t, row.Usage.TotalTokens) != 350 {
		t.Errorf("total = %+v, want 350", row.Usage.TotalTokens)
	}
	if row.Usage.CostMicroUSD.State != turns.UsageEstimated || usageValue(t, row.Usage.CostMicroUSD) != 150_000 {
		t.Errorf("cost = %+v, want estimated 150000 micro-USD", row.Usage.CostMicroUSD)
	}
	if row.Usage.LatencyNS.State != turns.UsageExact || usageValue(t, row.Usage.LatencyNS) != 350_000_000 {
		t.Errorf("latency = %+v, want 350_000_000 ns", row.Usage.LatencyNS)
	}
	// Model is the LAST reported.
	if row.Usage.Model != "model-b" {
		t.Errorf("model = %q, want model-b (last reported)", row.Usage.Model)
	}
}

// TestMaterialize_ReasoningOverflowClampsAndContinues pins the bounded
// reasoning contract (independent-review P1 #5): a trajectory with more
// than turns.MaxReasoningSteps*4 derived steps never fails the feed and
// never grows the accumulator unboundedly — the feed clamps at the
// projector's per-observation acceptance bound, the row reports Partial
// + Dropped honestly, and the pass keeps advancing (later activity and
// the terminal seal still apply; the cursor never wedges).
func TestMaterialize_ReasoningOverflowClampsAndContinues(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)

	quad := testQuad(h.id, "run-of")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-of", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-of"))
	steps := turns.MaxReasoningSteps*4 + 20 // > the projector's 128-step feed-acceptance bound
	for range steps {
		h.src.publish(t, decisionEv(h.id, quad.RunID, "CallTool"))
	}
	// Events AFTER the overflow still apply (the pass never wedges).
	h.src.publish(t, toolInvokedEv(h.id, quad.RunID, "post.tool", time.Now()))
	h.src.publish(t, toolCompletedEv(h.id, quad.RunID, "post.tool", 5))
	last := h.src.publish(t, failedEv(h.id, "task-of", "timeout"))

	res, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("materialize: %v (an over-bound reasoning feed must never hard-fail the pass)", err)
	}
	if res.Cursor != last.Sequence {
		t.Errorf("cursor = %d, want %d", res.Cursor, last.Sequence)
	}
	row := mustGetRow(t, h, "task-of")
	// The projector retained the FIRST turns.MaxReasoningSteps of the
	// clamped feed and reported the tail drop honestly as Partial.
	if len(row.Reasoning.Steps) != turns.MaxReasoningSteps {
		t.Fatalf("reasoning steps = %d, want %d retained", len(row.Reasoning.Steps), turns.MaxReasoningSteps)
	}
	if row.Reasoning.Complete != turns.CompletenessPartial ||
		row.Reasoning.Dropped != maxReasoningFeed-turns.MaxReasoningSteps {
		t.Errorf("reasoning = complete %q dropped %d, want partial with %d dropped",
			row.Reasoning.Complete, row.Reasoning.Dropped, maxReasoningFeed-turns.MaxReasoningSteps)
	}
	// The post-overflow activity still applied and the turn converged.
	if len(row.Activity.Rows) != 1 || row.Activity.Rows[0].Tool != "post.tool" ||
		row.Activity.Rows[0].Status != turns.ActivitySucceeded {
		t.Errorf("activity = %+v, want the post-overflow dispatch succeeded", row.Activity.Rows)
	}
	if !row.Sealed || row.Status != turns.StatusFailed {
		t.Fatalf("terminal = status %q sealed %v (cursor progress continued)", row.Status, row.Sealed)
	}
}

// TestMaterialize_InputDispositionDedupAndEdgeSkips pins the input
// attachment dedup (a replay can never double-list) and the edge skips:
// a duplicate disposition is a no-op, an empty artifact id is omitted,
// and an app discovery missing server/uri is omitted (never a broken
// ref).
func TestMaterialize_InputDispositionDedupAndEdgeSkips(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)
	quad := testQuad(h.id, "run-d")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-d", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-d"))
	h.src.publish(t, inputDispositionEv(h.id, "task-d", "art-1"))
	h.src.publish(t, inputDispositionEv(h.id, "task-d", "art-1")) // dedup
	h.src.publish(t, inputDispositionEv(h.id, "task-d", ""))      // empty id → omitted
	h.src.publish(t, appAvailableEv(h.id, quad.RunID, "agent-a", "srv-1", "ui://one"))
	h.src.publish(t, appAvailableEv(h.id, quad.RunID, "agent-a", "", "")) // missing server/uri → omitted
	h.src.publish(t, failedEv(h.id, "task-d", "timeout"))
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	row := mustGetRow(t, h, "task-d")
	if len(row.Inputs) != 1 || row.Inputs[0].ID != "art-1" {
		t.Errorf("inputs = %+v, want exactly art-1 (deduplicated)", row.Inputs)
	}
	if len(row.Apps) != 1 || row.Apps[0].ResourceURI != "ui://one" {
		t.Errorf("apps = %+v, want exactly ui://one (broken ref omitted)", row.Apps)
	}
}

// TestMaterialize_TaskPausedAndUnclassifiablePauseAreOmitted pins the
// explicit relationship rule on the pause surface: task.paused (not
// the live pause path) and a pause.requested whose reason does not
// classify into the closed producer-class set are omitted honestly —
// never a fabricated paused row.
func TestMaterialize_TaskPausedAndUnclassifiablePauseAreOmitted(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)
	quad := testQuad(h.id, "run-o")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-o", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-o"))
	// task.paused is not the live pause path (it carries no pause
	// class and cannot form a valid episode) — omitted.
	h.src.publish(t, events.Event{
		Type:     tasks.EventTypeTaskPaused,
		Identity: testQuad(h.id, ""),
		Payload:  tasks.TaskPausedPayload{TaskID: tasks.TaskID("task-o")},
	})
	// A pause.requested whose reason does not classify into the closed
	// producer-class set is omitted — never a fabricated class.
	h.src.publish(t, pauseRequestedEv(h.id, quad.RunID, "some-noncanonical-reason"))
	h.src.publish(t, failedEv(h.id, "task-o", "timeout"))
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	row := mustGetRow(t, h, "task-o")
	if row.Status != turns.StatusFailed || !row.Sealed {
		t.Fatalf("row = status %q sealed %v (task.paused / unclassifiable pause must not alter the lifecycle)", row.Status, row.Sealed)
	}
}

// TestMaterialize_TaskIDWithReservedSeparatorIsSkipped pins the
// routing-side cursor-separator guard: a task id containing the
// reserved '|' separator would make the row's own page cursor
// undecodable — the materializer skips it loudly at routing time (the
// projector refuses it at append too).
func TestMaterialize_TaskIDWithReservedSeparatorIsSkipped(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)
	quad := testQuad(h.id, "run-sep")
	bad := "task|sep"
	h.src.publish(t, spawnEv(h.id, quad.RunID, bad, tasks.KindForeground, ""))
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if _, err := h.proj.Get(context.Background(), h.id, turns.TurnID(bad)); !errors.Is(err, turns.ErrTurnNotFound) {
		t.Fatalf("separator task = %v, want ErrTurnNotFound", err)
	}
}

// TestMaterialize_RootSpawnWithoutRunIDUsesRuntimeTaskRunBinding pins the
// stock Runtime contract: task.spawned deliberately carries no envelope
// RunID because the task ID canonically doubles as the RunLoop's run ID.
// Only that exact task-bound run routes; an unrelated run remains
// unavailable and is skipped.
func TestMaterialize_RootSpawnWithoutRunIDUsesRuntimeTaskRunBinding(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)

	// The spawn carries NO run id (an empty envelope RunID).
	h.src.publish(t, spawnEv(h.id, "", "task-nr", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-nr"))
	h.src.publish(t, toolInvokedEv(h.id, "run-unknown", "ghost.tool", time.Now())) // run-scoped: unroutable
	h.src.publish(t, failedEv(h.id, "task-nr", "timeout"))
	res, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	row := mustGetRow(t, h, "task-nr")
	if row.RunID != "task-nr" || row.Status != turns.StatusFailed || !row.Sealed {
		t.Errorf("row = run %q status %q sealed %v", row.RunID, row.Status, row.Sealed)
	}
	if len(row.Activity.Rows) != 0 {
		t.Errorf("activity = %+v, want empty (unroutable run)", row.Activity.Rows)
	}
	if res.EventsSkipped == 0 {
		t.Error("unroutable run event not reported as skipped")
	}
}

// TestMaterialize_RunRoutingCollisionFailsClosed pins that the canonical
// TaskID-as-RunID derivation cannot steal an explicit run binding (or be
// stolen by one). The conflicting spawn leaves both routing indexes, durable
// rows, and the checkpoint at the last non-conflicting event.
func TestMaterialize_RunRoutingCollisionFailsClosed(t *testing.T) {
	tests := []struct {
		name    string
		first   func(identity.Identity) events.Event
		second  func(identity.Identity) events.Event
		row     string
		wantErr error
	}{
		{
			name: "derived then explicit root",
			first: func(id identity.Identity) events.Event {
				return spawnEv(id, "", "task-a", tasks.KindForeground, "")
			},
			second: func(id identity.Identity) events.Event {
				return spawnEv(id, "task-a", "task-b", tasks.KindForeground, "")
			},
			row:     "task-a",
			wantErr: ErrRunRoutingConflict,
		},
		{
			name: "explicit root then derived",
			first: func(id identity.Identity) events.Event {
				return spawnEv(id, "task-a", "task-b", tasks.KindForeground, "")
			},
			second: func(id identity.Identity) events.Event {
				return spawnEv(id, "", "task-a", tasks.KindForeground, "")
			},
			row:     "task-b",
			wantErr: ErrRunRoutingConflict,
		},
		{
			name: "root then child",
			first: func(id identity.Identity) events.Event {
				return spawnEv(id, "", "task-root", tasks.KindForeground, "")
			},
			second: func(id identity.Identity) events.Event {
				return spawnEv(id, "task-root", "task-child", tasks.KindBackground, "task-root")
			},
			row:     "task-root",
			wantErr: ErrRunRoutingConflict,
		},
		{
			name: "duplicate task changes parent",
			first: func(id identity.Identity) events.Event {
				return spawnEv(id, "run-a", "task-a", tasks.KindForeground, "")
			},
			second: func(id identity.Identity) events.Event {
				return spawnEv(id, "run-a", "task-a", tasks.KindBackground, "task-other")
			},
			row:     "task-a",
			wantErr: ErrTaskRoutingConflict,
		},
		{
			name: "duplicate root changes explicit run",
			first: func(id identity.Identity) events.Event {
				return spawnEv(id, "run-a", "task-a", tasks.KindForeground, "")
			},
			second: func(id identity.Identity) events.Event {
				return spawnEv(id, "run-b", "task-a", tasks.KindForeground, "")
			},
			row:     "task-a",
			wantErr: ErrRunRoutingConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, "")
			defer h.closeStore()
			first := h.src.publish(t, tt.first(h.id))
			h.src.publish(t, tt.second(h.id))

			m := h.newMaterializer(t)
			if _, err := m.Materialize(context.Background()); !errors.Is(err, tt.wantErr) {
				t.Fatalf("collision = %v, want %v", err, tt.wantErr)
			}
			row := mustGetRow(t, h, tt.row)
			if row.LastAppliedEventSeq != first.Sequence {
				t.Errorf("winning row sequence = %d, want %d", row.LastAppliedEventSeq, first.Sequence)
			}
			page, err := h.proj.List(context.Background(), h.id, turns.ListOptions{Limit: 20})
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if len(page.Rows) != 1 {
				t.Fatalf("rows = %d, want only the non-conflicting root", len(page.Rows))
			}
			cp, err := h.proj.Checkpoint(context.Background(), h.id)
			if err != nil {
				t.Fatalf("checkpoint: %v", err)
			}
			if cp != first.Sequence {
				t.Errorf("checkpoint = %d, want %d", cp, first.Sequence)
			}
		})
	}
}

func TestMaterialize_ChildSpawnWithoutRunIDDoesNotInventRoute(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	h.src.publish(t, spawnEv(h.id, "run-root", "task-root", tasks.KindForeground, ""))
	h.src.publish(t, spawnEv(h.id, "", "task-child", tasks.KindBackground, "task-root"))
	h.src.publish(t, toolInvokedEv(h.id, "task-child", "child.tool", time.Unix(1_700_000_300, 0)))
	h.src.publish(t, failedEv(h.id, "task-root", "timeout"))

	m := h.newMaterializer(t)
	res, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	row := mustGetRow(t, h, "task-root")
	if len(row.Activity.Rows) != 0 {
		t.Fatalf("child activity routed through invented run: %+v", row.Activity)
	}
	if res.EventsSkipped == 0 {
		t.Fatal("empty-run child activity was not reported as skipped")
	}
}

func TestMaterialize_RepeatedChildCannotChangeExplicitRun(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	root := h.src.publish(t, spawnEv(h.id, "run-root", "task-root", tasks.KindForeground, ""))
	h.src.publish(t, spawnEv(h.id, "run-child-a", "task-child", tasks.KindBackground, "task-root"))
	h.src.publish(t, spawnEv(h.id, "run-child-b", "task-child", tasks.KindBackground, "task-root"))

	m := h.newMaterializer(t)
	if _, err := m.Materialize(context.Background()); !errors.Is(err, ErrRunRoutingConflict) {
		t.Fatalf("child run rebind = %v, want ErrRunRoutingConflict", err)
	}
	row := mustGetRow(t, h, "task-root")
	if row.RunID != "run-root" || row.LastAppliedEventSeq != root.Sequence {
		t.Fatalf("root row changed by child rebind: %+v", row)
	}
	cp, err := h.proj.Checkpoint(context.Background(), h.id)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if cp != root.Sequence {
		t.Fatalf("checkpoint = %d, want last durably applied root spawn %d (child routing is in-memory only; conflicting child did not advance it past the root)", cp, root.Sequence)
	}
}
