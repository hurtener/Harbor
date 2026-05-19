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
	}
	wantClass := []events.EventType{
		notifications.EventTypeNotificationTaskFailed,
		notifications.EventTypeNotificationToolApprovalRequested,
		notifications.EventTypeNotificationGovernanceBudgetExceeded,
		notifications.EventTypeNotificationAuthRequired,
		notifications.EventTypeNotificationPauseRequested,
	}

	const N = 100
	var wg sync.WaitGroup
	wg.Add(N)
	errs := make([]error, N)
	for i := 0; i < N; i++ {
		i := i
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
	for i := 0; i < 16; i++ {
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
