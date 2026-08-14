package materializer

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/tasks"
)

func usageValue(t *testing.T, m turns.UsageMeasure) int64 {
	t.Helper()
	if m.Value == nil {
		t.Fatalf("usage measure value is nil (state %q)", m.State)
	}
	return *m.Value
}

// ---------------------------------------------------------------------------
// two-read-ready row
// ---------------------------------------------------------------------------

// TestMaterialize_TwoReadReadyRow pins the full consumer-safe row after
// one canonical lifecycle: distinct TaskID/RunID, honest unavailable
// query/answer/agent, input attachment metadata, derived reasoning,
// structured activity + exact totals, per-measure usage availability,
// ordered App refs, durable pause lifecycle, and the sealed terminal
// cause — all from canonical events only.
func TestMaterialize_TwoReadReadyRow(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)

	evs := h.lifecycle(t, testQuad(h.id, "run-1"), "task-1")
	res, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if res.EventsApplied == 0 {
		t.Fatal("no events applied")
	}
	if res.Cursor != evs[len(evs)-1].Sequence {
		t.Errorf("cursor = %d, want %d", res.Cursor, evs[len(evs)-1].Sequence)
	}

	row := mustGetRow(t, h, "task-1")

	// Distinct task/run identity: the row key, the authoritative task
	// id, and the actual runtime run id stay distinct and honest.
	if string(row.TurnID) != "task-1" || row.TaskID != "task-1" || row.RunID != "run-1" {
		t.Errorf("identity: turn=%q task=%q run=%q", row.TurnID, row.TaskID, row.RunID)
	}
	// Sealed terminal cause: failed + the closed content-free error
	// class derived from the task error code; the finish reason stays
	// empty (not reported for a failure).
	if row.Status != turns.StatusFailed || !row.Sealed || row.ErrorClass != turns.ErrorClassTimeout {
		t.Errorf("terminal: status=%q sealed=%v error_class=%q", row.Status, row.Sealed, row.ErrorClass)
	}
	// Honest unavailable components: no canonical event carries the
	// query, the answer, or an agent binding — never fabricated.
	if row.Query.Complete != turns.CompletenessUnavailable || row.Query.Text != "" {
		t.Errorf("query: complete=%q text=%q", row.Query.Complete, row.Query.Text)
	}
	if row.Answer.State != turns.AnswerStateUnavailable || row.Answer.Complete != turns.CompletenessUnavailable {
		t.Errorf("answer: state=%q complete=%q", row.Answer.State, row.Answer.Complete)
	}
	if row.Agent.Complete != turns.CompletenessUnavailable || row.Agent.BindingSource != turns.AgentBindingUnknown {
		t.Errorf("agent: complete=%q binding=%q", row.Agent.Complete, row.Agent.BindingSource)
	}
	// Input attachment metadata (never bytes) with reference
	// availability; filename/size/digest are not carried by the event.
	if len(row.Inputs) != 1 {
		t.Fatalf("inputs = %d, want 1", len(row.Inputs))
	}
	in := row.Inputs[0]
	if in.ID != "art-1" || in.MimeType != "image/png" || in.Disposition != "inline" ||
		in.Availability != turns.CompletenessComplete {
		t.Errorf("input: id=%q mime=%q disposition=%q availability=%q", in.ID, in.MimeType, in.Disposition, in.Availability)
	}
	// Derived reasoning: exactly the planner decision steps (never raw
	// provider thinking — the payload carried a reasoning trace that
	// must not reach the row).
	if len(row.Reasoning.Steps) != 1 {
		t.Fatalf("reasoning steps = %d, want 1", len(row.Reasoning.Steps))
	}
	if row.Reasoning.Steps[0].Kind != turns.ReasoningKindToolCall || row.Reasoning.Steps[0].Index != 0 {
		t.Errorf("reasoning step = %+v", row.Reasoning.Steps[0])
	}
	// Structured activity: one succeeded dispatch with the derived
	// terminal class and the exact turn-level totals.
	if len(row.Activity.Rows) != 1 {
		t.Fatalf("activity rows = %d, want 1", len(row.Activity.Rows))
	}
	act := row.Activity.Rows[0]
	if act.Tool != "clock.now" || act.Status != turns.ActivitySucceeded ||
		act.TerminalClass != turns.ActivityTerminalSucceeded ||
		act.Position != 0 || act.AttemptCount != 1 || act.Duration != 5*time.Millisecond {
		t.Errorf("activity row = %+v", act)
	}
	if row.Activity.Totals.Succeeded != 1 || row.Activity.Totals.Invoked != 0 {
		t.Errorf("activity totals = %+v", row.Activity.Totals)
	}
	// Per-measure usage availability: tokens exact, cost honestly
	// estimated (the float64 USD source converted to integer
	// micro-dollars), model last-reported.
	if row.Usage.PromptTokens.State != turns.UsageExact || usageValue(t, row.Usage.PromptTokens) != 100 {
		t.Errorf("prompt tokens = %+v", row.Usage.PromptTokens)
	}
	if row.Usage.CompletionTokens.State != turns.UsageExact || usageValue(t, row.Usage.CompletionTokens) != 50 {
		t.Errorf("completion tokens = %+v", row.Usage.CompletionTokens)
	}
	if row.Usage.TotalTokens.State != turns.UsageExact || usageValue(t, row.Usage.TotalTokens) != 150 {
		t.Errorf("total tokens = %+v", row.Usage.TotalTokens)
	}
	if row.Usage.CostMicroUSD.State != turns.UsageEstimated || usageValue(t, row.Usage.CostMicroUSD) != 250_000 {
		t.Errorf("cost = %+v (want estimated 250000 micro-USD)", row.Usage.CostMicroUSD)
	}
	if row.Usage.Model != "model-x" {
		t.Errorf("usage model = %q", row.Usage.Model)
	}
	// Ordered App ref: server + resource URI with availability; the
	// payload's Binding callback capability is structurally absent.
	if len(row.Apps) != 1 {
		t.Fatalf("apps = %d, want 1", len(row.Apps))
	}
	app := row.Apps[0]
	if app.ServerID != "server-1" || app.ResourceURI != "ui://doc" || app.Availability != turns.AppAvailable {
		t.Errorf("app ref = %+v", app)
	}
	// Durable pause: resumed cleared the episode; the opaque token
	// never reached the row (the Pause component is token-free).
	if row.Pause.Availability != turns.CompletenessUnavailable || row.Pause.Class != "" || row.Pause.Lifecycle != "" {
		t.Errorf("pause = %+v (want cleared)", row.Pause)
	}
	// Last-applied sequence anchors the row to the durable log.
	if row.LastAppliedEventSeq != evs[len(evs)-1].Sequence {
		t.Errorf("last applied seq = %d, want %d", row.LastAppliedEventSeq, evs[len(evs)-1].Sequence)
	}

	// Two-read-ready: the row pages through the projection read surface
	// with exactly one page.
	page, err := h.proj.List(context.Background(), h.id, turns.ListOptions{Limit: 20})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Rows) != 1 || page.Rows[0].TurnID != "task-1" || page.HasMore {
		t.Errorf("page = %d rows has_more=%v", len(page.Rows), page.HasMore)
	}
}

