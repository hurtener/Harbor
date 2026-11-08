package protocol

import (
	"context"
	stderrors "errors"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// ErrSessionPrepareUnavailable is returned by a SessionPreparer that cannot
// accept work (not yet bound, or shutting down). The surface maps it to
// CodeUnknownMethod so the route reads as absent rather than failing.
var ErrSessionPrepareUnavailable = stderrors.New("protocol: session preparation is unavailable")

// SessionPrepareRequest is the admitted, runtime-side form of a `prepare`
// call. Every field is derived after the same identity and signed Agent
// reach admission `start` applies; nothing in it comes from an unverified
// body field without that admission.
type SessionPrepareRequest struct {
	Identity         identity.Identity
	EffectiveAgentID string
	// ProviderRoute is the validated opaque route selector, or nil.
	ProviderRoute *llm.ProviderRoute
}

// SessionPreparer performs run-start preparation in the background. It must
// return promptly; ctx is the admitted request context and carries the
// verified identity, runtime, narrowed Agent reach and the sealed reach
// admission (tasks.AgentReachAdmissionSubjectFrom) the work runs under.
// Implementations must refuse a context missing any of them.
// Implementations are internally synchronized and safe for concurrent use.
type SessionPreparer interface {
	PrepareSession(ctx context.Context, req SessionPrepareRequest) (types.PrepareState, error)
}

// WithSessionPreparer wires the `prepare` method. A surface built without it
// answers `prepare` with CodeUnknownMethod (404), the partial-build
// convention. A nil preparer is treated as "not supplied".
func WithSessionPreparer(p SessionPreparer) Option {
	return func(s *ControlSurface) {
		if p != nil {
			s.preparer = p
		}
	}
}

// dispatchPrepare handles the `prepare` method. Its admission is the
// `start` admission verbatim — identity triple, effective-Agent reach before
// resolution, provider-route shape — minus everything that materializes
// state (no session row, no task, no idempotency record). Impersonation is
// refused at the transport edge before this runs.
func (s *ControlSurface) dispatchPrepare(ctx context.Context, req any) (*types.PrepareResponse, error) {
	method := methods.MethodPrepare
	if s.preparer == nil {
		return nil, protoerrors.Newf(protoerrors.CodeUnknownMethod,
			"method %q: session preparation is not configured on this Runtime", string(method))
	}
	pr, ok := req.(*types.PrepareRequest)
	if !ok || pr == nil {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: request is nil or not a *types.PrepareRequest", string(method))
	}
	if pr.Identity.IsImpersonating() {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: impersonation is not supported", string(method))
	}
	id := identity.Identity{TenantID: pr.Identity.Tenant, UserID: pr.Identity.User, SessionID: pr.Identity.Session}
	if err := identity.Validate(id); err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: identity scope incomplete: %v", string(method), err)
	}
	// The body triple must be the verified identity, and a verified identity
	// is mandatory. The transport's shared body-scope gate already reconciles
	// it; this re-check keeps an embedded or unauthenticated Dispatch caller
	// from preparing for a principal it was not verified as.
	verified, present := identity.FromVerified(ctx)
	if !present {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: a verified identity is required", string(method))
	}
	if verified != id {
		return nil, protoerrors.Newf(protoerrors.CodeScopeMismatch,
			"method %q: identity scope does not match the verified identity", string(method))
	}
	effectiveAgentID, err := s.validateNamedAgent(ctx, string(method), id, pr.AgentID)
	if err != nil {
		return nil, err
	}
	var route *llm.ProviderRoute
	if pr.ProviderRoute != nil {
		route = providerRouteFromWire(pr.ProviderRoute)
		if err := llm.ValidateProviderRoute(*route); err != nil || route.RouteID == "" {
			return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
				"method %q: provider_route requires opaque route, connection, generation, and model selector fields", string(method))
		}
	}
	// A preparation is consumable only by a run carrying a sealed reach
	// admission for the same subject, so without the admission authority it
	// could never be used: the method reads as absent.
	if s.reachAdmissions == nil {
		return nil, protoerrors.Newf(protoerrors.CodeUnknownMethod,
			"method %q: session preparation requires the agent reach admission authority", string(method))
	}
	// Narrow the carried Agent reach to the single admitted target, exactly
	// as run start re-seats it from its sealed admission receipt, and seal
	// the admission the preparer records. Only the admission authority can
	// mint that proof, so the preparer accepts nothing this edge did not
	// admit, and run start reuses only a record whose proof matches its own.
	workCtx, admitErr := s.reachAdmissions.Admit(auth.WithAgentReach(ctx, []string{effectiveAgentID}), id, effectiveAgentID)
	if admitErr != nil {
		return nil, protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: authenticate agent reach admission: %v", string(method), admitErr)
	}
	state, err := s.preparer.PrepareSession(workCtx, SessionPrepareRequest{
		Identity: id, EffectiveAgentID: effectiveAgentID, ProviderRoute: route,
	})
	if err != nil {
		if stderrors.Is(err, ErrSessionPrepareUnavailable) {
			return nil, protoerrors.Newf(protoerrors.CodeUnknownMethod,
				"method %q: session preparation is not available on this Runtime", string(method))
		}
		return nil, protoerrors.Newf(protoerrors.CodeRuntimeError, "method %q: %v", string(method), err)
	}
	return &types.PrepareResponse{State: state, ProtocolVersion: types.ProtocolVersion}, nil
}
