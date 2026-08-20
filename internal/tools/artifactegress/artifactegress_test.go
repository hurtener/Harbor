package artifactegress_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/tools/artifactegress"
	"github.com/hurtener/Harbor/internal/tools/artifactref"
)

// binaryFixture is a ten-byte document that is NOT valid UTF-8: a PDF
// magic followed by a UTF-16 BOM, a NUL, a lone continuation byte, and a
// truncated two-byte sequence. Every one of those is a byte
// `encoding/json` rewrites when it marshals a Go string, and none is
// when it marshals a []byte — which is the whole reason the wire
// encoding is normative.
var binaryFixture = []byte{0x25, 0x50, 0x44, 0x46, 0xFF, 0xFE, 0x00, 0x80, 0xC3, 0x28}

// resolverCtx seats a resolver that answers each id from the supplied
// map, and a not-found error otherwise — the same shape the run-scoped
// dispatch resolver has.
func resolverCtx(t *testing.T, contents map[string][]byte) context.Context {
	t.Helper()
	return artifactref.WithResolver(context.Background(), artifactref.ResolverFunc(
		func(_ context.Context, id string) ([]byte, error) {
			data, ok := contents[id]
			if !ok {
				return nil, fmt.Errorf("artifact %q not found", id)
			}
			return data, nil
		}))
}

func mustMapping(t *testing.T, in map[string][]string) artifactegress.Mapping {
	t.Helper()
	m, err := artifactegress.CompileMapping(in)
	if err != nil {
		t.Fatalf("CompileMapping: %v", err)
	}
	return m
}

// TestPayload_MarshalJSON_IsStandardBase64ByteExact is the NORMATIVE
// wire-encoding assertion: the substituted value marshals as RFC 4648 §4
// standard base64 with padding, and decoding it returns the stored bytes
// EXACTLY for arbitrary binary content.
func TestPayload_MarshalJSON_IsStandardBase64ByteExact(t *testing.T) {
	ctx := resolverCtx(t, map[string][]byte{"art-bin": binaryFixture})
	args := map[string]any{"doc": "art-bin"}
	if _, err := artifactegress.Encode(ctx, args, mustMapping(t, map[string][]string{"ingest": {"doc"}}), "ingest", 1024); err != nil {
		t.Fatalf("Encode: %v", err)
	}

	encoded, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal args: %v", err)
	}

	// The exact wire literal, computed from the spec rather than pasted:
	// standard alphabet, with padding.
	wantB64 := base64.StdEncoding.EncodeToString(binaryFixture)
	if wantB64 != "JVBERv/+AIDDKA==" {
		t.Fatalf("fixture base64 = %q, want JVBERv/+AIDDKA== (the fixture changed; the golden transcript must change with it)", wantB64)
	}
	if !strings.Contains(string(encoded), `"doc":"`+wantB64+`"`) {
		t.Fatalf("encoded args = %s, want doc to carry %q", encoded, wantB64)
	}

	// Round trip: decode the wire form and compare byte for byte.
	var back map[string]string
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, err := base64.StdEncoding.DecodeString(back["doc"])
	if err != nil {
		t.Fatalf("base64 decode: %v", err)
	}
	if len(got) != len(binaryFixture) {
		t.Fatalf("round trip returned %d bytes, want %d", len(got), len(binaryFixture))
	}
	for i := range got {
		if got[i] != binaryFixture[i] {
			t.Fatalf("round trip byte %d = %#x, want %#x", i, got[i], binaryFixture[i])
		}
	}
}

