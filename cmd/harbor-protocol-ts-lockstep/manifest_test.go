package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/protocol/wiresurface"
)

// TestRun_WritesDeterministicManifest drives the whole program behind the
// flag surface against a t.TempDir(): it must write a parseable,
// GENERATED-headed manifest covering the full canonical surface, and a
// second run must produce byte-identical output.
func TestRun_WritesDeterministicManifest(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "nested", "wire-manifest.gen.json")

	if err := run(out, repoRoot); err != nil {
		t.Fatalf("run: %v", err)
	}
	first, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}

	var m Manifest
	if err := json.Unmarshal(first, &m); err != nil {
		t.Fatalf("manifest is not valid JSON: %v", err)
	}
	if m.Comment == "" || m.ProtocolVersion == "" {
		t.Error("manifest missing banner or protocol version")
	}
	if len(m.Types) == 0 || len(m.Methods) == 0 || len(m.Errors) == 0 || len(m.Events) == 0 {
		t.Errorf("manifest surface incomplete: types=%d methods=%d errors=%d events=%d",
			len(m.Types), len(m.Methods), len(m.Errors), len(m.Events))
	}

	if err := run(out, repoRoot); err != nil {
		t.Fatalf("run (second): %v", err)
	}
	second, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read manifest (second): %v", err)
	}
	if string(first) != string(second) {
		t.Error("run is non-deterministic across invocations")
	}
}

// tokenFixture exercises every branch of mapFieldType / structFields: a
// scalar, a numeric, a bool, a string slice, a byte slice (base64
// string), a raw-JSON field (any), a map (object), an interface (any), a
// time.Time (string), a pointer (optional), an omitempty scalar
// (optional), a canonical named struct (object ref), a slice of a
// canonical type (array ref), an unexported field (dropped), and a
// json:"-" field (dropped).
type tokenFixture struct {
	Name      string            `json:"name"`
	Count     int64             `json:"count"`
	Enabled   bool              `json:"enabled"`
	Tags      []string          `json:"tags"`
	Blob      []byte            `json:"blob"`
	Raw       json.RawMessage   `json:"raw"`
	Labels    map[string]string `json:"labels"`
	Anything  any               `json:"anything"`
	When      time.Time         `json:"when"`
	Maybe     *string           `json:"maybe"`
	Sometimes int               `json:"sometimes,omitempty"`
	hidden    string            //nolint:unused // exercises the unexported-field drop
	Dropped   string            `json:"-"`
}

// TestStructFields_TokenMapping pins the token + ref + optionality
// mapping for the representative field shapes.
func TestStructFields_TokenMapping(t *testing.T) {
	fields := structFields(reflect.TypeOf(tokenFixture{}))
	got := make(map[string]FieldShape, len(fields))
	for _, f := range fields {
		got[f.Key] = f
	}

	if _, ok := got["hidden"]; ok {
		t.Error("unexported field leaked into the manifest")
	}
	if _, ok := got["-"]; ok {
		t.Error(`json:"-" field leaked into the manifest`)
	}

	want := map[string]struct {
		token    string
		optional bool
	}{
		"name":      {"string", false},
		"count":     {"number", false},
		"enabled":   {"boolean", false},
		"tags":      {"array", false},
		"blob":      {"string", false}, // []byte → base64 string
		"raw":       {"any", false},    // json.RawMessage → any
		"labels":    {"object", false},
		"anything":  {"any", false},
		"when":      {"string", false}, // time.Time → string
		"maybe":     {"string", true},  // pointer → optional
		"sometimes": {"number", true},  // omitempty → optional
	}
	for key, w := range want {
		f, ok := got[key]
		if !ok {
			t.Errorf("field %q missing", key)
			continue
		}
		if f.Type != w.token {
			t.Errorf("field %q token = %q, want %q", key, f.Type, w.token)
		}
		if f.Optional != w.optional {
			t.Errorf("field %q optional = %v, want %v", key, f.Optional, w.optional)
		}
	}
}

