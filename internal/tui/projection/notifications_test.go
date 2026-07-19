package projection

import (
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

// findBlock returns the block with the given ID, or nil.
func findBlock(p Projection, id string) *Block {
	if i := blockIndex(p.Blocks, id); i >= 0 {
		return &p.Blocks[i]
	}
	return nil
}

// notification.* payloads cross the wire as a redacted map with LOWERCASE
// keys (the audit redactor lowercases field names). These tests use the
// lowercase shape to prove the reducer's case-insensitive decode matches
// the real wire, not a hand-idealised PascalCase fixture (§17.8 spirit).

func TestReducer_NotificationCompleted_RendersMutedConversationalBlock(t *testing.T) {
	id := testIdentity("session")
	r := &Reducer{}
	p, _ := r.Hydrate(SnapshotBundle{Generation: 1, Identity: id})
	p, change, err := r.Apply(p, wireEvent(id, 1, "notification.task_completed", "", map[string]any{
		"summary": "Background task bg-1 completed",
		"taskid":  "bg-1",
	}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !change.Changed {
		t.Fatal("expected a change")
	}
	b := findBlock(p, "notification:1")
	if b == nil {
		t.Fatalf("no notification block: %#v", p.Blocks)
	}
	if b.Kind != "notification" {
		t.Errorf("Kind=%q, want notification", b.Kind)
	}
	if b.Text != "Background task bg-1 completed" {
		t.Errorf("Text=%q, want the runtime-composed Summary", b.Text)
	}
	if ClassifyEventType("notification.task_completed") != EventTyped {
		t.Error("notification.task_completed must classify as EventTyped, not the generic fallback")
	}
}

func TestReducer_NotificationGroupResolved_RendersBlock(t *testing.T) {
	id := testIdentity("session")
	r := &Reducer{}
	p, _ := r.Hydrate(SnapshotBundle{Generation: 1, Identity: id})
	p, _, err := r.Apply(p, wireEvent(id, 3, "notification.task_group_resolved", "", map[string]any{
		"summary": "Background group resolved: 2 of 3 succeeded, 1 failed",
		"groupid": "g-1",
	}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	b := findBlock(p, "notification:3")
	if b == nil || b.Kind != "notification" {
		t.Fatalf("no notification block: %#v", p.Blocks)
	}
	if b.Text != "Background group resolved: 2 of 3 succeeded, 1 failed" {
		t.Errorf("Text=%q", b.Text)
	}
	if ClassifyEventType("notification.task_group_resolved") != EventTyped {
		t.Error("notification.task_group_resolved must classify as EventTyped")
	}
}

func TestReducer_NotificationTaskFailed_SuppressedForForegroundTurn(t *testing.T) {
	id := testIdentity("session")
	r := &Reducer{}
	// A foreground turn: the task row carries a Query, so mergeTasks creates
	// a "user:fg-1" block (only composer-submitted turns do).
	p, _ := r.Hydrate(SnapshotBundle{Generation: 1, Identity: id, Tasks: types.TaskListResponse{Rows: []types.TaskRow{
		taskRow(id, "fg-1", "do the thing", types.TaskStatusFailed),
	}}})
	if findBlock(p, "user:fg-1") == nil {
		t.Fatal("precondition: expected a user block for the foreground turn")
	}
	p, change, err := r.Apply(p, wireEvent(id, 5, "notification.task_failed", "", map[string]any{
		"summary": "Task fg-1 failed (error_code=boom)",
		"taskid":  "fg-1",
	}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if change.Changed {
		t.Error("foreground-turn failure mirror must be suppressed (no change)")
	}
	if findBlock(p, "notification:5") != nil {
		t.Error("notification block leaked for a foreground turn — the turn-failure line owns this")
	}
}

func TestReducer_NotificationTaskFailed_RenderedForBackgroundTask(t *testing.T) {
	id := testIdentity("session")
	r := &Reducer{}
	// No user block for bg-9 → genuinely background → the mirror renders.
	p, _ := r.Hydrate(SnapshotBundle{Generation: 1, Identity: id})
	p, _, err := r.Apply(p, wireEvent(id, 7, "notification.task_failed", "", map[string]any{
		"summary": "Task bg-9 failed (error_code=timeout)",
		"taskid":  "bg-9",
	}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	b := findBlock(p, "notification:7")
	if b == nil || b.Kind != "notification" {
		t.Fatalf("background-failure mirror missing: %#v", p.Blocks)
	}
}

func TestReducer_NotificationMalformed_FallsBackGeneric(t *testing.T) {
	id := testIdentity("session")
	r := &Reducer{}
	p, _ := r.Hydrate(SnapshotBundle{Generation: 1, Identity: id})
	// Empty summary → the reducer must NOT fabricate one; it degrades to the
	// safe generic fallback event block (never a blank notification line).
	p, _, err := r.Apply(p, wireEvent(id, 9, "notification.task_completed", "", map[string]any{"taskid": "x"}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if findBlock(p, "notification:9") != nil {
		t.Error("a summary-less notification must not render a notification block")
	}
	if findBlock(p, "event:9") == nil {
		t.Errorf("expected the generic fallback event block: %#v", p.Blocks)
	}
}

func TestReducer_TaskFailed_ThreadsErrorCode(t *testing.T) {
	id := testIdentity("session")
	r := &Reducer{}
	p, _ := r.Hydrate(SnapshotBundle{Generation: 1, Identity: id})
	p, _, err := r.Apply(p, wireEvent(id, 2, "task.failed", "run-1", map[string]any{
		"TaskID":    "run-1",
		"ErrorCode": "planner_rejected",
	}))
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	b := findBlock(p, "task:run-1")
	if b == nil {
		t.Fatalf("no task block: %#v", p.Blocks)
	}
	if b.Status != "failed" {
		t.Errorf("Status=%q, want failed", b.Status)
	}
	if b.ErrorCode != "planner_rejected" {
		t.Errorf("ErrorCode=%q, want planner_rejected (threaded from task.failed)", b.ErrorCode)
	}
}

// TestReducer_EndToEnd_BackgroundNotifyRendersWhileTaskLifecycleStaysOffChat
// is the in-package adapter test (plan Test plan §Integration): a background
// task's notification.task_completed renders as a conversational notification
// block while its task.completed sibling stays a non-conversational "task"
// lifecycle block; and a foreground turn's task.failed carries the ErrorCode
// while its notification.task_failed mirror is suppressed.
func TestReducer_EndToEnd_BackgroundNotifyRendersWhileTaskLifecycleStaysOffChat(t *testing.T) {
	id := testIdentity("session")
	r := &Reducer{}
	// Foreground turn fg-1 (has a user block) + a background task bg-2.
	p, _ := r.Hydrate(SnapshotBundle{Generation: 1, Identity: id, Tasks: types.TaskListResponse{Rows: []types.TaskRow{
		taskRow(id, "fg-1", "the operator's turn", types.TaskStatusRunning),
	}}})

	// The foreground turn fails: task.failed threads the code; the mirror is
	// suppressed (fg-1 has a user block).
	p, _, _ = r.Apply(p, wireEvent(id, 10, "task.failed", "fg-1", map[string]any{"TaskID": "fg-1", "ErrorCode": "planner_rejected"}))
	p, _, _ = r.Apply(p, wireEvent(id, 11, "notification.task_failed", "", map[string]any{"summary": "Task fg-1 failed (error_code=planner_rejected)", "taskid": "fg-1"}))

	// The background task completes: its task.completed is a "task" lifecycle
	// block (off-chat); its notification.task_completed renders conversationally.
	p, _, _ = r.Apply(p, wireEvent(id, 12, "task.completed", "bg-2", map[string]any{"TaskID": "bg-2"}))
	p, _, _ = r.Apply(p, wireEvent(id, 13, "notification.task_completed", "", map[string]any{"summary": "Background task bg-2 completed", "taskid": "bg-2"}))

	if fg := findBlock(p, "task:fg-1"); fg == nil || fg.ErrorCode != "planner_rejected" {
		t.Fatalf("foreground failure did not thread ErrorCode: %#v", fg)
	}
	if findBlock(p, "notification:11") != nil {
		t.Error("foreground-turn failure mirror leaked")
	}
	if bg := findBlock(p, "task:bg-2"); bg == nil || bg.Kind != "task" {
		t.Fatalf("background task lifecycle block missing/wrong kind: %#v", bg)
	}
	notif := findBlock(p, "notification:13")
	if notif == nil || notif.Kind != "notification" || notif.Text != "Background task bg-2 completed" {
		t.Fatalf("background completion notification missing/wrong: %#v", notif)
	}
}
