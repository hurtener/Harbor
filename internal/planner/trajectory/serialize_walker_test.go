package trajectory_test

import (
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

// TestWalker_NilInterface_OK — a nil interface in an `any`-valued
// field is JSON-encodable (encodes as `null`).
func TestWalker_NilInterface_OK(t *testing.T) {
	tr := &trajectory.Trajectory{
		LLMContext: map[string]any{
			"explicit_nil": nil,
		},
	}
	_, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Serialize(nil-interface) err = %v want nil", err)
	}
}

// TestWalker_NilPointer_OK — a nil *T pointer encodes as `null`.
func TestWalker_NilPointer_OK(t *testing.T) {
	tr := &trajectory.Trajectory{
		Summary: nil, // *Summary nil pointer
	}
	_, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Serialize(nil-pointer) err = %v want nil", err)
	}
}

// TestWalker_NonNilPointer_OK — a non-nil *T pointer to a valid leaf
// encodes successfully.
func TestWalker_NonNilPointer_OK(t *testing.T) {
	tr := &trajectory.Trajectory{
		Summary: &trajectory.Summary{Note: "hello"},
	}
	_, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Serialize(non-nil-pointer) err = %v want nil", err)
	}
}

// TestWalker_BytesField_OK — []byte values are JSON-encoded as base64
// strings; the walker accepts them.
func TestWalker_BytesField_OK(t *testing.T) {
	tr := &trajectory.Trajectory{
		Steps: []trajectory.Step{{
			Streams: map[string][]trajectory.StreamChunk{
				"out": {{Data: []byte("hello world")}},
			},
		}},
	}
	_, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Serialize([]byte) err = %v want nil", err)
	}
}

// TestWalker_TimeMarshalsAsJSONMarshaler — time.Time implements
// json.Marshaler; the walker probes it via reflect, which exercises
// the addressable / unaddressable branches.
func TestWalker_TimeMarshalsAsJSONMarshaler(t *testing.T) {
	when := time.Date(2026, time.May, 12, 12, 0, 0, 0, time.UTC)
	tr := &trajectory.Trajectory{
		Steps: []trajectory.Step{{StartedAt: when}},
	}
	out, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Serialize(time) err = %v want nil", err)
	}
	// The marshalled bytes contain the canonical RFC3339 timestamp.
	if !has(string(out), "2026-05-12T12:00:00") {
		t.Errorf("time did not appear in canonical form: %s", out)
	}
}

// TestWalker_TimeInAnyField — time.Time wrapped in an `any`-valued
// field (the unaddressable path of the JSON-marshaler probe).
func TestWalker_TimeInAnyField(t *testing.T) {
	when := time.Date(2026, time.May, 12, 12, 0, 0, 0, time.UTC)
	tr := &trajectory.Trajectory{
		LLMContext: map[string]any{
			"timestamp": when,
		},
	}
	_, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Serialize(time-in-any) err = %v want nil", err)
	}
}

// TestWalker_StringSlice_OK — slices of strings encode fine.
func TestWalker_StringSlice_OK(t *testing.T) {
	tr := &trajectory.Trajectory{
		Summary: &trajectory.Summary{
			Goals: []string{"a", "b", "c"},
		},
	}
	_, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Serialize(string-slice) err = %v want nil", err)
	}
}

// TestWalker_ArrayValue_OK — fixed-size arrays (not slices) encode.
func TestWalker_ArrayValue_OK(t *testing.T) {
	type wrapper struct {
		Arr [3]int `json:"arr"`
	}
	tr := &trajectory.Trajectory{
		LLMContext: map[string]any{
			"wrap": wrapper{Arr: [3]int{1, 2, 3}},
		},
	}
	_, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Serialize(array) err = %v want nil", err)
	}
}

// TestWalker_NilSlice_OK — a nil slice encodes as JSON null; walker
// short-circuits cleanly.
func TestWalker_NilSlice_OK(t *testing.T) {
	tr := &trajectory.Trajectory{
		Steps: nil,
	}
	_, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Serialize(nil-slice) err = %v want nil", err)
	}
}

// TestWalker_NilMap_OK — a nil map encodes as JSON null.
func TestWalker_NilMap_OK(t *testing.T) {
	tr := &trajectory.Trajectory{
		LLMContext: nil,
	}
	_, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Serialize(nil-map) err = %v want nil", err)
	}
}

// TestWalker_FailingMarshaler — a struct whose MarshalJSON method
// returns an error surfaces as ErrUnserializable.
func TestWalker_FailingMarshaler(t *testing.T) {
	tr := &trajectory.Trajectory{
		LLMContext: map[string]any{
			"bad_marshaler": failingMarshaler{},
		},
	}
	_, err := tr.Serialize()
	var unserr trajectory.ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Fatalf("err = %v want ErrUnserializable on failing MarshalJSON", err)
	}
}

// failingMarshaler always returns an error from MarshalJSON.
type failingMarshaler struct{}

func (failingMarshaler) MarshalJSON() ([]byte, error) {
	return nil, errors.New("forced marshaler failure")
}

// TestWalker_StructAddressable — a struct embedded as a struct field
// is addressable; the walker handles the addressable json.Marshaler
// branch.
func TestWalker_StructAddressable_Time(t *testing.T) {
	tr := &trajectory.Trajectory{
		Background: map[string]trajectory.BackgroundResult{
			"g": {
				GroupID:    "g",
				Status:     "completed",
				ResolvedAt: time.Now().UTC(),
			},
		},
	}
	_, err := tr.Serialize()
	if err != nil {
		t.Fatalf("Serialize err = %v want nil", err)
	}
}
