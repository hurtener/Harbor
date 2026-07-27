package schema_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/tools/artifactref"
	"github.com/hurtener/Harbor/internal/tools/schema"
)

// TestDerive_ArtifactRefIsAString — an artifact-reference parameter is a
// declared FIELD TYPE, and the model must be shown the thing it authors:
// the id, as a plain string. Deriving the carrier struct instead would
// teach the model to send an object and every call would fail
// validation.
func TestDerive_ArtifactRefIsAString(t *testing.T) {
	got, err := schema.Derive(reflect.TypeOf(artifactref.Ref{}))
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	if got["type"] != "string" {
		t.Fatalf("type = %v, want string (got %v)", got["type"], got)
	}
	desc, _ := got["description"].(string)
	if !strings.Contains(desc, "artifact reference id") {
		t.Errorf("description does not tell the model to supply an id: %q", desc)
	}
	if strings.Contains(desc, "\n") {
		t.Errorf("description carries a newline, which renders badly in a tool declaration: %q", desc)
	}
	if _, ok := got["properties"]; ok {
		t.Errorf("the carrier struct leaked into the schema: %v", got)
	}
}

// TestDerive_ArtifactRefNestedInAToolArgsStruct — the substitution is
// only reachable where the deriver renders the field, so the string form
// must survive nesting in the shapes a tool actually declares.
func TestDerive_ArtifactRefNestedInAToolArgsStruct(t *testing.T) {
	type args struct {
		Doc      artifactref.Ref   `json:"doc"`
		Extras   []artifactref.Ref `json:"extras,omitempty"`
		MaxWords int               `json:"max_words"`
	}
	got, err := schema.Derive(reflect.TypeOf(args{}))
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	props, _ := got["properties"].(schema.Map)
	doc, _ := props["doc"].(schema.Map)
	if doc == nil || doc["type"] != "string" {
		t.Fatalf("doc is not a string: %s", encoded)
	}
	extras, _ := props["extras"].(schema.Map)
	if extras == nil || extras["type"] != "array" {
		t.Fatalf("extras is not an array: %s", encoded)
	}
	items, _ := extras["items"].(schema.Map)
	if items == nil || items["type"] != "string" {
		t.Fatalf("extras items are not strings: %s", encoded)
	}
	// A required reference stays required; an omitempty one does not.
	required, _ := got["required"].([]string)
	joined := strings.Join(required, ",")
	if !strings.Contains(joined, "doc") {
		t.Errorf("required = %v, want it to include doc", required)
	}
	if strings.Contains(joined, "extras") {
		t.Errorf("required = %v, want it to exclude the omitempty extras", required)
	}
}

// TestDerive_ArtifactRefCompilesAndValidatesTheStringForm — the derived
// document must be accepted by the compiler and must validate the exact
// shape the model writes.
func TestDerive_ArtifactRefCompilesAndValidatesTheStringForm(t *testing.T) {
	type args struct {
		Doc artifactref.Ref `json:"doc"`
	}
	doc, err := schema.Derive(reflect.TypeOf(args{}))
	if err != nil {
		t.Fatalf("Derive: %v", err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	compiled, err := schema.Compile(raw)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if err := schema.Validate(compiled, json.RawMessage(`{"doc":"tool_ab12cd34ef56"}`)); err != nil {
		t.Fatalf("the string form the model writes failed validation: %v", err)
	}
	if err := schema.Validate(compiled, json.RawMessage(`{"doc":{"id":"x"}}`)); err == nil {
		t.Fatal("an object-shaped reference passed validation")
	}
}
