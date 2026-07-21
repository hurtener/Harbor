package notifications_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/notifications"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools/approval"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// testQuadruple is the canonical identity quadruple every unit test
// uses. Keeps the per-test boilerplate small.
var testQuadruple = identity.Quadruple{
	Identity: identity.Identity{
		TenantID:  "t-1",
		UserID:    "u-1",
		SessionID: "s-1",
	},
	RunID: "r-1",
}

func TestMap_TaskFailed_SynthesisesNotificationTaskFailed(t *testing.T) {
	t.Parallel()
	ev := events.Event{
		Type:     tasks.EventTypeTaskFailed,
		Identity: testQuadruple,
		Sequence: 42,
		Payload: tasks.TaskFailedPayload{
			TaskID:    "task-abc",
			ErrorCode: "tool_invocation_failed",
		},
	}
	got, err := notifications.Map(context.Background(), ev)
	if err != nil {
		t.Fatalf("Map: unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Map: len=%d, want 1", len(got))
	}
	out := got[0]
	if out.Type != notifications.EventTypeNotificationTaskFailed {
		t.Errorf("Type=%q, want %q", out.Type, notifications.EventTypeNotificationTaskFailed)
	}
	if out.Identity != testQuadruple {
		t.Errorf("Identity=%v, want %v (must be preserved from trigger)", out.Identity, testQuadruple)
	}
	if out.Sequence != 0 {
		t.Errorf("Sequence=%d, want 0 (bus owns sequencing)", out.Sequence)
	}
	if !out.OccurredAt.IsZero() {
		t.Errorf("OccurredAt=%v, want zero (bus fills on Publish)", out.OccurredAt)
	}
	payload, ok := out.Payload.(notifications.NotificationPayload)
	if !ok {
		t.Fatalf("Payload type=%T, want NotificationPayload", out.Payload)
	}
	if payload.Class != notifications.EventTypeNotificationTaskFailed {
		t.Errorf("Payload.Class=%q, want %q", payload.Class, notifications.EventTypeNotificationTaskFailed)
	}
	if payload.Severity != notifications.SeverityError {
		t.Errorf("Payload.Severity=%q, want %q", payload.Severity, notifications.SeverityError)
	}
	if !strings.Contains(payload.Summary, "task-abc") || !strings.Contains(payload.Summary, "tool_invocation_failed") {
		t.Errorf("Payload.Summary=%q must mention task id + error code", payload.Summary)
	}
	if !strings.Contains(payload.DeepLink, "task-abc") {
		t.Errorf("Payload.DeepLink=%q must include task id", payload.DeepLink)
	}
	if payload.OriginEventType != tasks.EventTypeTaskFailed {
		t.Errorf("Payload.OriginEventType=%q, want %q", payload.OriginEventType, tasks.EventTypeTaskFailed)
	}
	if payload.OriginEventSequence != 42 {
		t.Errorf("Payload.OriginEventSequence=%d, want 42", payload.OriginEventSequence)
	}
}

func TestMap_ToolApprovalRequested_SynthesisesNotificationToolApprovalRequested(t *testing.T) {
	t.Parallel()
	ev := events.Event{
		Type:     approval.EventTypeToolApprovalRequested,
		Identity: testQuadruple,
		Sequence: 7,
		Payload: approval.ToolApprovalRequestedPayload{
			Tool:       "fs.write",
			PauseToken: "pt-123",
			Reason:     "destructive",
		},
	}
	got, err := notifications.Map(context.Background(), ev)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1", len(got))
	}
	out := got[0]
	if out.Type != notifications.EventTypeNotificationToolApprovalRequested {
		t.Errorf("Type=%q", out.Type)
	}
	payload := out.Payload.(notifications.NotificationPayload)
	if payload.Severity != notifications.SeverityWarning {
		t.Errorf("Severity=%q, want %q", payload.Severity, notifications.SeverityWarning)
	}
	if !strings.Contains(payload.Summary, "fs.write") {
		t.Errorf("Summary=%q must mention tool name", payload.Summary)
	}
	if !strings.Contains(payload.DeepLink, "fs.write") || !strings.Contains(payload.DeepLink, "pt-123") {
		t.Errorf("DeepLink=%q must include tool + pause token", payload.DeepLink)
	}
	if payload.OriginEventSequence != 7 {
		t.Errorf("OriginEventSequence=%d, want 7", payload.OriginEventSequence)
	}
}

