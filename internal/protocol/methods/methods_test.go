package methods_test

import (
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/methods"
)

// The canonical method names — the Phase 54 task-control ten (RFC §5.2
// "Task control" row verbatim) plus the Wave 13 streaming-events two
// (Phase 72 / 72a — RFC §5.2 "Streaming events" row) plus the five
// Phase 72c `search.*` methods (D-108) plus the five Phase 72f
// `runtime.*` / `metrics.*` posture methods (D-111). This slice is the
// test's independent source of truth; if methods.go drifts from the
// canonical set, the exhaustiveness test below fails.
var wantMethods = []methods.Method{
	methods.MethodStart,
	methods.MethodCancel,
	methods.MethodPause,
	methods.MethodResume,
	methods.MethodRedirect,
	methods.MethodInjectContext,
	methods.MethodApprove,
	methods.MethodReject,
	methods.MethodPrioritize,
	methods.MethodUserMessage,
	methods.MethodEventsSubscribe,
	methods.MethodEventsAggregate,
	methods.MethodSearchQuery,
	methods.MethodSearchSessions,
	methods.MethodSearchTasks,
	methods.MethodSearchEvents,
	methods.MethodSearchArtifacts,
	methods.MethodRuntimeInfo,
	methods.MethodRuntimeHealth,
	methods.MethodRuntimeCounters,
	methods.MethodRuntimeDrivers,
	methods.MethodMetricsSnapshot,
}

func TestMethods_ExhaustivenessAndWireStrings(t *testing.T) {
	got := methods.Methods()
	// Phase 54 task-control ten + Wave 13 streaming-events two + Phase
	// 72c search cluster five + Phase 72f posture cluster five = 22.
	if len(got) != 22 {
		t.Fatalf("Methods() returned %d methods, want 22", len(got))
	}
	if len(got) != len(wantMethods) {
		t.Fatalf("Methods() count %d != wantMethods count %d", len(got), len(wantMethods))
	}

	// Methods() is documented to return a deterministic sorted snapshot.
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("Methods() not sorted: %q >= %q at index %d", got[i-1], got[i], i)
		}
	}

	// Every wanted method must be valid and present.
	gotSet := map[methods.Method]struct{}{}
	for _, m := range got {
		gotSet[m] = struct{}{}
	}
	for _, want := range wantMethods {
		if !methods.IsValidMethod(want) {
			t.Errorf("IsValidMethod(%q) = false, want true", want)
		}
		if _, ok := gotSet[want]; !ok {
			t.Errorf("Methods() snapshot missing %q", want)
		}
	}

	// Wire strings are the RFC §5.2 verbatim lowercase snake_case for
	// the task-control ten; the streaming-events two use a dotted
	// `events.<verb>` shape; the Phase 72c cluster uses a dotted
	// `search.<index>` shape — both match the canonical event-type
	// naming convention (`tool.failed`, `runtime.error`, etc.).
	wireStrings := map[methods.Method]string{
		methods.MethodStart:           "start",
		methods.MethodCancel:          "cancel",
		methods.MethodPause:           "pause",
		methods.MethodResume:          "resume",
		methods.MethodRedirect:        "redirect",
		methods.MethodInjectContext:   "inject_context",
		methods.MethodApprove:         "approve",
		methods.MethodReject:          "reject",
		methods.MethodPrioritize:      "prioritize",
		methods.MethodUserMessage:     "user_message",
		methods.MethodEventsSubscribe: "events.subscribe",
		methods.MethodEventsAggregate: "events.aggregate",
		methods.MethodSearchQuery:     "search.query",
		methods.MethodSearchSessions:  "search.sessions",
		methods.MethodSearchTasks:     "search.tasks",
		methods.MethodSearchEvents:    "search.events",
		methods.MethodSearchArtifacts: "search.artifacts",
		methods.MethodRuntimeInfo:     "runtime.info",
		methods.MethodRuntimeHealth:   "runtime.health",
		methods.MethodRuntimeCounters: "runtime.counters",
		methods.MethodRuntimeDrivers:  "runtime.drivers",
		methods.MethodMetricsSnapshot: "metrics.snapshot",
	}
	for m, want := range wireStrings {
		if string(m) != want {
			t.Errorf("method wire string = %q, want %q", string(m), want)
		}
	}
}

func TestIsValidMethod_RejectsUnknown(t *testing.T) {
	for _, bad := range []methods.Method{
		"", "START", "Start", "cancel_task", "inject-context",
		"INJECT_CONTEXT", "usermessage", "unknown",
	} {
		if methods.IsValidMethod(bad) {
			t.Errorf("IsValidMethod(%q) = true, want false", bad)
		}
	}
}

func TestIsControlMethod_StartAndEventsSubscribeAreNotControls(t *testing.T) {
	if methods.IsControlMethod(methods.MethodStart) {
		t.Error("IsControlMethod(start) = true, want false — start maps to the task registry, not the steering inbox")
	}
	// Wave 13 streaming-events methods route through their own
	// transports (SSE / events-aggregate), NOT the steering inbox.
	if methods.IsControlMethod(methods.MethodEventsSubscribe) {
		t.Error("IsControlMethod(events.subscribe) = true, want false — streaming-events methods route through their own transports")
	}
	if methods.IsControlMethod(methods.MethodEventsAggregate) {
		t.Error("IsControlMethod(events.aggregate) = true, want false — streaming-events methods route through their own transports")
	}
	// Every non-start, non-streaming, non-search, non-posture canonical
	// method IS a control method.
	for _, m := range methods.Methods() {
		if m == methods.MethodStart || methods.IsStreamingEventsMethod(m) {
			continue
		}
		if methods.IsSearchMethod(m) || methods.IsPostureMethod(m) {
			continue
		}
		if !methods.IsControlMethod(m) {
			t.Errorf("IsControlMethod(%q) = false, want true", m)
		}
	}
	// An unknown method is not a control method.
	if methods.IsControlMethod(methods.Method("bogus")) {
		t.Error("IsControlMethod(bogus) = true, want false")
	}
}

