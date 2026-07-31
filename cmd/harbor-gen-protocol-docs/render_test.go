package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/singlesource"
)

// TestRun_WritesFourDeterministicPages runs the whole command twice
// against temp dirs and asserts (a) all four pages land with the
// generated header, (b) the two runs are byte-identical — the property
// the `make protocol-docs-gen-check` diff gate depends on.
func TestRun_WritesFourDeterministicPages(t *testing.T) {
	dirA, dirB := t.TempDir(), t.TempDir()
	if err := run(dirA); err != nil {
		t.Fatalf("run(A): %v", err)
	}
	if err := run(dirB); err != nil {
		t.Fatalf("run(B): %v", err)
	}
	for _, name := range []string{"methods.md", "events.md", "errors.md", "types.md"} {
		a, err := os.ReadFile(filepath.Join(dirA, name))
		if err != nil {
			t.Fatalf("read %s (A): %v", name, err)
		}
		b, err := os.ReadFile(filepath.Join(dirB, name))
		if err != nil {
			t.Fatalf("read %s (B): %v", name, err)
		}
		if !strings.HasPrefix(string(a), generatedHeader) {
			t.Errorf("%s: missing generated-file header", name)
		}
		if string(a) != string(b) {
			t.Errorf("%s: two runs produced different bytes — generation is not deterministic", name)
		}
	}
}

// TestRenderMethodsPage_CoversEveryCanonicalMethod asserts the rendered
// page names every methods.Methods() entry as a table row.
func TestRenderMethodsPage_CoversEveryCanonicalMethod(t *testing.T) {
	page, err := renderMethodsPage()
	if err != nil {
		t.Fatalf("renderMethodsPage: %v", err)
	}
	for _, m := range methods.Methods() {
		if !strings.Contains(page, "| `"+string(m)+"` |") {
			t.Errorf("methods.md is missing a row for %q", m)
		}
	}
}

// TestRenderEventsPage_CoversEveryRegisteredType asserts the rendered
// page carries one section per registered event type.
func TestRenderEventsPage_CoversEveryRegisteredType(t *testing.T) {
	page, err := renderEventsPage()
	if err != nil {
		t.Fatalf("renderEventsPage: %v", err)
	}
	for _, et := range events.EventTypes() {
		if !strings.Contains(page, "## `"+string(et)+"`") {
			t.Errorf("events.md is missing a section for %q", et)
		}
	}
}

// TestRenderErrorsPage_CoversEveryCode asserts the rendered page names
// every canonical error code as a table row.
func TestRenderErrorsPage_CoversEveryCode(t *testing.T) {
	page, err := renderErrorsPage()
	if err != nil {
		t.Fatalf("renderErrorsPage: %v", err)
	}
	for _, code := range protoerrors.Codes() {
		if !strings.Contains(page, "| `"+string(code)+"` |") {
			t.Errorf("errors.md is missing a row for %q", code)
		}
	}
}

// TestRenderTypesPage_CoversEveryCanonicalType asserts the rendered
// page carries one section per CanonicalWireTypes entry, and that the
// snake_case wire tags survive (spot-check on StartRequest).
func TestRenderTypesPage_CoversEveryCanonicalType(t *testing.T) {
	page, err := renderTypesPage()
	if err != nil {
		t.Fatalf("renderTypesPage: %v", err)
	}
	for name := range singlesource.CanonicalWireTypes {
		if !strings.Contains(page, "\n## "+name+"\n") {
			t.Errorf("types.md is missing a section for %q", name)
		}
	}
	for _, key := range []string{"`idempotency_key`", "`input_artifact_ids`", "`protocol_version`"} {
		if !strings.Contains(page, key) {
			t.Errorf("types.md: expected StartRequest/StartResponse wire key %s", key)
		}
	}
}

// tagFixture pins every wire-key derivation the renderer must reproduce
// from encoding/json: an untagged field keeps its Go name, a tagged one
// renames, `json:"-"` drops, `json:"-,"` names the key "-", a name-less
// options-only tag keeps the Go name, and the option list annotates.
type tagFixture struct {
	Untagged   string
	Tagged     string `json:"tagged"`
	Dropped    string `json:"-"`
	LiteralDsh string `json:"-,"` //nolint:staticcheck // fixture: pins encoding/json's `-,` form, whose wire key IS "-" (distinct from the dropping `-`)
	OptsOnly   string `json:",omitempty"`
	Optional   int    `json:"optional,omitempty"`
	Stringed   int64  `json:"stringed,string"`
	unexported string //nolint:unused // fixture: proves unexported fields never render
}