// ---------------------------------------------------------------------------
// running → paused → sealed reconciliation
// ---------------------------------------------------------------------------

// TestMaterialize_RunningPausedSealedReconciliation pins the mutable
// lifecycle reconciliation driven by canonical events: pending →
// running → paused (with a durable token-free episode) → running
// (episode cleared on resume) → sealed terminal.
func TestMaterialize_RunningPausedSealedReconciliation(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)

	quad := testQuad(h.id, "run-r")

	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-r", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-r"))
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize (running): %v", err)
	}
	row := mustGetRow(t, h, "task-r")
	if row.Status != turns.StatusRunning || row.Sealed {
		t.Fatalf("after start: status=%q sealed=%v", row.Status, row.Sealed)
	}

	// Pause requested → paused with an ACTIVE, available, token-free
	// episode whose class derives from the canonical planner reason.
	h.src.publish(t, pauseRequestedEv(h.id, quad.RunID, string(planner.PauseApprovalRequired)))
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize (paused): %v", err)
	}
	row = mustGetRow(t, h, "task-r")
	if row.Status != turns.StatusPaused || row.Sealed {
		t.Fatalf("after pause request: status=%q sealed=%v", row.Status, row.Sealed)
	}
	if row.Pause.Availability != turns.CompletenessComplete ||
		row.Pause.Lifecycle != turns.PauseLifecycleActive ||
		row.Pause.Class != turns.PauseClassHitlApproval ||
		row.Pause.Reason != string(planner.PauseApprovalRequired) {
		t.Errorf("pause episode = %+v", row.Pause)
	}

	// Pause resumed → running with the episode explicitly cleared.
	h.src.publish(t, pauseResumedEv(h.id, quad.RunID))
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize (resumed): %v", err)
	}
	row = mustGetRow(t, h, "task-r")
	if row.Status != turns.StatusRunning {
		t.Fatalf("after resume: status=%q", row.Status)
	}
	if row.Pause.Availability != turns.CompletenessUnavailable {
		t.Errorf("pause after resume = %+v (want cleared)", row.Pause)
	}

	// Terminal failure → SEALED failed; every later mutation is refused
	// by the projector (the row converged).
	h.src.publish(t, failedEv(h.id, "task-r", "permanent"))
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize (sealed): %v", err)
	}
	row = mustGetRow(t, h, "task-r")
	if !row.Sealed || row.Status != turns.StatusFailed || row.ErrorClass != turns.ErrorClassPermanent {
		t.Fatalf("sealed row: status=%q sealed=%v error_class=%q", row.Status, row.Sealed, row.ErrorClass)
	}

	// A late event after sealing is skipped, never applied, never an
	// error.
	h.src.publish(t, toolInvokedEv(h.id, quad.RunID, "late.tool", time.Now()))
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize (late event): %v", err)
	}
	after := mustGetRow(t, h, "task-r")
	if after.Version != row.Version || len(after.Activity.Rows) != 0 {
		t.Errorf("late event mutated the sealed row: version %d→%d activity=%d", row.Version, after.Version, len(after.Activity.Rows))
	}
}

// TestMaterialize_CompleteSealDeferredWhenAnswerSourceMissing pins the
// honest complete-seal contract: task.completed without an
// answer-carrying event defers the seal (the row stays MUTABLE —
// running — and the pass reports the pending count); the row is never
// fabricated as complete and the retry is idempotent.
func TestMaterialize_CompleteSealDeferredWhenAnswerSourceMissing(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)

	quad := testQuad(h.id, "run-c")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-c", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-c"))
	h.src.publish(t, completedEv(h.id, "task-c"))

	res, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if res.PendingComplete != 1 {
		t.Errorf("pending complete = %d, want 1", res.PendingComplete)
	}
	row := mustGetRow(t, h, "task-c")
	if row.Sealed || row.Status != turns.StatusRunning {
		t.Fatalf("deferred row: status=%q sealed=%v (must stay mutable until the answer source converges)", row.Status, row.Sealed)
	}
	if row.Answer.State != turns.AnswerStateUnavailable {
		t.Errorf("answer = %q (must be honest unavailable, never fabricated)", row.Answer.State)
	}

	// A second pass without new events: still deferred, still
	// idempotent (no version churn).
	before := row.Version
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("re-materialize: %v", err)
	}
	row = mustGetRow(t, h, "task-c")
	if row.Version != before {
		t.Errorf("re-materialize bumped the version %d→%d (not idempotent)", before, row.Version)
	}
}

// ---------------------------------------------------------------------------
// ordered Apps
// ---------------------------------------------------------------------------

// TestMaterialize_OrderedApps pins the ORDERED App reference collection:
// first declaration fixes position, a repeat of the identity
// (effective agent id, server id, resource uri) replaces in place with
// the latest correlation metadata, and a new identity appends at the
// end.
func TestMaterialize_OrderedApps(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)

	quad := testQuad(h.id, "run-a")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-a", tasks.KindForeground, ""))
	h.src.publish(t, appAvailableEv(h.id, quad.RunID, "agent-a", "srv-1", "ui://one"))
	h.src.publish(t, appAvailableEv(h.id, quad.RunID, "agent-a", "srv-1", "ui://two"))
	h.src.publish(t, appAvailableEv(h.id, quad.RunID, "agent-b", "srv-2", "ui://three"))

	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	row := mustGetRow(t, h, "task-a")
	if len(row.Apps) != 3 {
		t.Fatalf("apps = %d, want 3", len(row.Apps))
	}
	got := []string{row.Apps[0].ResourceURI, row.Apps[1].ResourceURI, row.Apps[2].ResourceURI}
	want := []string{"ui://one", "ui://two", "ui://three"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("app order = %v, want %v", got, want)
	}
	if row.Apps[0].EffectiveAgentID != "agent-a" || row.Apps[2].EffectiveAgentID != "agent-b" {
		t.Errorf("app agents = %q %q", row.Apps[0].EffectiveAgentID, row.Apps[2].EffectiveAgentID)
	}

	// A repeat of the FIRST identity replaces in place (position 0 is
	// fixed by the first declaration) with the latest correlation
	// metadata; a repeat of a LATER identity replaces in place too.
	h.src.publish(t, appAvailableEv(h.id, quad.RunID, "agent-a", "srv-1", "ui://one"))
	h.src.publish(t, appAvailableEv(h.id, quad.RunID, "agent-b", "srv-2", "ui://three"))
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize (repeats): %v", err)
	}
	row = mustGetRow(t, h, "task-a")
	if len(row.Apps) != 3 {
		t.Fatalf("apps after repeats = %d, want 3 (repeats replace, never append)", len(row.Apps))
	}
	got = []string{row.Apps[0].ResourceURI, row.Apps[1].ResourceURI, row.Apps[2].ResourceURI}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("app order after repeats = %v, want %v", got, want)
	}
}