// TestMethods_EventsSubscribe_Registered — pins the Phase 72 anchor:
// MethodEventsSubscribe is registered, IsValidMethod returns true,
// IsControlMethod returns false, the wire string is exactly
// "events.subscribe" (third-party Consoles branch on it).
func TestMethods_EventsSubscribe_Registered(t *testing.T) {
	if string(methods.MethodEventsSubscribe) != "events.subscribe" {
		t.Fatalf("MethodEventsSubscribe wire string = %q, want %q",
			string(methods.MethodEventsSubscribe), "events.subscribe")
	}
	if !methods.IsValidMethod(methods.MethodEventsSubscribe) {
		t.Error("IsValidMethod(events.subscribe) = false, want true")
	}
	if methods.IsControlMethod(methods.MethodEventsSubscribe) {
		t.Error("IsControlMethod(events.subscribe) = true, want false — streaming-events, not steering-control")
	}
	// String-form stability: a third-party Console computes the
	// canonical name as a literal and expects parity.
	if !methods.IsValidMethod(methods.Method("events.subscribe")) {
		t.Error(`IsValidMethod(Method("events.subscribe")) = false, want true — wire string stability broken`)
	}
}

// TestIsStreamingEventsMethod pins the streaming-events predicate —
// MethodEventsSubscribe and MethodEventsAggregate are the closed set.
func TestIsStreamingEventsMethod(t *testing.T) {
	if !methods.IsStreamingEventsMethod(methods.MethodEventsSubscribe) {
		t.Error("IsStreamingEventsMethod(events.subscribe) = false, want true")
	}
	if !methods.IsStreamingEventsMethod(methods.MethodEventsAggregate) {
		t.Error("IsStreamingEventsMethod(events.aggregate) = false, want true")
	}
	if methods.IsStreamingEventsMethod(methods.MethodStart) {
		t.Error("IsStreamingEventsMethod(start) = true, want false")
	}
	if methods.IsStreamingEventsMethod(methods.MethodCancel) {
		t.Error("IsStreamingEventsMethod(cancel) = true, want false")
	}
	if methods.IsStreamingEventsMethod(methods.Method("bogus")) {
		t.Error("IsStreamingEventsMethod(bogus) = true, want false")
	}
}

func TestIsSearchMethod(t *testing.T) {
	// The five Phase 72c search methods.
	for _, m := range []methods.Method{
		methods.MethodSearchQuery,
		methods.MethodSearchSessions,
		methods.MethodSearchTasks,
		methods.MethodSearchEvents,
		methods.MethodSearchArtifacts,
	} {
		if !methods.IsSearchMethod(m) {
			t.Errorf("IsSearchMethod(%q) = false, want true", m)
		}
		if methods.IsControlMethod(m) {
			t.Errorf("IsControlMethod(%q) = true, want false — search methods are not steering controls", m)
		}
	}
	// Non-search methods (start + the nine steering controls + streaming
	// + posture + unknown).
	for _, m := range []methods.Method{
		methods.MethodStart, methods.MethodCancel, methods.MethodPause,
		methods.MethodResume, methods.MethodRedirect, methods.MethodInjectContext,
		methods.MethodApprove, methods.MethodReject, methods.MethodPrioritize,
		methods.MethodUserMessage, methods.MethodEventsSubscribe,
		methods.MethodEventsAggregate, methods.MethodRuntimeInfo,
		methods.MethodMetricsSnapshot, methods.Method("bogus"), "",
	} {
		if methods.IsSearchMethod(m) {
			t.Errorf("IsSearchMethod(%q) = true, want false", m)
		}
	}
}

// TestIsPostureMethod pins the Phase 72f (D-111) posture predicate —
// the five `runtime.*` / `metrics.*` methods are the closed set, none
// of them is a control method, and they are valid canonical methods.
func TestIsPostureMethod(t *testing.T) {
	for _, m := range []methods.Method{
		methods.MethodRuntimeInfo,
		methods.MethodRuntimeHealth,
		methods.MethodRuntimeCounters,
		methods.MethodRuntimeDrivers,
		methods.MethodMetricsSnapshot,
	} {
		if !methods.IsPostureMethod(m) {
			t.Errorf("IsPostureMethod(%q) = false, want true", m)
		}
		if !methods.IsValidMethod(m) {
			t.Errorf("IsValidMethod(%q) = false, want true", m)
		}
		if methods.IsControlMethod(m) {
			t.Errorf("IsControlMethod(%q) = true, want false — posture methods are not steering controls", m)
		}
		if methods.IsSearchMethod(m) {
			t.Errorf("IsSearchMethod(%q) = true, want false", m)
		}
		if methods.IsStreamingEventsMethod(m) {
			t.Errorf("IsStreamingEventsMethod(%q) = true, want false", m)
		}
	}
	// Non-posture methods are not posture methods.
	for _, m := range []methods.Method{
		methods.MethodStart, methods.MethodCancel, methods.MethodEventsSubscribe,
		methods.MethodSearchQuery, methods.Method("bogus"), "",
	} {
		if methods.IsPostureMethod(m) {
			t.Errorf("IsPostureMethod(%q) = true, want false", m)
		}
	}
}