// TestStructFieldRows_HonoursJSONTag_TaggedUntaggedAndDropped asserts the
// field-table rows follow encoding/json's key derivation. The regression
// this pins: event payloads were rendered with the Go field name on the
// premise that "event payloads carry no json tags" — false since the
// memory subsystem shipped a tagged payload, so the generated page
// advertised `Bytes` where the wire carries `bytes`.
func TestStructFieldRows_HonoursJSONTag_TaggedUntaggedAndDropped(t *testing.T) {
	rows := structFieldRows(reflect.TypeOf(tagFixture{}))

	got := make([]string, 0, len(rows))
	notes := map[string]string{}
	for _, r := range rows {
		got = append(got, r.key)
		notes[r.key] = r.notes
	}
	want := []string{"Untagged", "tagged", "-", "OptsOnly", "optional", "stringed"}
	if len(got) != len(want) {
		t.Fatalf("wire keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("wire key %d = %q, want %q (order is wire order)", i, got[i], want[i])
		}
	}
	for _, key := range got {
		if key == "Dropped" || key == "unexported" {
			t.Errorf("field %q must not render", key)
		}
	}
	if notes["Untagged"] != "" {
		t.Errorf("untagged field notes = %q, want empty", notes["Untagged"])
	}
	if notes["optional"] != "optional (`omitempty`)" {
		t.Errorf("omitempty notes = %q", notes["optional"])
	}
	if notes["OptsOnly"] != "optional (`omitempty`)" {
		t.Errorf("name-less omitempty notes = %q", notes["OptsOnly"])
	}
	if notes["stringed"] != "JSON-string-encoded (`,string`)" {
		t.Errorf(",string notes = %q", notes["stringed"])
	}
}

// TestStructFieldRows_MatchesEncodingJSON_OnTheFixture cross-checks the
// derived keys against what encoding/json actually emits for the same
// struct — the property the renderer claims, verified against the
// implementation rather than against a second hand-written list.
func TestStructFieldRows_MatchesEncodingJSON_OnTheFixture(t *testing.T) {
	// Every field non-zero so the `omitempty` ones are present: the
	// renderer documents the full declared surface, not one instance.
	blob, err := json.Marshal(tagFixture{
		Untagged:   "a",
		Tagged:     "b",
		Dropped:    "c",
		LiteralDsh: "d",
		OptsOnly:   "e",
		Optional:   1,
		Stringed:   2,
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	var emitted map[string]any
	if err := json.Unmarshal(blob, &emitted); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	rows := structFieldRows(reflect.TypeOf(tagFixture{}))
	if len(rows) != len(emitted) {
		t.Fatalf("renderer emitted %d rows, encoding/json emitted %d keys (%v)", len(rows), len(emitted), emitted)
	}
	for _, r := range rows {
		if _, ok := emitted[r.key]; !ok {
			t.Errorf("renderer advertises wire key %q that encoding/json never emits", r.key)
		}
	}
}

// TestRenderEventsPage_TaggedPayloadRendersWireKey asserts the real
// tagged event payload in the tree documents its lower-case wire keys.
func TestRenderEventsPage_TaggedPayloadRendersWireKey(t *testing.T) {
	page, err := renderEventsPage()
	if err != nil {
		t.Fatalf("renderEventsPage: %v", err)
	}
	idx := strings.Index(page, "## `memory.caller_block_admitted`")
	if idx < 0 {
		t.Fatal("events.md missing memory.caller_block_admitted")
	}
	section := page[idx:min(idx+500, len(page))]
	for _, key := range []string{"| `bytes` |", "| `tier` |", "| `key` |"} {
		if !strings.Contains(section, key) {
			t.Errorf("expected tagged wire key %s; section:\n%s", key, section)
		}
	}
	for _, goName := range []string{"| `Bytes` |", "| `Tier` |", "| `Key` |"} {
		if strings.Contains(section, goName) {
			t.Errorf("Go field name %s leaked as a wire key; section:\n%s", goName, section)
		}
	}
}

// TestRenderEventsPage_UntaggedPayloadKeepsGoFieldName asserts the
// fallback is unchanged: an untagged payload still documents capitalised
// Go field names, because that is what encoding/json puts on the wire.
func TestRenderEventsPage_UntaggedPayloadKeepsGoFieldName(t *testing.T) {
	page, err := renderEventsPage()
	if err != nil {
		t.Fatalf("renderEventsPage: %v", err)
	}
	idx := strings.Index(page, "## `task.spawned`")
	if idx < 0 {
		t.Fatal("events.md missing task.spawned")
	}
	section := page[idx:min(idx+500, len(page))]
	if !strings.Contains(section, "| `TaskID` |") {
		t.Errorf("untagged payload should keep the Go field name; section:\n%s", section)
	}
}

// TestRenderEventsPage_SafeVsRedactedClassification spot-checks the
// mechanical SafePayload classification: a SafeSealed payload renders
// as typed-verbatim, a Sealed-only payload renders as redacted.
func TestRenderEventsPage_SafeVsRedactedClassification(t *testing.T) {
	page, err := renderEventsPage()
	if err != nil {
		t.Fatalf("renderEventsPage: %v", err)
	}
	// task.spawned carries a SafeSealed payload.
	idx := strings.Index(page, "## `task.spawned`")
	if idx < 0 {
		t.Fatal("events.md missing task.spawned")
	}
	section := page[idx:min(idx+400, len(page))]
	if !strings.Contains(section, "safe payload") {
		t.Errorf("task.spawned should classify as a safe payload; section:\n%s", section)
	}
	// runtime.error carries a Sealed (redacted) payload.
	idx = strings.Index(page, "## `runtime.error`")
	if idx < 0 {
		t.Fatal("events.md missing runtime.error")
	}
	section = page[idx:min(idx+400, len(page))]
	if !strings.Contains(section, "redacted") {
		t.Errorf("runtime.error should classify as redacted; section:\n%s", section)
	}
}
