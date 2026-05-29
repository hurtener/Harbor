// Package catalog ships Harbor's operator-config-driven tool-catalog
// wiring (Phase 64a / D-090). Operators declare per-tool middleware in
// `tools.entries[]` and the `Builder` defined here auto-wraps each
// registered tool descriptor with the matching `approval.ApprovalGate`
// and / or OAuth-aware invocation wrapper. No Go wiring code on the
// operator's side; the runtime composes the middleware stack at boot.
//
// # The wiring contract
//
// The builder consumes:
//
//   - The raw `[]config.ToolEntryConfig` from `internal/config.Tools.Entries`.
//   - The set of `*approval.ApprovalGate`s the dev cmd opened (one per
//     declared `tools.<name>.approval` entry — the builder allocates
//     fresh gates so each tool has its own pending-resolution map; gates
//     share the SAME Coordinator + Bus + Redactor so the
//     pause/resume primitive remains unified per CLAUDE.md §13).
//   - The set of `auth.OAuthProvider`s keyed by provider name (one per
//     declared `tools.<name>.oauth` provider).
//
// For each entry, the builder:
//
//  1. Resolves the named tool's descriptor from the catalog. A miss
//     fails the build with `ErrToolNotRegistered` (no silent skip —
//     §13 fail-loudly).
//  2. Maps `Approval.Policy` onto a concrete `approval.ApprovalPolicy`
//     instance. An unknown policy fails with `ErrUnknownApprovalPolicy`.
//  3. Maps `OAuth.Provider` + `OAuth.BindingScope` onto an existing
//     `auth.OAuthProvider`. A missing provider fails with
//     `ErrUnknownOAuthProvider`.
//  4. Composes the wrapper stack and registers the wrapped descriptor.
//
// # Wrapper composition order
//
// When BOTH approval AND OAuth are declared for the same tool, the
// outer wrapper is **approval**, the inner is **OAuth**. Rationale:
//
//   - Approval is the gate operators expect to fire FIRST: a HITL
//     "Approve call to <tool>?" prompt should pop BEFORE any OAuth flow
//     starts. Otherwise an operator who rejects a write would still
//     have triggered an OAuth dance that consumed user attention.
//   - OAuth's `*ErrAuthRequired` propagates UP through the approval
//     wrapper unchanged — the gate's `RunGuarded` returns the inner
//     tool's error verbatim when approval succeeds. So when OAuth is
//     needed, the planner still observes `*ErrAuthRequired` and can
//     pause for OAuth completion.
//
// D-090 pins this order. Reversing it (OAuth outermost) would mean
// an OAuth pause fires BEFORE the approval gate, which contradicts
// operator intent and burns the user's OAuth-completion attention on
// a call that may end up rejected.
//
// # Concurrent reuse (D-025)
//
// `*Builder` is a one-shot constructor — built, called once via Apply,
// then discarded. The wrapped descriptors `Apply` produces ARE the
// long-lived artifacts; each composed Invoke closure is safe for N
// concurrent invocations because:
//
//   - The wrapper holds the gate / provider by reference; both are
//     compiled artifacts safe for concurrent reuse (Phase 30 / 31
//     concurrent_test.go pins).
//   - Per-invocation state lives in ctx + the inner descriptor's
//     ToolResult, never on the wrapper.
//
// # Fail-loud at boot
//
// Every error path here is fatal at boot. The catalog builder NEVER
// degrades silently — an unknown policy / provider / tool name is the
// operator's typo, and they want to know about it BEFORE the runtime
// starts serving traffic. CLAUDE.md §13 amendment.
package catalog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrCatalogRequired — Apply was called with a nil Catalog. The
	// builder cannot resolve target descriptors without the catalog.
	ErrCatalogRequired = errors.New("catalog: ToolCatalog required")
	// ErrCoordinatorRequired — Apply was called with no Coordinator and
	// at least one entry declares an approval policy. The Coordinator
	// is the unified pause/resume primitive (Phase 50 / D-067).
	ErrCoordinatorRequired = errors.New("catalog: pauseresume.Coordinator required when entries declare approval")
	// ErrBusRequired — Apply was called with no EventBus and at least
	// one entry declares an approval policy. The bus carries the gate's
	// `tool.approval_requested` / `tool.approved` / `tool.rejected`
	// lifecycle events.
	ErrBusRequired = errors.New("catalog: events.EventBus required when entries declare approval")
	// ErrRedactorRequired — Apply was called with no Redactor and at
	// least one entry declares an approval policy. The redactor
	// processes the approval-request payload before emission
	// (CLAUDE.md §7 rule 6).
	ErrRedactorRequired = errors.New("catalog: audit.Redactor required when entries declare approval")
	// ErrToolNotRegistered — an `entries[].name` did not resolve to a
	// registered tool in the catalog. The error message names the
	// offending tool name + lists currently-registered names so the
	// operator sees the typo.
	ErrToolNotRegistered = errors.New("catalog: entry references a tool name that is not registered")
	// ErrUnknownApprovalPolicy — an `entries[].approval.policy` named
	// a policy the bundled set does not provide. The error message
	// names the offending value.
	ErrUnknownApprovalPolicy = errors.New("catalog: unknown approval policy")
	// ErrUnknownOAuthProvider — an `entries[].oauth.provider` named a
	// provider the supplied OAuth registry does not contain. The
	// error message names the offending value + lists configured
	// providers.
	ErrUnknownOAuthProvider = errors.New("catalog: unknown oauth provider")
	// ErrInvalidBindingScope — an `entries[].oauth.binding_scope` was
	// not one of the canonical values. (Mirrors the config-time check;
	// duplicated here as defence-in-depth in case a programmatic
	// caller builds entries without going through config.Validate.)
	ErrInvalidBindingScope = errors.New("catalog: invalid oauth binding_scope")
	// ErrAlreadyApplied — Apply was called twice on the same Builder.
	// The builder is one-shot.
	ErrAlreadyApplied = errors.New("catalog: builder already applied")
	// ErrInvalidLoadingMode — entries[].loading_mode names a value
	// not in {"", "always", "deferred"}. Phase 107c / D-167. Fired
	// from the Builder as defence-in-depth; the config validator
	// rejects the same shape pre-boot.
	ErrInvalidLoadingMode = errors.New("catalog: invalid loading_mode")
)