// ---------------------------------------------------------------------------
// child activity
// ---------------------------------------------------------------------------

// TestMaterialize_ChildActivityFoldsIntoParent pins the explicit
// relationship rule: a child/background task is NEVER a turn (never an
// invented user message), and its run-scoped events (tool dispatches)
// fold into the parent turn's Activity via the run → task → parent
// walk. The child's own lifecycle events do not seal or corrupt the
// parent.
func TestMaterialize_ChildActivityFoldsIntoParent(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)

	parentQuad := testQuad(h.id, "run-parent")
	childQuad := testQuad(h.id, "run-child")

	h.src.publish(t, spawnEv(h.id, parentQuad.RunID, "task-parent", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-parent"))
	h.src.publish(t, toolInvokedEv(h.id, parentQuad.RunID, "parent.tool", time.Now()))
	// Child task: spawned under the parent, drives its own run.
	h.src.publish(t, spawnEv(h.id, childQuad.RunID, "task-child", tasks.KindBackground, "task-parent"))
	h.src.publish(t, startedEv(h.id, "task-child"))
	// The child's tool dispatch folds into the PARENT turn's activity.
	h.src.publish(t, toolInvokedEv(h.id, childQuad.RunID, "child.tool", time.Now()))
	h.src.publish(t, toolCompletedEv(h.id, childQuad.RunID, "child.tool", 3))
	// The child's own completion folds nowhere (it is not a turn).
	h.src.publish(t, completedEv(h.id, "task-child"))
	// The parent fails terminally.
	h.src.publish(t, failedEv(h.id, "task-parent", "timeout"))

	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	// Exactly ONE turn: the parent. The child never became a row.
	page, err := h.proj.List(context.Background(), h.id, turns.ListOptions{Limit: 20})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Rows) != 1 || string(page.Rows[0].TurnID) != "task-parent" {
		t.Fatalf("rows = %d (turn %v), want exactly the parent turn", len(page.Rows), page.Rows[0].TurnID)
	}

	// The parent turn folds BOTH its own dispatch and the child's.
	row := mustGetRow(t, h, "task-parent")
	if row.Status != turns.StatusFailed || !row.Sealed || row.ErrorClass != turns.ErrorClassTimeout {
		t.Fatalf("parent: status=%q sealed=%v error_class=%q (the parent's OWN terminal event sealed it failed)", row.Status, row.Sealed, row.ErrorClass)
	}
	if len(row.Activity.Rows) != 2 {
		t.Fatalf("parent activity = %d, want 2 (parent + folded child dispatch)", len(row.Activity.Rows))
	}
	if row.Activity.Rows[0].Tool != "parent.tool" || row.Activity.Rows[1].Tool != "child.tool" {
		t.Errorf("parent activity tools = %q, %q", row.Activity.Rows[0].Tool, row.Activity.Rows[1].Tool)
	}
	if row.Activity.Rows[1].Status != turns.ActivitySucceeded {
		t.Errorf("folded child dispatch status = %q", row.Activity.Rows[1].Status)
	}
	// The parent's own dispatch stayed in flight (invoked — the parent
	// failed mid-dispatch) and the folded child dispatch succeeded: the
	// exact turn-level totals survive.
	if row.Activity.Totals.Invoked != 1 || row.Activity.Totals.Succeeded != 1 {
		t.Errorf("totals = %+v, want invoked=1 (parent mid-flight) succeeded=1 (folded child)", row.Activity.Totals)
	}
	// The child's completion did not seal or complete the parent.
	if row.Status != turns.StatusFailed || row.ErrorClass != turns.ErrorClassTimeout {
		t.Errorf("parent terminal = %q %q (child lifecycle must not touch it)", row.Status, row.ErrorClass)
	}
}

// ---------------------------------------------------------------------------
// response-loss replay / restart catch-up
// ---------------------------------------------------------------------------

// TestMaterialize_ResponseLossReplayConverges pins the at-least-once
// idempotency contract: a second materializer instance replaying the
// same retained events (a response-loss replay or a restarted replica)
// converges to byte-identical rows with NO version churn, and then
// catches up past the checkpoint when new events arrive.
func TestMaterialize_ResponseLossReplayConverges(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m1 := h.newMaterializer(t)

	h.lifecycle(t, testQuad(h.id, "run-1"), "task-1")
	if _, err := m1.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize (first): %v", err)
	}
	first := mustGetRow(t, h, "task-1")

	// Second materializer over the SAME store and source: its cursor
	// starts at zero and replays the whole retained log.
	m2 := h.newMaterializer(t)
	res, err := m2.Materialize(context.Background())
	if err != nil {
		t.Fatalf("materialize (replay): %v", err)
	}
	if res.Cursor != m1.Cursor() {
		t.Errorf("replay cursor = %d, want %d", res.Cursor, m1.Cursor())
	}
	second := mustGetRow(t, h, "task-1")
	if !reflect.DeepEqual(first, second) {
		t.Errorf("replay changed the row:\nbefore: %+v\nafter:  %+v", first, second)
	}
	if second.Version != first.Version {
		t.Errorf("replay bumped the version %d→%d (not idempotent)", first.Version, second.Version)
	}

	// The replayed materializer catches up past the checkpoint with NEW
	// events: a second root foreground lifecycle materializes into a
	// second row. (The first turn is sealed terminal — post-terminal
	// events for it are correctly skipped, never applied.)
	h.src.publish(t, spawnEv(h.id, "run-2", "task-2", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-2"))
	h.src.publish(t, failedEv(h.id, "task-2", "timeout"))
	if _, err := m2.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize (post-replay catch-up): %v", err)
	}
	secondTurn := mustGetRow(t, h, "task-2")
	if !secondTurn.Sealed || secondTurn.Status != turns.StatusFailed || secondTurn.RunID != "run-2" {
		t.Errorf("post-replay turn = %+v", secondTurn)
	}
	// The first row is untouched by the catch-up.
	firstAfter := mustGetRow(t, h, "task-1")
	if !reflect.DeepEqual(first, firstAfter) {
		t.Error("post-replay catch-up mutated the already-converged first row")
	}
}

