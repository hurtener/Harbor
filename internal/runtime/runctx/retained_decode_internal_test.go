package runctx

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestRetainedDecode_HostFieldsAndOpaqueBoundaries(t *testing.T) {
	t.Parallel()
	for name, data := range map[string]string{
		"escaped duplicate":     `{"version":3,"\u0076ersion":3}`,
		"nested turn alias":     `{"version":3,"turns":[{"Status":"complete"}]}`,
		"nested turn duplicate": `{"version":3,"turns":[{"status":"interrupted","status":"complete"}]}`,
		"checkpoint alias":      `{"version":3,"checkpoint":{"Generation":1}}`,
		"narrative duplicate":   `{"version":3,"checkpoint":{"narrative":{"facts":[],"facts":["x"]}}}`,
		"source alias":          `{"version":3,"checkpoint":{"sources":[{"RUN_ID":"source"}]}}`,
		"array shape":           `{"version":3,"active":{}}`,
		"member shape":          `{"version":3,"active":[1]}`,
		"unexpected field":      `{"version":3,"private":"PRIVATE-CONTENT"}`,
		"unclosed":              `{"version":3`,
		"trailing":              `{"version":3} {}`,
		"invalid text":          string([]byte{255}),
	} {
		t.Run(name, func(t *testing.T) {
			var window retainedWindow
			err := decodeRetained([]byte(data), &window)
			if !errors.Is(err, ErrRetainedContextUnavailable) || strings.Contains(err.Error(), "PRIVATE-CONTENT") {
				t.Fatalf("invalid host state not rejected safely: %v", err)
			}
		})
	}
	// Field ordering and whitespace are not part of authority; only canonical
	// spelling and unambiguous host values are enforced. A payload stays opaque.
	input := []byte(` { "turns": [{"steps": [{"Pending":true,"pending":false,"n":9007199254740993}]}], "checkpoint":null, "version":3 } `)
	var window retainedWindow
	if err := decodeRetained(input, &window); err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(window.Turns[0].Steps[0], &payload); err != nil {
		t.Fatal(err)
	}
	if string(payload["Pending"]) != "true" || string(payload["pending"]) != "false" || string(payload["n"]) != "9007199254740993" {
		t.Fatal("opaque result changed")
	}
	if err := decodeRetained([]byte(`{}`), nil); !errors.Is(err, ErrRetainedContextUnavailable) {
		t.Fatal("nil destination accepted")
	}
}
