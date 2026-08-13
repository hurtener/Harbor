package turns

import (
	"reflect"
	"sort"
	"testing"
)

// TestOpsDTOs_StructurallyFreeOfForbiddenContent is the structural pin
// behind the mutation-DTO contract (ops.go) AND the operations-safe
// READ projection (row.go): every pinned struct's field set is held to
// EXACTLY its documented allowlist, so the ops family cannot silently
// grow a field that carries transcript, reasoning traces, App
// correlation, or pause tokens, and the operations read projection
// cannot silently grow a consumer field (query, answer, resource URI,
// tool_call_id, ...). A future field that is genuinely needed must be
// added to opsFieldSet (and reviewed) first — the pin fails loud the
// moment the code and the allowlist drift.
//
// The named content channels (ReasoningInput / AppRefInput) are pinned
// too: they are the ONLY shapes allowed to touch those components, and
// AppRef itself is pinned — its optional ToolCallID is the deliberate,
// documented correlation-metadata exception (lazy `mcp.apps.tool_context`
// delivery, never authority), and it can never carry pause /
// resume / approval tokens or raw App context / input / result.
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

// TestOpsDTOs_ForbiddenContentIsNotRepresentable walks the mutation
// DTOs and their component types and fails if ANY field name hints at
// the forbidden categories (transcript, App-correlation tokens, pause /
// resume / approval tokens); it then walks the OPERATIONS-SAFE READ
// projection and additionally fails if any field name hints at
// consumer-only content (query, answer, reasoning, resource URI,
// inline content).
//
// Deliberately excluded from the scan: TurnRow (the CONSUMER-safe row
// legitimately carries the bounded reasoning component, the ordered
// App collection, the durable pause component, and the optional
// AppRef.ToolCallID — that is its purpose) and the two named content
// channels ReasoningInput / AppRefInput (AppRefInput legitimately
// carries the optional ToolCallID correlation metadata through
// AppRef). The prohibition the scan enforces is on the MUTATION ops
// and the OPERATIONS READ projection carrying them, which the
// allowlist pin above already enforces field-by-field.
//
// Token-count fields (Usage.PromptTokens and friends) are usage
// NUMBERS, not pause tokens, and are not flagged.
func TestOpsDTOs_ForbiddenContentIsNotRepresentable(t *testing.T) {
	// Mutation DTOs + the component types they build on: never
	// transcript, never App-correlation tokens, never pause /
	// resume / approval tokens.
	names := []string{
		"Append", "Update", "Seal", "ReasoningInput",
		"Answer", "Usage", "Attachment", "ActivityRow", "ReasoningStep",
		"Agent", "Query", "AnswerRef", "Pause",
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
			case "Token", "PauseToken", "ResumeToken", "ApprovalToken":
				t.Errorf("%s.%s: pause/resume/approval tokens must never be represented", typeName, f.Name)
			}
			if hasSubstring(f.Name, "Correlation") || f.Name == "ToolCallID" || f.Name == "ToolCallId" {
				t.Errorf("%s.%s: App-correlation tokens must never be represented in a mutation DTO", typeName, f.Name)
			}
		}
	}

	// Operations-safe READ projection: additionally never consumer-only
	// content (query, answer, reasoning, resource URI, inline text).
	opsReadNames := []string{"OpsTurnRow", "AppOpsRef"}
	for _, typeName := range opsReadNames {
		typ, ok := typeByName(typeName)
		if !ok {
			t.Fatalf("missing type %q in the operations-read scan", typeName)
		}
		for i := 0; i < typ.NumField(); i++ {
			f := typ.Field(i)
			switch f.Name {
			case "Transcript", "History", "Messages":
				t.Errorf("%s.%s: transcript-shaped field must never exist", typeName, f.Name)
			case "Token", "PauseToken", "ResumeToken", "ApprovalToken":
				t.Errorf("%s.%s: pause/resume/approval tokens must never be represented", typeName, f.Name)
			}
			if f.Name == "Query" || f.Name == "Answer" || f.Name == "Reasoning" ||
				f.Name == "ResourceURI" || f.Name == "Inline" || f.Name == "Ref" {
				t.Errorf("%s.%s: consumer-only content must never be represented in the operations read projection", typeName, f.Name)
			}
			if hasSubstring(f.Name, "Correlation") || f.Name == "ToolCallID" || f.Name == "ToolCallId" {
				t.Errorf("%s.%s: App-correlation tokens must never be represented in the operations read projection", typeName, f.Name)
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
	case "Reasoning":
		return reflect.TypeOf(Reasoning{}), true
	case "AppRef":
		return reflect.TypeOf(AppRef{}), true
	case "AppRefKey":
		return reflect.TypeOf(AppRefKey{}), true
	case "Agent":
		return reflect.TypeOf(Agent{}), true
	case "Query":
		return reflect.TypeOf(Query{}), true
	case "AnswerRef":
		return reflect.TypeOf(AnswerRef{}), true
	case "Pause":
		return reflect.TypeOf(Pause{}), true
	case "OpsTurnRow":
		return reflect.TypeOf(OpsTurnRow{}), true
	case "AppOpsRef":
		return reflect.TypeOf(AppOpsRef{}), true
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
