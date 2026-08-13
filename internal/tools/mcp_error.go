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
	// MCPResultErrorNamespace is the only provider-authored namespace Harbor
	// interprets in standard MCP result metadata or structured content.
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
	// Recognized records whether the provider supplied a valid namespaced
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

// MCPResultErrorClassification extracts only the explicitly namespaced,
// structural classification. It never searches human-readable text.
func MCPResultErrorClassification(metadata map[string]any, structured any) (MCPToolErrorClass, string, bool) {
	if class, message, ok := mcpResultErrorObject(metadata[MCPResultErrorNamespace]); ok {
		return class, message, true
	}
	if object, ok := structured.(map[string]any); ok {
		if class, message, found := mcpResultErrorObject(object[MCPResultErrorNamespace]); found {
			return class, message, true
		}
	}
	return MCPToolErrorTransient, "", false
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
