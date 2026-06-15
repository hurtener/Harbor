package main

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/singlesource"
)

// repoRoot is the repository root relative to this test's package dir
// (cmd/harbor-protocol-ts-lockstep).
const repoRoot = "../.."

// committedManifestPath is the committed manifest's path relative to the
// repository root — the same default the generator writes.
const committedManifestPath = "web/console/src/lib/protocol/wire-manifest.gen.json"

// TestManifest_TypeInstanceIndexInLockstep pins the reflection index
// against singlesource.CanonicalWireTypes: every canonical name has a
// struct instance whose reflect name matches, and no instance is stale.
// A new canonical wire type without an index entry fails here — the
// mechanism that makes the generator's coverage exhaustive.
func TestManifest_TypeInstanceIndexInLockstep(t *testing.T) {
	for name := range singlesource.CanonicalWireTypes {
		rt, ok := typeInstanceIndex[name]
		if !ok {
			t.Errorf("canonical wire type %q has no typeInstanceIndex instance — add it", name)
			continue
		}
		if rt.Name() != name {
			t.Errorf("typeInstanceIndex[%q] resolves to %s — name mismatch", name, rt)
		}
		if rt.Kind() != reflect.Struct {
			t.Errorf("typeInstanceIndex[%q] is %s, want a struct", name, rt.Kind())
		}
	}
	for name := range typeInstanceIndex {
		if _, ok := singlesource.CanonicalWireTypes[name]; !ok {
			t.Errorf("typeInstanceIndex names %q, which is not in CanonicalWireTypes — remove the stale entry", name)
		}
	}
}

// TestManifest_BuildCoversCanonicalSurface asserts the built manifest is
// exhaustive over the canonical Protocol surface and carries no stale
// entries: every canonical wire type / method / error code is present,
// and nothing in the manifest is unregistered. A new method or error
// code added Go-side appears in the freshly-built manifest automatically;
// the `git diff` half of the gate then fails until the committed manifest
// is regenerated.
func TestManifest_BuildCoversCanonicalSurface(t *testing.T) {
	m, err := BuildManifest(repoRoot)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}

	for name := range singlesource.CanonicalWireTypes {
		if _, ok := m.Types[name]; !ok {
			t.Errorf("canonical wire type %q missing from manifest", name)
		}
	}
	if len(m.Types) != len(singlesource.CanonicalWireTypes) {
		t.Errorf("manifest has %d types, CanonicalWireTypes has %d", len(m.Types), len(singlesource.CanonicalWireTypes))
	}

	manifestMethods := toSet(m.Methods)
	for _, mm := range methods.Methods() {
		if _, ok := manifestMethods[string(mm)]; !ok {
			t.Errorf("canonical method %q missing from manifest", mm)
		}
	}
	for _, mm := range m.Methods {
		if !methods.IsValidMethod(methods.Method(mm)) {
			t.Errorf("manifest names method %q, which is not canonical — stale entry", mm)
		}
	}

	manifestErrors := toSet(m.Errors)
	for _, c := range protoerrors.Codes() {
		if _, ok := manifestErrors[string(c)]; !ok {
			t.Errorf("canonical error code %q missing from manifest", c)
		}
	}
	for _, c := range m.Errors {
		if !protoerrors.IsValidCode(protoerrors.Code(c)) {
			t.Errorf("manifest names error code %q, which is not canonical — stale entry", c)
		}
	}

	if len(m.Events) == 0 {
		t.Error("manifest carries no events — the event tree scan found nothing")
	}
}

