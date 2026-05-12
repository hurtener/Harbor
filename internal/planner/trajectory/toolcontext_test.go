package trajectory_test

import (
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

// TestPauseStateSerialisation_FailsLoudlyOnUnserializableContext
// is the load-bearing §11 mandatory test (CLAUDE.md §11):
//
//	"Pause/resume serialization tests are mandatory: assert
//	 ErrUnserializable is raised loudly when a non-serializable
//	 handle is in pause state. No silent nil/None returns."
//
// We construct a pause-state-shaped Trajectory whose ToolContext
// carries a NON-SERIALISABLE leaf in the Serializable half (a live
// channel disguised as a "config" value), and assert that Serialize
// returns (nil, ErrUnserializable) — never silently nil bytes.
//
// This is the exact predecessor-bug scenario the contract closes:
// `try { json.dumps(tool_context) } catch { return None }` is rejected.
func TestPauseStateSerialisation_FailsLoudlyOnUnserializableContext(t *testing.T) {
	// A live channel masquerading as a serialisable value — the
	// predecessor's silent-context-loss surface area.
	t.Run("non_encodable_in_serializable_half_fails_loudly", func(t *testing.T) {
		pauseState := &trajectory.Trajectory{
			Query: "approve this action?",
			ToolContext: trajectory.ToolContext{
				Serializable: map[string]any{
					"endpoint":      "https://api.test/v1",
					"live_callback": make(chan string), // <-- the bug
				},
				Handles: []trajectory.HandleID{"h-real-callback"},
			},
			ResumeHint: &trajectory.ResumeHint{
				PauseToken: "tok-1",
			},
		}

		bytes, err := pauseState.Serialize()
		if bytes != nil {
			t.Fatalf("Serialize returned non-nil bytes (%d) on non-encodable input — silent-drop contract violated", len(bytes))
		}
		if err == nil {
			t.Fatalf("Serialize returned nil error — fail-loudly contract violated")
		}
		var unserr trajectory.ErrUnserializable
		if !errors.As(err, &unserr) {
			t.Fatalf("err = %v want errors.As(ErrUnserializable)", err)
		}
		if unserr.Field == "" {
			t.Fatalf("ErrUnserializable.Field is empty — must name the offending leaf")
		}
		// The path must mention either ToolContext or its tagged
		// counterpart tool_context, and the specific offending key.
		ok := false
		for _, marker := range []string{"ToolContext", "tool_context"} {
			if has(unserr.Field, marker) {
				ok = true
				break
			}
		}
		if !ok {
			t.Errorf("Field = %q does not include 'ToolContext' / 'tool_context'", unserr.Field)
		}
		if !has(unserr.Field, "live_callback") {
			t.Errorf("Field = %q does not name the offending key 'live_callback'", unserr.Field)
		}
	})
}

// TestResumeWithStaleHandle_ReturnsErrToolContextLost — the second
// half of the §11 contract. A handle ID that was serialised at
// pause-time but whose registry mapping has died (process restart,
// distributed-registry miss) MUST surface as ErrToolContextLost on
// resume — never (nil, nil).
func TestResumeWithStaleHandle_ReturnsErrToolContextLost(t *testing.T) {
	// Build the pause-time state: register a live handle, serialise
	// the trajectory carrying the HandleID.
	registry := trajectory.NewProcessLocalRegistry()
	callback := func() string { return "from-pre-pause" }
	registry.Set("h-callback", callback)

	pauseState := &trajectory.Trajectory{
		Query: "approve this action?",
		ToolContext: trajectory.ToolContext{
			Serializable: map[string]any{"endpoint": "https://api.test/v1"},
			Handles:      []trajectory.HandleID{"h-callback"},
		},
	}
	serialised, err := pauseState.Serialize()
	if err != nil {
		t.Fatalf("pause-time Serialize err = %v", err)
	}

	// Now simulate a resume in a DIFFERENT process: a fresh
	// registry with no h-callback mapping.
	freshRegistry := trajectory.NewProcessLocalRegistry()

	resumed, err := trajectory.Deserialize(serialised)
	if err != nil {
		t.Fatalf("Deserialize err = %v", err)
	}

	// The runtime would walk Handles and Get each one; we mimic.
	for _, h := range resumed.ToolContext.Handles {
		_, err := freshRegistry.Get(h)
		var lost trajectory.ErrToolContextLost
		if !errors.As(err, &lost) {
			t.Fatalf("stale-handle Get err = %v want ErrToolContextLost", err)
		}
		if lost.Handle != h {
			t.Errorf("ErrToolContextLost.Handle = %q want %q", lost.Handle, h)
		}
	}
}

// TestResumeWithLiveHandle_Succeeds — when the handle IS registered,
// resume succeeds and the live value (the callback) is recovered by
// reference.
func TestResumeWithLiveHandle_Succeeds(t *testing.T) {
	registry := trajectory.NewProcessLocalRegistry()
	called := false
	registry.Set("h-cb", func() { called = true })

	pauseState := &trajectory.Trajectory{
		ToolContext: trajectory.ToolContext{
			Handles: []trajectory.HandleID{"h-cb"},
		},
	}
	bytes, err := pauseState.Serialize()
	if err != nil {
		t.Fatalf("Serialize err = %v", err)
	}
	resumed, err := trajectory.Deserialize(bytes)
	if err != nil {
		t.Fatalf("Deserialize err = %v", err)
	}
	for _, h := range resumed.ToolContext.Handles {
		v, err := registry.Get(h)
		if err != nil {
			t.Fatalf("Get(%s) err = %v", h, err)
		}
		fn, ok := v.(func())
		if !ok {
			t.Fatalf("retrieved handle is not a func()")
		}
		fn()
	}
	if !called {
		t.Errorf("recovered callback did not execute")
	}
}

// TestToolContext_Split_RoundTrip — the serialisable half persists
// across pause/resume; the Handles slice carries opaque IDs only.
// The actual values stay in the registry — they are never written
// into the serialised bytes.
func TestToolContext_Split_RoundTrip(t *testing.T) {
	pauseState := &trajectory.Trajectory{
		ToolContext: trajectory.ToolContext{
			Serializable: map[string]any{"tool_name": "search", "version": "v1"},
			Handles:      []trajectory.HandleID{"h-a", "h-b"},
		},
	}
	bytes, err := pauseState.Serialize()
	if err != nil {
		t.Fatalf("Serialize err = %v", err)
	}
	// The serialised bytes contain handle IDs (strings) but NEVER any
	// live value. Spot-check: a func() / chan would be in the
	// register or in Serializable; the bytes should only contain JSON.
	s := string(bytes)
	if has(s, "func") || has(s, "chan") {
		t.Errorf("serialised bytes contain a live-value marker: %s", s)
	}

	resumed, err := trajectory.Deserialize(bytes)
	if err != nil {
		t.Fatalf("Deserialize err = %v", err)
	}
	if got := resumed.ToolContext.Serializable["tool_name"]; got != "search" {
		t.Errorf("Serializable.tool_name = %v want \"search\"", got)
	}
	if len(resumed.ToolContext.Handles) != 2 {
		t.Errorf("Handles len = %d want 2", len(resumed.ToolContext.Handles))
	}
}

func has(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