// TestMaterialize_RestartDurableStore pins the durable-driver restart
// contract: a file-backed store retains rows AND the per-session
// checkpoint across a store reopen; a fresh materializer re-pages the
// retained log from zero, no-ops everything at or below the checkpoint
// (no version churn), rebuilds its in-memory accumulators
// deterministically, and continues past the checkpoint.
func TestMaterialize_RestartDurableStore(t *testing.T) {
	dsn := t.TempDir() + "/turns.sqlite"
	h := newHarness(t, dsn)
	m1 := h.newMaterializer(t)

	evs := h.lifecycle(t, testQuad(h.id, "run-1"), "task-1")
	if _, err := m1.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize (pre-restart): %v", err)
	}
	pre := mustGetRow(t, h, "task-1")
	preCP, err := h.proj.Checkpoint(context.Background(), h.id)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if preCP != evs[len(evs)-1].Sequence {
		t.Errorf("checkpoint = %d, want %d", preCP, evs[len(evs)-1].Sequence)
	}

	// "Restart": close the store/projector and reopen the same file.
	// The fake source (the durable-log analog) keeps the events.
	h.closeStore()
	store2, err := sqlite.New(sqlite.Config{DSN: dsn})
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	proj2, err := turns.New(store2)
	if err != nil {
		t.Fatalf("reopen projector: %v", err)
	}
	defer func() { _ = proj2.Close(context.Background()) }()
	h2 := &harness{id: h.id, store: store2, proj: proj2, src: h.src}

	m2, err := New(h2.src, h2.proj)
	if err != nil {
		t.Fatalf("new post-restart materializer: %v", err)
	}
	if _, err := m2.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize (post-restart): %v", err)
	}
	post := mustGetRow(t, h2, "task-1")
	if !reflect.DeepEqual(pre, post) {
		t.Errorf("restart changed the row:\nbefore: %+v\nafter:  %+v", pre, post)
	}
	if post.Version != pre.Version {
		t.Errorf("restart bumped the version %d→%d", pre.Version, post.Version)
	}
	postCP, err := h2.proj.Checkpoint(context.Background(), h.id)
	if err != nil {
		t.Fatalf("post-restart checkpoint: %v", err)
	}
	if postCP != preCP {
		t.Errorf("post-restart checkpoint = %d, want %d (never regressed)", postCP, preCP)
	}

	// New events after the restart apply and converge: a second
	// lifecycle materializes into a second row past the durable
	// checkpoint.
	h2.src.publish(t, spawnEv(h.id, "run-2", "task-2", tasks.KindForeground, ""))
	h2.src.publish(t, startedEv(h.id, "task-2"))
	h2.src.publish(t, failedEv(h.id, "task-2", "timeout"))
	if _, err := m2.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize (post-restart catch-up): %v", err)
	}
	converged := mustGetRow(t, h2, "task-2")
	if !converged.Sealed || converged.Status != turns.StatusFailed || converged.RunID != "run-2" {
		t.Errorf("post-restart convergence: %+v", converged)
	}
	preStill := mustGetRow(t, h2, "task-1")
	if !reflect.DeepEqual(pre, preStill) {
		t.Error("post-restart catch-up mutated the pre-restart row")
	}
}

// ---------------------------------------------------------------------------
// erasure
// ---------------------------------------------------------------------------

// TestMaterialize_ErasureFencesAndSkipsPermanently pins the erasure
// contract: once the session is erased (the store-local durable fence
// is set), every further event for the session is skipped permanently
// — no resurrection, no error, no checkpoint advance — whether the
// source still serves the session's retained events or excludes them
// like the durable driver does.
func TestMaterialize_ErasureFencesAndSkipsPermanently(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)

	h.lifecycle(t, testQuad(h.id, "run-1"), "task-1")
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if _, err := h.proj.Get(context.Background(), h.id, "task-1"); err != nil {
		t.Fatalf("pre-erasure get: %v", err)
	}

	// Erase the session: the fence is set and the rows are deleted.
	if _, err := h.proj.Erase(context.Background(), h.id); err != nil {
		t.Fatalf("erase: %v", err)
	}
	if _, err := h.proj.Get(context.Background(), h.id, "task-1"); !errors.Is(err, turns.ErrTurnNotFound) {
		t.Fatalf("post-erasure get = %v, want ErrTurnNotFound", err)
	}

	// The source still serves the session's retained events (a fence
	// landing after the events were retained). The materializer must
	// skip them permanently — the store-local fence refuses every write
	// and the materializer marks the session fenced.
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize (post-erasure): %v", err)
	}
	if _, err := h.proj.Get(context.Background(), h.id, "task-1"); !errors.Is(err, turns.ErrTurnNotFound) {
		t.Fatalf("post-materialize get = %v, want ErrTurnNotFound (no resurrection)", err)
	}

	// New events after the erasure are skipped permanently (the fence
	// refused the append and the materializer marked the session
	// fenced) — never a hard pass failure, never a resurrected row.
	h.src.publish(t, spawnEv(h.id, "run-2", "task-2", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-2"))
	res, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("materialize (post-erasure new events): %v", err)
	}
	if _, err := h.proj.Get(context.Background(), h.id, "task-2"); !errors.Is(err, turns.ErrTurnNotFound) {
		t.Fatalf("post-erasure new turn = %v, want ErrTurnNotFound", err)
	}
	if res.FencedSessions == 0 && res.EventsSkipped == 0 {
		t.Errorf("post-erasure new-events pass reported no fence/skip: %+v", res)
	}
	// The checkpoint never advanced for the fenced session (the store
	// refuses it): a restart still cannot resurrect it.
	cp, err := h.proj.Checkpoint(context.Background(), h.id)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if cp != 0 {
		t.Errorf("fenced session checkpoint = %d, want 0 (no advance, no resurrection)", cp)
	}
}