func TestMap_GovernanceBudgetExceeded_SynthesisesNotificationGovernanceBudgetExceeded(t *testing.T) {
	t.Parallel()
	ev := events.Event{
		Type:     governance.EventTypeBudgetExceeded,
		Identity: testQuadruple,
		Sequence: 19,
		Payload: governance.BudgetExceededPayload{
			Identity:  testQuadruple,
			Tier:      "free",
			Model:     "gpt-4",
			TotalCost: 12.34,
			Ceiling:   10.00,
			Currency:  "USD",
		},
	}
	got, err := notifications.Map(context.Background(), ev)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1", len(got))
	}
	out := got[0]
	if out.Type != notifications.EventTypeNotificationGovernanceBudgetExceeded {
		t.Errorf("Type=%q", out.Type)
	}
	payload := out.Payload.(notifications.NotificationPayload)
	if payload.Severity != notifications.SeverityError {
		t.Errorf("Severity=%q, want Error", payload.Severity)
	}
	if !strings.Contains(payload.Summary, "free") || !strings.Contains(payload.Summary, "gpt-4") {
		t.Errorf("Summary=%q must mention tier + model", payload.Summary)
	}
	if !strings.Contains(payload.DeepLink, "free") {
		t.Errorf("DeepLink=%q must include tier", payload.DeepLink)
	}
}

func TestMap_ToolAuthRequired_SynthesisesNotificationAuthRequired(t *testing.T) {
	t.Parallel()
	ev := events.Event{
		Type:     auth.EventTypeToolAuthRequired,
		Identity: testQuadruple,
		Sequence: 3,
		Payload: auth.ToolAuthRequiredPayload{
			Source:       "src-gh",
			SourceName:   "GitHub",
			BindingScope: "user",
			AuthorizeURL: "https://example.com/authorize",
			State:        "csrf-token-xyz",
			PauseToken:   "pt-auth-1",
			Scopes:       []string{"repo"},
		},
	}
	got, err := notifications.Map(context.Background(), ev)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1", len(got))
	}
	out := got[0]
	if out.Type != notifications.EventTypeNotificationAuthRequired {
		t.Errorf("Type=%q", out.Type)
	}
	payload := out.Payload.(notifications.NotificationPayload)
	if payload.Severity != notifications.SeverityWarning {
		t.Errorf("Severity=%q, want Warning", payload.Severity)
	}
	if !strings.Contains(payload.Summary, "GitHub") || !strings.Contains(payload.Summary, "user") {
		t.Errorf("Summary=%q must mention source + binding scope", payload.Summary)
	}
	if !strings.Contains(payload.DeepLink, "src-gh") || !strings.Contains(payload.DeepLink, "csrf-token-xyz") {
		t.Errorf("DeepLink=%q must include source + state", payload.DeepLink)
	}
}

func TestMap_PauseRequested_SynthesisesNotificationPauseRequested(t *testing.T) {
	t.Parallel()
	ev := events.Event{
		Type:     pauseresume.EventTypePauseRequested,
		Identity: testQuadruple,
		Sequence: 11,
		Payload: pauseresume.PauseRequestedPayload{
			Token:  "pt-pause-1",
			Reason: string(pauseresume.ReasonApprovalRequired),
		},
	}
	got, err := notifications.Map(context.Background(), ev)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1", len(got))
	}
	out := got[0]
	if out.Type != notifications.EventTypeNotificationPauseRequested {
		t.Errorf("Type=%q", out.Type)
	}
	payload := out.Payload.(notifications.NotificationPayload)
	if payload.Severity != notifications.SeverityInfo {
		t.Errorf("Severity=%q, want Info", payload.Severity)
	}
	if !strings.Contains(payload.DeepLink, "pt-pause-1") {
		t.Errorf("DeepLink=%q must include token", payload.DeepLink)
	}
}