// Deps bundles the collaborators the Builder consumes. When the entry
// list contains NO approval entries, the Coordinator / Bus / Redactor
// fields are unused (the builder still validates structurally).
type Deps struct {
	// Catalog is the tool catalog whose descriptors get re-registered
	// with their wrappers. Mandatory.
	Catalog tools.ToolCatalog
	// Coordinator is the unified pause/resume primitive. Mandatory
	// when any entry declares approval; ignored otherwise.
	Coordinator pauseresume.Coordinator
	// Bus is the event bus the approval gate emits on. Mandatory
	// when any entry declares approval; ignored otherwise.
	Bus events.EventBus
	// Redactor processes the gate's approval-request payload before
	// emission. Mandatory when any entry declares approval; ignored
	// otherwise.
	Redactor audit.Redactor
	// OAuthProviders maps the operator-facing provider name (the
	// string under `entries[].oauth.provider`) to a constructed
	// `auth.OAuthProvider`. An entry referencing a name not in this
	// map fails Apply with `ErrUnknownOAuthProvider`. Empty when no
	// entry declares OAuth.
	OAuthProviders map[string]auth.OAuthProvider
	// AppliedGates is an optional out-channel: when set, the builder
	// pushes every constructed `*approval.ApprovalGate` into this map
	// keyed by the tool name. Callers (the dev cmd, the integration
	// test) use this to drive in-process `ResolveApproval` calls.
	// Nil disables the surfacing.
	AppliedGates map[string]*approval.ApprovalGate
}

// Builder applies a list of `ToolEntryConfig`s to a `ToolCatalog`,
// wrapping each named descriptor with the declared middleware. One
// Builder per Apply — the type is one-shot.
//
// Builder is NOT a long-lived artifact; the wrapped descriptors it
// installs ARE. The D-025 concurrent-reuse invariant lives on those
// descriptors, not on Builder.
type Builder struct {
	entries []config.ToolEntryConfig
	deps    Deps
	applied bool
}

// New constructs a Builder. Validation is deferred to Apply so the
// caller can introspect the entries before applying.
func New(entries []config.ToolEntryConfig, deps Deps) *Builder {
	return &Builder{
		entries: entries,
		deps:    deps,
	}
}