// TestPayload_GoStringSlotCorruptsBinary pins the REASON for the
// encoding choice, not merely the choice.
//
// A future author who "simplifies" the carrier to a Go string would
// reintroduce exactly this corruption, and a test that only asserted the
// base64 output would still pass against a string-typed slot carrying
// ASCII. This one measures the corruption directly, so the rationale is
// checked rather than remembered.
func TestPayload_GoStringSlotCorruptsBinary(t *testing.T) {
	encoded, err := json.Marshal(map[string]any{"doc": string(binaryFixture)})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back map[string]string
	if err := json.Unmarshal(encoded, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := []byte(back["doc"])
	if len(got) == len(binaryFixture) {
		t.Fatalf("a Go string slot round-tripped %d bytes unchanged; the corruption this encoding exists to avoid did not occur, so the rationale is no longer pinned", len(got))
	}
	// Three invalid-UTF-8 runs (FF FE, 80, C3 28's lone C3) each become
	// U+FFFD = ef bf bd, so the payload grows.
	if got := strings.Count(string(got), "�"); got == 0 {
		t.Fatalf("expected U+FFFD replacement characters in the string round trip, found none")
	}
	if len(got) <= len(binaryFixture) {
		t.Fatalf("string round trip = %d bytes, want more than %d (replacement characters are 3 bytes each)", len(got), len(binaryFixture))
	}
}

// TestPayload_ProjectionsEmitAReferenceNotContent is the carrier's
// projection bound: every serialisation door but MarshalJSON emits a
// reference. The content carries a planted marker, and each arm asserts
// the marker's ABSENCE — while a companion proves the marker really was
// bound, so the test cannot pass vacuously.
func TestPayload_ProjectionsEmitAReferenceNotContent(t *testing.T) {
	const marker = "MARKER-c0ffee-DO-NOT-LEAK"
	ctx := resolverCtx(t, map[string][]byte{"art-1": []byte(marker)})
	args := map[string]any{"doc": "art-1"}
	records, err := artifactegress.Encode(ctx, args, mustMapping(t, map[string][]string{"ingest": {"doc"}}), "ingest", 1024)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}

	payload, ok := args["doc"].(artifactegress.Payload)
	if !ok {
		t.Fatalf("args[doc] = %T, want artifactegress.Payload", args["doc"])
	}

	// Non-vacuity: the marker IS bound, so an absence assertion below is
	// meaningful.
	if payload.Size() != len(marker) {
		t.Fatalf("payload size = %d, want %d — the content was not bound, so the absence arms below would pass vacuously", payload.Size(), len(marker))
	}
	raw, err := payload.MarshalJSON()
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	if !strings.Contains(base64Decode(t, raw), marker) {
		t.Fatalf("MarshalJSON did not carry the content; the one door that must carry it does not")
	}

	// String.
	if s := payload.String(); strings.Contains(s, marker) {
		t.Errorf("String() leaked content: %q", s)
	} else if !strings.Contains(s, "art-1") {
		t.Errorf("String() = %q, want it to name the artifact id", s)
	}

	// fmt over the whole argument map — the shape a debug log takes.
	if s := fmt.Sprintf("%v", args); strings.Contains(s, marker) {
		t.Errorf("fmt over the argument map leaked content: %q", s)
	}

	// LogValue.
	if v := payload.LogValue(); strings.Contains(v.String(), marker) {
		t.Errorf("LogValue() leaked content: %q", v.String())
	}
	// ...and through a real slog handler, which is how it would actually
	// reach a log record.
	var buf strings.Builder
	slog.New(slog.NewTextHandler(&buf, nil)).Info("call", slog.Any("payload", payload))
	if strings.Contains(buf.String(), marker) {
		t.Errorf("slog record leaked content: %q", buf.String())
	}
	if !strings.Contains(buf.String(), "art-1") {
		t.Errorf("slog record = %q, want it to name the artifact id", buf.String())
	}

	// The record is content-free and names which bytes moved.
	if len(records) != 1 {
		t.Fatalf("records = %d, want 1", len(records))
	}
	if records[0].ArtifactID != "art-1" || records[0].Param != "doc" || records[0].SizeBytes != len(marker) {
		t.Errorf("record = %+v, want the id, the param and the size", records[0])
	}
	if !strings.HasPrefix(records[0].Digest, "sha256:") {
		t.Errorf("record digest = %q, want a sha256: prefix", records[0].Digest)
	}
	blob, err := json.Marshal(records)
	if err != nil {
		t.Fatalf("marshal records: %v", err)
	}
	if strings.Contains(string(blob), marker) {
		t.Errorf("the substitution record leaked content: %s", blob)
	}
}

