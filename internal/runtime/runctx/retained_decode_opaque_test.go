package runctx

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestRetainedDecode_OpaqueRootsStillValidate(t *testing.T) {
	t.Parallel()
	for name, data := range map[string]string{
		"malformed":        `{"source":`,
		"trailing":         `{"source":"ok"} false`,
		"bad escape":       `{"source":"\q"}`,
		"bad number":       `{"version":01}`,
		"invalid encoding": "{\"source\":\"\xff\"}",
	} {
		t.Run(name, func(t *testing.T) {
			var value any
			if err := decodeRetained([]byte(data), &value); !errors.Is(err, ErrRetainedContextUnavailable) {
				t.Fatalf("opaque root accepted invalid input: %v", err)
			}
		})
	}
	var evidence any
	if err := decodeRetained([]byte(`{"version":1,"Version":2,"version":9007199254740993,"more":false}`), &evidence); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(evidence, map[string]any{"version": json.Number("9007199254740993"), "Version": json.Number("2"), "more": false}) {
		t.Fatal("opaque tool keys or exact numbers were changed")
	}
	var timestamp time.Time
	if err := decodeRetained([]byte(`"not a timestamp"`), &timestamp); !errors.Is(err, ErrRetainedContextUnavailable) {
		t.Fatal("custom scalar validation bypassed")
	}
	var number int
	if err := decodeRetained([]byte(`"not a number"`), &number); !errors.Is(err, ErrRetainedContextUnavailable) {
		t.Fatal("scalar type validation bypassed")
	}
}

func TestRetainedDecode_DiscardStillRejectsMalformedOpaqueLeaves(t *testing.T) {
	t.Parallel()
	for _, data := range []string{
		`{"version":3,"turns":[{"steps":[{"bad":}]}]}`,
		`{"version":3,"turns":[{"steps":["\q"]}]}`,
		`{"version":3,"turns":[{"steps":[{"version":01}]}]}`,
		`{"version":3,"turns":[{"steps":[false false]}]}`,
		`{"version":3,"turns":[{"steps":[{"nested":[1,]}]}]}`,
	} {
		var window retainedWindow
		if err := decodeRetained([]byte(data), &window); !errors.Is(err, ErrRetainedContextUnavailable) {
			t.Fatalf("malformed leaf was accepted: %v", err)
		}
	}
	// Host containers inside slices must not be mistaken for opaque roots.
	var turns []retainedTurn
	if err := decodeRetained([]byte(`[{"status":"interrupted","status":"complete"}]`), &turns); !errors.Is(err, ErrRetainedContextUnavailable) {
		t.Fatal("duplicate typed field in a root slice was accepted")
	}
}

func BenchmarkRetainedDecode_OpaqueEvidence(b *testing.B) {
	data := []byte(`{"source":"` + strings.Repeat("x", 14585) + `","resource_id":"doc-a","version":9007199254740993,"more":false}`)
	b.ReportAllocs()
	b.SetBytes(int64(len(data)))
	for b.Loop() {
		var value any
		if err := decodeRetained(data, &value); err != nil {
			b.Fatal(err)
		}
	}
}
