package approval

import (
	"testing"

	"github.com/hurtener/Harbor/internal/events"
)

func TestEventTypes_RegisteredInCanonicalRegistry(t *testing.T) {
	cases := []events.EventType{
		EventTypeToolApprovalRequested,
		EventTypeToolApproved,
		EventTypeToolRejected,
	}
	for _, et := range cases {
		t.Run(string(et), func(t *testing.T) {
			if !events.IsValidEventType(et) {
				t.Fatalf("event type %q is not registered in the canonical registry", et)
			}
		})
	}
}

func TestEventTypeStrings_StableWireValues(t *testing.T) {
	if EventTypeToolApprovalRequested != "tool.approval_requested" {
		t.Errorf("EventTypeToolApprovalRequested = %q want %q",
			EventTypeToolApprovalRequested, "tool.approval_requested")
	}
	if EventTypeToolApproved != "tool.approved" {
		t.Errorf("EventTypeToolApproved = %q want %q",
			EventTypeToolApproved, "tool.approved")
	}
	if EventTypeToolRejected != "tool.rejected" {
		t.Errorf("EventTypeToolRejected = %q want %q",
			EventTypeToolRejected, "tool.rejected")
	}
}

func TestPayloads_AreSafePayload(t *testing.T) {
	// Compile-time assertions: every payload type embeds SafeSealed
	// (which composes Sealed). A regression here would surface as a
	// compile error.
	var _ events.EventPayload = ToolApprovalRequestedPayload{}
	var _ events.SafePayload = ToolApprovalRequestedPayload{}
	var _ events.EventPayload = ToolApprovedPayload{}
	var _ events.SafePayload = ToolApprovedPayload{}
	var _ events.EventPayload = ToolRejectedPayload{}
	var _ events.SafePayload = ToolRejectedPayload{}
}