func TestMap_TaskGroupResolved_AllSucceeded_SeverityInfo(t *testing.T) {
	t.Parallel()
	ev := events.Event{
		Type:     tasks.EventTypeTaskGroupResolved,
		Identity: testQuadruple,
		Sequence: 10,
		Payload: tasks.TaskGroupResolvedPayload{Completion: tasks.GroupCompletion{
			GroupID:     "grp-1",
			FinalStatus: tasks.GroupCompleted,
			Members: []tasks.MemberOutcome{
				{TaskID: "m-1", Status: tasks.StatusComplete, Description: "fetch A"},
				{TaskID: "m-2", Status: tasks.StatusComplete, Description: "fetch B"},
			},
		}},
	}
	got, err := notifications.Map(context.Background(), ev)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1", len(got))
	}
	payload := got[0].Payload.(notifications.NotificationPayload)
	if got[0].Type != notifications.EventTypeNotificationTaskGroupResolved {
		t.Errorf("Type=%q", got[0].Type)
	}
	if payload.Severity != notifications.SeverityInfo {
		t.Errorf("Severity=%q, want info (all succeeded)", payload.Severity)
	}
	if payload.GroupID != "grp-1" {
		t.Errorf("GroupID=%q, want grp-1", payload.GroupID)
	}
	if payload.MemberSucceeded != 2 || payload.MemberFailed != 0 || payload.MemberCancelled != 0 {
		t.Errorf("counts succeeded/failed/cancelled = %d/%d/%d, want 2/0/0",
			payload.MemberSucceeded, payload.MemberFailed, payload.MemberCancelled)
	}
	if len(payload.Members) != 2 || payload.MembersTruncated {
		t.Errorf("Members len=%d truncated=%v, want 2/false", len(payload.Members), payload.MembersTruncated)
	}
	if payload.Members[0].Description != "fetch A" || payload.Members[0].TaskID != "m-1" {
		t.Errorf("member[0]=%+v, want ref-shaped taskid+description", payload.Members[0])
	}
	if !strings.Contains(payload.Summary, "2 of 2") {
		t.Errorf("Summary=%q must report the rollup", payload.Summary)
	}
}

func TestMap_TaskGroupResolved_MixedOutcome_SeverityWarning(t *testing.T) {
	t.Parallel()
	ev := events.Event{
		Type:     tasks.EventTypeTaskGroupResolved,
		Identity: testQuadruple,
		Sequence: 11,
		Payload: tasks.TaskGroupResolvedPayload{Completion: tasks.GroupCompletion{
			GroupID:     "grp-2",
			FinalStatus: tasks.GroupCompleted,
			Members: []tasks.MemberOutcome{
				{TaskID: "m-1", Status: tasks.StatusComplete},
				{TaskID: "m-2", Status: tasks.StatusFailed},
				{TaskID: "m-3", Status: tasks.StatusCancelled},
			},
		}},
	}
	got, err := notifications.Map(context.Background(), ev)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	payload := got[0].Payload.(notifications.NotificationPayload)
	if payload.Severity != notifications.SeverityWarning {
		t.Errorf("Severity=%q, want warning (a member failed/cancelled)", payload.Severity)
	}
	if payload.MemberSucceeded != 1 || payload.MemberFailed != 1 || payload.MemberCancelled != 1 {
		t.Errorf("counts = %d/%d/%d, want 1/1/1",
			payload.MemberSucceeded, payload.MemberFailed, payload.MemberCancelled)
	}
}

