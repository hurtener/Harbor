// Package llm is the public SDK facade over Harbor's internal/llm
// package — the provider-corrected LLM client, the chat message
// vocabulary, and the artifact-stub content contract (RFC §3.6,
// §6.5). Alias-based re-exports only: no behavior lives here. Driver
// factories, wrapper-hook registration, posture surfaces, chunk
// publishing, and event payload structs are deliberately private.
package llm

import (
	internal "github.com/hurtener/Harbor/internal/llm"
)

// Client + request/response vocabulary — aliases of the internal types.
type (
	// LLMClient is the provider-corrected completion client interface.
	LLMClient = internal.LLMClient
	// ConfigSnapshot is the resolved LLM configuration a client opens with.
	ConfigSnapshot = internal.ConfigSnapshot
	// Deps carries the shared dependencies llm.Open threads to drivers
	// and wrapper layers (bus, artifact store, logger).
	Deps = internal.Deps
	// CompleteRequest is the canonical completion request.
	CompleteRequest = internal.CompleteRequest
	// CompleteResponse is the canonical completion response.
	CompleteResponse = internal.CompleteResponse
	// ChatMessage is one role-tagged message.
	ChatMessage = internal.ChatMessage
	// Content is a message body (text or multimodal parts).
	Content = internal.Content
	// ContentPart is one multimodal content part.
	ContentPart = internal.ContentPart
	// PartType discriminates ContentPart kinds.
	PartType = internal.PartType
	// ImagePart is an image content part.
	ImagePart = internal.ImagePart
	// AudioPart is an audio content part.
	AudioPart = internal.AudioPart
	// FilePart is a file content part.
	FilePart = internal.FilePart
	// Role tags a ChatMessage (system / user / assistant / tool).
	Role = internal.Role
	// ToolDeclaration declares one tool to the provider.
	ToolDeclaration = internal.ToolDeclaration
	// ToolCallStructured is one structured tool call in a response.
	ToolCallStructured = internal.ToolCallStructured
	// ResponseFormat requests text / JSON / schema-constrained output.
	ResponseFormat = internal.ResponseFormat
	// ResponseFormatKind discriminates ResponseFormat.
	ResponseFormatKind = internal.ResponseFormatKind
	// OutputMode selects the structured-output strategy.
	OutputMode = internal.OutputMode
	// Usage is the token usage block of a response.
	Usage = internal.Usage
	// Cost is the computed cost block of a response.
	Cost = internal.Cost
	// ArtifactStub replaces raw heavy content at the LLM edge.
	ArtifactStub = internal.ArtifactStub
	// StubFetch resolves an ArtifactStub back to bytes on demand.
	StubFetch = internal.StubFetch
)

// DefaultDriver is the production LLM driver name.
const DefaultDriver = internal.DefaultDriver

// PartType values.
const (
	// PartText — a text part.
	PartText = internal.PartText
	// PartImage — an image part.
	PartImage = internal.PartImage
	// PartAudio — an audio part.
	PartAudio = internal.PartAudio
	// PartFile — a file part.
	PartFile = internal.PartFile
)

// Role values.
const (
	// RoleSystem — the system prompt role.
	RoleSystem = internal.RoleSystem
	// RoleUser — the user role.
	RoleUser = internal.RoleUser
	// RoleAssistant — the assistant role.
	RoleAssistant = internal.RoleAssistant
	// RoleTool — the tool-result role.
	RoleTool = internal.RoleTool
)

// ResponseFormatKind values.
const (
	// FormatText — plain text output.
	FormatText = internal.FormatText
	// FormatJSONObject — any-JSON-object output.
	FormatJSONObject = internal.FormatJSONObject
	// FormatJSONSchema — schema-constrained output.
	FormatJSONSchema = internal.FormatJSONSchema
)

// OutputMode values.
const (
	// OutputModeUnset — resolve per model profile.
	OutputModeUnset = internal.OutputModeUnset
	// OutputModeNative — provider-native structured output.
	OutputModeNative = internal.OutputModeNative
	// OutputModeTools — structured output via a forced tool call.
	OutputModeTools = internal.OutputModeTools
	// OutputModePrompted — structured output via prompting.
	OutputModePrompted = internal.OutputModePrompted
)

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrUnknownDriver — the named LLM driver is not registered.
	ErrUnknownDriver = internal.ErrUnknownDriver
	// ErrClientClosed — the client has been closed.
	ErrClientClosed = internal.ErrClientClosed
	// ErrIdentityMissing — the call context carries no identity.
	ErrIdentityMissing = internal.ErrIdentityMissing
	// ErrInvalidContent — the message content is malformed.
	ErrInvalidContent = internal.ErrInvalidContent
	// ErrContextLeak — raw heavy content reached the LLM edge.
	ErrContextLeak = internal.ErrContextLeak
	// ErrContextWindowExceeded — the estimate breaches the window reserve.
	ErrContextWindowExceeded = internal.ErrContextWindowExceeded
	// ErrInvalidConfig — the LLM configuration is invalid.
	ErrInvalidConfig = internal.ErrInvalidConfig
	// ErrUnsupportedModel — the model has no configured ModelProfile.
	ErrUnsupportedModel = internal.ErrUnsupportedModel
	// ErrInvalidJSONSchema — the response failed schema validation.
	ErrInvalidJSONSchema = internal.ErrInvalidJSONSchema
	// ErrDowngradeExhausted — the structured-output downgrade chain ran out.
	ErrDowngradeExhausted = internal.ErrDowngradeExhausted
	// ErrRetryExhausted — the retry-with-feedback budget ran out.
	ErrRetryExhausted = internal.ErrRetryExhausted
	// ErrValidationFailed — the response validator rejected the output.
	ErrValidationFailed = internal.ErrValidationFailed
)

// Open resolves the configured driver and composes the production
// wrapper chain (corrections, downgrade, retry, governance) around it.
var Open = internal.Open

// SnapshotFromConfig projects the operator config blocks into the
// resolved ConfigSnapshot Open consumes.
var SnapshotFromConfig = internal.SnapshotFromConfig

// RegisteredDrivers lists the seated LLM driver names (blank-import
// sdk/drivers/prod to seat the production set).
var RegisteredDrivers = internal.RegisteredDrivers
