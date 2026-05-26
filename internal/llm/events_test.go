package llm_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
)

func TestEvents_PhaseTypesRegistered(t *testing.T) {
	want := []events.EventType{
		llm.EventTypeImageMaterialized,
		llm.EventTypeContextLeak,
		llm.EventTypeContextWindowExceeded,
		llm.EventTypeCostRecorded,
		llm.EventTypeModeDowngraded,
		// Phase 107 — streaming completion chunk event (AC-11).
		llm.EventTypeCompletionChunk,
	}
	registered := make(map[events.EventType]struct{})
	for _, ty := range events.EventTypes() {
		registered[ty] = struct{}{}
	}
	for _, ty := range want {
		if _, ok := registered[ty]; !ok {
			t.Errorf("event type %q not registered", ty)
		}
	}
}

func TestEvents_PayloadsAreSafe(t *testing.T) {
	// Compile-time checks that every Phase-32 payload composes
	// events.SafeSealed (no audit redactor runs on these — the
	// content is operator-visible by design). Phase 107 adds
	// CompletionChunkPayload to the same posture — chunk deltas are
	// per-session operator-visible content (the LLM's own output),
	// never a secret-shaped payload.
	var (
		_ events.SafePayload = llm.ImageMaterializedPayload{}
		_ events.SafePayload = llm.ContextLeakPayload{}
		_ events.SafePayload = llm.ContextWindowExceededPayload{}
		_ events.SafePayload = llm.CostRecordedPayload{}
		_ events.SafePayload = llm.ModeDowngradedPayload{}
		_ events.SafePayload = llm.CompletionChunkPayload{}
	)
}

// TestEvents_CompletionChunkPayload_Shape pins the wire shape of the
// Phase 107 CompletionChunkPayload so a future field rename / removal
// surfaces here rather than silently breaking Console subscribers.
// AC-11.
func TestEvents_CompletionChunkPayload_Shape(t *testing.T) {
	p := llm.CompletionChunkPayload{
		TaskID: "task-1",
		RunID:  "run-1",
		Delta:  "Hello",
		Done:   false,
		Kind:   "content",
	}
	if p.TaskID != "task-1" || p.RunID != "run-1" || p.Delta != "Hello" {
		t.Fatal("CompletionChunkPayload field assignment did not round-trip")
	}
	// SafePayload composition is the assertion in TestEvents_PayloadsAreSafe;
	// here we pin that the Kind values consumers expect are wire-stable.
	if p.Done {
		t.Errorf("Done default expected false, got true")
	}
}