func base64Decode(t *testing.T, jsonString []byte) string {
	t.Helper()
	var s string
	if err := json.Unmarshal(jsonString, &s); err != nil {
		t.Fatalf("unmarshal base64 string: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("decode base64: %v", err)
	}
	return string(raw)
}

func TestCompileMapping_Refusals(t *testing.T) {
	cases := []struct {
		name string
		in   map[string][]string
	}{
		{"empty tool name", map[string][]string{"  ": {"doc"}}},
		{"no parameter names", map[string][]string{"ingest": {}}},
		{"empty parameter name", map[string][]string{"ingest": {"doc", " "}}},
		{"empty optional parameter name", map[string][]string{"ingest": {"?"}}},
		{"duplicate parameter", map[string][]string{"ingest": {"doc", "doc"}}},
		{"required and optional duplicate parameter", map[string][]string{"ingest": {"doc", "doc?"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := artifactegress.CompileMapping(tc.in); !errors.Is(err, artifactegress.ErrInvalidMapping) {
				t.Fatalf("err = %v, want ErrInvalidMapping", err)
			}
		})
	}
}

func TestCompileMapping_EmptyIsInertAndDeterministic(t *testing.T) {
	m, err := artifactegress.CompileMapping(nil)
	if err != nil {
		t.Fatalf("CompileMapping(nil): %v", err)
	}
	if !m.IsEmpty() || m.ParamsFor("ingest") != nil || m.Tools() != nil {
		t.Fatalf("the empty mapping is not inert: %+v", m)
	}

	m = mustMapping(t, map[string][]string{"ingest": {"zeta", "alpha"}, "scan": {"blob"}})
	if got := m.ParamsFor("ingest"); len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Fatalf("ParamsFor = %v, want deterministic sorted order", got)
	}
	if got := m.Tools(); len(got) != 2 || got[0] != "ingest" || got[1] != "scan" {
		t.Fatalf("Tools = %v, want sorted", got)
	}
	// The returned slice is a copy — a caller cannot reach into the
	// compiled mapping and mutate it under a concurrent invocation.
	got := m.ParamsFor("ingest")
	got[0] = "MUTATED"
	if again := m.ParamsFor("ingest"); again[0] != "alpha" {
		t.Fatalf("ParamsFor returned an aliased slice; the compiled mapping is mutable from outside")
	}
}

func TestCompileMapping_OptionalMarkerIsStrippedForSchemaAndRecords(t *testing.T) {
	m := mustMapping(t, map[string][]string{"ingest": {" zeta? ", "alpha"}})
	got := m.ParamsFor("ingest")
	if len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Fatalf("ParamsFor = %v, want bare deterministic names [alpha zeta]", got)
	}

	ctx := resolverCtx(t, map[string][]byte{"art-alpha": []byte("a")})
	args := map[string]any{"alpha": "art-alpha"}
	records, err := artifactegress.Encode(ctx, args, m, "ingest", 1024)
	if err != nil {
		t.Fatalf("Encode with absent optional parameter: %v", err)
	}
	if len(records) != 1 || records[0].Param != "alpha" || args["zeta"] != nil {
		t.Fatalf("absent optional parameter changed the call: records=%+v args=%v", records, args)
	}
}

