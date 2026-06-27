package main

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/singlesource"
)

// repoRoot is the repository root relative to this test's package dir
// (cmd/harbor-protocol-ts-types).
const repoRoot = "../.."

// committedModulePath is the committed external-client module's path
// relative to the repository root — the same default the generator writes.
const committedModulePath = "examples/protocol-clients/event-viewer-ts/harbor-protocol.gen.ts"

// TestTSTypes_TypeInstanceIndexInLockstep pins the reflection index
// against singlesource.CanonicalWireTypes: every canonical name has a
// struct instance whose reflect name matches, and no instance is stale.
// A new canonical wire type without an index entry fails here — the
// mechanism that makes the generated module's coverage exhaustive.
func TestTSTypes_TypeInstanceIndexInLockstep(t *testing.T) {
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

// TestTSTypes_BuildCoversCanonicalSurface asserts the built module is
// exhaustive over the canonical Protocol surface and carries no stale
// entries: every canonical wire type / method / error code is present,
// and nothing in the module is unregistered. A new method or error code
// added Go-side appears in the freshly-built module automatically; the
// `git diff` half of the gate then fails until the committed module is
// regenerated.
func TestTSTypes_BuildCoversCanonicalSurface(t *testing.T) {
	m, err := BuildModule(repoRoot)
	if err != nil {
		t.Fatalf("BuildModule: %v", err)
	}

	ifaceNames := make(map[string]struct{}, len(m.Types))
	for _, it := range m.Types {
		ifaceNames[it.Name] = struct{}{}
	}
	for name := range singlesource.CanonicalWireTypes {
		if _, ok := ifaceNames[name]; !ok {
			t.Errorf("canonical wire type %q missing from module", name)
		}
	}
	if len(m.Types) != len(singlesource.CanonicalWireTypes) {
		t.Errorf("module has %d interfaces, CanonicalWireTypes has %d", len(m.Types), len(singlesource.CanonicalWireTypes))
	}

	moduleMethods := toSet(m.Methods)
	for _, mm := range methods.Methods() {
		if _, ok := moduleMethods[string(mm)]; !ok {
			t.Errorf("canonical method %q missing from module", mm)
		}
	}
	for _, mm := range m.Methods {
		if !methods.IsValidMethod(methods.Method(mm)) {
			t.Errorf("module names method %q, which is not canonical — stale entry", mm)
		}
	}

	moduleErrors := toSet(m.Errors)
	for _, c := range protoerrors.Codes() {
		if _, ok := moduleErrors[string(c)]; !ok {
			t.Errorf("canonical error code %q missing from module", c)
		}
	}
	for _, c := range m.Errors {
		if !protoerrors.IsValidCode(protoerrors.Code(c)) {
			t.Errorf("module names error code %q, which is not canonical — stale entry", c)
		}
	}

	// Events are harvested by a textual source scan (the same sanctioned
	// baseline cmd/harbor-gen-protocol-docs uses), not a typed registry like
	// Methods()/Codes()/CanonicalWireTypes — there is no single canonical
	// event-name list to bijection-check against. So this asserts presence +
	// a known anchor rather than a full set equality; drift on an
	// already-emitted event is caught by the `git diff --exit-code` gate.
	if len(m.Events) == 0 {
		t.Error("module carries no events — the event tree scan found nothing")
	}
	if _, ok := toSet(m.Events)["task.started"]; !ok {
		t.Error(`expected canonical event type "task.started" in the module`)
	}
}

// TestTSTypes_DeterministicAndStable asserts two independent builds render
// byte-identical output — the property the `git diff --exit-code` gate
// relies on.
func TestTSTypes_DeterministicAndStable(t *testing.T) {
	a, err := BuildModule(repoRoot)
	if err != nil {
		t.Fatalf("BuildModule a: %v", err)
	}
	b, err := BuildModule(repoRoot)
	if err != nil {
		t.Fatalf("BuildModule b: %v", err)
	}
	ra := render(a)
	rb := render(b)
	if string(ra) != string(rb) {
		t.Error("module render is non-deterministic across builds")
	}
}

// TestTSTypes_CommittedFileInSync regenerates the module in memory and
// compares it byte-for-byte to the committed file, so a stale committed
// module fails `go test` as well as the `make protocol-ts-types-gen-check`
// gate (belt and suspenders — a Go wire change that forgets the regen is
// caught by the test suite directly).
func TestTSTypes_CommittedFileInSync(t *testing.T) {
	m, err := BuildModule(repoRoot)
	if err != nil {
		t.Fatalf("BuildModule: %v", err)
	}
	want := render(m)
	got, err := os.ReadFile(filepath.Join(repoRoot, committedModulePath))
	if err != nil {
		t.Fatalf("read committed module: %v", err)
	}
	if string(got) != string(want) {
		t.Errorf("committed %s is stale — run 'make protocol-ts-types-gen' and commit the result", committedModulePath)
	}
}

// TestTSTypes_FieldShape_Known sanity-checks the field walker + TS-type
// mapping on well-understood types: a request with an optional scalar and
// a nested object ref (StartRequest), an array-of-scalar + map field
// (StartRequest), the Error wire type homed in the errors package, and a
// known string-union member.
func TestTSTypes_FieldShape_Known(t *testing.T) {
	m, err := BuildModule(repoRoot)
	if err != nil {
		t.Fatalf("BuildModule: %v", err)
	}
	byName := make(map[string]TypeIface, len(m.Types))
	for _, it := range m.Types {
		byName[it.Name] = it
	}

	start, ok := byName["StartRequest"]
	if !ok {
		t.Fatal("StartRequest missing from module")
	}
	fields := fieldMap(start)
	if f, ok := fields["identity"]; !ok || f.TSType != "IdentityScope" || f.Optional {
		t.Errorf("StartRequest.identity = %+v, want required IdentityScope ref", f)
	}
	if f, ok := fields["query"]; !ok || f.TSType != "string" || !f.Optional {
		t.Errorf("StartRequest.query = %+v, want optional string", f)
	}
	if f, ok := fields["input_artifact_ids"]; !ok || f.TSType != "string[]" {
		t.Errorf("StartRequest.input_artifact_ids = %+v, want string[]", f)
	}
	if f, ok := fields["input_artifact_dispositions"]; !ok || !strings.HasPrefix(f.TSType, "Record<string,") {
		t.Errorf("StartRequest.input_artifact_dispositions = %+v, want a Record<string, …>", f)
	}

	if _, ok := byName["Error"]; !ok {
		t.Fatal("Error missing from module")
	}
	if _, ok := fieldMap(byName["Error"])["code"]; !ok {
		t.Error("Error missing code field")
	}
}

// TestTSTypes_RenderShape asserts the rendered module carries the
// load-bearing banner, the pinned constants, the union types, and a known
// interface — a cheap guard the render layout did not silently change
// shape.
func TestTSTypes_RenderShape(t *testing.T) {
	m, err := BuildModule(repoRoot)
	if err != nil {
		t.Fatalf("BuildModule: %v", err)
	}
	src := string(render(m))
	for _, want := range []string{
		"CODE GENERATED BY cmd/harbor-protocol-ts-types. DO NOT EDIT.",
		"export const PROTOCOL_VERSION =",
		"export const WIRE_SURFACE_DIGEST =",
		"export type HarborMethod =",
		"export type HarborErrorCode =",
		"export type HarborEventType =",
		"export interface RuntimeInfo {",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("rendered module missing %q", want)
		}
	}
	if !strings.HasSuffix(src, "\n") {
		t.Error("rendered module must be newline-terminated for a stable git diff")
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

// fieldMap indexes a TypeIface's fields by key.
func fieldMap(it TypeIface) map[string]FieldDef {
	out := make(map[string]FieldDef, len(it.Fields))
	for _, f := range it.Fields {
		out[f.Key] = f
	}
	return out
}