// TestManifest_EventsMatchTreeScan pins the manifest's event set against a
// fresh scan of the production tree, so the committed manifest cannot
// silently drop or invent an event type. A new EventType constant
// declared anywhere in internal/ is picked up by the scan; the committed
// manifest then fails the `git diff` gate until regenerated.
func TestManifest_EventsMatchTreeScan(t *testing.T) {
	scanned, err := scanEventTypes(repoRoot)
	if err != nil {
		t.Fatalf("scanEventTypes: %v", err)
	}
	m, err := BuildManifest(repoRoot)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	if !reflect.DeepEqual(scanned, m.Events) {
		t.Errorf("manifest events differ from a fresh tree scan: manifest=%d scanned=%d", len(m.Events), len(scanned))
	}
	// Spot-check a well-known event type is present so the regex cannot
	// silently rot to matching nothing.
	if _, ok := toSet(m.Events)["task.started"]; !ok {
		t.Error(`expected canonical event type "task.started" in the manifest`)
	}
}

// TestManifest_DeterministicAndStable asserts two independent builds
// render byte-identical output — the property the `git diff --exit-code`
// gate relies on.
func TestManifest_DeterministicAndStable(t *testing.T) {
	a, err := BuildManifest(repoRoot)
	if err != nil {
		t.Fatalf("BuildManifest a: %v", err)
	}
	b, err := BuildManifest(repoRoot)
	if err != nil {
		t.Fatalf("BuildManifest b: %v", err)
	}
	ra, err := render(a)
	if err != nil {
		t.Fatalf("render a: %v", err)
	}
	rb, err := render(b)
	if err != nil {
		t.Fatalf("render b: %v", err)
	}
	if string(ra) != string(rb) {
		t.Error("manifest render is non-deterministic across builds")
	}
}

// TestManifest_CommittedFileInSync regenerates the manifest in memory and
// compares it byte-for-byte to the committed file, so a stale committed
// manifest fails `go test` as well as the `make protocol-ts-gen-check`
// gate (belt and suspenders — a Go wire change that forgets the regen is
// caught by the test suite directly).
func TestManifest_CommittedFileInSync(t *testing.T) {
	m, err := BuildManifest(repoRoot)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}
	want, err := render(m)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(repoRoot, committedManifestPath))
	if err != nil {
		t.Fatalf("read committed manifest: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("committed %s is stale — run 'make protocol-ts-gen' and commit the result", committedManifestPath)
	}
}

// TestManifest_FieldShape_Known sanity-checks the field walker + token
// mapping on two well-understood types: a request with an optional scalar
// and a nested object ref (StartRequest), and the Error wire type homed in
// the errors package.
func TestManifest_FieldShape_Known(t *testing.T) {
	m, err := BuildManifest(repoRoot)
	if err != nil {
		t.Fatalf("BuildManifest: %v", err)
	}

	start, ok := m.Types["StartRequest"]
	if !ok {
		t.Fatal("StartRequest missing from manifest")
	}
	if start.Home != "types" {
		t.Errorf("StartRequest home = %q, want types", start.Home)
	}
	fields := fieldMap(start)
	identity, ok := fields["identity"]
	if !ok {
		t.Fatal("StartRequest missing identity field")
	}
	if identity.Type != "object" || identity.Ref != "IdentityScope" {
		t.Errorf("StartRequest.identity = {%s, ref=%s}, want {object, ref=IdentityScope}", identity.Type, identity.Ref)
	}
	query, ok := fields["query"]
	if !ok || query.Type != "string" || !query.Optional {
		t.Errorf("StartRequest.query = %+v, want optional string", query)
	}

	errType, ok := m.Types["Error"]
	if !ok {
		t.Fatal("Error missing from manifest")
	}
	if errType.Home != "errors" {
		t.Errorf("Error home = %q, want errors", errType.Home)
	}
	if _, ok := fieldMap(errType)["code"]; !ok {
		t.Error("Error missing code field")
	}
}

// toSet collapses a slice into a presence set.
func toSet(xs []string) map[string]struct{} {
	out := make(map[string]struct{}, len(xs))
	for _, x := range xs {
		out[x] = struct{}{}
	}
	return out
}

// fieldMap indexes a TypeShape's fields by key.
func fieldMap(ts TypeShape) map[string]FieldShape {
	out := make(map[string]FieldShape, len(ts.Fields))
	for _, f := range ts.Fields {
		out[f.Key] = f
	}
	return out
}
