// Package tools is the public SDK facade over Harbor's
// internal/tools package — the transport-agnostic tool catalog, the
// descriptor/policy vocabulary, and the planner-facing catalog view
// (RFC §3.6, §6.10). Alias-based re-exports only: no behavior
// lives here. The policy execution shell, invoke hooks, error
// classification, search-cache seam, and event payload structs are
// deliberately private.
package tools

import (
	internal "github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/artifactref"
)

// ArtifactRef is an artifact-reference tool PARAMETER: declare a field
// of this type in a tool's typed input struct and the derived JSON
// Schema renders it to the model as a plain artifact-id string. The
// model supplies the id; the runtime resolves it at dispatch and the
// tool reads the stored bytes through [ArtifactRef.Bytes]. Bytes flow
// store → tool — the model authors an id and never sees content.
//
// The resolved value is dispatch-local. Every serialisation door on the
// type projects the id instead of the content, so an ArtifactRef that
// reaches an event payload, a formatted string or a log emits the id.
//
// [ArtifactRef.Bytes] fails with [ErrArtifactRefUnresolved] when the
// reference was never resolved, rather than returning an empty slice a
// tool would mistake for an empty artifact.
//
// The RESOLUTION side is deliberately not re-exported: seating a
// resolver and binding a value are the runtime's acts at the dispatch
// boundary, and a tool that could do either would be reaching past the
// identity scope its run was given.
type ArtifactRef = artifactref.Ref

// Catalog vocabulary — aliases of the internal types.
type (
	// Tool is one registered tool (descriptor + invoker), any transport.
	Tool = internal.Tool
	// ToolCatalog is the catalog interface tools register on and the
	// runtime dispatches against.
	ToolCatalog = internal.ToolCatalog
	// CatalogOption customises NewCatalog.
	CatalogOption = internal.CatalogOption
	// CatalogFilter scopes catalog reads by the identity triple.
	CatalogFilter = internal.CatalogFilter
	// ToolDescriptor is a tool's catalog-visible metadata.
	ToolDescriptor = internal.ToolDescriptor
	// ToolExample is one worked invocation example on a descriptor.
	ToolExample = internal.ToolExample
	// ToolResult is the canonical invocation result envelope.
	ToolResult = internal.ToolResult
	// ToolPolicy is the per-tool timeout/retry/validation policy.
	ToolPolicy = internal.ToolPolicy
	// ToolProvider is the per-transport invocation seam.
	ToolProvider = internal.ToolProvider
	// ToolSourceID names where a tool came from (config, MCP server, ...).
	ToolSourceID = internal.ToolSourceID
	// DescriptorOption customises a descriptor at registration time.
	DescriptorOption = internal.DescriptorOption
	// LoadingMode is the catalog loading strategy for a tool.
	LoadingMode = internal.LoadingMode
	// SideEffect declares a tool's side-effect class.
	SideEffect = internal.SideEffect
	// TransportKind names a tool's transport.
	TransportKind = internal.TransportKind
	// ValidateMode selects schema validation of args and/or results.
	ValidateMode = internal.ValidateMode
	// PlannerView is the identity-filtered, planner-facing catalog view.
	PlannerView = internal.PlannerView
	// ErrorClass classifies a tool failure for the policy shell
	// (retry-vs-stop decisions). Re-exported here because
	// harbortest.SimulateFailure takes one, so external test authors
	// must be able to name the class values.
	ErrorClass = internal.ErrorClass
)

// ErrorClass values.
const (
	// ErrClassTransient — retryable infrastructure failures.
	ErrClassTransient = internal.ErrClassTransient
	// ErrClassTimeout — the per-attempt deadline elapsed.
	ErrClassTimeout = internal.ErrClassTimeout
	// ErrClass5xx — server-side 5xx failures.
	ErrClass5xx = internal.ErrClass5xx
	// ErrClassPermanent — non-retryable failures.
	ErrClassPermanent = internal.ErrClassPermanent
)

// LoadingMode values.
const (
	// LoadingAlways — the tool is always visible to the planner.
	LoadingAlways = internal.LoadingAlways
	// LoadingDeferred — the tool is discovered via tool_search.
	LoadingDeferred = internal.LoadingDeferred
)

// SideEffect values.
const (
	// SideEffectPure — no side effects.
	SideEffectPure = internal.SideEffectPure
	// SideEffectRead — reads external state.
	SideEffectRead = internal.SideEffectRead
	// SideEffectWrite — writes external state.
	SideEffectWrite = internal.SideEffectWrite
	// SideEffectExternal — calls an external system.
	SideEffectExternal = internal.SideEffectExternal
	// SideEffectStateful — mutates durable state.
	SideEffectStateful = internal.SideEffectStateful
)