func TestMap_TaskGroupResolved_CapsMembersAndSetsTruncated(t *testing.T) {
	t.Parallel()
	members := make([]tasks.MemberOutcome, notifications.MaxMemberSummaries+5)
	for i := range members {
		members[i] = tasks.MemberOutcome{TaskID: tasks.TaskID(fmt.Sprintf("m-%d", i)), Status: tasks.StatusComplete}
	}
	ev := events.Event{
		Type:     tasks.EventTypeTaskGroupResolved,
		Identity: testQuadruple,
		Sequence: 12,
		Payload:  tasks.TaskGroupResolvedPayload{Completion: tasks.GroupCompletion{GroupID: "grp-3", FinalStatus: tasks.GroupCompleted, Members: members}},
	}
	got, err := notifications.Map(context.Background(), ev)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	payload := got[0].Payload.(notifications.NotificationPayload)
	if len(payload.Members) != notifications.MaxMemberSummaries {
		t.Errorf("Members len=%d, want cap %d", len(payload.Members), notifications.MaxMemberSummaries)
	}
	if !payload.MembersTruncated {
		t.Error("MembersTruncated=false, want true (cap hit — never a silent drop)")
	}
	// Counts reflect the FULL membership, not the capped slice.
	if payload.MemberSucceeded != len(members) {
		t.Errorf("MemberSucceeded=%d, want %d (full membership)", payload.MemberSucceeded, len(members))
	}
}

func TestMap_TaskGroupResolved_WrongPayload_ErrUnmappable(t *testing.T) {
	t.Parallel()
	ev := events.Event{
		Type:     tasks.EventTypeTaskGroupResolved,
		Identity: testQuadruple,
		Payload:  events.RedactedMap{Data: map[string]any{"completion": "x"}},
	}
	got, err := notifications.Map(context.Background(), ev)
	if got != nil {
		t.Errorf("got=%v, want nil", got)
	}
	if !errors.Is(err, notifications.ErrUnmappable) {
		t.Fatalf("err=%v, want ErrUnmappable", err)
	}
}

// groupCancelledEvent builds a task.group_cancelled trigger with the
// given origin and a fixed mixed-outcome membership (one already
// succeeded, one failed, one cancelled).
func groupCancelledEvent(origin tasks.CancelOrigin) events.Event {
	return events.Event{
		Type:     tasks.EventTypeTaskGroupCancelled,
		Identity: testQuadruple,
		Sequence: 20,
		Payload: tasks.TaskGroupCancelledPayload{Origin: origin, Completion: tasks.GroupCompletion{
			GroupID:     "gc-1",
			FinalStatus: tasks.GroupCancelled,
			Reason:      "fail-fast:boom",
			Members: []tasks.MemberOutcome{
				{TaskID: "m-1", Status: tasks.StatusComplete, Description: "won"},
				{TaskID: "m-2", Status: tasks.StatusFailed, Description: "lost"},
				{TaskID: "m-3", Status: tasks.StatusCancelled, Description: "stopped"},
			},
		}},
	}
}

