package session

import (
	"encoding/json"
	"errors"
	"fmt"
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
		"source alias":          `{"version":4,"checkpoint":{"source_through":{"RUN_ID":"source"}}}`,
		"sequence duplicate":    `{"version":4,"active":[{"sequence":1,"sequence":2}]}`,
		"evidence source alias": `{"version":4,"evidence":[{"admission":{"Sequence":1}}]}`,
		"unclosed array":        `{"version":3,"active":[`,
		"wrong array end":       `{"version":3,"active":[{}}}`,
		"null then trailing":    `null {}`,
		"scalar host":           `17`,
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

// Exercise realistic bounded receipt bodies. Host-shape validation must not
// repeatedly decode or copy opaque results at every containing object/array.
func BenchmarkRetainedDecode_Window(b *testing.B) {
	for _, turns := range []int{1, 16, 32} {
		b.Run(fmt.Sprint(turns), func(b *testing.B) {
			body := json.RawMessage(`{"source":"` + strings.Repeat("x", 14585) + `","resource_id":"doc-a","version":9007199254740993,"more":false}`)
			window := retainedWindow{Version: retainedContextVersion}
			for range turns {
				window.Turns = append(window.Turns, retainedTurn{Status: "complete", Steps: []json.RawMessage{body}})
			}
			encoded, err := json.Marshal(window)
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(encoded)))
			b.ReportAllocs()
			for b.Loop() {
				var restored retainedWindow
				if err := decodeRetained(encoded, &restored); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestRetainedDecode_LargeOpaquePayloadAndFollowingHostFields(t *testing.T) {
	t.Parallel()
	payload := `{"source":"` + strings.Repeat("x", 14585) + `","resource_id":"doc-a","version":9007199254740993,"more":false}`
	for _, sibling := range []bool{false, true} {
		for _, host := range []string{`"status":"complete"`, `"status":"interrupted","status":"complete"`, `"Status":"complete"`} {
			input := `{"version":3,"turns":[{"steps":[` + payload + `],` + host + `}]}`
			if sibling {
				// Repeating the same canonical field in a sibling object is valid.
				input = `{"version":3,"turns":[{"steps":[` + payload + `],"status":"complete"},{` + host + `}]}`
			}
			var window retainedWindow
			err := decodeRetained([]byte(input), &window)
			if host == `"status":"complete"` {
				if err != nil || string(window.Turns[0].Steps[0]) != payload {
					t.Fatalf("opaque payload or sibling boundary changed: %v", err)
				}
			} else if !errors.Is(err, ErrRetainedContextUnavailable) {
				t.Fatalf("host metadata after an opaque value bypassed validation: %v", err)
			}
		}
	}
}
