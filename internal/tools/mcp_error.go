package tools

import (
	"errors"
	"fmt"
	"strings"
)

// ErrMCPToolError identifies a server-side MCP tool result error. It is
// transport independent; callers can use errors.Is without knowing whether
// the result arrived over HTTP, stdio, or another MCP transport.
var ErrMCPToolError = errors.New("mcp: server returned tool error")

// MCPToolErrorClass is the provider-authored, structural failure class in an
// MCP tool result. The detailed values retain useful domain information while
// the policy projects them onto permanent or retryable behavior.
type MCPToolErrorClass string

// MCPErrorClass is the concise alias used by callers that do not need the
// ToolResult qualifier.
type MCPErrorClass = MCPToolErrorClass

const (
	MCPToolErrorInvalidArgument     MCPToolErrorClass = "invalid_argument"
	MCPToolErrorValidation          MCPToolErrorClass = "validation"
	MCPToolErrorAuthorization       MCPToolErrorClass = "authorization"
	MCPToolErrorNotFound            MCPToolErrorClass = "not_found"
	MCPToolErrorConflict            MCPToolErrorClass = "conflict"
	MCPToolErrorToolDomain          MCPToolErrorClass = "tool_domain"
	MCPToolErrorTransient           MCPToolErrorClass = "transient"
	MCPToolErrorProviderUnavailable MCPToolErrorClass = "provider_unavailable"
)

const (
	MCPErrorClassInvalidArgument     = MCPToolErrorInvalidArgument
	MCPErrorClassValidation          = MCPToolErrorValidation
	MCPErrorClassAuthorization       = MCPToolErrorAuthorization
	MCPErrorClassNotFound            = MCPToolErrorNotFound
	MCPErrorClassConflict            = MCPToolErrorConflict
	MCPErrorClassToolDomain          = MCPToolErrorToolDomain
	MCPErrorClassTransient           = MCPToolErrorTransient
	MCPErrorClassProviderUnavailable = MCPToolErrorProviderUnavailable
)

const (
	// MCPResultErrorNamespace carries Harbor's explicit provider-authored
	// classification in standard MCP result metadata or structured content.
	MCPResultErrorNamespace  = "harbor"
	MCPToolErrorMessageLimit = 512
)

// MCPToolResultError carries a bounded, typed MCP result failure and the
// lowered result is returned separately by the driver for planner and App
// consumers. It unwraps to ErrMCPToolError for compatibility.
type MCPToolResultError struct {
	Class   MCPToolErrorClass
	Message string
	// Result is the bounded lowered value, when the driver has completed
	// lowering. It lets callers retain planner/App projections while the
	// policy consumes the error.
	Result ToolResult
	// Recognized records whether the provider supplied a recognized structural
	// classification. Policy may still retry an unrecognized result as the
	// legacy transient case, but lifecycle projection reports it honestly.
	Recognized bool
}

func (e *MCPToolResultError) Error() string {
	class := e.Class
	if !isMCPToolErrorClass(class) {
		class = MCPToolErrorTransient
	}
	return fmt.Sprintf("%s (%s): %s", ErrMCPToolError, class, boundMCPErrorMessage(e.Message))
}

func (e *MCPToolResultError) Unwrap() error { return ErrMCPToolError }

// NewMCPToolResultError constructs a safe error. Invalid or absent provider
// classifications deliberately become the legacy transient class.
func NewMCPToolResultError(class MCPToolErrorClass, message string) error {
	return NewMCPToolResultErrorClassification(class, message, isMCPToolErrorClass(class))
}

// NewMCPToolResultErrorClassification constructs a result error while
// retaining whether its provider classification was structurally recognized.
// Unrecognized classes remain transient for retry compatibility.
func NewMCPToolResultErrorClassification(class MCPToolErrorClass, message string, recognized bool) error {
	if !recognized || !isMCPToolErrorClass(class) {
		class = MCPToolErrorTransient
		recognized = false
	}
	return &MCPToolResultError{Class: class, Message: boundMCPErrorMessage(message), Recognized: recognized}
}

func isMCPToolErrorClass(class MCPToolErrorClass) bool {
	switch class {
	case MCPToolErrorInvalidArgument, MCPToolErrorValidation,
		MCPToolErrorAuthorization, MCPToolErrorNotFound, MCPToolErrorConflict,
		MCPToolErrorToolDomain, MCPToolErrorTransient, MCPToolErrorProviderUnavailable:
		return true
	default:
		return false
	}
}

func boundMCPErrorMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > MCPToolErrorMessageLimit {
		message = message[:MCPToolErrorMessageLimit]
	}
	return message
}

// MCPResultErrorClassification prefers the explicitly namespaced classification,
// then accepts a closed set of deterministic top-level structured error codes.
// It never classifies from human-readable text or nested domain payloads.
// providerText is the result's human-readable content: on the structured-code
// compatibility path it is carried, bounded, into the derived message so the
// planner keeps the provider's repair detail; it never affects the class.
func MCPResultErrorClassification(metadata map[string]any, structured any, providerText string) (MCPToolErrorClass, string, bool) {
	if class, message, ok := mcpResultErrorObject(metadata[MCPResultErrorNamespace]); ok {
		return class, message, true
	}
	if object, ok := structured.(map[string]any); ok {
		if class, message, found := mcpResultErrorObject(object[MCPResultErrorNamespace]); found {
			return class, message, true
		}
		// An explicitly supplied but invalid Harbor contract must not acquire
		// a different meaning from the compatibility fallback.
		_, metadataContract := metadata[MCPResultErrorNamespace]
		_, structuredContract := object[MCPResultErrorNamespace]
		if !metadataContract && !structuredContract {
			return mcpStructuredErrorCode(object, providerText)
		}
	}
	return MCPToolErrorTransient, "", false
}

// mcpStructuredErrorCode handles providers that publish a typed code without
// the Harbor extension. Unknown codes and explicit retryability retain the
// legacy fallback; no prose, HTTP status, or successful result is interpreted.
func mcpStructuredErrorCode(object map[string]any, providerText string) (MCPToolErrorClass, string, bool) {
	if raw, exists := object["retryable"]; exists {
		retryable, ok := raw.(bool)
		if !ok || retryable {
			return MCPToolErrorTransient, "", false
		}
	}
	code, ok := object["code"].(string)
	if !ok {
		return MCPToolErrorTransient, "", false
	}
	var class MCPToolErrorClass
	switch code {
	case "invalid_argument":
		class = MCPToolErrorInvalidArgument
	case "validation_failed":
		class = MCPToolErrorValidation
	case "forbidden_scope", "scope_blocked", "scope_mismatch":
		class = MCPToolErrorAuthorization
	case "not_found":
		class = MCPToolErrorNotFound
	case "document_too_large":
		return MCPToolErrorToolDomain, mcpCodeMessage(code, providerText, documentTooLargeGuidance), true
	default:
		return MCPToolErrorTransient, "", false
	}
	return class, mcpCodeMessage(code, providerText, ""), true
}

// documentTooLargeGuidance is the fixed repair hint appended to a
// `document_too_large` code. Generic on purpose: providers name their read
// limits differently, and some have none.
const documentTooLargeGuidance = "read a smaller section using the provider's outline/section selectors or reduce the provider's read-size limits; do not repeat the same full-document read"

// mcpCodeMessage builds the bounded message for a code-classified result:
// the stable code, then the provider's own text (trimmed to the room the
// fixed guidance leaves), then the guidance. The provider text is carried
// verbatim for the planner and never inspected.
func mcpCodeMessage(code, providerText, guidance string) string {
	message := code
	tail := ""
	if guidance != "" {
		tail = " — " + guidance
	}
	text := strings.TrimSpace(providerText)
	if text != "" && text != code {
		room := MCPToolErrorMessageLimit - len(message) - len(": ") - len(tail)
		if room > 0 {
			if len(text) > room {
				text = strings.ToValidUTF8(text[:room], "")
			}
			message += ": " + text
		}
	}
	return message + tail
}

func mcpResultErrorObject(raw any) (MCPToolErrorClass, string, bool) {
	object, ok := raw.(map[string]any)
	if !ok {
		return "", "", false
	}
	errorObject, ok := object["error"].(map[string]any)
	if !ok {
		return "", "", false
	}
	class, ok := errorObject["class"].(string)
	if !ok || !isMCPToolErrorClass(MCPToolErrorClass(class)) {
		return "", "", false
	}
	message, ok := errorObject["message"].(string)
	if !ok {
		return "", "", false
	}
	return MCPToolErrorClass(class), boundMCPErrorMessage(message), true
}