// TestMap_TaskGroupCancelled_CascadeSynthesises proves an unprompted
// cascade cancel is mirrored with the correct full-membership counts,
// ref-shaped member summaries, and Warning severity.
func TestMap_TaskGroupCancelled_CascadeSynthesises(t *testing.T) {
	t.Parallel()
	got, err := notifications.Map(context.Background(), groupCancelledEvent(tasks.CancelOriginCascade))
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1 (cascade cancel is mirrored)", len(got))
	}
	if got[0].Type != notifications.EventTypeNotificationTaskGroupCancelled {
		t.Errorf("Type=%q, want notification.task_group_cancelled", got[0].Type)
	}
	payload := got[0].Payload.(notifications.NotificationPayload)
	if payload.Severity != notifications.SeverityWarning {
		t.Errorf("Severity=%q, want warning", payload.Severity)
	}
	if payload.GroupID != "gc-1" {
		t.Errorf("GroupID=%q, want gc-1", payload.GroupID)
	}
	if payload.MemberSucceeded != 1 || payload.MemberFailed != 1 || payload.MemberCancelled != 1 {
		t.Errorf("counts succeeded/failed/cancelled = %d/%d/%d, want 1/1/1",
			payload.MemberSucceeded, payload.MemberFailed, payload.MemberCancelled)
	}
	if len(payload.Members) != 3 || payload.MembersTruncated {
		t.Errorf("Members len=%d truncated=%v, want 3/false", len(payload.Members), payload.MembersTruncated)
	}
	if payload.Members[1].TaskID != "m-2" || payload.Members[1].Status != string(tasks.StatusFailed) {
		t.Errorf("member[1]=%+v, want ref-shaped m-2/failed", payload.Members[1])
	}
	if !strings.Contains(payload.Summary, "cascade") || !strings.Contains(payload.Summary, "1 cancelled of 3") {
		t.Errorf("Summary=%q must name the origin + rollup", payload.Summary)
	}
}

// TestMap_TaskGroupCancelled_FailFastSynthesises proves a fail-fast
// cancel is mirrored.
func TestMap_TaskGroupCancelled_FailFastSynthesises(t *testing.T) {
	t.Parallel()
	got, err := notifications.Map(context.Background(), groupCancelledEvent(tasks.CancelOriginFailFast))
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1 (fail-fast cancel is mirrored)", len(got))
	}
	payload := got[0].Payload.(notifications.NotificationPayload)
	if !strings.Contains(payload.Summary, "fail-fast") {
		t.Errorf("Summary=%q must name the fail-fast origin", payload.Summary)
	}
}

// TestMap_TaskGroupCancelled_OperatorSuppressed proves the settled
// suppression rule: an operator-driven cancel produces NO notification
// (the operator already knows), returning (nil, nil).
func TestMap_TaskGroupCancelled_OperatorSuppressed(t *testing.T) {
	t.Parallel()
	got, err := notifications.Map(context.Background(), groupCancelledEvent(tasks.CancelOriginOperator))
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if got != nil {
		t.Fatalf("got=%v, want nil (operator-driven cancel is suppressed)", got)
	}
}

// TestMap_TaskGroupCancelled_UnknownOriginSynthesises proves the
// fail-loud-not-swallow rule: an empty/unknown origin (an older or
// hand-built event) is SURFACED, never silently dropped — the failure
// mode is an extra notification, never a hidden one.
func TestMap_TaskGroupCancelled_UnknownOriginSynthesises(t *testing.T) {
	t.Parallel()
	for _, origin := range []tasks.CancelOrigin{"", "some-future-origin"} {
		got, err := notifications.Map(context.Background(), groupCancelledEvent(origin))
		if err != nil {
			t.Fatalf("origin=%q Map: %v", origin, err)
		}
		if len(got) != 1 {
			t.Fatalf("origin=%q len=%d, want 1 (unclassified cancel is surfaced, not swallowed)", origin, len(got))
		}
		payload := got[0].Payload.(notifications.NotificationPayload)
		if !strings.Contains(payload.Summary, "unspecified") {
			t.Errorf("origin=%q Summary=%q must read as unspecified", origin, payload.Summary)
		}
	}
}