// Apply installs every entry's middleware onto the catalog. The
// catalog MUST already have the underlying tool descriptors
// registered; Apply REPLACES them with wrapped versions by calling
// `Resolve` + re-registering via the catalog's `Register` after a
// `Deregister`-equivalent. Since the canonical ToolCatalog interface
// has no Deregister method (RegisterMany is one-shot at boot), Apply
// works around this by:
//
//  1. Resolving the underlying descriptor.
//  2. Mutating the catalog through a small `ReplaceForBuilder` shim
//     when the catalog implements it (the in-memory catalog does).
//
// For the in-memory `*catalog` shipped today, Apply uses the
// `(c *Replaceable).Replace` shim wired in `internal/tools`. Future
// catalog implementations either provide an equivalent or document
// "no per-tool wiring at boot."
//
// Apply is idempotent in the failure path: a partial application
// is rolled back when ANY entry errors (the in-memory catalog snapshots
// the descriptor set before mutation; on error, the snapshot is
// restored). A successful Apply is destructive; the original
// descriptors are GONE from the catalog after return.
//
// Apply is one-shot — a second call returns `ErrAlreadyApplied`.
func (b *Builder) Apply(ctx context.Context) error {
	if b.applied {
		return ErrAlreadyApplied
	}
	b.applied = true

	if b.deps.Catalog == nil {
		return ErrCatalogRequired
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("catalog: Apply cancelled: %w", err)
	}

	// Determine whether any entry needs the approval / oauth deps.
	needsApproval := false
	needsOAuth := false
	for _, e := range b.entries {
		if e.Approval != nil {
			needsApproval = true
		}
		if e.OAuth != nil {
			needsOAuth = true
		}
	}
	if needsApproval {
		if b.deps.Coordinator == nil {
			return ErrCoordinatorRequired
		}
		if b.deps.Bus == nil {
			return ErrBusRequired
		}
		if b.deps.Redactor == nil {
			return ErrRedactorRequired
		}
	}
	if needsOAuth && len(b.deps.OAuthProviders) == 0 {
		return fmt.Errorf("%w: at least one entry declares oauth but Deps.OAuthProviders is empty", ErrUnknownOAuthProvider)
	}

	// Resolve every descriptor BEFORE mutating the catalog so we fail
	// loud on the first miss. The catalog accepts no `Deregister`
	// (yet), so a Register that races a Resolve would be confusing —
	// the resolve-then-replace pattern is the simplest path.
	resolved := make([]tools.ToolDescriptor, 0, len(b.entries))
	for i, e := range b.entries {
		d, ok := b.deps.Catalog.Resolve(e.Name)
		if !ok {
			return fmt.Errorf("%w: entries[%d].name = %q (registered names: %s)",
				ErrToolNotRegistered, i, e.Name, registeredNames(b.deps.Catalog))
		}
		resolved = append(resolved, d)
	}

	// Build the wrapped descriptors.
	wrapped := make([]tools.ToolDescriptor, 0, len(b.entries))
	for i, e := range b.entries {
		d := resolved[i]
		w, err := b.wrap(ctx, e, d)
		if err != nil {
			return fmt.Errorf("entries[%d] (tool %q): %w", i, e.Name, err)
		}
		wrapped = append(wrapped, w)
	}

	// Install the wrapped descriptors. We use the `Replacer`
	// interface so the in-memory catalog can atomically swap. A
	// catalog implementation that does not support replacement
	// returns ErrReplaceUnsupported (we propagate verbatim).
	rep, ok := b.deps.Catalog.(tools.CatalogReplacer)
	if !ok {
		return fmt.Errorf("catalog: ToolCatalog (%T) does not support per-tool replacement; cannot wire entries", b.deps.Catalog)
	}
	if err := rep.Replace(wrapped); err != nil {
		return fmt.Errorf("catalog: Replace: %w", err)
	}
	return nil
}

