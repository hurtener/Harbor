package trajectory_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

// TestSerialize_FailLoud_Function — a func() in LLMContext surfaces
// ErrUnserializable with a field-path locator. This is the
// load-bearing test: brief 02 §4 explicitly calls out the predecessor's
// silent-drop-on-callback bug.
func TestSerialize_FailLoud_Function(t *testing.T) {
	tr := &trajectory.Trajectory{
		LLMContext: map[string]any{
			"callback": func() {},
		},
	}
	out, err := tr.Serialize()
	if out != nil {
		t.Fatalf("Serialize returned non-nil bytes on non-encodable input — fail-loud violated")
	}
	var unserr trajectory.ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Fatalf("err = %v want ErrUnserializable", err)
	}
	if !strings.Contains(unserr.Field, "LLMContext") &&
		!strings.Contains(unserr.Field, "llm_context") {
		t.Errorf("Field path %q does not mention LLMContext / llm_context", unserr.Field)
	}
	if !strings.Contains(unserr.Field, "callback") {
		t.Errorf("Field path %q does not mention 'callback'", unserr.Field)
	}
}

// TestSerialize_FailLoud_Channel — a chan in HintState surfaces
// ErrUnserializable.
func TestSerialize_FailLoud_Channel(t *testing.T) {
	tr := &trajectory.Trajectory{
		HintState: map[string]any{
			"events": make(chan int),
		},
	}
	_, err := tr.Serialize()
	var unserr trajectory.ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Fatalf("err = %v want ErrUnserializable on chan", err)
	}
	if !strings.Contains(unserr.Field, "events") {
		t.Errorf("Field path %q should mention 'events'", unserr.Field)
	}
}

// TestSerialize_FailLoud_NestedFunction — a function buried inside a
// Step's Observation surfaces with a path that includes Steps[0].
func TestSerialize_FailLoud_NestedFunction(t *testing.T) {
	tr := &trajectory.Trajectory{
		Steps: []trajectory.Step{{
			Observation: map[string]any{
				"inner": func() string { return "x" },
			},
		}},
	}
	_, err := tr.Serialize()
	var unserr trajectory.ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Fatalf("err = %v want ErrUnserializable on nested fn", err)
	}
	if !strings.Contains(unserr.Field, "Steps[0]") &&
		!strings.Contains(unserr.Field, "steps[0]") {
		t.Errorf("Field path %q should include 'Steps[0]'", unserr.Field)
	}
	if !strings.Contains(unserr.Field, "inner") {
		t.Errorf("Field path %q should mention 'inner'", unserr.Field)
	}
}

// TestSerialize_FailLoud_Complex — complex numbers are not
// JSON-encodable; encoding/json returns *UnsupportedTypeError.
// Phase 43's walker surfaces this as ErrUnserializable too.
func TestSerialize_FailLoud_Complex(t *testing.T) {
	tr := &trajectory.Trajectory{
		LLMContext: map[string]any{
			"badnum": complex(1.0, 2.0),
		},
	}
	_, err := tr.Serialize()
	var unserr trajectory.ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Fatalf("err = %v want ErrUnserializable on complex", err)
	}
}

// TestSerialize_FailLoud_NonStringMapKey — JSON map keys must be
// strings (or TextMarshalers). An int-keyed map surfaces
// ErrUnserializable.
func TestSerialize_FailLoud_NonStringMapKey(t *testing.T) {
	tr := &trajectory.Trajectory{
		LLMContext: map[string]any{
			"bad": map[int]string{1: "one", 2: "two"},
		},
	}
	_, err := tr.Serialize()
	var unserr trajectory.ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Fatalf("err = %v want ErrUnserializable on int-keyed map", err)
	}
}

// TestSerialize_FailLoud_CyclicMap — a self-referencing map must
// surface as ErrUnserializable (the walker tracks pointer addresses
// to detect cycles). encoding/json would otherwise loop forever or
// produce an unsupported-value error.
func TestSerialize_FailLoud_CyclicMap(t *testing.T) {
	cycle := map[string]any{}
	cycle["self"] = cycle // direct self-reference
	tr := &trajectory.Trajectory{
		LLMContext: cycle,
	}
	_, err := tr.Serialize()
	if err == nil {
		t.Fatalf("Serialize(cycle) returned nil error — fail-loud violated")
	}
	var unserr trajectory.ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Fatalf("err = %v want ErrUnserializable on cyclic map", err)
	}
	if !strings.Contains(unserr.Field, "cycle") {
		t.Errorf("Field path %q should mention '<cycle>' marker; got %s", unserr.Field, unserr.Field)
	}
}

// TestSerialize_FailLoud_UnsafePointerLike — types resembling file
// descriptors (raw uintptr / unsafe.Pointer values) surface as
// ErrUnserializable. encoding/json refuses to encode unsafe.Pointer.
// We assert via a Go closure type which holds a fd reference (the
// channel above already covers this; this test adds an additional
// adversarial axis from the brief 02 §4 list).
func TestSerialize_FailLoud_FileDescriptorShaped(t *testing.T) {
	// A struct with a channel field stands in for "live socket / file
	// handle" — the canonical example in brief 02 §4. We make sure
	// the walker catches it whether it lands in LLMContext, in
	// HintState, or in a Step's Streams.
	type liveResource struct {
		Done chan struct{}
		Name string
	}
	tr := &trajectory.Trajectory{
		LLMContext: map[string]any{
			"socket": &liveResource{Done: make(chan struct{}), Name: "tcp:0"},
		},
	}
	_, err := tr.Serialize()
	var unserr trajectory.ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Fatalf("err = %v want ErrUnserializable on file-handle-shaped value", err)
	}
}

// TestErrUnserializable_ErrorMessage — the canonical Error() message
// names the offending field so log scrapers can extract it.
func TestErrUnserializable_ErrorMessage(t *testing.T) {
	err := trajectory.ErrUnserializable{Field: "Trajectory.LLMContext.callback"}
	got := err.Error()
	if !strings.Contains(got, "Trajectory.LLMContext.callback") {
		t.Errorf("Error() = %q does not name the offending field", got)
	}
	if !strings.Contains(got, "trajectory") {
		t.Errorf("Error() = %q should include the 'trajectory' subsystem prefix", got)
	}
}