// TestMap_TaskGroupCancelled_CapsMembersAndSetsTruncated proves the
// member cap + truncation flag hold on a large fail-fast batch (counts
// still reflect full membership).
func TestMap_TaskGroupCancelled_CapsMembersAndSetsTruncated(t *testing.T) {
	t.Parallel()
	members := make([]tasks.MemberOutcome, notifications.MaxMemberSummaries+7)
	for i := range members {
		members[i] = tasks.MemberOutcome{TaskID: tasks.TaskID(fmt.Sprintf("m-%d", i)), Status: tasks.StatusCancelled}
	}
	ev := events.Event{
		Type:     tasks.EventTypeTaskGroupCancelled,
		Identity: testQuadruple,
		Payload:  tasks.TaskGroupCancelledPayload{Origin: tasks.CancelOriginFailFast, Completion: tasks.GroupCompletion{GroupID: "gc-big", FinalStatus: tasks.GroupCancelled, Members: members}},
	}
	got, err := notifications.Map(context.Background(), ev)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	payload := got[0].Payload.(notifications.NotificationPayload)
	if len(payload.Members) != notifications.MaxMemberSummaries {
		t.Errorf("Members len=%d, want cap %d", len(payload.Members), notifications.MaxMemberSummaries)
	}
	if !payload.MembersTruncated {
		t.Error("MembersTruncated=false, want true (cap hit — never a silent drop)")
	}
	if payload.MemberCancelled != len(members) {
		t.Errorf("MemberCancelled=%d, want %d (full membership)", payload.MemberCancelled, len(members))
	}
}

// TestMap_TaskGroupCancelled_WrongPayload_ErrUnmappable proves the
// mapper fails loud on a wrong payload type rather than degrading.
func TestMap_TaskGroupCancelled_WrongPayload_ErrUnmappable(t *testing.T) {
	t.Parallel()
	ev := events.Event{
		Type:     tasks.EventTypeTaskGroupCancelled,
		Identity: testQuadruple,
		Payload:  events.RedactedMap{Data: map[string]any{"completion": "x"}},
	}
	got, err := notifications.Map(context.Background(), ev)
	if got != nil {
		t.Errorf("got=%v, want nil", got)
	}
	if !errors.Is(err, notifications.ErrUnmappable) {
		t.Fatalf("err=%v, want ErrUnmappable", err)
	}
}

func TestMap_TaskCompleted_NotifyFalse_ReturnsNilNil(t *testing.T) {
	t.Parallel()
	ev := events.Event{
		Type:     tasks.EventTypeTaskCompleted,
		Identity: testQuadruple,
		Payload:  tasks.TaskCompletedPayload{TaskID: "t-quiet", NotifyOnComplete: false},
	}
	got, err := notifications.Map(context.Background(), ev)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if got != nil {
		t.Fatalf("NotifyOnComplete=false must produce no notification, got %v", got)
	}
}