// wrap composes the wrapper stack for one entry. The order:
//
//	approval ( oauth ( inner ) )
//
// — see the package godoc for the rationale.
func (b *Builder) wrap(_ context.Context, e config.ToolEntryConfig, d tools.ToolDescriptor) (tools.ToolDescriptor, error) {
	// 1. Inner-most → start from the registered descriptor.
	current := d

	// Phase 107c / D-167 — propagate operator-declared LoadingMode
	// from the yaml entry onto the Tool. The descriptor's underlying
	// transport (in-proc registrar, MCP, HTTP, A2A) may have a
	// registration-time default; the yaml wins per CLAUDE.md §15
	// (config > defaults). Validation has already rejected unknown
	// values in `internal/config/validate.go::validateTools`.
	switch e.LoadingMode {
	case "":
		// Unset — leave whatever the registrar declared.
	case string(tools.LoadingAlways), string(tools.LoadingDeferred):
		current.Tool.Loading = tools.LoadingMode(e.LoadingMode)
	default:
		return tools.ToolDescriptor{}, fmt.Errorf(
			"%w: entries[%q].loading_mode=%q (allowed: always, deferred)",
			ErrInvalidLoadingMode, e.Name, e.LoadingMode)
	}

	// 2. OAuth — innermost wrapper after the registered descriptor.
	//    Wraps Invoke so the wrapper can call provider.Token BEFORE
	//    dispatching to the underlying tool. A token-fetch that returns
	//    `*auth.ErrAuthRequired` short-circuits — the descriptor's
	//    return propagates up through the approval gate unchanged.
	if e.OAuth != nil {
		prov, ok := b.deps.OAuthProviders[e.OAuth.Provider]
		if !ok {
			return tools.ToolDescriptor{}, fmt.Errorf("%w: oauth.provider=%q (configured: %s)",
				ErrUnknownOAuthProvider, e.OAuth.Provider, providerNames(b.deps.OAuthProviders))
		}
		scope := auth.BindingScope(e.OAuth.BindingScope)
		if !auth.IsValidBindingScope(scope) {
			return tools.ToolDescriptor{}, fmt.Errorf("%w: oauth.binding_scope=%q",
				ErrInvalidBindingScope, e.OAuth.BindingScope)
		}
		current = WrapWithOAuth(current, prov, OAuthWrapperOptions{
			ProviderName: e.OAuth.Provider,
			BindingScope: scope,
		})
	}

	// 3. Approval — outermost wrapper. The gate's RunGuarded fires
	//    BEFORE the (potentially OAuth-wrapped) inner descriptor; an
	//    approval reject short-circuits the call without touching
	//    OAuth or the underlying tool.
	if e.Approval != nil {
		policy, err := resolveApprovalPolicy(e.Approval)
		if err != nil {
			return tools.ToolDescriptor{}, err
		}
		gate, err := approval.NewApprovalGate(approval.GateDeps{
			Policy:      policy,
			Coordinator: b.deps.Coordinator,
			Bus:         b.deps.Bus,
			Redactor:    b.deps.Redactor,
		})
		if err != nil {
			return tools.ToolDescriptor{}, fmt.Errorf("approval.NewApprovalGate: %w", err)
		}
		if b.deps.AppliedGates != nil {
			b.deps.AppliedGates[e.Name] = gate
		}
		current = WrapWithApproval(current, gate, ApprovalWrapperOptions{
			Tags: append([]string(nil), e.Approval.RequireTags...),
		})
	}
	return current, nil
}

// resolveApprovalPolicy maps a `ToolApprovalConfig` onto a concrete
// `approval.ApprovalPolicy` instance.
func resolveApprovalPolicy(c *config.ToolApprovalConfig) (approval.ApprovalPolicy, error) {
	switch c.Policy {
	case "deny-all":
		return approval.AlwaysDenyPolicy{Reason: c.Reason}, nil
	case "approve-all":
		return approval.AlwaysApprovePolicy{}, nil
	case "tagged":
		// validate.go's `validateTools` enforces a non-empty
		// RequireTags for the tagged policy. Defensive check here so
		// a programmatic caller that bypasses config.Validate still
		// fails loud.
		if len(c.RequireTags) == 0 {
			return nil, fmt.Errorf("%w: %q (tagged policy requires require_tags)",
				ErrUnknownApprovalPolicy, c.Policy)
		}
		return approval.TaggedPolicy{
			RequireTags: append([]string(nil), c.RequireTags...),
			Reason:      c.Reason,
		}, nil
	default:
		return nil, fmt.Errorf("%w: %q (allowed: deny-all, approve-all, tagged)",
			ErrUnknownApprovalPolicy, c.Policy)
	}
}

// ApprovalWrapperOptions tunes the approval-gate wrapper's behaviour.
type ApprovalWrapperOptions struct {
	// Tags is the static tag set the wrapper attaches to every
	// approval request originating from this tool. The tagged policy
	// matches against this list.
	Tags []string
}

