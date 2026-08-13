package turns

import (
	"reflect"
	"sort"
	"testing"
)

// TestOpsDTOs_StructurallyFreeOfForbiddenContent is the structural pin
// behind the operations-safe DTO contract (ops.go): every ops struct
// field set is held to EXACTLY its documented allowlist, so the ops
// family cannot silently grow a field that carries transcript,
// reasoning, App-correlation, or pause tokens. A future field that is
// genuinely needed must be added to opsFieldSet (and reviewed) first —
// the pin fails loud the moment the code and the allowlist drift.
//
// The named content channels (ReasoningInput / AppRefInput) are pinned
// too: they are the ONLY shapes allowed to touch those components, and
// AppRef itself is pinned to render-metadata-plus-availability (it
// structurally cannot carry a correlation token).
func TestOpsDTOs_StructurallyFreeOfForbiddenContent(t *testing.T) {
	for typeName, want := range opsFieldSet {
		typ, ok := typeByName(typeName)
		if !ok {
			t.Fatalf("opsFieldSet names %q, but no such type exists — stale allowlist", typeName)
		}
		if typ.Kind() != reflect.Struct {
			t.Fatalf("%s is not a struct", typeName)
		}
		got := fieldNames(typ)
		sort.Strings(want)
		sort.Strings(got)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s field set drift:\n  got:  %v\n  want: %v\n"+
				"a field carrying transcript / reasoning / App-correlation / pause-token content "+
				"cannot be added to the DTO family silently — update opsFieldSet deliberately.",
				typeName, got, want)
		}
	}
}

// TestOpsDTOs_ForbiddenContentIsNotRepresentable walks every ops and
// named-channel type and fails if ANY field name hints at the four
// forbidden categories. TurnRow is deliberately excluded: the
// CONSUMER-safe row legitimately carries the bounded reasoning
// component and the App ref (that is its purpose) — the prohibition is
// on the OPERATIONS carrying them, which the allowlist pin above
// already enforces. Token-count fields (Usage.PromptTokens and
// friends) are usage NUMBERS, not pause tokens, and are not flagged.
func TestOpsDTOs_ForbiddenContentIsNotRepresentable(t *testing.T) {
	names := []string{
		"Append", "Update", "Seal", "ReasoningInput", "AppRefInput",
		"Answer", "Usage", "Attachment", "ActivityRow", "ReasoningStep",
		"AppRef", "Agent", "Query", "AnswerRef",
	}
	for _, typeName := range names {
		typ, ok := typeByName(typeName)
		if !ok {
			t.Fatalf("missing type %q in the forbidden-content scan", typeName)
		}
		if typ.Kind() != reflect.Struct {
			continue
		}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			switch f.Name {
			case "Transcript", "History", "Messages":
				t.Errorf("%s.%s: transcript-shaped field must never exist", typeName, f.Name)
			case "Token", "PauseToken", "ResumeToken":
				t.Errorf("%s.%s: pause/resume tokens must never be represented", typeName, f.Name)
			}
			if hasSubstring(f.Name, "Correlation") || f.Name == "ToolCallID" || f.Name == "ToolCallId" {
				t.Errorf("%s.%s: App-correlation tokens must never be represented", typeName, f.Name)
			}
		}
	}
}

func typeByName(name string) (reflect.Type, bool) {
	switch name {
	case "Append":
		return reflect.TypeOf(Append{}), true
	case "Update":
		return reflect.TypeOf(Update{}), true
	case "Seal":
		return reflect.TypeOf(Seal{}), true
	case "ReasoningInput":
		return reflect.TypeOf(ReasoningInput{}), true
	case "AppRefInput":
		return reflect.TypeOf(AppRefInput{}), true
	case "Answer":
		return reflect.TypeOf(Answer{}), true
	case "Usage":
		return reflect.TypeOf(Usage{}), true
	case "Attachment":
		return reflect.TypeOf(Attachment{}), true
	case "ActivityRow":
		return reflect.TypeOf(ActivityRow{}), true
	case "ReasoningStep":
		return reflect.TypeOf(ReasoningStep{}), true
	case "AppRef":
		return reflect.TypeOf(AppRef{}), true
	case "Agent":
		return reflect.TypeOf(Agent{}), true
	case "Query":
		return reflect.TypeOf(Query{}), true
	case "AnswerRef":
		return reflect.TypeOf(AnswerRef{}), true
	case "TurnRow":
		return reflect.TypeOf(TurnRow{}), true
	}
	return nil, false
}

func fieldNames(typ reflect.Type) []string {
	out := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		out = append(out, typ.Field(i).Name)
	}
	return out
}

func hasSubstring(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