// TestManifest_WireSurfaceDigest pins the manifest's top-level
// wire_surface_digest equal to wiresurface.Digest() — the committed
// manifest's digest can never drift from the runtime's within a build — and
// asserts the reflected RuntimeInfo type-shape carries the new
// wire_surface_digest field so a manifest-vendoring client sees it.
func TestManifest_WireSurfaceDigest(t *testing.T) {
	m, err := BuildManifest(repoRoot)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}

	if m.WireSurfaceDigest != wiresurface.Digest() {
		t.Errorf("manifest WireSurfaceDigest = %q, want wiresurface.Digest() = %q",
			m.WireSurfaceDigest, wiresurface.Digest())
	}
	if !regexp.MustCompile(`^sha256:[0-9a-f]{64}$`).MatchString(m.WireSurfaceDigest) {
		t.Errorf("manifest WireSurfaceDigest = %q, want a sha256: name-level digest", m.WireSurfaceDigest)
	}

	ri, ok := m.Types["RuntimeInfo"]
	if !ok {
		t.Fatal("RuntimeInfo missing from manifest")
	}
	if _, present := fieldMap(ri)["wire_surface_digest"]; !present {
		t.Error("RuntimeInfo type-shape missing the wire_surface_digest field")
	}
}

// TestManifest_WireSurfaceDigest_Committed cross-checks the committed
// manifest file's top-level wire_surface_digest equals wiresurface.Digest(),
// so a stale committed digest fails `go test` directly (belt-and-suspenders
// alongside the git-diff gate).
func TestManifest_WireSurfaceDigest_Committed(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(repoRoot, committedManifestPath))
	if err != nil {
		t.Fatalf("read committed manifest: %v", err)
	}
	var committed Manifest
	if err := json.Unmarshal(raw, &committed); err != nil {
		t.Fatalf("decode committed manifest: %v", err)
	}
	if committed.WireSurfaceDigest != wiresurface.Digest() {
		t.Errorf("committed manifest wire_surface_digest = %q, want %q — run 'make protocol-ts-gen' and commit",
			committed.WireSurfaceDigest, wiresurface.Digest())
	}
}

// EmbeddedBase is promoted into structs that embed it anonymously.
type EmbeddedBase struct {
	Inherited string `json:"inherited"`
}

// EmptyMarker is an empty embedded struct (a seal-marker shape) that must
// contribute no fields when promoted.
type EmptyMarker struct{}

// promoteFixture embeds EmbeddedBase + EmptyMarker anonymously (untagged)
// so encoding/json promotes their fields; the walker must flatten them.
type promoteFixture struct {
	EmbeddedBase
	EmptyMarker
	Own string `json:"own"`
}

// TestStructFields_PromotesAnonymousEmbeds asserts an anonymous untagged
// struct embed is flattened (its fields promoted) and an empty marker
// contributes nothing — matching encoding/json's promotion semantics.
func TestStructFields_PromotesAnonymousEmbeds(t *testing.T) {
	fields := structFields(reflect.TypeOf(promoteFixture{}))
	keys := make(map[string]bool, len(fields))
	for _, f := range fields {
		keys[f.Key] = true
	}
	if !keys["inherited"] {
		t.Error("promoted field 'inherited' missing — anonymous embed not flattened")
	}
	if !keys["own"] {
		t.Error("own field missing")
	}
	if len(fields) != 2 {
		t.Errorf("got %d fields, want 2 (inherited + own; the empty marker contributes none)", len(fields))
	}
}

// TestCanonicalName_RefResolution checks canonicalName resolves a
// canonical named struct (and a slice element of one) to its name and
// rejects non-canonical types.
func TestCanonicalName_RefResolution(t *testing.T) {
	// A wire type field that references IdentityScope resolves the ref.
	start, err := BuildManifest(repoRoot)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	ms := start.Types["MetricsSnapshot"]
	var counters *FieldShape
	for i := range ms.Fields {
		if ms.Fields[i].Key == "counters" {
			counters = &ms.Fields[i]
		}
	}
	if counters == nil {
		t.Fatal("MetricsSnapshot.counters missing")
	}
	if counters.Type != "array" || counters.Ref != "NamedCounter" {
		t.Errorf("MetricsSnapshot.counters = {%s, ref=%s}, want {array, ref=NamedCounter}", counters.Type, counters.Ref)
	}
	// A plain string field carries no ref.
	if canonicalName(reflect.TypeOf("")) != "" {
		t.Error("a string type should resolve to no canonical name")
	}
}