func TestEncode_Refusals(t *testing.T) {
	mapping := mustMapping(t, map[string][]string{"ingest": {"doc"}})
	seated := resolverCtx(t, map[string][]byte{"art-1": []byte("hello"), "art-big": make([]byte, 100)})

	t.Run("no resolver seated", func(t *testing.T) {
		args := map[string]any{"doc": "art-1"}
		_, err := artifactegress.Encode(context.Background(), args, mapping, "ingest", 1024)
		if !errors.Is(err, artifactref.ErrNoResolver) {
			t.Fatalf("err = %v, want ErrNoResolver", err)
		}
		if _, mutated := args["doc"].(artifactegress.Payload); mutated {
			t.Fatalf("a refused encode still wrote into the argument map")
		}
	})

	t.Run("mapped parameter absent", func(t *testing.T) {
		_, err := artifactegress.Encode(seated, map[string]any{"other": "x"}, mapping, "ingest", 1024)
		if !errors.Is(err, artifactegress.ErrMappedArgumentMissing) {
			t.Fatalf("err = %v, want ErrMappedArgumentMissing", err)
		}
	})

	t.Run("mapped parameter is not a string", func(t *testing.T) {
		_, err := artifactegress.Encode(seated, map[string]any{"doc": 42.0}, mapping, "ingest", 1024)
		if !errors.Is(err, artifactegress.ErrMappedArgumentNotString) {
			t.Fatalf("err = %v, want ErrMappedArgumentNotString", err)
		}
	})

	t.Run("empty artifact id", func(t *testing.T) {
		_, err := artifactegress.Encode(seated, map[string]any{"doc": "   "}, mapping, "ingest", 1024)
		if !errors.Is(err, artifactegress.ErrEmptyArtifactID) {
			t.Fatalf("err = %v, want ErrEmptyArtifactID", err)
		}
	})

	t.Run("unresolvable id", func(t *testing.T) {
		_, err := artifactegress.Encode(seated, map[string]any{"doc": "art-nope"}, mapping, "ingest", 1024)
		if err == nil || !strings.Contains(err.Error(), "art-nope") {
			t.Fatalf("err = %v, want a resolver failure naming the id", err)
		}
	})

	t.Run("oversize value is refused, never truncated", func(t *testing.T) {
		args := map[string]any{"doc": "art-big"}
		_, err := artifactegress.Encode(seated, args, mapping, "ingest", 99)
		if !errors.Is(err, artifactegress.ErrEgressTooLarge) {
			t.Fatalf("err = %v, want ErrEgressTooLarge", err)
		}
		for _, want := range []string{"art-big", "100", "99"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name %q (artifact, size, ceiling)", err, want)
			}
		}
		if _, mutated := args["doc"].(artifactegress.Payload); mutated {
			t.Fatalf("an oversize value was written into the argument map; it must be refused, not truncated")
		}
	})

	t.Run("non-positive ceiling", func(t *testing.T) {
		_, err := artifactegress.Encode(seated, map[string]any{"doc": "art-1"}, mapping, "ingest", 0)
		if !errors.Is(err, artifactegress.ErrInvalidCeiling) {
			t.Fatalf("err = %v, want ErrInvalidCeiling", err)
		}
	})

	t.Run("nil argument map", func(t *testing.T) {
		_, err := artifactegress.Encode(seated, nil, mapping, "ingest", 1024)
		if !errors.Is(err, artifactegress.ErrMappedArgumentMissing) {
			t.Fatalf("err = %v, want ErrMappedArgumentMissing", err)
		}
	})
}

// TestEncode_CeilingBoundary pins the comparison at the exact edge: a
// value EQUAL to the ceiling is served, one byte over is refused.
func TestEncode_CeilingBoundary(t *testing.T) {
	mapping := mustMapping(t, map[string][]string{"ingest": {"doc"}})
	ctx := resolverCtx(t, map[string][]byte{"at": make([]byte, 64), "over": make([]byte, 65)})

	if _, err := artifactegress.Encode(ctx, map[string]any{"doc": "at"}, mapping, "ingest", 64); err != nil {
		t.Fatalf("a value exactly at the ceiling was refused: %v", err)
	}
	if _, err := artifactegress.Encode(ctx, map[string]any{"doc": "over"}, mapping, "ingest", 64); !errors.Is(err, artifactegress.ErrEgressTooLarge) {
		t.Fatalf("a value one byte over the ceiling was accepted: %v", err)
	}
}