// TransportKind values.
const (
	// TransportInProcess — an in-process Go function tool.
	TransportInProcess = internal.TransportInProcess
	// TransportHTTP — an HTTP endpoint tool.
	TransportHTTP = internal.TransportHTTP
	// TransportMCP — a southbound MCP server tool.
	TransportMCP = internal.TransportMCP
	// TransportA2A — a southbound A2A remote-agent tool.
	TransportA2A = internal.TransportA2A
	// TransportFlow — a typed DAG registered as one Tool.
	TransportFlow = internal.TransportFlow
)

// ValidateMode values.
const (
	// ValidateNone — no schema validation.
	ValidateNone = internal.ValidateNone
	// ValidateBoth — validate args and results.
	ValidateBoth = internal.ValidateBoth
	// ValidateIn — validate args only.
	ValidateIn = internal.ValidateIn
	// ValidateOut — validate results only.
	ValidateOut = internal.ValidateOut
)

// Tool lifecycle event types (published on the events bus).
const (
	// EventTypeToolInvoked — a tool invocation started.
	EventTypeToolInvoked = internal.EventTypeToolInvoked
	// EventTypeToolCompleted — a tool invocation succeeded.
	EventTypeToolCompleted = internal.EventTypeToolCompleted
	// EventTypeToolFailed — a tool invocation failed.
	EventTypeToolFailed = internal.EventTypeToolFailed
	// EventTypeToolInvalidArgs — args failed schema validation.
	EventTypeToolInvalidArgs = internal.EventTypeToolInvalidArgs
	// EventTypeToolPolicyExhausted — the policy retry budget ran out.
	EventTypeToolPolicyExhausted = internal.EventTypeToolPolicyExhausted
)

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrToolNotFound — the named tool is not in the catalog.
	ErrToolNotFound = internal.ErrToolNotFound
	// ErrToolInvalidArgs — arguments failed the tool's schema.
	ErrToolInvalidArgs = internal.ErrToolInvalidArgs
	// ErrToolPolicyExhausted — policy retries are exhausted.
	ErrToolPolicyExhausted = internal.ErrToolPolicyExhausted
	// ErrToolDuplicateName — a tool with this name is already registered.
	ErrToolDuplicateName = internal.ErrToolDuplicateName
	// ErrArtifactRefUnresolved — an [ArtifactRef] was read before the
	// runtime resolved it (the argument omitted the reference, or the
	// value was built outside a dispatch).
	ErrArtifactRefUnresolved = artifactref.ErrUnresolved
)

// NewArtifactRef builds an [ArtifactRef] carrying an artifact id and no
// resolved value — for callers assembling arguments in Go (tests, typed
// embedders) rather than decoding them from a model's JSON. It resolves
// at dispatch exactly as a decoded one does.
var NewArtifactRef = artifactref.NewRef

// NewCatalog constructs an empty tool catalog.
var NewCatalog = internal.NewCatalog

// NewPlannerView builds the identity-filtered planner-facing view of
// a catalog.
var NewPlannerView = internal.NewPlannerView

// DefaultPolicy returns the default per-tool execution policy.
var DefaultPolicy = internal.DefaultPolicy

// VisibleNames lists the tool names visible under a filter.
var VisibleNames = internal.VisibleNames

// WithCatalog returns a child context carrying the catalog.
var WithCatalog = internal.WithCatalog

// Catalog extracts the catalog from ctx, reporting presence.
var Catalog = internal.Catalog

// MustCatalog extracts the catalog from ctx, panicking when absent.
var MustCatalog = internal.MustCatalog

// Descriptor options (see internal/tools DescriptorOption docs).
var (
	// WithAuthScopes sets the OAuth scopes the tool requires.
	WithAuthScopes = internal.WithAuthScopes
	// WithCostHint sets the human-readable cost hint.
	WithCostHint = internal.WithCostHint
	// WithDescription sets the planner-visible description.
	WithDescription = internal.WithDescription
	// WithExamples attaches worked invocation examples.
	WithExamples = internal.WithExamples
	// WithLatencyHint sets the expected latency hint.
	WithLatencyHint = internal.WithLatencyHint
	// WithLoading sets the catalog LoadingMode.
	WithLoading = internal.WithLoading
	// WithPolicy sets the per-tool execution policy.
	WithPolicy = internal.WithPolicy
	// WithSafetyNotes sets the planner-visible safety notes.
	WithSafetyNotes = internal.WithSafetyNotes
	// WithSideEffect declares the tool's side-effect class.
	WithSideEffect = internal.WithSideEffect
	// WithSource records the tool's provenance.
	WithSource = internal.WithSource
	// WithTags attaches discovery tags.
	WithTags = internal.WithTags
)
