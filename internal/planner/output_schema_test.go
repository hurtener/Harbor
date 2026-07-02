package planner

import (
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
)

const testObjectSchema = `{
	"type": "object",
	"required": ["sentiment"],
	"properties": {
		"sentiment": {"type": "string", "enum": ["positive", "negative", "neutral"]},
		"score": {"type": "number"}
	},
	"additionalProperties": false
}`

// TestCompileOutputSchema_RejectsEmpty — a nil/empty/whitespace schema
// is a loud config error, never a silent no-op (CLAUDE.md §13).
func TestCompileOutputSchema_RejectsEmpty(t *testing.T) {
	t.Parallel()
	for _, raw := range []json.RawMessage{nil, json.RawMessage(""), json.RawMessage("   \n\t")} {
		if _, err := CompileOutputSchema(raw); err == nil {
			t.Errorf("CompileOutputSchema(%q) = nil error, want a loud error", string(raw))
		}
	}
}

// TestCompileOutputSchema_RejectsInvalid — an un-parseable / un-
// compilable schema fails loud naming the fault.
func TestCompileOutputSchema_RejectsInvalid(t *testing.T) {
	t.Parallel()
	if _, err := CompileOutputSchema(json.RawMessage(`{not json`)); err == nil {
		t.Error("CompileOutputSchema(bad json) = nil error, want a parse error")
	}
	if _, err := CompileOutputSchema(json.RawMessage(`{"type": 123}`)); err == nil {
		t.Error("CompileOutputSchema(invalid schema) = nil error, want a compile error")
	}
}

// TestOutputSchemaValidator_ValidateTable — valid / invalid / non-JSON /
// empty payloads validate as expected.
func TestOutputSchemaValidator_ValidateTable(t *testing.T) {
	t.Parallel()
	v, err := CompileOutputSchema(json.RawMessage(testObjectSchema))
	if err != nil {
		t.Fatalf("CompileOutputSchema: %v", err)
	}
	cases := []struct {
		name    string
		payload string
		wantErr bool
	}{
		{"conforming", `{"sentiment":"positive","score":0.9}`, false},
		{"conforming-minimal", `{"sentiment":"neutral"}`, false},
		{"missing-required", `{"score":0.1}`, true},
		{"bad-enum", `{"sentiment":"ecstatic"}`, true},
		{"additional-prop", `{"sentiment":"positive","extra":1}`, true},
		{"non-json", `not json at all`, true},
		{"empty-is-null", ``, true}, // null fails an object schema — no vacuous pass
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := v.Validate(json.RawMessage(tc.payload))
			if tc.wantErr && err == nil {
				t.Errorf("Validate(%q) = nil, want error", tc.payload)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("Validate(%q) = %v, want nil", tc.payload, err)
			}
		})
	}
}

// TestOutputSchemaValidator_RawIsImmutableCopy — Raw() returns the
// validator's own copy; mutating the caller's original slice after
// compilation does not change what the validator enforces (D-025
// immutability).
func TestOutputSchemaValidator_RawIsImmutableCopy(t *testing.T) {
	t.Parallel()
	raw := json.RawMessage(testObjectSchema)
	v, err := CompileOutputSchema(raw)
	if err != nil {
		t.Fatalf("CompileOutputSchema: %v", err)
	}
	// Mutate the caller's slice; the validator's Raw() copy must be intact.
	for i := range raw {
		raw[i] = 'x'
	}
	if !strings.Contains(string(v.Raw()), "sentiment") {
		t.Errorf("Raw() was aliased to the caller's slice: %s", v.Raw())
	}
	if err := v.Validate(json.RawMessage(`{"sentiment":"positive"}`)); err != nil {
		t.Errorf("Validate after caller mutation = %v, want nil (compiled schema is immutable)", err)
	}
}

// TestOutputSchemaValidator_NilIsNoOp — a nil validator passes any
// payload (no schema constrained the run).
func TestOutputSchemaValidator_NilIsNoOp(t *testing.T) {
	t.Parallel()
	var v *OutputSchemaValidator
	if err := v.Validate(json.RawMessage(`anything`)); err != nil {
		t.Errorf("nil validator Validate = %v, want nil", err)
	}
	if v.Raw() != nil {
		t.Errorf("nil validator Raw() = %v, want nil", v.Raw())
	}
}

// TestOutputSchemaValidator_ConcurrentValidate — N concurrent
// Validate calls against ONE shared compiled validator are race-free
// (D-025). Run under -race.
func TestOutputSchemaValidator_ConcurrentValidate(t *testing.T) {
	t.Parallel()
	v, err := CompileOutputSchema(json.RawMessage(testObjectSchema))
	if err != nil {
		t.Fatalf("CompileOutputSchema: %v", err)
	}
	const n = 200
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				if err := v.Validate(json.RawMessage(`{"sentiment":"positive"}`)); err != nil {
					t.Errorf("concurrent Validate (valid) = %v", err)
				}
			} else {
				if err := v.Validate(json.RawMessage(`{"score":1}`)); err == nil {
					t.Error("concurrent Validate (invalid) = nil, want error")
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestErrOutputInvalid_IsComparable — the sentinel is stable and
// wrap-comparable via errors.Is.
func TestErrOutputInvalid_IsComparable(t *testing.T) {
	t.Parallel()
	wrapped := errors.Join(ErrOutputInvalid, errors.New("schema said no"))
	if !errors.Is(wrapped, ErrOutputInvalid) {
		t.Error("errors.Is(wrapped, ErrOutputInvalid) = false, want true")
	}
}
