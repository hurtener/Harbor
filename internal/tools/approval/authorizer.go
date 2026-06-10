// authorizer.go — the injected resolve-privilege seam (Phase 111f,
// D-203).
//
// Pre-111f, ResolveApproval hard-coded a Protocol-vocabulary check
// (`internal/protocol/auth` scope claims on ctx), which forced the
// runtime's own steering bridge to SELF-ELEVATE with protocol scopes
// to call its own gate — wire-layer auth vocabulary inside an
// in-process runtime control path (the SDK friction audit §4 tell
// that the check sat one layer too low). The seam moves the privilege
// decision onto GateDeps.Authorizer:
//
//   - Direct construction / the runtime control path wires
//     IdentityAuthorizer (runtime vocabulary: the resolving ctx
//     carries the pause's originating identity tuple, or the elevated
//     `registry.WithControlScope` claim).
//   - Wire-driven assembly injects the Protocol-side adapter
//     (`internal/server.ProtocolScopeAuthorizer`) which preserves
//     today's admin / console:fleet acceptance exactly and falls
//     through to the runtime default for in-process callers.
//
// D-203 records the direction rule this closes: runtime packages may
// import `internal/protocol/types` (pure data projection); they must
// never import protocol auth / methods / transports (behaviour).
package approval

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	agentregistry "github.com/hurtener/Harbor/internal/runtime/registry"
)

// Sentinel errors for the authorizer seam. Callers compare via
// errors.Is.
var (
	// ErrAuthorizerRequired — `NewApprovalGate` was called with a nil
	// `GateDeps.Authorizer`. An approval gate with no resolve
	// privilege check is a misconfiguration, not a permissive mode
	// (CLAUDE.md §13 fail-loudly; the same posture as the other
	// mandatory GateDeps fields).
	ErrAuthorizerRequired = errors.New("approval: ResolveAuthorizer required at construction")

	// ErrResolveForbidden — the gate's configured ResolveAuthorizer
	// rejected the resolving ctx. The Phase 111f replacement for the
	// pre-seam ErrApprovalScopeRequired: same fail-closed posture,
	// runtime vocabulary. With the default IdentityAuthorizer it means
	// the ctx carried neither the pause's originating identity tuple
	// nor the elevated control-scope claim.
	ErrResolveForbidden = errors.New("approval: resolve not authorized")
)

// PendingInfo describes the parked approval a resolver is targeting.
// The gate builds it from its pending entry and hands it to the
// configured ResolveAuthorizer so the privilege decision can compare
// the resolving ctx against the ORIGINATING call's identity / shape.
// No caller-controlled arg bytes ride here — Tool name, token,
// identity triple, and classification tags only.
type PendingInfo struct {
	// Tool is the name of the tool whose call is awaiting approval.
	Tool string
	// Token is the pause token the resolver presented.
	Token pauseresume.Token
	// Identity is the (tenant, user, session) triple the gated call
	// is running under — the pause's originating identity.
	Identity identity.Identity
	// Tags is the caller-supplied classification surface from the
	// original ApprovalRequest.
	Tags []string
}

// ResolveAuthorizer is the injected privilege check deciding who may
// resolve a pending approval (Phase 111f, D-203). `AuthorizeResolve`
// returns nil to permit the resolution, or an error (conventionally
// wrapping ErrResolveForbidden) to reject it. The gate calls it after
// locating the pending entry and BEFORE driving the Coordinator, so
// an unauthorized resolver never mutates pause state.
//
// Implementations MUST be safe for concurrent use (D-025): one
// authorizer instance serves every ResolveApproval call on the gate.
type ResolveAuthorizer interface {
	// AuthorizeResolve reports whether the caller represented by ctx
	// may resolve the pending approval described by pending.
	AuthorizeResolve(ctx context.Context, pending PendingInfo) error
}

// IdentityAuthorizer is the package-default, runtime-vocabulary
// ResolveAuthorizer: a resolving ctx is authorized when it carries
//
//   - the pause's ORIGINATING identity tuple (identity.From(ctx)
//     equals PendingInfo.Identity — the steering bridge's shape: the
//     run resolving its own gate after the Protocol edge already
//     vetted the wire caller's RFC §6.3 steering scope), OR
//   - the elevated fleet-control-scope claim
//     (`registry.WithControlScope` — reused rather than minting a new
//     claim shape; the same trust-based-in-V1 claim the Agent
//     Registry's fleet-control commands consume, with the same
//     audit-trail posture).
//
// Everything else is rejected with a wrapped ErrResolveForbidden —
// fail closed, no permissive default. Note the Coordinator's own
// Resume-side identity check still applies AFTER the authorizer
// (defence in depth): a control-scoped resolver whose ctx identity
// does not match the pause's triple is rejected by
// `pauseresume.ErrScopeMismatch`, exactly as before the seam.
//
// IdentityAuthorizer is stateless and safe for concurrent use.
type IdentityAuthorizer struct{}

// NewIdentityAuthorizer constructs the package-default
// runtime-vocabulary authorizer. The constructor exists so call sites
// read as an explicit wiring choice (`Authorizer:
// approval.NewIdentityAuthorizer()`), not a zero-value accident.
func NewIdentityAuthorizer() IdentityAuthorizer {
	return IdentityAuthorizer{}
}

// AuthorizeResolve implements ResolveAuthorizer.
func (IdentityAuthorizer) AuthorizeResolve(ctx context.Context, pending PendingInfo) error {
	if id, ok := identity.From(ctx); ok && id == pending.Identity {
		return nil
	}
	if agentregistry.HasControlScope(ctx) {
		return nil
	}
	// The originating triple is deliberately NOT echoed into the error:
	// the message flows back to the REJECTED (foreign) resolver via
	// ResolveApproval's wrap and the steering apply-error surface —
	// disclosing whose pause it is would leak identity to an
	// unauthorized caller. The audit/log layer carries the identity
	// attributes where they belong.
	return fmt.Errorf("%w: ctx carries neither the pause's originating identity nor the control-scope claim",
		ErrResolveForbidden)
}
