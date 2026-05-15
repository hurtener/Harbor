// Package tools defines Harbor's unified tool catalog surface — the
// single planner-addressable concept that hides whether a tool is an
// in-process Go function, an HTTP endpoint, an MCP server, an A2A
// remote agent, or a Flow (a typed DAG of Nodes registered as one
// Tool — see internal/runtime/flow).
//
// The unification is at the **type level** (RFC §6.4, brief 03 §1):
// every Tool is the same struct regardless of Source / Transport;
// the dispatch is one switch in one place. Adding a new transport
// (Phase 27 HTTP, Phase 28 MCP, Phase 29 A2A) is a ToolProvider
// driver; nothing else changes.
//
// Identity is mandatory. CatalogFilter keys on the
// (tenant, user, session) triple plus GrantedScopes; every
// ToolDescriptor.Invoke reads identity from ctx and stamps it in
// audit-emitted events (RFC §4, D-001).
//
// Reliability shell. Every tool invocation (regardless of Transport)
// is wrapped in the ToolPolicy shell — timeout, retry-with-
// exponential-backoff, validation. Defaults fire on a zero-valued
// ToolPolicy so a plain `tools.RegisterFunc(name, fn)` is
// production-resilient with no ceremony (RFC §6.4, D-024).
//
// Concurrent reuse contract (D-025). A constructed *catalog is safe
// to share across N concurrent goroutines: descriptors are immutable
// after Register; storage is RWMutex-guarded; per-invocation state
// lives in the call's ctx + ToolResult, never on the catalog or the
// Tool itself.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"time"
)

// TransportKind discriminates a Tool's source. Same Tool struct
// regardless of Transport; the value lets observability + audit
// filter or annotate without re-shaping the Tool.
type TransportKind string

const (
	// TransportInProcess — a Go function registered via
	// drivers/inproc.RegisterFunc.
	TransportInProcess TransportKind = "inprocess"
	// TransportHTTP — Phase 27 (UTCP-style HTTP tool).
	TransportHTTP TransportKind = "http"
	// TransportMCP — Phase 28 (MCP southbound).
	TransportMCP TransportKind = "mcp"
	// TransportA2A — Phase 29 (A2A southbound).
	TransportA2A TransportKind = "a2a"
	// TransportFlow — Phase 26a (a typed DAG of Nodes registered as
	// a Tool via internal/runtime/flow.RegisterAsTool).
	TransportFlow TransportKind = "flow"
)

// SideEffect is the declared side-effect class. Operators reason
// about which tools are safe to retry from this field; the policy's
// retry behaviour is orthogonal (a tool with SideEffectWrite may
// still have RetryOn = []ErrorClass{ErrClassTransient} if the
// caller knows the write is idempotent).
type SideEffect string

const (
	SideEffectPure     SideEffect = "pure"
	SideEffectRead     SideEffect = "read"
	SideEffectWrite    SideEffect = "write"
	SideEffectExternal SideEffect = "external"
	SideEffectStateful SideEffect = "stateful"
)

// LoadingMode controls when a Tool appears in the planner's prompt-
// time catalog. Always-loaded tools live in every planner step;
// Deferred tools are loaded on demand (lazy resolution).
type LoadingMode string

const (
	LoadingAlways   LoadingMode = "always"
	LoadingDeferred LoadingMode = "deferred"
)

// ToolSourceID identifies the provider that produced a Tool. Empty
// for in-process tools (no provider lifecycle to track); populated
// for HTTP / MCP / A2A providers.
type ToolSourceID string

// ToolExample is a usage example surfaced to the planner. The
// `Args` map is JSON-marshalled into the prompt.
type ToolExample struct {
	Args        map[string]any
	Description string
	Tags        []string
}

// Tool is Harbor's planner-addressable unit. Same struct regardless
// of Transport. The planner reasons about this; the dispatcher uses
// the matching ToolDescriptor.
//
// Concurrent reuse: Tool is a value type; all fields are read-only
// after Register. The Tool itself never carries per-invocation state.
type Tool struct {
	// Name is the unique catalog key. Two descriptors with the same
	// Name return ErrToolDuplicateName from Register.
	Name string
	// Description is the planner-facing summary. Surfaced in the
	// prompt-time catalog so the LLM can choose between tools.
	Description string
	// ArgsSchema is the JSON-Schema (object) describing the tool's
	// argument shape. Catalogs validate against this before dispatch
	// — failures yield ErrToolInvalidArgs.
	ArgsSchema json.RawMessage
	// OutSchema is the JSON-Schema (object) describing the tool's
	// result shape. Used for output validation when policy enables it.
	OutSchema json.RawMessage
	// SideEffects classifies the tool's effect domain — operators
	// gate which classes are safe to retry / parallelize.
	SideEffects SideEffect
	// Tags allow operators to filter / categorise tools.
	Tags []string
	// AuthScopes lists the scopes a planner step's identity MUST
	// carry (via CatalogFilter.GrantedScopes) for the Tool to be
	// visible. Empty = no scope requirement.
	AuthScopes []string
	// CostHint is free-form (cheap / normal / expensive) and
	// non-authoritative — Governance owns real cost gates.
	CostHint string
	// LatencyHint is non-authoritative; surfaced for prompt ordering.
	LatencyHint time.Duration
	// SafetyNotes is free-form text the planner sees alongside
	// Description; used for "this tool writes to production" hints.
	SafetyNotes string
	// Loading mode: Always (visible in every planner step) or
	// Deferred (loaded on demand).
	Loading LoadingMode
	// Examples surface canonical argument shapes to the planner.
	Examples []ToolExample
	// Source identifies the provider (empty for in-process tools).
	Source ToolSourceID
	// Transport discriminates the tool's source. Determines which
	// driver's Invoke implementation runs.
	Transport TransportKind
	// Policy is the reliability shell wrapping every invocation.
	// Zero value → DefaultPolicy is applied at dispatch time.
	Policy ToolPolicy
}

