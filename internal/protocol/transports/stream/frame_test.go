package stream

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

func TestEncodeEvent_FrameShape(t *testing.T) {
	ev := events.Event{
		Type:       events.EventTypeRuntimeRunCancelled,
		Sequence:   42,
		OccurredAt: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
		Identity: identity.Quadruple{
			Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
			RunID:    "r1",
		},
		Payload: events.RunCancelledPayload{RunID: "r1", CancelledAt: 123},
	}
	frame, err := encodeEvent(ev)
	if err != nil {
		t.Fatalf("encodeEvent: %v", err)
	}
	got := string(frame)

	if !strings.Contains(got, "event: runtime.run_cancelled\n") {
		t.Errorf("frame missing event: line\n%s", got)
	}
	if !strings.Contains(got, "id: 42\n") {
		t.Errorf("frame missing id: line (the reconnect cursor)\n%s", got)
	}
	if !strings.HasSuffix(got, "\n\n") {
		t.Errorf("frame not terminated by a blank line\n%q", got)
	}

	// The data: line must carry valid JSON of the flat wire shape.
	var dataLine string
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "data: ") {
			dataLine = strings.TrimPrefix(line, "data: ")
			break
		}
	}
	if dataLine == "" {
		t.Fatalf("frame missing data: line\n%s", got)
	}
	var we wireEvent
	if err := json.Unmarshal([]byte(dataLine), &we); err != nil {
		t.Fatalf("data: line is not valid JSON: %v", err)
	}
	if we.Type != "runtime.run_cancelled" || we.Sequence != 42 {
		t.Errorf("wireEvent = %+v, want type=runtime.run_cancelled seq=42", we)
	}
	if we.Tenant != "t1" || we.User != "u1" || we.Session != "s1" || we.Run != "r1" {
		t.Errorf("wireEvent identity = (%q,%q,%q,%q), want (t1,u1,s1,r1)",
			we.Tenant, we.User, we.Session, we.Run)
	}
}

func TestKeepaliveFrame_IsSSEComment(t *testing.T) {
	if !strings.HasPrefix(string(keepaliveFrame), ":") {
		t.Errorf("keepalive frame %q is not an SSE comment (must begin with ':')", keepaliveFrame)
	}
	if !strings.HasSuffix(string(keepaliveFrame), "\n\n") {
		t.Errorf("keepalive frame %q not terminated by a blank line", keepaliveFrame)
	}
}

func TestRetryFrame_Shape(t *testing.T) {
	got := string(retryFrame(3000))
	if got != "retry: 3000\n\n" {
		t.Errorf("retryFrame(3000) = %q, want \"retry: 3000\\n\\n\"", got)
	}
}