// TestMaterialize_SourceLevelFenceExclusion pins the durable-driver
// fence shape: a source that excludes fenced sessions (like the
// durable driver's ProjectionSource) simply never serves their events
// — the materializer has nothing to skip and the projection stays
// empty.
func TestMaterialize_SourceLevelFenceExclusion(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()

	fencedID := identity.Identity{TenantID: "tenant-b", UserID: "user-b", SessionID: "sess-b"}
	quad := testQuad(fencedID, "run-b")
	h.src.skip = func(ev events.Event) bool {
		id := identity.Identity{TenantID: ev.Identity.TenantID, UserID: ev.Identity.UserID, SessionID: ev.Identity.SessionID}
		return id == fencedID
	}
	m := h.newMaterializer(t)

	// The fenced session's events are excluded at the source (never
	// served); a live session's events still materialize.
	h.src.publish(t, spawnEv(fencedID, quad.RunID, "task-fenced", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(fencedID, "task-fenced"))
	h.src.publish(t, spawnEv(h.id, "run-a", "task-a", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-a"))

	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if _, err := h.proj.Get(context.Background(), fencedID, "task-fenced"); !errors.Is(err, turns.ErrTurnNotFound) {
		t.Fatalf("fenced session turn = %v, want ErrTurnNotFound", err)
	}
	if _, err := h.proj.Get(context.Background(), h.id, "task-a"); err != nil {
		t.Fatalf("live session turn: %v", err)
	}
}

// ---------------------------------------------------------------------------
// source honesty
// ---------------------------------------------------------------------------

// TestMaterialize_SourceRetentionGapIsSurfaced pins the never-silently-
// lossy contract: a ring wrap (evicted events) is reported on the
// Result, never treated as a complete stream, and the materializer
// still converges the events it does see.
func TestMaterialize_SourceRetentionGapIsSurfaced(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)

	h.lifecycle(t, testQuad(h.id, "run-1"), "task-1")
	h.src.evicted = true // the ring wrapped; older events may be missing
	res, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if !res.RetentionGap {
		t.Error("retention gap not surfaced on the Result")
	}
	row := mustGetRow(t, h, "task-1")
	if !row.Sealed {
		t.Errorf("row did not converge: sealed=%v", row.Sealed)
	}
}

// TestMaterialize_SourceUnavailableFailsLoud pins the no-silent-empty-
// stream contract: a source without a retained substrate
// (ProjectionUnavailable) makes Materialize fail loud with
// ErrSourceUnavailable.
func TestMaterialize_SourceUnavailableFailsLoud(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)

	h.src.unavailable = true
	h.src.publish(t, spawnEv(h.id, "run-1", "task-1", tasks.KindForeground, ""))
	_, err := m.Materialize(context.Background())
	wantErrIs(t, err, ErrSourceUnavailable)
}

// ---------------------------------------------------------------------------
// durable-shape decoding
// ---------------------------------------------------------------------------

// TestMaterialize_DecodesRedactedMapPayloads pins the production
// durable-source shape: the durable driver rehydrates persisted
// payloads as events.RedactedMap (Go field-name keys), and the
// materializer decodes every canonical family from that generic map —
// not by type assertion.
func TestMaterialize_DecodesRedactedMapPayloads(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)

	quad := testQuad(h.id, "run-1")
	publish := func(ev events.Event) {
		t.Helper()
		ev.Payload = redacted(t, ev.Payload)
		h.src.publish(t, ev)
	}
	publish(spawnEv(h.id, quad.RunID, "task-1", tasks.KindForeground, ""))
	publish(startedEv(h.id, "task-1"))
	publish(decisionEv(h.id, quad.RunID, "CallTool"))
	publish(toolInvokedEv(h.id, quad.RunID, "clock.now", time.Unix(1_700_000_100, 0)))
	publish(toolCompletedEv(h.id, quad.RunID, "clock.now", 5))
	publish(inputDispositionEv(h.id, "task-1", "art-1"))
	publish(appAvailableEv(h.id, quad.RunID, "agent-a", "server-1", "ui://doc"))
	publish(costRecordedEv(h.id, quad.RunID, "model-x", llm.Usage{
		PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, LatencyMS: 120,
	}, 0.25))
	publish(pauseRequestedEv(h.id, quad.RunID, string(planner.PauseApprovalRequired)))
	publish(pauseResumedEv(h.id, quad.RunID))
	publish(failedEv(h.id, "task-1", "timeout"))

	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	row := mustGetRow(t, h, "task-1")
	if row.TaskID != "task-1" || row.RunID != "run-1" {
		t.Errorf("identity: task=%q run=%q", row.TaskID, row.RunID)
	}
	if row.Status != turns.StatusFailed || row.ErrorClass != turns.ErrorClassTimeout {
		t.Errorf("terminal: %q %q", row.Status, row.ErrorClass)
	}
	if len(row.Activity.Rows) != 1 || row.Activity.Rows[0].Tool != "clock.now" ||
		row.Activity.Rows[0].Status != turns.ActivitySucceeded {
		t.Errorf("activity = %+v", row.Activity.Rows)
	}
	if len(row.Inputs) != 1 || row.Inputs[0].ID != "art-1" {
		t.Errorf("inputs = %+v", row.Inputs)
	}
	if len(row.Apps) != 1 || row.Apps[0].ResourceURI != "ui://doc" {
		t.Errorf("apps = %+v", row.Apps)
	}
	if row.Usage.PromptTokens.State != turns.UsageExact || usageValue(t, row.Usage.PromptTokens) != 100 {
		t.Errorf("usage = %+v", row.Usage.PromptTokens)
	}
	if row.Pause.Availability != turns.CompletenessUnavailable {
		t.Errorf("pause = %+v", row.Pause)
	}
}

// ---------------------------------------------------------------------------
// query path / skip behaviour
// ---------------------------------------------------------------------------

// TestMaterialize_NoSynchronousRebuildInQueryPath pins the acceptance
// that chat open (the projection read surface) never touches the event
// source: List/Get issue zero source pages after the materializer has
// caught up.
func TestMaterialize_NoSynchronousRebuildInQueryPath(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)

	h.lifecycle(t, testQuad(h.id, "run-1"), "task-1")
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	}
	pagesBefore := h.src.pageCallCount()

	// The two-read chat-open path: one lifecycle read (here the
	// projection's own List) plus one turn-page read — zero source
	// pages, zero raw history scans.
	if _, err := h.proj.List(context.Background(), h.id, turns.ListOptions{Limit: 20}); err != nil {
		t.Fatalf("list: %v", err)
	}
	if _, err := h.proj.Get(context.Background(), h.id, "task-1"); err != nil {
		t.Fatalf("get: %v", err)
	}
	if pagesAfter := h.src.pageCallCount(); pagesAfter != pagesBefore {
		t.Errorf("query path paged the source: %d → %d pages", pagesBefore, pagesAfter)
	}
}

// TestMaterialize_SkipsUnroutableAndUnknownEvents pins the fail-closed
// routing: unknown event types, incomplete identities, events for
// sessions with no materialized turns, and run events that cannot be
// routed are counted as skipped — never applied, never an error, never
// a fabricated row.
func TestMaterialize_SkipsUnroutableAndUnknownEvents(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)

	// Unknown type (a session event): not turn-relevant.
	h.src.publish(t, events.Event{Type: "session.opened", Identity: testQuad(h.id, "")})
	// Incomplete identity: fail closed, never fabricated.
	h.src.publish(t, events.Event{Type: tasks.EventTypeTaskSpawned, Identity: identity.Quadruple{RunID: "run-x"}})
	// A run event for a run that was never spawned: unroutable.
	h.src.publish(t, toolInvokedEv(h.id, "run-ghost", "ghost.tool", time.Now()))
	// A real lifecycle.
	h.src.publish(t, spawnEv(h.id, "run-1", "task-1", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-1"))
	h.src.publish(t, failedEv(h.id, "task-1", "timeout"))

	res, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if res.EventsSkipped < 3 {
		t.Errorf("skipped = %d, want >= 3 (unknown, incomplete identity, unroutable run)", res.EventsSkipped)
	}
	page, err := h.proj.List(context.Background(), h.id, turns.ListOptions{Limit: 20})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Rows) != 1 || string(page.Rows[0].TurnID) != "task-1" {
		t.Fatalf("rows = %+v, want only task-1", page.Rows)
	}
}

