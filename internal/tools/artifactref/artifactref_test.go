package artifactref_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/tools/artifactref"
)

func reflectTypeOf(v any) reflect.Type { return reflect.TypeOf(v) }

// resolverFor builds a resolver over a fixed id→bytes table. A missing
// id is an error, never empty content.
func resolverFor(table map[string][]byte) artifactref.Resolver {
	return artifactref.ResolverFunc(func(_ context.Context, id string) ([]byte, error) {
		b, ok := table[id]
		if !ok {
			return nil, fmt.Errorf("no artifact %q", id)
		}
		return b, nil
	})
}

type simpleArgs struct {
	Doc      artifactref.Ref `json:"doc"`
	MaxWords int             `json:"max_words"`
}

func TestRef_UnmarshalJSON_DecodesTheIDAsAPlainString(t *testing.T) {
	var a simpleArgs
	if err := json.Unmarshal([]byte(`{"doc":"tool_abc123","max_words":7}`), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := a.Doc.ID(); got != "tool_abc123" {
		t.Fatalf("ID = %q, want tool_abc123", got)
	}
	if !a.Doc.Supplied() {
		t.Fatal("Supplied = false for a reference the argument carried")
	}
	if a.Doc.Resolved() {
		t.Fatal("Resolved = true before Substitute ran")
	}
	if a.MaxWords != 7 {
		t.Fatalf("MaxWords = %d, want 7", a.MaxWords)
	}
}

func TestRef_UnmarshalJSON_RejectsANonStringReference(t *testing.T) {
	var a simpleArgs
	err := json.Unmarshal([]byte(`{"doc":{"id":"x"},"max_words":1}`), &a)
	if err == nil {
		t.Fatal("an object-shaped reference was accepted; want a loud decode failure")
	}
}

func TestRef_UnmarshalJSON_NullLeavesTheReferenceUnsupplied(t *testing.T) {
	var a simpleArgs
	if err := json.Unmarshal([]byte(`{"doc":null,"max_words":1}`), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if a.Doc.Supplied() {
		t.Fatal("a JSON null was read as a supplied reference")
	}
}

func TestRef_Bytes_FailsLoudlyWhenUnresolved(t *testing.T) {
	r := artifactref.NewRef("tool_abc123")
	if _, err := r.Bytes(); !errors.Is(err, artifactref.ErrUnresolved) {
		t.Fatalf("Bytes err = %v, want ErrUnresolved", err)
	}
	var zero artifactref.Ref
	if _, err := zero.Bytes(); !errors.Is(err, artifactref.ErrUnresolved) {
		t.Fatalf("zero Ref Bytes err = %v, want ErrUnresolved", err)
	}
}

func TestSubstitute_ResolvesTheReferenceAndHandsTheToolTheBytes(t *testing.T) {
	ctx := artifactref.WithResolver(context.Background(),
		resolverFor(map[string][]byte{"tool_abc123": []byte("the stored bytes")}))

	var a simpleArgs
	if err := json.Unmarshal([]byte(`{"doc":"tool_abc123","max_words":7}`), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := artifactref.Substitute(ctx, &a); err != nil {
		t.Fatalf("Substitute: %v", err)
	}
	got, err := a.Doc.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if string(got) != "the stored bytes" {
		t.Fatalf("Bytes = %q, want %q", got, "the stored bytes")
	}
	if a.Doc.Size() != len("the stored bytes") {
		t.Fatalf("Size = %d, want %d", a.Doc.Size(), len("the stored bytes"))
	}
}

// TestSubstitute_TheResolvedValueDoesNotSurviveReSerialisation is the
// carrier half of the substitution invariant, tested from the ARRIVAL
// side: whatever a downstream layer does with the argument value — marshal
// it into an event payload, format it into a message, log it — it gets
// the id and never the content.
func TestSubstitute_TheResolvedValueDoesNotSurviveReSerialisation(t *testing.T) {
	const secret = "RESOLVED-ARTIFACT-CONTENT-MARKER"
	ctx := artifactref.WithResolver(context.Background(),
		resolverFor(map[string][]byte{"tool_abc123": []byte(secret)}))

	var a simpleArgs
	if err := json.Unmarshal([]byte(`{"doc":"tool_abc123","max_words":7}`), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := artifactref.Substitute(ctx, &a); err != nil {
		t.Fatalf("Substitute: %v", err)
	}
	// Prove the value IS bound, so the negatives below are not vacuous.
	if got, _ := a.Doc.Bytes(); string(got) != secret {
		t.Fatalf("the resolved value was not bound: %q", got)
	}

	encoded, err := json.Marshal(a)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), secret) {
		t.Fatalf("json.Marshal leaked the resolved value: %s", encoded)
	}
	if !strings.Contains(string(encoded), "tool_abc123") {
		t.Fatalf("json.Marshal dropped the reference id: %s", encoded)
	}

	if formatted := fmt.Sprintf("%v|%s|%+v", a.Doc, a.Doc, a); strings.Contains(formatted, secret) {
		t.Fatalf("fmt leaked the resolved value: %s", formatted)
	}

	var logBuf bytes.Buffer
	slog.New(slog.NewJSONHandler(&logBuf, nil)).
		Info("dispatch", slog.Any("args", a.Doc), slog.String("ref", a.Doc.ID()))
	if strings.Contains(logBuf.String(), secret) {
		t.Fatalf("slog leaked the resolved value: %s", logBuf.String())
	}
	if !strings.Contains(logBuf.String(), "tool_abc123") {
		t.Fatalf("slog dropped the reference id: %s", logBuf.String())
	}
}

func TestSubstitute_FailsLoudlyWithNoResolverSeated(t *testing.T) {
	var a simpleArgs
	if err := json.Unmarshal([]byte(`{"doc":"tool_abc123","max_words":1}`), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	err := artifactref.Substitute(context.Background(), &a)
	if !errors.Is(err, artifactref.ErrNoResolver) {
		t.Fatalf("err = %v, want ErrNoResolver", err)
	}
}

func TestSubstitute_FailsLoudlyOnAnEmptyReferenceID(t *testing.T) {
	ctx := artifactref.WithResolver(context.Background(), resolverFor(nil))
	var a simpleArgs
	if err := json.Unmarshal([]byte(`{"doc":"","max_words":1}`), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := artifactref.Substitute(ctx, &a); !errors.Is(err, artifactref.ErrEmptyID) {
		t.Fatalf("err = %v, want ErrEmptyID", err)
	}
}

func TestSubstitute_SurfacesTheResolverErrorWithTheID(t *testing.T) {
	sentinel := errors.New("store said no")
	ctx := artifactref.WithResolver(context.Background(),
		artifactref.ResolverFunc(func(_ context.Context, _ string) ([]byte, error) {
			return nil, sentinel
		}))
	var a simpleArgs
	if err := json.Unmarshal([]byte(`{"doc":"tool_missing","max_words":1}`), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	err := artifactref.Substitute(ctx, &a)
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want it to wrap the resolver error", err)
	}
	if !strings.Contains(err.Error(), "tool_missing") {
		t.Fatalf("err = %v, want it to name the reference id", err)
	}
}

// TestSubstitute_AnOmittedOptionalReferenceIsNotAnError — an argument
// that carried no reference is not a malformed argument. Reading it
// anyway still fails loudly.
func TestSubstitute_AnOmittedOptionalReferenceIsNotAnError(t *testing.T) {
	type optArgs struct {
		Doc artifactref.Ref `json:"doc,omitempty"`
		N   int             `json:"n"`
	}
	var a optArgs
	if err := json.Unmarshal([]byte(`{"n":3}`), &a); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// No resolver seated on purpose: an omitted reference must not need
	// one.
	if err := artifactref.Substitute(context.Background(), &a); err != nil {
		t.Fatalf("Substitute over an omitted reference: %v", err)
	}
	if _, err := a.Doc.Bytes(); !errors.Is(err, artifactref.ErrUnresolved) {
		t.Fatalf("Bytes err = %v, want ErrUnresolved", err)
	}
}

func TestSubstitute_WalksNestedStructsSlicesPointersAndMaps(t *testing.T) {
	type inner struct {
		Ref artifactref.Ref `json:"ref"`
	}
	type nested struct {
		Direct  artifactref.Ref            `json:"direct"`
		Ptr     *artifactref.Ref           `json:"ptr"`
		Struct  inner                      `json:"struct"`
		Slice   []artifactref.Ref          `json:"slice"`
		Map     map[string]artifactref.Ref `json:"map"`
		Array   [1]artifactref.Ref         `json:"array"`
		Ignored string                     `json:"ignored"`
	}
	table := map[string][]byte{}
	for _, id := range []string{"a", "b", "c", "d", "e", "f", "g"} {
		table[id] = []byte("content-" + id)
	}
	ctx := artifactref.WithResolver(context.Background(), resolverFor(table))

	var n nested
	raw := `{"direct":"a","ptr":"b","struct":{"ref":"c"},"slice":["d","e"],` +
		`"map":{"k":"f"},"array":["g"],"ignored":"x"}`
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if err := artifactref.Substitute(ctx, &n); err != nil {
		t.Fatalf("Substitute: %v", err)
	}
	check := func(label string, r artifactref.Ref, wantID string) {
		t.Helper()
		got, err := r.Bytes()
		if err != nil {
			t.Fatalf("%s: Bytes: %v", label, err)
		}
		if string(got) != "content-"+wantID {
			t.Fatalf("%s: Bytes = %q, want content-%s", label, got, wantID)
		}
	}
	check("direct", n.Direct, "a")
	if n.Ptr == nil {
		t.Fatal("ptr reference decoded to nil")
	}
	check("ptr", *n.Ptr, "b")
	check("struct", n.Struct.Ref, "c")
	check("slice[0]", n.Slice[0], "d")
	check("slice[1]", n.Slice[1], "e")
	check("map[k]", n.Map["k"], "f")
	check("array[0]", n.Array[0], "g")
}

func TestSubstitute_RejectsANonPointerTarget(t *testing.T) {
	ctx := artifactref.WithResolver(context.Background(), resolverFor(nil))
	var a simpleArgs
	if err := artifactref.Substitute(ctx, a); !errors.Is(err, artifactref.ErrNotAddressable) {
		t.Fatalf("err = %v, want ErrNotAddressable", err)
	}
	if err := artifactref.Substitute(ctx, nil); !errors.Is(err, artifactref.ErrNotAddressable) {
		t.Fatalf("nil target err = %v, want ErrNotAddressable", err)
	}
}

func TestTypeContainsRef_AnswersTheStaticQuestion(t *testing.T) {
	type deep struct {
		Inner []struct {
			R *artifactref.Ref
		}
	}
	type plain struct {
		A string
		B int
		C map[string]string
		D any
	}
	type unexportedOnly struct {
		r artifactref.Ref //nolint:unused // the point of the case is that reflection cannot reach it.
	}
	cases := []struct {
		name string
		val  any
		want bool
	}{
		{"direct", simpleArgs{}, true},
		{"deeply nested", deep{}, true},
		{"no reference", plain{}, false},
		{"unexported reference is unreachable", unexportedOnly{}, false},
		{"scalar", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := artifactref.TypeContainsRef(reflectTypeOf(tc.val)); got != tc.want {
				t.Fatalf("TypeContainsRef = %v, want %v", got, tc.want)
			}
		})
	}
}
