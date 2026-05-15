package durable

import (
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
)

// scalarPayload marshals to a bare JSON scalar (not an object) — it
// drives marshalPayload's "wrap a non-object value" branch.
type scalarPayload struct {
	events.Sealed
}

// scalarPayload's JSON form: an empty object actually. We need a real
// scalar. Use a payload type whose MarshalJSON returns a scalar.
type stringScalarPayload struct {
	events.Sealed
	v string
}

func (p stringScalarPayload) MarshalJSON() ([]byte, error) {
	return []byte(`"` + p.v + `"`), nil
}

func TestEncodeDecode_RoundTrip(t *testing.T) {
	ev := events.Event{
		Type: events.EventTypeRuntimeWarning,
		Identity: identity.Quadruple{
			Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
			RunID:    "r",
		},
		OccurredAt: time.Date(2026, 5, 14, 1, 2, 3, 0, time.UTC),
		Sequence:   42,
		Payload:    events.RedactedMap{Data: map[string]any{"k": "v"}},
		Extra:      map[string]string{"e": "1"},
	}
	b, err := encodeEvent(ev)
	if err != nil {
		t.Fatalf("encodeEvent: %v", err)
	}
	got, err := decodeEvent(b)
	if err != nil {
		t.Fatalf("decodeEvent: %v", err)
	}
	if got.Type != ev.Type || got.Sequence != ev.Sequence {
		t.Fatalf("type/seq mismatch: %+v", got)
	}
	if got.Identity != ev.Identity {
		t.Fatalf("identity mismatch: got %+v want %+v", got.Identity, ev.Identity)
	}
	if !got.OccurredAt.Equal(ev.OccurredAt) {
		t.Fatalf("OccurredAt mismatch: got %v want %v", got.OccurredAt, ev.OccurredAt)
	}
	rm, ok := got.Payload.(events.RedactedMap)
	if !ok || rm.Data["k"] != "v" {
		t.Fatalf("payload mismatch: %+v", got.Payload)
	}
}

func TestMarshalPayload_NilRejected(t *testing.T) {
	if _, err := marshalPayload(nil); err == nil {
		t.Fatalf("expected error for nil payload")
	}
}

func TestMarshalPayload_ScalarWrapped(t *testing.T) {
	m, err := marshalPayload(stringScalarPayload{v: "hello"})
	if err != nil {
		t.Fatalf("marshalPayload scalar: %v", err)
	}
	if m["value"] != "hello" {
		t.Fatalf("expected scalar wrapped under 'value', got %v", m)
	}
}

func TestMarshalPayload_StructToMap(t *testing.T) {
	type structPayload struct {
		events.Sealed
		Field string
	}
	m, err := marshalPayload(structPayload{Field: "x"})
	if err != nil {
		t.Fatalf("marshalPayload struct: %v", err)
	}
	if m["Field"] != "x" {
		t.Fatalf("expected Field round-tripped, got %v", m)
	}
}

func TestDecodeHead_EmptyBytes(t *testing.T) {
	h, err := decodeHead(nil)
	if err != nil {
		t.Fatalf("decodeHead(nil): %v", err)
	}
	if len(h.Sequences) != 0 {
		t.Fatalf("expected empty head, got %+v", h)
	}
}

func TestEncodeDecodeHead_RoundTrip(t *testing.T) {
	h := headRecord{Sequences: []uint64{1, 2, 5, 9}}
	b, err := encodeHead(h)
	if err != nil {
		t.Fatalf("encodeHead: %v", err)
	}
	got, err := decodeHead(b)
	if err != nil {
		t.Fatalf("decodeHead: %v", err)
	}
	if len(got.Sequences) != 4 || got.Sequences[3] != 9 {
		t.Fatalf("head round-trip mismatch: %+v", got)
	}
}

func TestDecodeHead_Corrupt(t *testing.T) {
	if _, err := decodeHead([]byte("{not json")); err == nil {
		t.Fatalf("expected error for corrupt head bytes")
	}
}

func TestDecodeEvent_Corrupt(t *testing.T) {
	if _, err := decodeEvent([]byte("{not json")); err == nil {
		t.Fatalf("expected error for corrupt event bytes")
	}
}

func TestUnixNanoToTime_Zero(t *testing.T) {
	if !unixNanoToTime(0).IsZero() {
		t.Fatalf("expected zero time for unix nano 0")
	}
}

func TestSeqToken_SortsLexicographically(t *testing.T) {
	if seqToken(2) >= seqToken(10) {
		t.Fatalf("seqToken must sort lexicographically: %q vs %q", seqToken(2), seqToken(10))
	}
}

func TestWrapRedacted_Variants(t *testing.T) {
	// Already an EventPayload — passed through.
	rm := events.RedactedMap{Data: map[string]any{"a": 1}}
	if got, ok := wrapRedacted(rm).(events.RedactedMap); !ok || got.Data["a"] != 1 {
		t.Fatalf("expected EventPayload passthrough, got %T", wrapRedacted(rm))
	}
	// A bare map — wrapped in RedactedMap.
	got := wrapRedacted(map[string]any{"b": 2})
	if g, ok := got.(events.RedactedMap); !ok || g.Data["b"] != 2 {
		t.Fatalf("expected map wrapped in RedactedMap, got %T", got)
	}
	// A scalar — wrapped under "value".
	got = wrapRedacted("scalar")
	if g, ok := got.(events.RedactedMap); !ok || g.Data["value"] != "scalar" {
		t.Fatalf("expected scalar wrapped under 'value', got %T", got)
	}
}

// scalarPayload is referenced to keep the type used; it documents the
// EventPayload seal requirement for test payloads.
var _ events.EventPayload = scalarPayload{}
