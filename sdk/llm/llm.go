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
	// ReasoningEffort is the provider-neutral reasoning ceiling/request hint.
	ReasoningEffort = internal.ReasoningEffort
	// Usage is the token usage block of a response.
	Usage = internal.Usage
	// Cost is the computed cost block of a response.
	Cost = internal.Cost
	// ArtifactStub replaces raw heavy content at the LLM edge.
	ArtifactStub = internal.ArtifactStub
	// StubFetch resolves an ArtifactStub back to bytes on demand.
	StubFetch = internal.StubFetch
	// ExternalGrant is the signed, content-free execution authority.
	ExternalGrant = internal.ExternalGrant
	// ComputeLease is the bounded provider-attempt allowance.
	ComputeLease = internal.ComputeLease
	// ExternalGrantMode selects optional versus strict grant handling.
	ExternalGrantMode = internal.ExternalGrantMode
	// ExternalGrantRouteMode selects runtime-default or coordinator-bound
	// provider route authority.
	ExternalGrantRouteMode = internal.ExternalGrantRouteMode
	// ExternalGrantVerifier verifies a grant against a verified request.
	ExternalGrantVerifier = internal.ExternalGrantVerifier
	// CredentialResolver resolves an opaque binding after verification.
	CredentialResolver = internal.CredentialResolver
	// ResolvedCredential is the short-lived provider secret at the driver edge.
	ResolvedCredential = internal.ResolvedCredential
	// LeaseTopUpper requests a coordinator-signed lease extension.
	LeaseTopUpper = internal.LeaseTopUpper
	// LeaseReservationRequest is the durable attempt reservation input.
	LeaseReservationRequest = internal.LeaseReservationRequest
	// LeaseReservation is the durable attempt reservation result.
	LeaseReservation = internal.LeaseReservation
	// LeaseSettlement settles one provider attempt exactly once.
	LeaseSettlement = internal.LeaseSettlement
	// LeaseReservationManager is the durable reservation/settlement seam.
	LeaseReservationManager = internal.LeaseReservationManager
	// AttemptUsageReceipt is the content-free provider-attempt fact.
	AttemptUsageReceipt = internal.AttemptUsageReceipt
	// UsageReceiptSink durably accepts a content-free receipt.
	UsageReceiptSink = internal.UsageReceiptSink
	// ExternalGrantConfig wires the execution-edge grant seams.
	ExternalGrantConfig = internal.ExternalGrantConfig
	// VerifiedGrantContext is the verified grant available at the driver edge.
	VerifiedGrantContext = internal.VerifiedGrantContext
	// AttemptScope carries server-derived logical call and retry coordinates.
	AttemptScope = internal.AttemptScope
)

// DefaultDriver is the production LLM driver name.
const DefaultDriver = internal.DefaultDriver

// ExternalGrant modes.
const (
	ExternalGrantDisabled = internal.ExternalGrantDisabled
	ExternalGrantOptional = internal.ExternalGrantOptional
	ExternalGrantRequired = internal.ExternalGrantRequired
)

