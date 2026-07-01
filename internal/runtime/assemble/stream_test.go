// stream_test.go — in-package unit coverage for the WithStream dispatch
// wrapper (streamDispatchHook). The count>1 path cannot be driven
// end-to-end through RunOnce with the dev mock LLM (it never emits a
// CallParallel), so the wrapper is a named helper and pinned here
// directly: one StreamToolDispatched event per dispatched tool (D-274),
// prev-hook chaining, and prev-error short-circuit.
package assemble

import (
	"context"
	"errors"
	"testing"
)

// TestStreamDispatchHook_CountEmitsOneEventPerTool — a CallParallel-shaped
// dispatch (count == len(Branches)) emits N StreamToolDispatched events,
// not one; a CallTool-shaped dispatch (count == 1) emits exactly one.
func TestStreamDispatchHook_CountEmitsOneEventPerTool(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		count int
		want  int
	}{
		{"calltool_count1", 1, 1},
		{"callparallel_count3", 3, 3},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var events []StreamEvent
			hook := streamDispatchHook(nil, func(e StreamEvent) { events = append(events, e) })
			if err := hook(context.Background(), tc.count); err != nil {
				t.Fatalf("hook: %v", err)
			}
			if len(events) != tc.want {
				t.Fatalf("sink saw %d events, want %d (one per dispatched tool)", len(events), tc.want)
			}
			for i, e := range events {
				if e.Kind != StreamToolDispatched {
					t.Errorf("event[%d].Kind = %q, want %q", i, e.Kind, StreamToolDispatched)
				}
				if e.Text != "" {
					t.Errorf("event[%d].Text = %q, want empty (dispatch is the signal; args never stream)", i, e.Text)
				}
			}
		})
	}
}

// TestStreamDispatchHook_ChainsPrevHook — a previously-installed hook
// runs first with the same count; its error short-circuits BEFORE any
// stream event is emitted (the counter hook is the integrity surface;
// the stream must not report a dispatch the chain rejected).
func TestStreamDispatchHook_ChainsPrevHook(t *testing.T) {
	t.Parallel()

	var prevCounts []int
	var events []StreamEvent
	hook := streamDispatchHook(
		func(_ context.Context, count int) error {
			prevCounts = append(prevCounts, count)
			return nil
		},
		func(e StreamEvent) { events = append(events, e) },
	)
	if err := hook(context.Background(), 2); err != nil {
		t.Fatalf("hook: %v", err)
	}
	if len(prevCounts) != 1 || prevCounts[0] != 2 {
		t.Errorf("prev hook counts = %v, want [2] (chained once with the same count)", prevCounts)
	}
	if len(events) != 2 {
		t.Errorf("sink saw %d events, want 2", len(events))
	}

	// The prev hook's error short-circuits: no stream event is emitted.
	prevErr := errors.New("counter blew up")
	var lateEvents []StreamEvent
	failing := streamDispatchHook(
		func(_ context.Context, _ int) error { return prevErr },
		func(e StreamEvent) { lateEvents = append(lateEvents, e) },
	)
	if err := failing(context.Background(), 3); !errors.Is(err, prevErr) {
		t.Fatalf("hook err = %v, want the prev hook's error", err)
	}
	if len(lateEvents) != 0 {
		t.Errorf("sink saw %d events after a prev-hook error, want 0", len(lateEvents))
	}
}