// ToolResult is the canonical result type returned by every
// ToolDescriptor.Invoke. Heavy outputs route through the
// ArtifactStore upstream (D-022, D-026); ToolResult.Value carries
// either typed Go values or ArtifactRef-shaped placeholders.
type ToolResult struct {
	Value any
	Meta  map[string]any
}

// ToolDescriptor is the callable binding produced by a driver.
// The planner sees Tool; the dispatcher uses ToolDescriptor.
//
// Invoke is wrapped by the ToolPolicy shell at dispatch time — the
// descriptor's Invoke is the inner-most function, called once per
// retry attempt by the shell. Drivers populate Invoke with the
// transport-specific code; the shell handles timeout / retry /
// backoff / validation uniformly.
//
// Validate is the cached compiled JSON-Schema validator. Drivers
// build it once at Register; the shell calls it once before the
// first Invoke attempt (validation BEFORE retry — failing args
// don't get retried).
type ToolDescriptor struct {
	Tool     Tool
	Invoke   func(ctx context.Context, args json.RawMessage) (ToolResult, error)
	Validate func(args json.RawMessage) error
}

// CatalogFilter is the server-enforced subscription predicate on
// ToolCatalog.List. Keys on the (tenant, user, session) triple plus
// GrantedScopes — every component participates in visibility:
//
//   - TenantID / UserID / SessionID: typically mandatory at the
//     dispatch boundary, but List is tolerant of empty fields so
//     callers can build admin-view filters (an empty triple matches
//     every tool — see HasFullTriple). Production wiring (Phase 60+
//     Protocol surface) MUST gate the empty-triple case behind an
//     elevated scope.
//
//   - GrantedScopes: a Tool is visible only if every entry in its
//     AuthScopes is contained in GrantedScopes. Tools with no
//     AuthScopes (the in-process default) are always visible.
//
//   - LoadingModes: defaults to [LoadingAlways] when empty (the
//     prompt-time view); pass [LoadingAlways, LoadingDeferred] for
//     a full-discovery view.
//
//   - NameRegex: optional final filter for operator queries; nil
//     matches every tool.
type CatalogFilter struct {
	TenantID, UserID, SessionID string
	GrantedScopes               []string
	LoadingModes                []LoadingMode
	NameRegex                   *regexp.Regexp
}

// HasFullTriple reports whether the filter has all three identity
// components. Phase 60+ Protocol surface uses this to reject empty-
// triple non-admin reads.
func (f CatalogFilter) HasFullTriple() bool {
	return f.TenantID != "" && f.UserID != "" && f.SessionID != ""
}