// WrapWithApproval wraps `d` so every Invoke call routes through the
// gate's `RunGuarded`. On gate REJECT the wrapper returns
// `*approval.ErrToolRejected`; on gate APPROVE the original args
// flow into `d.Invoke`. Identity is read from ctx (mandatory —
// `identity.MustFrom`).
func WrapWithApproval(d tools.ToolDescriptor, gate *approval.ApprovalGate, opts ApprovalWrapperOptions) tools.ToolDescriptor {
	innerInvoke := d.Invoke
	tags := append([]string(nil), opts.Tags...)
	out := d
	out.Invoke = func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
		id, ok := identity.From(ctx)
		if !ok {
			return tools.ToolResult{}, fmt.Errorf("catalog: approval wrapper: %w",
				approval.ErrIdentityRequired)
		}
		req := &approval.ApprovalRequest{
			Tool:     d.Tool,
			Args:     args,
			Identity: id,
			Tags:     tags,
		}
		approvedArgs, err := gate.RunGuarded(ctx, req)
		if err != nil {
			// Gate errors propagate verbatim — the caller (the planner /
			// runtime dispatcher) reaches `*approval.ErrToolRejected`
			// via errors.As.
			return tools.ToolResult{}, err
		}
		return innerInvoke(ctx, approvedArgs)
	}
	return out
}

// OAuthWrapperOptions tunes the OAuth wrapper's behaviour.
type OAuthWrapperOptions struct {
	// ProviderName is the operator-facing name of the OAuth source
	// the tool binds to. Surfaced in error messages.
	ProviderName string
	// BindingScope is the resolved auth.BindingScope (`user` or
	// `agent`) the tool's calls should authenticate under.
	BindingScope auth.BindingScope
}

// WrapWithOAuth wraps `d` so every Invoke call first ensures an
// access token exists via `prov.Token`. A `*auth.ErrAuthRequired`
// return short-circuits — the wrapper does NOT call the underlying
// tool; the runtime catches the typed sentinel and pauses the run
// via the unified pause/resume primitive (Phase 50).
//
// The Phase 64a wrapper does NOT inject the token into the upstream
// request — that is a per-driver concern (HTTP / MCP / A2A drivers
// each compose their own bearer-token injection). The wrapper's job
// is to PRE-CHECK token availability so the runtime can pause for
// OAuth completion BEFORE attempting the call.
func WrapWithOAuth(d tools.ToolDescriptor, prov auth.OAuthProvider, opts OAuthWrapperOptions) tools.ToolDescriptor {
	innerInvoke := d.Invoke
	source := d.Tool.Source
	out := d
	out.Invoke = func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
		// Identity is mandatory (CLAUDE.md §6 rule 9). The Phase 30
		// provider also enforces this; we surface a wrapped error
		// here so the trace points back at the catalog wrapper.
		if _, ok := identity.From(ctx); !ok {
			return tools.ToolResult{}, fmt.Errorf("catalog: oauth wrapper (provider=%q, scope=%q): %w",
				opts.ProviderName, opts.BindingScope, auth.ErrIdentityRequired)
		}
		// Pre-check token availability. A missing token surfaces
		// `*auth.ErrAuthRequired` which propagates upward — the
		// planner / runtime catches it and pauses via the
		// Coordinator. We do NOT swallow the err; the §13 fail-loud
		// principle is non-negotiable here.
		if _, err := prov.Token(ctx, source); err != nil {
			return tools.ToolResult{}, err
		}
		return innerInvoke(ctx, args)
	}
	return out
}

// registeredNames lists every name currently in the catalog. Used in
// error messages so an operator sees the typo (their config named
// "summarize_doc" but the registered name is "summarise_doc").
func registeredNames(cat tools.ToolCatalog) string {
	listed := cat.List(tools.CatalogFilter{
		LoadingModes: []tools.LoadingMode{tools.LoadingAlways, tools.LoadingDeferred},
	})
	if len(listed) == 0 {
		return "(none)"
	}
	out := ""
	for i, t := range listed {
		if i > 0 {
			out += ", "
		}
		out += t.Name
	}
	return out
}

// providerNames lists every OAuth provider name configured. Used in
// `ErrUnknownOAuthProvider` messages.
func providerNames(m map[string]auth.OAuthProvider) string {
	if len(m) == 0 {
		return "(none)"
	}
	out := ""
	i := 0
	for name := range m {
		if i > 0 {
			out += ", "
		}
		out += name
		i++
	}
	return out
}
