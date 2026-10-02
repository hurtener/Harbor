package assemble_test

import (
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/tasks"
)

func TestRunOnce_DoesNotInheritNativeTaskAuthority(t *testing.T) {
	s, _, calls := retainedRecordingStack(t)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "headless-output"}
	ctx, err := identity.With(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx = tasks.WithOutputTask(ctx, "unrelated-native-task")
	// This recording provider emits its actual tool call on the first run ID.
	if _, err := s.RunOnce(ctx, "continue", id, assemble.WithRunID("first")); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatalf("headless tool executions = %d, want 1", calls.Load())
	}
	for _, taskID := range []tasks.TaskID{"unrelated-native-task", "first"} {
		if _, err := s.Tasks.Get(ctx, taskID); !errors.Is(err, tasks.ErrNotFound) {
			t.Fatalf("headless execution invented a task %q: %v", taskID, err)
		}
	}
}
