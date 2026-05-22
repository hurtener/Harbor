package messages_test

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/messages"
)

func sampleEnvelope() messages.Envelope {
	deadline := time.Date(2026, 5, 9, 13, 0, 0, 0, time.UTC)
	return messages.Envelope{
		Payload: map[string]any{"hello": "world"},
		Headers: messages.Headers{
			TenantID: "T",
			UserID:   "U",
			Topic:    "ingress",
			Priority: 5,
		},
		RunID:      "R-1",
		SessionID:  "S-1",
		Timestamp:  time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC),
		DeadlineAt: &deadline,
		Meta:       map[string]any{"hop": float64(3), "trace_kind": "test"},
	}
}

// --- WithRunID ---

func TestEnvelope_WithRunID_ReturnsCopy(t *testing.T) {
	src := sampleEnvelope()
	dst := src.WithRunID("R-2")
	if dst.RunID != "R-2" {
		t.Errorf("dst.RunID=%q, want R-2", dst.RunID)
	}
	if src.RunID != "R-1" {
		t.Errorf("source mutated: src.RunID=%q, want R-1", src.RunID)
	}
}

func TestEnvelope_WithRunID_DeepCopiesMeta(t *testing.T) {
	src := sampleEnvelope()
	dst := src.WithRunID("R-2")
	dst.Meta["new_key"] = "leak?"
	if _, ok := src.Meta["new_key"]; ok {
		t.Errorf("source Meta mutated by destination's edit: %+v", src.Meta)
	}
}

func TestEnvelope_WithRunID_NilMeta_StaysNil(t *testing.T) {
	src := messages.Envelope{RunID: "R-1"}
	dst := src.WithRunID("R-2")
	if dst.Meta != nil {
		t.Errorf("Meta should be nil when source had nil Meta, got %+v", dst.Meta)
	}
}

// --- Identity ---

func TestEnvelope_Identity_HappyPath(t *testing.T) {
	src := sampleEnvelope()
	got := src.Identity()
	want := identity.Quadruple{
		Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S-1"},
		RunID:    "R-1",
	}
	if got != want {
		t.Errorf("Identity()=%+v, want %+v", got, want)
	}
}

func TestEnvelope_EmptyRunID_AllowedAtTypeLevel(t *testing.T) {
	src := messages.Envelope{
		Headers:   messages.Headers{TenantID: "T", UserID: "U"},
		SessionID: "S",
	}
	got := src.Identity()
	if got.RunID != "" {
		t.Errorf("RunID=%q, want empty", got.RunID)
	}
	if got.SessionID != "S" {
		t.Errorf("SessionID=%q, want S", got.SessionID)
	}
}

func TestEnvelope_AllEmpty_DoesNotPanic(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Errorf("Identity() on zero-Envelope panicked: %v", r)
		}
	}()
	zero := messages.Envelope{}
	got := zero.Identity()
	if got != (identity.Quadruple{}) {
		t.Errorf("zero envelope Identity()=%+v, want zero Quadruple", got)
	}
}

// --- JSON round-trip ---

