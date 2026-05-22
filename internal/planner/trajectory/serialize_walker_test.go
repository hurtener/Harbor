package trajectory_test

import (
	"errors"
	"strings"
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

// --- ValidateEncodable: the exported reusable-walker entry point -------
//
// ValidateEncodable is the same walker Trajectory.Serialize's
// pre-flight pass uses, exported (Phase 51) so other runtime serialise
// contracts — the pauseresume pause-record envelope is the first
// consumer — share it rather than forking a second fail-loudly walker
// (CLAUDE.md §13, D-069). These tests pin the exported entry directly.

// TestValidateEncodable_OK_RootedPath — a fully-encodable value passes
// and the root prefix is the caller's, not "Trajectory".
func TestValidateEncodable_OK_RootedPath(t *testing.T) {
	v := map[string]any{
		"region": "eu-west-1",
		"scopes": []any{"read", "write"},
		"count":  float64(3),
	}
	if err := trajectory.ValidateEncodable(v, "PauseRecord.payload"); err != nil {
		t.Fatalf("ValidateEncodable on an encodable value: %v", err)
	}
}

// TestValidateEncodable_FailLoud_FunctionLeaf — a func leaf fails loud
// with ErrUnserializable whose Field path is rooted at the caller's
// own prefix (not "Trajectory").
func TestValidateEncodable_FailLoud_FunctionLeaf(t *testing.T) {
	v := map[string]any{"callback": func() {}}
	err := trajectory.ValidateEncodable(v, "PauseRecord.payload")
	var unserr trajectory.ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Fatalf("err = %v, want trajectory.ErrUnserializable", err)
	}
	if !strings.Contains(unserr.Field, "PauseRecord.payload") {
		t.Errorf("Field %q is not rooted at the caller-supplied prefix", unserr.Field)
	}
	if !strings.Contains(unserr.Field, "callback") {
		t.Errorf("Field %q does not name the offending 'callback' key", unserr.Field)
	}
}

// TestValidateEncodable_FailLoud_ChannelInStruct — ValidateEncodable
// walks struct fields too (not just maps), so a non-encodable leaf
// nested in a struct field surfaces with a dotted path.
func TestValidateEncodable_FailLoud_ChannelInStruct(t *testing.T) {
	type envelope struct {
		Payload map[string]any `json:"payload"`
		Name    string         `json:"name"`
	}
	v := envelope{Name: "x", Payload: map[string]any{"sock": make(chan int)}}
	err := trajectory.ValidateEncodable(v, "Envelope")
	var unserr trajectory.ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Fatalf("err = %v, want trajectory.ErrUnserializable on a struct-nested channel", err)
	}
	if !strings.Contains(unserr.Field, "payload") || !strings.Contains(unserr.Field, "sock") {
		t.Errorf("Field %q should name the payload.sock path", unserr.Field)
	}
}

// TestValidateEncodable_SharesSerializeWalker — ValidateEncodable and
// Trajectory.Serialize's pre-flight produce the same ErrUnserializable
// type for the same offending shape: the observable proof Serialize is
// re-pointed at the exported entry (one walker, not two).
func TestValidateEncodable_SharesSerializeWalker(t *testing.T) {
	tr := &trajectory.Trajectory{HintState: map[string]any{"f": func() {}}}
	_, serErr := tr.Serialize()

	vErr := trajectory.ValidateEncodable(*tr, "Trajectory")

	var serUnser, vUnser trajectory.ErrUnserializable
	if !errors.As(serErr, &serUnser) {
		t.Fatalf("Serialize err = %v, want trajectory.ErrUnserializable", serErr)
	}
	if !errors.As(vErr, &vUnser) {
		t.Fatalf("ValidateEncodable err = %v, want trajectory.ErrUnserializable", vErr)
	}
	if serUnser.Field != vUnser.Field {
		t.Errorf("Serialize and ValidateEncodable disagree on the field path: %q vs %q",
			serUnser.Field, vUnser.Field)
	}
}
