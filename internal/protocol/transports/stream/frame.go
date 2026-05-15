package stream

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/hurtener/Harbor/internal/events"
)

// wireEvent is the JSON shape carried in an SSE frame's `data:` line. It
// is a flat, Protocol-owned projection of events.Event — never a
// re-export of the internal events.Event struct (a Protocol type that
// mapped 1:1 onto an internal Go struct would be the RFC §5.1
// reject-on-sight smell). The wire stream deliberately flattens the
// identity quadruple to strings and carries the payload as a generic
// JSON value: the Protocol surface owns its own wire vocabulary.
//
// Payload is whatever the event's EventPayload marshals to. The bus has
// already run the payload through the audit redactor on Publish for any
// payload that is not SafePayload (Phase 05, D-020), so what reaches the
// wire here is redaction-safe by construction — the SSE transport does
// not re-redact and does not bypass the redactor.
type wireEvent struct {
	Type       string            `json:"type"`
	Sequence   uint64            `json:"sequence"`
	OccurredAt string            `json:"occurred_at"`
	Tenant     string            `json:"tenant"`
	User       string            `json:"user"`
	Session    string            `json:"session"`
	Run        string            `json:"run,omitempty"`
	Payload    any               `json:"payload,omitempty"`
	Extra      map[string]string `json:"extra,omitempty"`
}

// toWireEvent projects an events.Event onto its flat wire shape.
func toWireEvent(ev events.Event) wireEvent {
	return wireEvent{
		Type:       string(ev.Type),
		Sequence:   ev.Sequence,
		OccurredAt: ev.OccurredAt.UTC().Format("2006-01-02T15:04:05.000000000Z07:00"),
		Tenant:     ev.Identity.TenantID,
		User:       ev.Identity.UserID,
		Session:    ev.Identity.SessionID,
		Run:        ev.Identity.RunID,
		Payload:    ev.Payload,
		Extra:      ev.Extra,
	}
}

// encodeEvent renders an events.Event as a single SSE frame: an `event:`
// line carrying the event type, an `id:` line carrying the per-bus
// monotonic Sequence (the SSE reconnect cursor — a reconnecting client
// echoes it back as Last-Event-ID), and a `data:` line carrying the
// JSON-encoded wire event. The frame is terminated by a blank line per
// the SSE grammar.
//
// A `data:` payload that contains newlines is split across multiple
// `data:` lines per the SSE spec — JSON from encoding/json is
// single-line, but the split is defensive against a future payload
// shape.
func encodeEvent(ev events.Event) ([]byte, error) {
	data, err := json.Marshal(toWireEvent(ev))
	if err != nil {
		// Fail loud — a payload the SSE transport cannot encode is a
		// real bug, not a frame to silently drop (CLAUDE.md §5).
		return nil, fmt.Errorf("stream: encode event type=%q seq=%d: %w", ev.Type, ev.Sequence, err)
	}
	var b strings.Builder
	b.WriteString("event: ")
	b.WriteString(string(ev.Type))
	b.WriteByte('\n')
	b.WriteString("id: ")
	b.WriteString(strconv.FormatUint(ev.Sequence, 10))
	b.WriteByte('\n')
	for _, line := range strings.Split(string(data), "\n") {
		b.WriteString("data: ")
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	return []byte(b.String()), nil
}

// keepaliveFrame is the SSE comment the stream handler emits on the
// keepalive interval. An SSE comment is a line beginning with a colon;
// clients ignore it, but it keeps the connection — and any intermediary
// proxy — from treating an idle stream as dead. A trailing blank line is
// not required for a comment, but the extra newline is harmless and
// keeps the wire output uniform.
var keepaliveFrame = []byte(": keepalive\n\n")

// retryFrame is the SSE `retry:` directive — it tells a client how long
// (in milliseconds) to wait before reconnecting after the stream drops.
// Sent once at stream open so a reconnecting client backs off
// deterministically rather than hammering the server.
func retryFrame(ms int) []byte {
	return []byte("retry: " + strconv.Itoa(ms) + "\n\n")
}