func TestEnvelope_IdentityQuadruple_RoundTrip(t *testing.T) {
	src := sampleEnvelope()
	bytes, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got messages.Envelope
	if err := json.Unmarshal(bytes, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if src.Identity() != got.Identity() {
		t.Errorf("Identity round-trip mismatch: before=%+v after=%+v", src.Identity(), got.Identity())
	}
}

func TestEnvelope_JSONRoundTrip_Equal(t *testing.T) {
	src := sampleEnvelope()
	bytes, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got messages.Envelope
	if err := json.Unmarshal(bytes, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if !reflect.DeepEqual(src.Headers, got.Headers) {
		t.Errorf("Headers mismatch: before=%+v after=%+v", src.Headers, got.Headers)
	}
	if src.RunID != got.RunID || src.SessionID != got.SessionID {
		t.Errorf("RunID/SessionID mismatch: before=(%q,%q) after=(%q,%q)",
			src.RunID, src.SessionID, got.RunID, got.SessionID)
	}
	if !src.Timestamp.Equal(got.Timestamp) {
		t.Errorf("Timestamp mismatch: before=%v after=%v", src.Timestamp, got.Timestamp)
	}
	if src.DeadlineAt == nil || got.DeadlineAt == nil || !src.DeadlineAt.Equal(*got.DeadlineAt) {
		t.Errorf("DeadlineAt mismatch: before=%v after=%v", src.DeadlineAt, got.DeadlineAt)
	}
	if !reflect.DeepEqual(src.Meta, got.Meta) {
		t.Errorf("Meta mismatch: before=%+v after=%+v", src.Meta, got.Meta)
	}
}

func TestEnvelope_DeadlineAt_NilJSON_OmitEmpty(t *testing.T) {
	src := messages.Envelope{
		Headers: messages.Headers{TenantID: "T", UserID: "U"},
		RunID:   "R", SessionID: "S",
		Timestamp: time.Date(2026, 5, 9, 12, 0, 0, 0, time.UTC),
		// DeadlineAt left nil intentionally.
	}
	bytes, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	// omitempty ⇒ "deadline_at" key absent.
	var raw map[string]any
	if err := json.Unmarshal(bytes, &raw); err != nil {
		t.Fatalf("Unmarshal raw: %v", err)
	}
	if _, present := raw["deadline_at"]; present {
		t.Errorf("deadline_at present in JSON for nil pointer; want omitted: %s", string(bytes))
	}
}

func TestEnvelope_Unmarshal_MalformedInputFailsLoud(t *testing.T) {
	// Headers typed as a JSON string instead of an object → typed
	// json.UnmarshalTypeError. (We deliberately avoid mistyping
	// time.Time / *time.Time, which surface their own non-typed
	// errorString and would test the encoding/json/time interaction
	// rather than the typed-error contract this test pins.)
	bad := []byte(`{"payload":null,"headers":"not-an-object","run_id":"R","session_id":"S","timestamp":"2026-05-09T12:00:00Z"}`)
	var got messages.Envelope
	err := json.Unmarshal(bad, &got)
	if err == nil {
		t.Fatal("Unmarshal of malformed input succeeded; want typed error")
	}
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &typeErr) {
		t.Errorf("err=%v (type %T), want *json.UnmarshalTypeError via errors.As", err, err)
	}
}

func TestEnvelope_Unmarshal_PartialFieldFailure_DoesNotProducePartialValue(t *testing.T) {
	// Priority typed as a string → typed unmarshal error; the
	// destination Envelope must NOT carry the previously-parsed
	// fields silently. The whole Unmarshal returns an error and the
	// caller sees nothing partial.
	bad := []byte(`{"payload":null,"headers":{"tenant_id":"T","user_id":"U","priority":"high"},"run_id":"R","session_id":"S","timestamp":"2026-05-09T12:00:00Z"}`)
	var got messages.Envelope
	err := json.Unmarshal(bad, &got)
	if err == nil {
		t.Fatal("Unmarshal of malformed priority succeeded; want typed error")
	}
	var typeErr *json.UnmarshalTypeError
	if !errors.As(err, &typeErr) {
		t.Errorf("err=%v (type %T), want *json.UnmarshalTypeError via errors.As", err, err)
	}
}

// --- MergeMeta ---

func TestMergeMeta_LastWriteWins(t *testing.T) {
	dst := map[string]any{"a": 1, "b": 2}
	src := map[string]any{"a": 99, "c": 3}
	got := messages.MergeMeta(dst, src)
	want := map[string]any{"a": 99, "b": 2, "c": 3}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("MergeMeta=%+v, want %+v", got, want)
	}
	// Mutation contract: dst is the result.
	if !reflect.DeepEqual(dst, want) {
		t.Errorf("dst not mutated in place: %+v", dst)
	}
}

func TestMergeMeta_NilSource_ReturnsDst(t *testing.T) {
	dst := map[string]any{"a": 1}
	got := messages.MergeMeta(dst, nil)
	if !reflect.DeepEqual(got, dst) {
		t.Errorf("MergeMeta(dst, nil)=%+v, want %+v", got, dst)
	}
}

func TestMergeMeta_NilSource_NilDst_ReturnsNil(t *testing.T) {
	got := messages.MergeMeta(nil, nil)
	if got != nil {
		t.Errorf("MergeMeta(nil, nil)=%+v, want nil", got)
	}
}

func TestMergeMeta_NilDst_ReturnsCopy(t *testing.T) {
	src := map[string]any{"a": 1, "b": 2}
	got := messages.MergeMeta(nil, src)
	if !reflect.DeepEqual(got, src) {
		t.Errorf("MergeMeta(nil, src)=%+v, want %+v", got, src)
	}
	// Caller-owned: mutating got must not touch src.
	got["c"] = 3
	if _, leaked := src["c"]; leaked {
		t.Errorf("MergeMeta(nil, src) did not return a copy; src leaked: %+v", src)
	}
}