// ---------------------------------------------------------------------------
// pause derivation
// ---------------------------------------------------------------------------

// TestMaterialize_PauseClassDerivation pins the closed class mapping
// from the canonical planner pause reasons and the token-free pause
// component contract.
func TestMaterialize_PauseClassDerivation(t *testing.T) {
	cases := []struct {
		reason string
		class  turns.PauseClass
	}{
		{string(planner.PauseApprovalRequired), turns.PauseClassHitlApproval},
		{string(planner.PauseAwaitInput), turns.PauseClassA2AInputRequired},
		{string(planner.PauseExternalEvent), turns.PauseClassSteering},
		{string(planner.PauseConstraintsConflict), turns.PauseClassSteering},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			h := newHarness(t, "")
			defer h.closeStore()
			m := h.newMaterializer(t)
			quad := testQuad(h.id, "run-p")
			h.src.publish(t, spawnEv(h.id, quad.RunID, "task-p", tasks.KindForeground, ""))
			h.src.publish(t, startedEv(h.id, "task-p"))
			h.src.publish(t, pauseRequestedEv(h.id, quad.RunID, tc.reason))
			if _, err := m.Materialize(context.Background()); err != nil {
				t.Fatalf("materialize: %v", err)
			}
			row := mustGetRow(t, h, "task-p")
			if row.Status != turns.StatusPaused {
				t.Fatalf("status = %q, want paused", row.Status)
			}
			if row.Pause.Class != tc.class {
				t.Errorf("pause class = %q, want %q", row.Pause.Class, tc.class)
			}
			// Token-free and non-actionable: the component carries
			// class/reason/lifecycle/availability only — the payload's
			// opaque Token never reached the row.
			if row.Pause.Reason != tc.reason || row.Pause.Lifecycle != turns.PauseLifecycleActive ||
				row.Pause.Availability != turns.CompletenessComplete {
				t.Errorf("pause episode = %+v", row.Pause)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Run loop
// ---------------------------------------------------------------------------

// TestMaterialize_RunLoopWatchesAndCatchesUp pins the background loop:
// Run catches up immediately, then waits for the source's best-effort
// wake notifications and materializes newly persisted events without
// polling, and exits cleanly on cancellation.
func TestMaterialize_RunLoopWatchesAndCatchesUp(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- m.Run(ctx) }()

	// Publish a lifecycle AFTER Run has caught up (its initial pass saw
	// nothing): the wake notification must drive the materialization.
	h.lifecycle(t, testQuad(h.id, "run-1"), "task-1")
	h.src.notify()

	if !eventually(t, func() bool {
		row, err := h.proj.Get(context.Background(), h.id, "task-1")
		return err == nil && row.Sealed
	}) {
		t.Fatal("run loop never materialized the published lifecycle")
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run returned: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run did not exit on cancellation")
	}
}

// TestMaterialize_RunUnavailableFailsLoud pins the background loop's
// source-honesty posture: a source without a retained substrate makes
// Run fail loud with ErrSourceUnavailable.
func TestMaterialize_RunUnavailableFailsLoud(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)
	h.src.unavailable = true
	err := m.Run(context.Background())
	wantErrIs(t, err, ErrSourceUnavailable)
}

// TestMaterialize_ErasureProbeGatesRestartRebuild pins the restart
// gate: a runtime whose durable erasure authority (the probe) reports a
// session erased is never re-materialized from sequence zero — even
// when the store-local fence is absent (an in-memory-backed store after
// a restart, or a durable store that was never erased through this
// projector). The probe is consulted ONCE at first contact and the
// session is skipped permanently.
func TestMaterialize_ErasureProbeGatesRestartRebuild(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()

	erased := identity.Identity{TenantID: "tenant-p", UserID: "user-p", SessionID: "sess-p"}
	m := h.newMaterializer(t, WithErasureProbe(staticProbe{erased: true}))

	// The erased session's events sit in the source, but the probe
	// reports the session erased: no rows are ever materialized.
	h.src.publish(t, spawnEv(erased, "run-p", "task-p", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(erased, "task-p"))
	h.src.publish(t, failedEv(erased, "task-p", "timeout"))

	res, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if _, err := h.proj.Get(context.Background(), erased, "task-p"); !errors.Is(err, turns.ErrTurnNotFound) {
		t.Fatalf("probe-erased turn = %v, want ErrTurnNotFound", err)
	}
	if res.FencedSessions == 0 && res.EventsSkipped == 0 {
		t.Errorf("probe-erased pass reported no fence/skip: %+v", res)
	}
}

// staticProbe is a test ErasureProbe reporting a fixed erased answer.
type staticProbe struct{ erased bool }

func (p staticProbe) Erased(context.Context, identity.Identity) (bool, error) {
	return p.erased, nil
}

// ---------------------------------------------------------------------------
// independent-review P1 regressions
// ---------------------------------------------------------------------------

// TestMaterialize_ChildAndBackgroundTerminalsNeverSealRoot pins the
// root-foreground terminal invariant (P1 #1): a CHILD task's (or a
// standalone BACKGROUND task's) task.failed / task.cancelled NEVER
// seals the root foreground turn — the root converges only through its
// OWN terminal lifecycle. The child/background task never becomes a
// row either.
func TestMaterialize_ChildAndBackgroundTerminalsNeverSealRoot(t *testing.T) {
	cases := []struct {
		name     string
		isChild  bool
		terminal func(h *harness) events.Event
	}{
		{"child failed", true, func(h *harness) events.Event { return failedEv(h.id, "task-child", "timeout") }},
		{"child cancelled", true, func(h *harness) events.Event { return cancelledEv(h.id, "task-child") }},
		{"background failed", false, func(h *harness) events.Event { return failedEv(h.id, "task-bg", "timeout") }},
		{"background cancelled", false, func(h *harness) events.Event { return cancelledEv(h.id, "task-bg") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, "")
			defer h.closeStore()
			m := h.newMaterializer(t)

			parentQuad := testQuad(h.id, "run-parent")
			h.src.publish(t, spawnEv(h.id, parentQuad.RunID, "task-parent", tasks.KindForeground, ""))
			h.src.publish(t, startedEv(h.id, "task-parent"))
			childID := "task-child"
			if tc.isChild {
				childQuad := testQuad(h.id, "run-child")
				h.src.publish(t, spawnEv(h.id, childQuad.RunID, "task-child", tasks.KindBackground, "task-parent"))
				h.src.publish(t, startedEv(h.id, "task-child"))
			} else {
				childID = "task-bg"
				bgQuad := testQuad(h.id, "run-bg")
				h.src.publish(t, spawnEv(h.id, bgQuad.RunID, "task-bg", tasks.KindBackground, ""))
				h.src.publish(t, startedEv(h.id, "task-bg"))
			}
			// The child/background task's terminal must NOT seal the root.
			h.src.publish(t, tc.terminal(h))

			if _, err := m.Materialize(context.Background()); err != nil {
				t.Fatalf("materialize: %v", err)
			}
			// The root turn is untouched: still mutable running.
			row := mustGetRow(t, h, "task-parent")
			if row.Sealed || row.Status != turns.StatusRunning {
				t.Fatalf("child/background terminal touched the root: status=%q sealed=%v", row.Status, row.Sealed)
			}
			// The child/background task never became a row.
			if _, err := h.proj.Get(context.Background(), h.id, turns.TurnID(childID)); !errors.Is(err, turns.ErrTurnNotFound) {
				t.Fatalf("child/background turn = %v, want ErrTurnNotFound", err)
			}
			// The root's OWN terminal still seals it.
			h.src.publish(t, failedEv(h.id, "task-parent", "timeout"))
			if _, err := m.Materialize(context.Background()); err != nil {
				t.Fatalf("materialize (root terminal): %v", err)
			}
			row = mustGetRow(t, h, "task-parent")
			if !row.Sealed || row.Status != turns.StatusFailed {
				t.Fatalf("root's own terminal did not seal: status=%q sealed=%v", row.Status, row.Sealed)
			}
		})
	}
}

// failNthUpdate wraps a turns.Store and injects ONE deterministic
// failure into the Nth UpdateTurnIf call (1-based), then delegates to
// the real driver — the mid-page-failure injection point for the
// transactional-retry test.
type failNthUpdate struct {
	turns.Store
	mu    sync.Mutex
	n     int // the UpdateTurnIf call number to fail (1-based)
	calls int
}

func (f *failNthUpdate) UpdateTurnIf(ctx context.Context, id identity.Identity, turnID turns.TurnID, expectedVersion int, row turns.TurnRow) (turns.TurnRow, error) {
	f.mu.Lock()
	f.calls++
	fail := f.calls == f.n
	f.mu.Unlock()
	if fail {
		return turns.TurnRow{}, errors.New("injected store write failure")
	}
	return f.Store.UpdateTurnIf(ctx, id, turnID, expectedVersion, row)
}

// TestMaterialize_MidPageFailureRetryIsTransactional pins the
// transactional event application across the complete page (P1 #2): a
// mid-page write failure aborts the pass loudly, and the SAME-instance
// retry re-pages from the cursor and converges to EXACTLY ONE of each
// accumulated observation — no duplicated activity / reasoning / apps /
// usage, no phantom turn — because the in-memory accumulators commit
// only after the durable write succeeds and already-incorporated events
// are no-ops.
func TestMaterialize_MidPageFailureRetryIsTransactional(t *testing.T) {
	real, err := sqlite.New(sqlite.Config{DSN: ":memory:"})
	if err != nil {
		t.Fatalf("open sqlite turns store: %v", err)
	}
	wrapped := &failNthUpdate{Store: real, n: 3} // the toolInvoked update
	proj, err := turns.New(wrapped)
	if err != nil {
		_ = real.Close(context.Background())
		t.Fatalf("new projector: %v", err)
	}
	h := &harness{
		id:    identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-a"},
		store: wrapped,
		proj:  proj,
		src:   &fakeSource{},
		closeStore: func() {
			_ = proj.Close(context.Background())
		},
	}
	defer h.closeStore()
	m := h.newMaterializer(t)

	quad := testQuad(h.id, "run-tx")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-tx", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-tx"))
	h.src.publish(t, decisionEv(h.id, quad.RunID, "CallTool"))
	h.src.publish(t, toolInvokedEv(h.id, quad.RunID, "tx.tool", time.Now())) // UpdateTurnIf #3 → injected failure
	h.src.publish(t, costRecordedEv(h.id, quad.RunID, "model-x", llm.Usage{
		PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, LatencyMS: 120,
	}, 0.25))
	h.src.publish(t, appAvailableEv(h.id, quad.RunID, "agent-a", "srv-1", "ui://one"))
	h.src.publish(t, toolCompletedEv(h.id, quad.RunID, "tx.tool", 5))
	last := h.src.publish(t, failedEv(h.id, "task-tx", "timeout"))

	// Pass 1 fails loud at the injected write — never a silent partial
	// application.
	if _, err := m.Materialize(context.Background()); err == nil {
		t.Fatal("first pass must fail loud at the injected write")
	}

	// The same-instance retry converges with EXACTLY ONE of each
	// observation: no duplicated activity/reasoning/apps/usage, no
	// phantom turn, no error.
	res, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("retry materialize: %v", err)
	}
	if res.Cursor != last.Sequence {
		t.Errorf("cursor = %d, want %d (the retry advanced past the whole page)", res.Cursor, last.Sequence)
	}
	row := mustGetRow(t, h, "task-tx")
	if !row.Sealed || row.Status != turns.StatusFailed {
		t.Fatalf("converged row = status %q sealed %v", row.Status, row.Sealed)
	}
	if len(row.Activity.Rows) != 1 || row.Activity.Rows[0].Status != turns.ActivitySucceeded {
		t.Fatalf("activity = %+v, want exactly one succeeded dispatch", row.Activity.Rows)
	}
	if row.Activity.Totals.Invoked != 0 || row.Activity.Totals.Succeeded != 1 {
		t.Errorf("activity totals = %+v, want invoked=0 succeeded=1", row.Activity.Totals)
	}
	if len(row.Reasoning.Steps) != 1 {
		t.Fatalf("reasoning = %+v, want exactly one step", row.Reasoning.Steps)
	}
	if len(row.Apps) != 1 || row.Apps[0].ResourceURI != "ui://one" {
		t.Fatalf("apps = %+v, want exactly one ref", row.Apps)
	}
	if row.Usage.PromptTokens.State != turns.UsageExact || usageValue(t, row.Usage.PromptTokens) != 100 {
		t.Fatalf("prompt tokens = %+v, want 100 (no double accumulation)", row.Usage.PromptTokens)
	}
	if row.Usage.TotalTokens.State != turns.UsageExact || usageValue(t, row.Usage.TotalTokens) != 150 {
		t.Errorf("total tokens = %+v, want 150", row.Usage.TotalTokens)
	}
	page, err := h.proj.List(context.Background(), h.id, turns.ListOptions{Limit: 20})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(page.Rows) != 1 || string(page.Rows[0].TurnID) != "task-tx" {
		t.Fatalf("rows = %+v, want exactly the one turn (no phantom rows)", page.Rows)
	}
}

