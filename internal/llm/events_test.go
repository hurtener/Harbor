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
	// content is operator-visible by design).
	var (
		_ events.SafePayload = llm.ImageMaterializedPayload{}
		_ events.SafePayload = llm.ContextLeakPayload{}
		_ events.SafePayload = llm.ContextWindowExceededPayload{}
		_ events.SafePayload = llm.CostRecordedPayload{}
		_ events.SafePayload = llm.ModeDowngradedPayload{}
	)
}