func TestMap_TaskCompleted_NotifyTrue_SynthesisesNotification(t *testing.T) {
	t.Parallel()
	ev := events.Event{
		Type:     tasks.EventTypeTaskCompleted,
		Identity: testQuadruple,
		Sequence: 20,
		Payload:  tasks.TaskCompletedPayload{TaskID: "t-loud", NotifyOnComplete: true},
	}
	got, err := notifications.Map(context.Background(), ev)
	if err != nil {
		t.Fatalf("Map: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("len=%d, want 1", len(got))
	}
	payload := got[0].Payload.(notifications.NotificationPayload)
	if got[0].Type != notifications.EventTypeNotificationTaskCompleted {
		t.Errorf("Type=%q", got[0].Type)
	}
	if payload.Severity != notifications.SeverityInfo {
		t.Errorf("Severity=%q, want info", payload.Severity)
	}
	if payload.TaskID != "t-loud" {
		t.Errorf("TaskID=%q, want t-loud", payload.TaskID)
	}
	if !strings.Contains(payload.Summary, "t-loud") {
		t.Errorf("Summary=%q must name the task", payload.Summary)
	}
	if payload.OriginEventSequence != 20 {
		t.Errorf("OriginEventSequence=%d, want 20", payload.OriginEventSequence)
	}
}

func TestMap_TaskCompleted_WrongPayload_ErrUnmappable(t *testing.T) {
	t.Parallel()
	ev := events.Event{
		Type:     tasks.EventTypeTaskCompleted,
		Identity: testQuadruple,
		Payload:  events.RedactedMap{Data: map[string]any{"taskid": "x"}},
	}
	got, err := notifications.Map(context.Background(), ev)
	if got != nil {
		t.Errorf("got=%v, want nil", got)
	}
	if !errors.Is(err, notifications.ErrUnmappable) {
		t.Fatalf("err=%v, want ErrUnmappable", err)
	}
}

func TestMap_UnmappedEventType_ReturnsNilNil(t *testing.T) {
	t.Parallel()
	// bus.dropped is a real registered event type that is NOT in the
	// V1 trigger set — the canonical "unmapped" case.
	ev := events.Event{
		Type:     events.EventTypeBusDropped,
		Identity: testQuadruple,
		Payload: events.BusDroppedPayload{
			FromSeq:      1,
			ToSeq:        2,
			DroppedCount: 1,
			SubscriberID: 1,
		},
	}
	got, err := notifications.Map(context.Background(), ev)
	if err != nil {
		t.Fatalf("unmapped should return nil error, got %v", err)
	}
	if got != nil {
		t.Fatalf("unmapped should return nil slice, got %v", got)
	}
}

func TestMap_StructurallyInvalidEvent_ReturnsErrUnmappable(t *testing.T) {
	t.Parallel()
	// task.failed declared but payload of the WRONG typed shape.
	// The mapper must fail-loudly with ErrUnmappable, NOT silently
	// emit nothing.
	ev := events.Event{
		Type:     tasks.EventTypeTaskFailed,
		Identity: testQuadruple,
		Payload:  events.RedactedMap{Data: map[string]any{"task_id": "x"}},
	}
	got, err := notifications.Map(context.Background(), ev)
	if got != nil {
		t.Errorf("got=%v, want nil", got)
	}
	if !errors.Is(err, notifications.ErrUnmappable) {
		t.Fatalf("err=%v, want errors.Is(err, ErrUnmappable)", err)
	}
}

// TestMap_ConcurrentReuse — D-025 binding test: N=100 concurrent
// invocations against a single shared mapper run cleanly under -race,
// each returns the correctly-shaped output for its trigger, and the
// baseline goroutine count is restored after all calls return.
//
// The Map function is pure (no global state, no shared mutables), so
// the assertion is straightforward — but the test is still mandatory
// per CLAUDE.md §11 + §5 concurrent-reuse contract.
func TestMap_ConcurrentReuse(t *testing.T) {
	t.Parallel()

	// Snapshot baseline goroutines after settling.
	baseline := stableNumGoroutine(t)

	// Build a rotation over the five V1 mappings so each goroutine
	// exercises a different trigger shape. Pre-built outside the
	// goroutine loop so the test is deterministic.
	triggers := []events.Event{
		{
			Type:     tasks.EventTypeTaskFailed,
			Identity: testQuadruple,
			Sequence: 1,
			Payload:  tasks.TaskFailedPayload{TaskID: "t-a", ErrorCode: "ec-1"},
		},
		{
			Type:     approval.EventTypeToolApprovalRequested,
			Identity: testQuadruple,
			Sequence: 2,
			Payload:  approval.ToolApprovalRequestedPayload{Tool: "fs.write", PauseToken: "pt", Reason: "r"},
		},
		{
			Type:     governance.EventTypeBudgetExceeded,
			Identity: testQuadruple,
			Sequence: 3,
			Payload: governance.BudgetExceededPayload{
				Identity:  testQuadruple,
				Tier:      "free",
				Model:     "m",
				TotalCost: 1.0,
				Ceiling:   0.5,
				Currency:  "USD",
			},
		},
		{
			Type:     auth.EventTypeToolAuthRequired,
			Identity: testQuadruple,
			Sequence: 4,
			Payload:  auth.ToolAuthRequiredPayload{Source: "src", SourceName: "Src", BindingScope: "user", State: "st"},
		},
		{
			Type:     pauseresume.EventTypePauseRequested,
			Identity: testQuadruple,
			Sequence: 5,
			Payload:  pauseresume.PauseRequestedPayload{Token: "pt-1", Reason: "approval_required"},
		},
		{
			Type:     tasks.EventTypeTaskGroupResolved,
			Identity: testQuadruple,
			Sequence: 6,
			Payload: tasks.TaskGroupResolvedPayload{Completion: tasks.GroupCompletion{
				GroupID:     "g-1",
				FinalStatus: tasks.GroupCompleted,
				Members: []tasks.MemberOutcome{
					{TaskID: "m-1", Status: tasks.StatusComplete, Description: "member one"},
				},
			}},
		},
		{
			Type:     tasks.EventTypeTaskGroupCancelled,
			Identity: testQuadruple,
			Sequence: 7,
			Payload: tasks.TaskGroupCancelledPayload{Origin: tasks.CancelOriginCascade, Completion: tasks.GroupCompletion{
				GroupID:     "g-2",
				FinalStatus: tasks.GroupCancelled,
				Members: []tasks.MemberOutcome{
					{TaskID: "m-1", Status: tasks.StatusCancelled, Description: "member one"},
				},
			}},
		},
		{
			Type:     tasks.EventTypeTaskCompleted,
			Identity: testQuadruple,
			Sequence: 8,
			Payload:  tasks.TaskCompletedPayload{TaskID: "t-done", NotifyOnComplete: true},
		},
	}
	wantClass := []events.EventType{
		notifications.EventTypeNotificationTaskFailed,
		notifications.EventTypeNotificationToolApprovalRequested,
		notifications.EventTypeNotificationGovernanceBudgetExceeded,
		notifications.EventTypeNotificationAuthRequired,
		notifications.EventTypeNotificationPauseRequested,
		notifications.EventTypeNotificationTaskGroupResolved,
		notifications.EventTypeNotificationTaskGroupCancelled,
		notifications.EventTypeNotificationTaskCompleted,
	}

	const N = 100
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make([]error, N)
	for i := range N {

		go func() {
			defer wg.Done()
			idx := i % len(triggers)
			out, err := notifications.Map(context.Background(), triggers[idx])
			if err != nil {
				errs[i] = err
				return
			}
			if len(out) != 1 {
				errs[i] = fmt.Errorf("iter %d: len=%d, want 1", i, len(out))
				return
			}
			if out[0].Type != wantClass[idx] {
				errs[i] = fmt.Errorf("iter %d: type=%q, want %q", i, out[0].Type, wantClass[idx])
			}
			// Identity preservation under contention.
			if out[0].Identity != testQuadruple {
				errs[i] = fmt.Errorf("iter %d: identity bled (%v vs %v)", i, out[0].Identity, testQuadruple)
			}
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("iter %d: %v", i, err)
		}
	}

	// Goroutine-leak assertion — Map is pure and spawns nothing, so
	// the count returns to baseline immediately. The eventual loop is
	// defensive against transient runtime noise.
	if got := eventualBaseline(t, baseline); got > baseline {
		t.Errorf("goroutine leak: baseline=%d post=%d", baseline, got)
	}
}

// stableNumGoroutine reads runtime.NumGoroutine after a brief settle
// to skip transient goroutines a parent test may have spawned.
func stableNumGoroutine(t *testing.T) int {
	t.Helper()
	// Three sequential reads spaced by short yields — if they agree,
	// we're at steady state.
	for range 16 {
		a := runtime.NumGoroutine()
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
		b := runtime.NumGoroutine()
		if a == b {
			return a
		}
	}
	return runtime.NumGoroutine()
}

// eventualBaseline waits up to 2s for runtime.NumGoroutine to return
// to baseline. Returns the final observation.
func eventualBaseline(t *testing.T, baseline int) int {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := runtime.NumGoroutine()
		if got <= baseline {
			return got
		}
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	return runtime.NumGoroutine()
}