func TestEncode_OptionalParameters(t *testing.T) {
	mapping := mustMapping(t, map[string][]string{"generate_image": {"required", "reference_image?"}})
	seated := resolverCtx(t, map[string][]byte{"art-required": []byte("required"), "art-reference": []byte("reference")})

	t.Run("missing optional parameter skips without resolver", func(t *testing.T) {
		args := map[string]any{}
		records, err := artifactegress.Encode(context.Background(), args, mustMapping(t, map[string][]string{"generate_image": {"reference_image?"}}), "generate_image", 0)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if records != nil {
			t.Fatalf("records = %+v, want nil for an all-optional omission", records)
		}
	})

	t.Run("nil optional parameter skips", func(t *testing.T) {
		args := map[string]any{"required": "art-required", "reference_image": nil}
		records, err := artifactegress.Encode(seated, args, mapping, "generate_image", 1024)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if len(records) != 1 || records[0].Param != "required" {
			t.Fatalf("records = %+v, want only the required substitution", records)
		}
		if _, ok := args["reference_image"]; !ok || args["reference_image"] != nil {
			t.Fatalf("nil optional argument was rewritten: %v", args["reference_image"])
		}
	})

	t.Run("present optional parameter resolves", func(t *testing.T) {
		args := map[string]any{"required": "art-required", "reference_image": "art-reference"}
		records, err := artifactegress.Encode(seated, args, mapping, "generate_image", 1024)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if len(records) != 2 || records[0].Param != "reference_image" || records[1].Param != "required" {
			t.Fatalf("records = %+v, want deterministic bare parameter names", records)
		}
		if _, ok := args["reference_image"].(artifactegress.Payload); !ok {
			t.Fatalf("optional parameter was not substituted: %T", args["reference_image"])
		}
	})

	t.Run("explicit empty optional parameter is refused", func(t *testing.T) {
		_, err := artifactegress.Encode(seated, map[string]any{"required": "art-required", "reference_image": "  "}, mapping, "generate_image", 1024)
		if !errors.Is(err, artifactegress.ErrEmptyArtifactID) {
			t.Fatalf("err = %v, want ErrEmptyArtifactID", err)
		}
	})

	t.Run("optional non-string parameter is refused", func(t *testing.T) {
		_, err := artifactegress.Encode(seated, map[string]any{"required": "art-required", "reference_image": 42}, mapping, "generate_image", 1024)
		if !errors.Is(err, artifactegress.ErrMappedArgumentNotString) {
			t.Fatalf("err = %v, want ErrMappedArgumentNotString", err)
		}
	})

	t.Run("required omission still fails", func(t *testing.T) {
		_, err := artifactegress.Encode(seated, map[string]any{"reference_image": "art-reference"}, mapping, "generate_image", 1024)
		if !errors.Is(err, artifactegress.ErrMappedArgumentMissing) {
			t.Fatalf("err = %v, want ErrMappedArgumentMissing", err)
		}
	})
}

// TestEncode_UnmappedToolIsInert asserts the no-op guarantee at the
// function boundary: a tool the mapping does not address is untouched,
// resolver or no resolver, ceiling or no ceiling.
func TestEncode_UnmappedToolIsInert(t *testing.T) {
	mapping := mustMapping(t, map[string][]string{"ingest": {"doc"}})
	args := map[string]any{"doc": "art-1"}
	records, err := artifactegress.Encode(context.Background(), args, mapping, "other_tool", 0)
	if err != nil || records != nil {
		t.Fatalf("Encode over an unmapped tool = (%v, %v), want (nil, nil)", records, err)
	}
	if args["doc"] != "art-1" {
		t.Fatalf("an unmapped tool's arguments were rewritten: %v", args)
	}
}