// matches reports whether t passes f's visibility predicate. Used
// internally by ToolCatalog.List; exposed as a method so tests can
// pin the math directly.
func (f CatalogFilter) matches(t Tool) bool {
	// AuthScopes subset check: every required scope must be present
	// in GrantedScopes. Tools with no AuthScopes are always visible.
	if len(t.AuthScopes) > 0 {
		granted := make(map[string]struct{}, len(f.GrantedScopes))
		for _, s := range f.GrantedScopes {
			granted[s] = struct{}{}
		}
		for _, required := range t.AuthScopes {
			if _, ok := granted[required]; !ok {
				return false
			}
		}
	}
	// LoadingModes: default is [LoadingAlways]. Empty slice ==
	// LoadingAlways-only view.
	if len(f.LoadingModes) == 0 {
		if t.Loading != LoadingAlways && t.Loading != "" {
			return false
		}
	} else {
		found := false
		for _, m := range f.LoadingModes {
			loading := t.Loading
			if loading == "" {
				loading = LoadingAlways
			}
			if m == loading {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	if f.NameRegex != nil && !f.NameRegex.MatchString(t.Name) {
		return false
	}
	return true
}

// ToolCatalog is Harbor's planner-addressable registry. Three
// methods. Concrete V1 implementation is the in-memory catalog
// (catalog.go); future drivers (remote-catalog, persistent-catalog)
// plug in behind the interface.
//
// Concurrent reuse (D-025): implementations MUST be safe for N
// concurrent goroutines. The in-memory catalog uses a single
// RWMutex; descriptors are immutable after Register.
type ToolCatalog interface {
	// Register adds a descriptor. Returns ErrToolDuplicateName when
	// the Tool's Name is already registered.
	Register(d ToolDescriptor) error
	// Resolve returns the descriptor for name. found=false when
	// absent; the caller compares against the boolean (no error).
	Resolve(name string) (ToolDescriptor, bool)
	// List returns Tool *views* (never ToolDescriptors) matching
	// filter. The slice is owned by the caller; mutations on it do
	// not affect the catalog.
	List(filter CatalogFilter) []Tool
}

// CatalogReplacer is the optional surface a ToolCatalog exposes when
// it supports atomic per-tool descriptor replacement at boot. The
// Phase 64a catalog wiring (internal/tools/catalog.Builder) uses
// this to install wrapped descriptors over previously-registered
// names without a Deregister+Register round trip (which would race
// concurrent Resolve calls).
//
// Replacement semantics:
//   - Each descriptor in `wrapped` MUST name an already-registered
//     Tool (the builder enforces this via Resolve BEFORE calling
//     Replace). Otherwise Replace returns ErrToolNotFound naming
//     the offending tool.
//   - Replacement is atomic from the catalog's external view: a
//     concurrent Resolve sees EITHER every old descriptor OR every
//     new descriptor, never a half-applied mix.
//   - Descriptors NOT named in `wrapped` are untouched.
//
// A catalog implementation that does not support replacement skips
// this interface. Callers branch on the type assertion; the absence
// is the signal.
type CatalogReplacer interface {
	// Replace atomically swaps each named descriptor in `wrapped`
	// with its wrapped version. Returns ErrToolNotFound (wrapped)
	// when any name does not resolve.
	Replace(wrapped []ToolDescriptor) error
}

// ToolProvider is the seam for external tool sources. Phase 27+
// drivers (HTTP, MCP, A2A) implement Connect / Discover / Close to
// pull in remote tools as ToolDescriptors. The in-process registrar
// does not need a provider lifecycle (it's a thin wrapper around
// ToolCatalog.Register), so Phase 26 ships the interface shape
// without a default driver.
//
// Identity-mandatory: Connect / Discover propagate identity via ctx
// so transports can scope their authentication.
type ToolProvider interface {
	// Connect is called once at provider attach. Drivers establish
	// long-lived connections, authenticate, etc.
	Connect(ctx context.Context) error
	// Discover returns the current set of descriptors. May be
	// called periodically by the catalog manager.
	Discover(ctx context.Context) ([]ToolDescriptor, error)
	// Close releases provider resources. Must be idempotent.
	Close(ctx context.Context) error
	// SourceID is the stable identifier for this provider.
	SourceID() ToolSourceID
}

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrToolNotFound — Resolve returned (_, false); typically the
	// planner asked for a tool name the catalog never registered.
	ErrToolNotFound = errors.New("tools: tool not found")
	// ErrToolInvalidArgs — argument validation failed at the
	// catalog edge. The planner is expected to reformulate via LLM
	// retry feedback; this is NOT a tool error.
	ErrToolInvalidArgs = errors.New("tools: invalid arguments")
	// ErrToolPolicyExhausted — the policy's retry budget was
	// exhausted; the wrapped cause carries the last attempt's
	// failure.
	ErrToolPolicyExhausted = errors.New("tools: policy retries exhausted")
	// ErrToolDuplicateName — Register called with a Name already
	// in the catalog.
	ErrToolDuplicateName = errors.New("tools: duplicate tool name")
)

// wrap formats a sentinel error with %w plus contextual key=value
// pairs. Keeps the call sites compact.
func wrap(sentinel error, format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{sentinel}, args...)...)
}

// ctxKey is the unexported key under which a ToolCatalog is
// propagated on a context. Independent from identity / events /
// audit ctx keys.
type ctxKey int

const catalogCtxKey ctxKey = iota

// WithCatalog attaches cat to ctx so downstream handlers can
// recover it via MustCatalog / Catalog.
func WithCatalog(ctx context.Context, cat ToolCatalog) context.Context {
	return context.WithValue(ctx, catalogCtxKey, cat)
}

// MustCatalog returns the ToolCatalog in ctx. Panics with
// ErrToolNotFound (used as the sentinel for "no catalog configured")
// when absent. Use in handler/runtime paths where a catalog is
// mandatory.
func MustCatalog(ctx context.Context) ToolCatalog {
	c, ok := Catalog(ctx)
	if !ok {
		panic(ErrToolNotFound)
	}
	return c
}

// Catalog returns the ToolCatalog in ctx and a presence bool. Use
// when absence is recoverable.
func Catalog(ctx context.Context) (ToolCatalog, bool) {
	c, ok := ctx.Value(catalogCtxKey).(ToolCatalog)
	return c, ok
}