// TestMaterialize_EvictedTurnRetiresAndPassContinues pins the honest
// per-turn terminal projection gap (P1 #3): once a turn's durable row
// is evicted past the store's retention bound, every later event routed
// to it RETIRES the turn's routing state (skipped, never resurrected,
// never a hard pass failure) and the pass keeps advancing — later
// events for other turns in the same session and other sessions still
// materialize, and the cursor never wedges.
func TestMaterialize_EvictedTurnRetiresAndPassContinues(t *testing.T) {
	store, err := sqlite.New(sqlite.Config{DSN: ":memory:", Retention: 1})
	if err != nil {
		t.Fatalf("open sqlite turns store: %v", err)
	}
	proj, err := turns.New(store)
	if err != nil {
		_ = store.Close(context.Background())
		t.Fatalf("new projector: %v", err)
	}
	h := &harness{
		id:    identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "sess-a"},
		store: store,
		proj:  proj,
		src:   &fakeSource{},
		closeStore: func() {
			_ = proj.Close(context.Background())
		},
	}
	defer h.closeStore()
	m := h.newMaterializer(t)

	// Turn 1 becomes mutable (running), then turn 2's append evicts
	// turn 1's row (retention bound is 1 newest row per session).
	quad1 := testQuad(h.id, "run-1")
	quad2 := testQuad(h.id, "run-2")
	h.src.publish(t, spawnEv(h.id, quad1.RunID, "task-1", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-1"))
	h.src.publish(t, toolInvokedEv(h.id, quad1.RunID, "one.tool", time.Now()))
	h.src.publish(t, spawnEv(h.id, quad2.RunID, "task-2", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-2"))
	// A late event for the EVICTED turn 1: with the honest gap handling
	// the turn is retired and the pass continues; without it the pass
	// would wedge on ErrTurnNotFound.
	h.src.publish(t, toolInvokedEv(h.id, quad1.RunID, "late.tool", time.Now()))
	// Turn 2's own terminal seal still lands in the same pass.
	last := h.src.publish(t, failedEv(h.id, "task-2", "timeout"))

	res, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("materialize: %v (an evicted turn must never wedge the pass)", err)
	}
	if res.Cursor != last.Sequence {
		t.Errorf("cursor = %d, want %d (the pass advanced past the evicted turn)", res.Cursor, last.Sequence)
	}
	// Turn 2 converged sealed.
	row2 := mustGetRow(t, h, "task-2")
	if !row2.Sealed || row2.Status != turns.StatusFailed {
		t.Fatalf("turn 2 = status %q sealed %v (later events must continue)", row2.Status, row2.Sealed)
	}
	// Turn 1 is NOT resurrected: the retired gap stays a gap.
	if _, err := h.proj.Get(context.Background(), h.id, "task-1"); !errors.Is(err, turns.ErrTurnNotFound) {
		t.Fatalf("evicted turn 1 = %v, want ErrTurnNotFound (no resurrection)", err)
	}

	// A LATER SESSION's lifecycle still materializes on the same
	// materializer (the eviction never wedged the global cursor).
	other := identity.Identity{TenantID: "tenant-b", UserID: "user-b", SessionID: "sess-b"}
	oq := testQuad(other, "run-3")
	h.src.publish(t, spawnEv(other, oq.RunID, "task-3", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(other, "task-3"))
	h.src.publish(t, failedEv(other, "task-3", "timeout"))
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize (later session): %v", err)
	}
	row3, err := h.proj.Get(context.Background(), other, "task-3")
	if err != nil {
		t.Fatalf("later-session turn: %v", err)
	}
	if !row3.Sealed || row3.Status != turns.StatusFailed {
		t.Fatalf("later-session turn = status %q sealed %v", row3.Status, row3.Sealed)
	}
}

// TestMaterialize_DeferredCompleteRetryNoOpDoesNotFalseSeal pins the
// deferred-complete retry contract (P1 #4): when the retried seal's
// EventSeq (the session checkpoint) is at or below the row's
// last-applied sequence, the projector treats it as an already-applied
// NO-OP and returns the row UNCHANGED — the retry must never equate
// that no-op with a successful seal. The local state stays unsealed and
// pending, and the row stays mutable until a REAL terminal event seals
// it.
func TestMaterialize_DeferredCompleteRetryNoOpDoesNotFalseSeal(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)

	quad := testQuad(h.id, "run-fs")
	h.src.publish(t, spawnEv(h.id, quad.RunID, "task-fs", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(h.id, "task-fs"))
	// task.completed without an answer source: the seal is deferred
	// (the row honestly stays mutable running).
	h.src.publish(t, completedEv(h.id, "task-fs"))
	// Later run-scoped events advance the ROW's last-applied sequence
	// to the session checkpoint — exactly the shape that makes the
	// end-of-pass retry a sequence no-op.
	h.src.publish(t, toolInvokedEv(h.id, quad.RunID, "fs.tool", time.Now()))
	h.src.publish(t, toolCompletedEv(h.id, quad.RunID, "fs.tool", 5))

	res, err := m.Materialize(context.Background())
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	if res.PendingComplete != 1 {
		t.Errorf("pending complete = %d, want 1 (the deferred seal stays pending)", res.PendingComplete)
	}
	row := mustGetRow(t, h, "task-fs")
	if row.Sealed || row.Status != turns.StatusRunning {
		t.Fatalf("row = status %q sealed %v (a no-op retry must NOT false-seal)", row.Status, row.Sealed)
	}

	// A second pass without new events: still pending, still no seal.
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("re-materialize: %v", err)
	}
	row = mustGetRow(t, h, "task-fs")
	if row.Sealed || row.Status != turns.StatusRunning {
		t.Fatalf("row after no-op retry = status %q sealed %v", row.Status, row.Sealed)
	}

	// A REAL terminal event for the root still seals the row (the local
	// state was never falsely marked sealed).
	h.src.publish(t, failedEv(h.id, "task-fs", "timeout"))
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize (real terminal): %v", err)
	}
	row = mustGetRow(t, h, "task-fs")
	if !row.Sealed || row.Status != turns.StatusFailed {
		t.Fatalf("real terminal did not seal: status=%q sealed=%v", row.Status, row.Sealed)
	}
}

// ---------------------------------------------------------------------------
// constructor guards
// ---------------------------------------------------------------------------

func TestMaterialize_NewRequiresSourceAndProjector(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	if _, err := New(nil, h.proj); err == nil {
		t.Fatal("New with nil source must fail loud")
	}
	if _, err := New(h.src, nil); err == nil {
		t.Fatal("New with nil projector must fail loud")
	}
}
