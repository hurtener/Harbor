package planner_test

import (
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
)

// TestEventTypes_AllRegistered confirms every planner-emitted event
// type registered with the canonical events registry. A failure here
// means the events package would reject Publish calls for the
// affected type — typically caught by a smoke script, but the unit
// test surfaces the regression first.
func TestEventTypes_AllRegistered(t *testing.T) {
	t.Parallel()
	cases := []events.EventType{
		planner.EventTypePlannerDecision,
		planner.EventTypePlannerFinish,
		planner.EventTypePlannerError,
		planner.EventTypePlannerRepairExhausted,
		planner.EventTypePlannerMaxStepsExceeded,
		planner.EventTypeTrajectoryCompressed,
		planner.EventTypeTrajectoryCompressionFailed,
	}
	for _, et := range cases {
		t.Run(string(et), func(t *testing.T) {
			if !events.IsValidEventType(et) {
				t.Errorf("event type %q is NOT registered in the canonical registry", et)
			}
		})
	}
}

// TestRepairExhaustedPayload_IsSafePayload confirms the Phase 44
// payload composes the events.SafeSealed marker so the audit
// pipeline's safe-payload bypass applies (no redactor round-trip
// needed; the payload's fields are operator-visible by design).
func TestRepairExhaustedPayload_IsSafePayload(t *testing.T) {
	t.Parallel()
	payload := planner.RepairExhaustedPayload{
		Identity: identity.Quadruple{
			Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
			RunID:    "r",
		},
		Attempts:               3,
		ConsecutiveArgFailures: 2,
		Reasons:                []string{"a", "b"},
		OccurredAt:             time.Now(),
	}
	// Compile-time guarantee: the type implements events.SafePayload.
	var _ events.SafePayload = payload
}

// TestMaxStepsExceededPayload_IsSafePayload confirms the Phase 45
// payload composes the events.SafeSealed marker so the audit
// pipeline's safe-payload bypass applies. Same fail-loudly shape as
// RepairExhaustedPayload — D-051.
func TestMaxStepsExceededPayload_IsSafePayload(t *testing.T) {
	t.Parallel()
	payload := planner.MaxStepsExceededPayload{
		Identity: identity.Quadruple{
			Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
			RunID:    "r",
		},
		MaxSteps:      12,
		StepsObserved: 13,
		LastTool:      "search",
		OccurredAt:    time.Now(),
	}
	// Compile-time guarantee: the type implements events.SafePayload.
	var _ events.SafePayload = payload
}

// TestTrajectoryCompressedPayload_IsSafePayload confirms the Phase 46
// success-path payload composes events.SafeSealed. D-055.
func TestTrajectoryCompressedPayload_IsSafePayload(t *testing.T) {
	t.Parallel()
	payload := planner.TrajectoryCompressedPayload{
		Identity: identity.Quadruple{
			Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
			RunID:    "r",
		},
		StepsBefore:   12,
		StepsAfter:    12,
		TokenEstimate: 8192,
		OccurredAt:    time.Now(),
	}
	// Compile-time guarantee: the type implements events.SafePayload.
	var _ events.SafePayload = payload
}

// TestTrajectoryCompressionFailedPayload_IsSafePayload confirms the
// Phase 46 fail-loudly payload composes events.SafeSealed. The emit
// is the load-bearing observability surface for compression failures
// (§13 — silent degradation banned). D-055.
func TestTrajectoryCompressionFailedPayload_IsSafePayload(t *testing.T) {
	t.Parallel()
	payload := planner.TrajectoryCompressionFailedPayload{
		Identity: identity.Quadruple{
			Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
			RunID:    "r",
		},
		StepsObserved: 12,
		TokenEstimate: 8192,
		ErrorCode:     "summariser_error",
		ErrorMessage:  "downstream LLM returned 500",
		OccurredAt:    time.Now(),
	}
	// Compile-time guarantee: the type implements events.SafePayload.
	var _ events.SafePayload = payload
}
