package steering

import (
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/tools"
)

func TestInbox_UserMessageRejectsUnsupportedInputBeforeInterruption(t *testing.T) {
	for name, payload := range map[string]map[string]any{
		"missing":           nil,
		"empty":             {"message": ""},
		"non_string":        {"message": 42},
		"attachments":       {"message": "edit", "attachments": []any{"ref"}},
		"artifact_ids":      {"message": "edit", "input_artifact_ids": []any{"ref"}},
		"empty_attachments": {"message": "edit", "attachments": nil},
		"unknown":           {"message": "edit", "extra": "ignored instruction"},
	} {
		t.Run(name, func(t *testing.T) {
			inbox := newInbox(t, runA, newFakeClock())
			cancelled := false
			if !inbox.beginAttempt(0, func() { cancelled = true }) {
				t.Fatal("attempt refused")
			}
			fenced, err := inbox.fenceInvocation(t.Context(), 0)
			if err != nil {
				t.Fatal(err)
			}
			err = inbox.Enqueue(ControlEvent{Type: ControlUserMessage, Identity: runA, CallerTenant: runA.TenantID, CallerScope: ScopeOwnerUser, Payload: payload})
			if !errors.Is(err, ErrPayloadInvalid) || cancelled || inbox.Len() != 0 || tools.CheckInvocationFence(fenced) != nil {
				t.Fatalf("invalid input affected execution: err=%v cancelled=%v queued=%d", err, cancelled, inbox.Len())
			}
		})
	}
}

func TestInbox_UserMessagePreservesExactText(t *testing.T) {
	inbox := newInbox(t, runA, newFakeClock())
	const message = "  Keep version 9007199254740993.\n\tUse ámbar. "
	if err := inbox.Enqueue(ControlEvent{Type: ControlUserMessage, Identity: runA, CallerTenant: runA.TenantID, CallerScope: ScopeOwnerUser, Payload: map[string]any{"message": message}}); err != nil {
		t.Fatal(err)
	}
	events, err := inbox.Drain()
	if err != nil || len(events) != 1 || events[0].Payload["message"] != message {
		t.Fatalf("steering text changed: events=%+v err=%v", events, err)
	}
}
