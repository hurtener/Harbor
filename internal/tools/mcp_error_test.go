package tools

import (
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestMCPResultErrorClassification_StructuralClasses(t *testing.T) {
	meta := map[string]any{MCPResultErrorNamespace: map[string]any{
		"error": map[string]any{"class": string(MCPToolErrorAuthorization), "message": "denied"},
	}}
	class, message, ok := MCPResultErrorClassification(meta, nil, "")
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
	class, _, ok := MCPResultErrorClassification(nil, structured, "")
	if !ok || class != MCPToolErrorProviderUnavailable {
		t.Fatalf("structured classification = %q, %v", class, ok)
	}
	class, _, ok = MCPResultErrorClassification(map[string]any{
		MCPResultErrorNamespace: map[string]any{"error": map[string]any{"class": "unknown", "message": "ignored"}},
	}, nil, "")
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
	if _, _, ok := MCPResultErrorClassification(meta, "transient", ""); ok {
		t.Fatal("malformed metadata or text produced a classification")
	}
}

func TestMCPResultErrorClassification_DeterministicStructuredCodes(t *testing.T) {
	for _, tc := range []struct {
		code  string
		class MCPToolErrorClass
	}{
		{"invalid_argument", MCPToolErrorInvalidArgument},
		{"validation_failed", MCPToolErrorValidation},
		{"forbidden_scope", MCPToolErrorAuthorization},
		{"scope_blocked", MCPToolErrorAuthorization},
		{"scope_mismatch", MCPToolErrorAuthorization},
		{"not_found", MCPToolErrorNotFound},
		{"document_too_large", MCPToolErrorToolDomain},
	} {
		t.Run(tc.code, func(t *testing.T) {
			for _, retryable := range []any{nil, false} {
				structured := map[string]any{"code": tc.code, "message": "ignored provider prose"}
				if retryable != nil {
					structured["retryable"] = retryable
				}
				class, message, ok := MCPResultErrorClassification(nil, structured, "")
				if !ok || class != tc.class || !strings.HasPrefix(message, tc.code) || strings.Contains(message, "ignored") {
					t.Fatalf("classification = %q, %q, %v", class, message, ok)
				}
				if ClassifyError(NewMCPToolResultError(class, message), false) != ErrClassPermanent {
					t.Fatal("deterministic code must be permanent")
				}
				if tc.code == "document_too_large" && (!strings.Contains(message, "outline/section selectors") || !strings.Contains(message, "read-size limits") || !strings.Contains(message, "do not repeat")) {
					t.Fatalf("oversize guidance missing: %q", message)
				}
			}
		})
	}
}

func TestMCPResultErrorClassification_StructuredCodeFallbackRemainsConservative(t *testing.T) {
	for _, structured := range []any{
		map[string]any{"code": "not_found", "retryable": true},
		map[string]any{"code": "not_found", "retryable": "false"},
		map[string]any{"code": "provider_unavailable"},
		map[string]any{"code": "internal"},
		map[string]any{"code": "future_code"},
		map[string]any{"code": "not_found later"},
		map[string]any{"code": 404},
		map[string]any{"error": map[string]any{"code": "not_found"}},
		map[string]any{"message": "not_found"},
		map[string]any{"code": "not_found", MCPResultErrorNamespace: "malformed"},
		`{"code":"not_found"}`,
	} {
		class, message, ok := MCPResultErrorClassification(nil, structured, "")
		if ok || class != MCPToolErrorTransient || message != "" {
			t.Fatalf("%#v classification = %q, %q, %v, want unrecognized transient", structured, class, message, ok)
		}
	}
	class, _, ok := MCPResultErrorClassification(map[string]any{MCPResultErrorNamespace: "malformed"}, map[string]any{"code": "not_found"}, "")
	if ok || class != MCPToolErrorTransient {
		t.Fatalf("malformed contract classification = %q, %v", class, ok)
	}
}

func TestMCPResultErrorClassification_ExplicitContractWinsOverStructuredCode(t *testing.T) {
	contract := map[string]any{"error": map[string]any{"class": "provider_unavailable", "message": "temporarily unavailable"}}
	for _, metadata := range []bool{false, true} {
		structured := map[string]any{"code": "not_found"}
		var meta map[string]any
		if metadata {
			meta = map[string]any{MCPResultErrorNamespace: contract}
		} else {
			structured[MCPResultErrorNamespace] = contract
		}
		class, message, ok := MCPResultErrorClassification(meta, structured, "")
		if !ok || class != MCPToolErrorProviderUnavailable || message != "temporarily unavailable" {
			t.Fatalf("explicit contract classification = %q, %q, %v", class, message, ok)
		}
	}
}

// A code-only structured error keeps the provider's human-readable repair
// detail (bounded) in the derived message; the class still comes only from
// the code, and document_too_large keeps both the text and its guidance.
func TestMCPResultErrorClassification_StructuredCodeKeepsProviderText(t *testing.T) {
	const detail = "field section_id must name an existing section; available sections: intro, results"
	class, message, ok := MCPResultErrorClassification(nil, map[string]any{"code": "invalid_argument"}, detail)
	if !ok || class != MCPToolErrorInvalidArgument || message != "invalid_argument: "+detail {
		t.Fatalf("classification = %q, %q, %v", class, message, ok)
	}
	if _, message, _ := MCPResultErrorClassification(nil, map[string]any{"code": "not_found"}, "not_found"); message != "not_found" {
		t.Fatalf("text equal to the code duplicated: %q", message)
	}
	long := strings.Repeat("é", MCPToolErrorMessageLimit)
	class, message, ok = MCPResultErrorClassification(nil, map[string]any{"code": "document_too_large"}, "document has 9000 pages; "+long)
	if !ok || class != MCPToolErrorToolDomain {
		t.Fatalf("classification = %q, %v", class, ok)
	}
	if !strings.HasPrefix(message, "document_too_large: document has 9000 pages") || !strings.HasSuffix(message, documentTooLargeGuidance) {
		t.Fatalf("oversize message lost provider text or guidance: %q", message)
	}
	if len(message) > MCPToolErrorMessageLimit || !utf8.ValidString(message) {
		t.Fatalf("message not bounded/valid: len=%d", len(message))
	}
	if _, message, ok := MCPResultErrorClassification(nil, map[string]any{"code": "future_code"}, detail); ok || message != "" {
		t.Fatalf("provider text must not classify: %q %v", message, ok)
	}
}