// External grant route modes.
const (
	ExternalGrantRouteRuntimeDefault   = internal.ExternalGrantRouteRuntimeDefault
	ExternalGrantRouteCoordinatorBound = internal.ExternalGrantRouteCoordinatorBound
)

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
	// ReasoningEffort levels.
	ReasoningOff    = internal.ReasoningOff
	ReasoningLow    = internal.ReasoningLow
	ReasoningMedium = internal.ReasoningMedium
	ReasoningHigh   = internal.ReasoningHigh

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
	// ErrExternalGrantRequired — strict mode received no grant.
	ErrExternalGrantRequired = internal.ErrExternalGrantRequired
	// ErrExternalGrantInvalid — the grant or its binding is invalid.
	ErrExternalGrantInvalid = internal.ErrExternalGrantInvalid
	// ErrExternalGrantSignature — the grant signature is not trusted.
	ErrExternalGrantSignature = internal.ErrExternalGrantSignature
	// ErrExternalGrantRevoked — the credential binding is stale or revoked.
	ErrExternalGrantRevoked = internal.ErrExternalGrantRevoked
	// ErrExternalGrantLeaseInsufficient — the bounded lease cannot cover a call.
	ErrExternalGrantLeaseInsufficient = internal.ErrExternalGrantLeaseInsufficient
	// ErrExternalGrantAttemptInFlight — the exact attempt is already reserved.
	ErrExternalGrantAttemptInFlight = internal.ErrExternalGrantAttemptInFlight
	// ErrExternalGrantAttemptSettled — the exact attempt already has an outcome.
	ErrExternalGrantAttemptSettled = internal.ErrExternalGrantAttemptSettled
	// ErrExternalGrantCrossProviderFallback — the signed route forbids a hop.
	ErrExternalGrantCrossProviderFallback = internal.ErrExternalGrantCrossProviderFallback
	// ErrUsageReceiptUnavailable — strict accounting could not enqueue a receipt.
	ErrUsageReceiptUnavailable = internal.ErrUsageReceiptUnavailable
	// ErrInvalidUsageReceipt — the receipt is malformed or unbound.
	ErrInvalidUsageReceipt = internal.ErrInvalidUsageReceipt
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

// CanonicalAttemptUsageReceiptBodyHash returns the deterministic receipt hash.
var CanonicalAttemptUsageReceiptBodyHash = internal.CanonicalAttemptUsageReceiptBodyHash

// MarshalCanonicalAttemptUsageReceipt returns the public transport-neutral
// receipt JSON representation.
var MarshalCanonicalAttemptUsageReceipt = internal.MarshalCanonicalAttemptUsageReceipt

// ValidateAttemptUsageReceipt validates the content-free receipt shape.
var ValidateAttemptUsageReceipt = internal.ValidateAttemptUsageReceipt

// ValidateAttemptUsageReceiptAgainstGrant binds a receipt to a signed grant's
// identity, route and planner-derived attempt coordinates.
var ValidateAttemptUsageReceiptAgainstGrant = internal.ValidateAttemptUsageReceiptAgainstGrant

// CanonicalAttemptID returns the durable per-attempt identity.
var CanonicalAttemptID = internal.CanonicalAttemptID

// EffectiveExternalGrantRouteMode normalizes legacy blank route mode to the
// coordinator-bound shape.
var EffectiveExternalGrantRouteMode = internal.EffectiveExternalGrantRouteMode

// WithVerifiedOrganization attaches the server-derived organization scope.
var WithVerifiedOrganization = internal.WithVerifiedOrganization

// VerifiedOrganizationFrom reads the verified organization scope.
var VerifiedOrganizationFrom = internal.VerifiedOrganizationFrom

// WithVerifiedGrantContext attaches the verified driver grant context.
var WithVerifiedGrantContext = internal.WithVerifiedGrantContext

// VerifiedGrantContextFrom reads the verified driver grant context.
var VerifiedGrantContextFrom = internal.VerifiedGrantContextFrom

// WithAttemptStep installs a server-derived planner step coordinate.
var WithAttemptStep = internal.WithAttemptStep

// WithAttemptScope installs a per-call attempt scope.
var WithAttemptScope = internal.WithAttemptScope

// AttemptScopeFrom reads the per-call attempt scope.
var AttemptScopeFrom = internal.AttemptScopeFrom

// EnsureAttemptScope allocates one invocation identity.
var EnsureAttemptScope = internal.EnsureAttemptScope

// EnsureGrantAttemptScope derives a stable grant-bound call/step identity.
var EnsureGrantAttemptScope = internal.EnsureGrantAttemptScope

// WithAttemptCoordinates derives retry/downgrade/fallback coordinates.
var WithAttemptCoordinates = internal.WithAttemptCoordinates
