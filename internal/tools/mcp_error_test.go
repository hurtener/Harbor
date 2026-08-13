package tools

import (
	"errors"
	"strings"
	"testing"
)

func TestMCPResultErrorClassification_StructuralClasses(t *testing.T) {
	meta := map[string]any{MCPResultErrorNamespace: map[string]any{
		"error": map[string]any{"class": string(MCPToolErrorAuthorization), "message": "denied"},
	}}
	class, message, ok := MCPResultErrorClassification(meta, nil)
	if !ok || class != MCPToolErrorAuthorization || message != "denied" {
		t.Fatalf("classification = %q, %q, %v", class, message, ok)
	}
	err := NewMCPToolResultError(class, message)
	if !errors.Is(err, ErrMCPToolError) {
		t.Fatal("typed result error does not unwrap to ErrMCPToolError")
	}
	if got := ClassifyError(err, false); got != ErrClassPermanent {
		t.Fatalf("ClassifyError = %s, want permanent", got)
	}
}

func TestMCPResultErrorClassification_StructuredAndLegacyFallback(t *testing.T) {
	structured := map[string]any{MCPResultErrorNamespace: map[string]any{
		"error": map[string]any{"class": string(MCPToolErrorProviderUnavailable), "message": "try again"},
	}}
	class, _, ok := MCPResultErrorClassification(nil, structured)
	if !ok || class != MCPToolErrorProviderUnavailable {
		t.Fatalf("structured classification = %q, %v", class, ok)
	}
	class, _, ok = MCPResultErrorClassification(map[string]any{
		MCPResultErrorNamespace: map[string]any{"error": map[string]any{"class": "unknown", "message": "ignored"}},
	}, nil)
	if ok || class != MCPToolErrorTransient {
		t.Fatalf("unknown classification = %q, %v, want legacy transient", class, ok)
	}
	long := strings.Repeat("x", MCPToolErrorMessageLimit+50)
	var resultErr *MCPToolResultError
	if !errors.As(NewMCPToolResultError(MCPToolErrorTransient, long), &resultErr) || len(resultErr.Message) != MCPToolErrorMessageLimit {
		t.Fatalf("message length = %d, want %d", len(resultErr.Message), MCPToolErrorMessageLimit)
	}
}

func TestMCPResultErrorClassification_DoesNotParseText(t *testing.T) {
	meta := map[string]any{MCPResultErrorNamespace: map[string]any{"message": "authorization"}}
	if _, _, ok := MCPResultErrorClassification(meta, "transient"); ok {
		t.Fatal("malformed metadata or text produced a classification")
	}
}
