package steering

import (
	"errors"
	"testing"
	"time"
)

func TestControlHistory_CapsAtMax_NewestWins(t *testing.T) {
	h := newControlHistory(4)
	for i := 0; i < 10; i++ {
		h.record("sess-1", AppliedControl{
			Type:      ControlInjectContext,
			RunID:     "run-1",
			AppliedAt: time.Unix(int64(i), 0),
		})
	}
	got := h.snapshot("sess-1")
	if len(got) != 4 {
		t.Fatalf("len(history) = %d, want 4 (cap)", len(got))
	}
	// Newest-wins: after 10 records into a cap-4 ring, the surviving
	// entries are records 6,7,8,9 (their AppliedAt seconds).
	for idx, want := range []int64{6, 7, 8, 9} {
		if got[idx].AppliedAt.Unix() != want {
			t.Errorf("history[%d].AppliedAt = %d, want %d", idx, got[idx].AppliedAt.Unix(), want)
		}
	}
}

func TestControlHistory_NonPositiveCap_FallsBackToDefault(t *testing.T) {
	h := newControlHistory(0)
	if h.cap != MaxControlHistory {
		t.Fatalf("cap = %d, want MaxControlHistory (%d)", h.cap, MaxControlHistory)
	}
	h = newControlHistory(-7)
	if h.cap != MaxControlHistory {
		t.Fatalf("cap = %d, want MaxControlHistory (%d)", h.cap, MaxControlHistory)
	}
}

func TestControlHistory_SessionsDoNotBleed(t *testing.T) {
	h := newControlHistory(8)
	h.record("sess-a", AppliedControl{Type: ControlCancel, RunID: "run-a"})
	h.record("sess-a", AppliedControl{Type: ControlPause, RunID: "run-a"})
	h.record("sess-b", AppliedControl{Type: ControlRedirect, RunID: "run-b"})

	if got := h.len("sess-a"); got != 2 {
		t.Errorf("sess-a len = %d, want 2", got)
	}
	if got := h.len("sess-b"); got != 1 {
		t.Errorf("sess-b len = %d, want 1", got)
	}
	for _, e := range h.snapshot("sess-a") {
		if e.RunID != "run-a" {
			t.Errorf("sess-a history carried a foreign RunID %q", e.RunID)
		}
	}
	for _, e := range h.snapshot("sess-b") {
		if e.RunID != "run-b" {
			t.Errorf("sess-b history carried a foreign RunID %q", e.RunID)
		}
	}
}

func TestControlHistory_RecordsFailedApply(t *testing.T) {
	h := newControlHistory(8)
	applyErr := errors.New("PRIORITIZE: task not found")
	h.record("sess-1", AppliedControl{
		Type:  ControlPrioritize,
		RunID: "run-1",
		Err:   applyErr,
	})
	got := h.snapshot("sess-1")
	if len(got) != 1 {
		t.Fatalf("len = %d, want 1 — a failed apply is still recorded (audit trail)", len(got))
	}
	if got[0].Err == nil {
		t.Error("a failed apply must carry its error in the history — silent drop forbidden (CLAUDE.md §5)")
	}
}

func TestControlHistory_Forget(t *testing.T) {
	h := newControlHistory(8)
	h.record("sess-1", AppliedControl{Type: ControlCancel, RunID: "run-1"})
	if h.len("sess-1") != 1 {
		t.Fatal("setup: expected 1 entry")
	}
	h.forget("sess-1")
	if got := h.len("sess-1"); got != 0 {
		t.Errorf("after forget, len = %d, want 0", got)
	}
	// Forget on an unknown session is a no-op.
	h.forget("sess-never-existed")
}

func TestControlHistory_SnapshotIsACopy(t *testing.T) {
	h := newControlHistory(8)
	h.record("sess-1", AppliedControl{Type: ControlCancel, RunID: "run-1"})
	snap := h.snapshot("sess-1")
	snap[0].RunID = "mutated"
	// The internal ring must be unaffected by a caller mutating the copy.
	again := h.snapshot("sess-1")
	if again[0].RunID != "run-1" {
		t.Errorf("snapshot is not a copy — caller mutation reached the internal ring (RunID=%q)", again[0].RunID)
	}
}