func TestEncode_WritesEveryMappedParameterInPlace(t *testing.T) {
	mapping := mustMapping(t, map[string][]string{"ingest": {"a", "b"}})
	ctx := resolverCtx(t, map[string][]byte{"art-a": []byte("AAA"), "art-b": []byte("BBBB")})
	args := map[string]any{"a": "art-a", "b": "art-b", "untouched": "plain"}

	records, err := artifactegress.Encode(ctx, args, mapping, "ingest", 1024)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("records = %d, want 2", len(records))
	}
	// Deterministic order follows the compiled mapping.
	if records[0].Param != "a" || records[1].Param != "b" {
		t.Fatalf("records are not in deterministic parameter order: %+v", records)
	}
	if records[0].SizeBytes != 3 || records[1].SizeBytes != 4 {
		t.Fatalf("record sizes = %d/%d, want 3/4", records[0].SizeBytes, records[1].SizeBytes)
	}
	if records[0].Digest == records[1].Digest {
		t.Fatalf("two distinct artifacts produced the same digest")
	}
	if args["untouched"] != "plain" {
		t.Fatalf("an unmapped key was rewritten: %v", args["untouched"])
	}
}

func TestEncode_ConcurrentReuse_OptionalMapping(t *testing.T) {
	mapping := mustMapping(t, map[string][]string{"generate_image": {"required", "reference_image?"}})
	ctx := resolverCtx(t, map[string][]byte{"required-id": []byte("required"), "reference-id": []byte("reference")})

	const calls = 128
	var wg sync.WaitGroup
	for i := 0; i < calls; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			args := map[string]any{"required": "required-id"}
			if i%2 == 0 {
				args["reference_image"] = "reference-id"
			}
			records, err := artifactegress.Encode(ctx, args, mapping, "generate_image", 1024)
			if err != nil {
				t.Errorf("call %d: Encode: %v", i, err)
				return
			}
			wantRecords := 1
			if i%2 == 0 {
				wantRecords = 2
			}
			if len(records) != wantRecords {
				t.Errorf("call %d: records=%d, want %d", i, len(records), wantRecords)
			}
			if _, ok := args["required"].(artifactegress.Payload); !ok {
				t.Errorf("call %d: required argument was not substituted: %T", i, args["required"])
			}
			if i%2 == 0 {
				if _, ok := args["reference_image"].(artifactegress.Payload); !ok {
					t.Errorf("call %d: optional argument was not substituted: %T", i, args["reference_image"])
				}
			}
		}(i)
	}
	wg.Wait()
}

// TestArtifactEgressPackage_ContainsOnlyTheEncoder bounds the residual
// blind spot the AST scan cannot see.
//
// The scan resolves the egress package by import path and returns early
// for a file that does not import it — so a file INSIDE this package is
// invisible to its own scan. The bound is therefore "the package is
// small enough to read in full", and this test CHECKS that rather than
// asserting it: the non-test file set is pinned, so a new production
// file here is a deliberate, visible change rather than a quiet place to
// add a second content-emitting path.
func TestArtifactEgressPackage_ContainsOnlyTheEncoder(t *testing.T) {
	want := map[string]struct{}{"artifactegress.go": {}}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	got := map[string]struct{}{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if filepath.Ext(name) != ".go" || strings.HasSuffix(name, "_test.go") {
			continue
		}
		got[name] = struct{}{}
	}
	for name := range got {
		if _, ok := want[name]; !ok {
			t.Errorf("unexpected non-test file %q in the egress package: the AST scan cannot see a call site inside this package, so its bound is that the package is short enough to read — adding a file here needs a reviewed reason and this list updated", name)
		}
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("expected file %q is missing from the egress package", name)
		}
	}
}
