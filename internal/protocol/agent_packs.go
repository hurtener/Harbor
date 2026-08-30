// Package protocol contains the transport-independent Harbor Protocol
// surfaces. This file owns the admin-only same-runtime Agent pack seam;
// storage and composition remain runtime concerns behind AgentPacksAdminPort.
package protocol

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

const (
	maxAgentPacksAgentIDBytes = 128
	maxAgentPacksPackIDs      = 128
	maxAgentPacksIDBytes      = 128
	maxAgentPacksKeyBytes     = 256
)

// AgentPacksErrorCode is the stable error class used by the assembly seam to
// classify a domain error. The runtime may keep its own sentinel vocabulary
// and provide an AgentPacksErrorClassifier at construction; arbitrary error
// text is never sent to clients.
type AgentPacksErrorCode string

const (
	AgentPacksErrorInvalid  AgentPacksErrorCode = "invalid"
	AgentPacksErrorNotFound AgentPacksErrorCode = "not_found"
	// AgentPacksErrorStale is a compare-and-swap failure: the source or target
	// composition changed after the caller inspected it.
	AgentPacksErrorStale AgentPacksErrorCode = "stale"
	// AgentPacksErrorConflict is an independently authored target-pack
	// collision. It is not a stale composition precondition.
	AgentPacksErrorConflict AgentPacksErrorCode = "conflict"
	// AgentPacksErrorIdempotencyConflict is reuse of a key with a different
	// copy fingerprint.
	AgentPacksErrorIdempotencyConflict AgentPacksErrorCode = "idempotency_conflict"
	AgentPacksErrorUnavailable         AgentPacksErrorCode = "unavailable"
)

// AgentPacksErrorClassifier translates a runtime sentinel into the small
// domain error vocabulary understood by this Protocol surface. Supplying a
// classifier lets the runtime keep returning its own sentinels; it never has
// to construct a Protocol error or expose its error text to the wire.
type AgentPacksErrorClassifier func(error) AgentPacksErrorCode

// AgentPacksCodedError is the optional runtime-error adapter contract. A
// runtime port can wrap its own sentinels with this interface, keeping the
// Protocol package independent from concrete storage/service packages.
type AgentPacksCodedError interface {
	error
	AgentPacksErrorCode() AgentPacksErrorCode
}

// AgentPacksAdminPort is the narrow runtime seam for the admin-only
// same-runtime Agent pack Protocol surface. The runtime owns pack storage,
// effective boot/revision composition, compare-and-swap, and idempotency;
// it need only implement these wire-shaped operations and does not import
// the Protocol transport packages.
type AgentPacksAdminPort interface {
	Inspect(context.Context, types.AgentConfigAgentPacksInspectRequest) (types.AgentConfigAgentPacksInspectResponse, error)
	Copy(context.Context, types.AgentConfigAgentPacksCopyRequest) (types.AgentConfigAgentPacksCopyResponse, error)
}

// AgentPacksDeps are the immutable dependencies for NewAgentPacksSurface.
type AgentPacksDeps struct {
	// Port is the runtime's same-runtime pack inspection/copy implementation.
	// Inspect returns distinct complete boot/revision layers plus the deduped
	// effective view. Copy applies the selected pack set atomically. It is
	// mandatory. Runtime sentinels can be translated by ClassifyError without
	// importing runtime implementation types into this Protocol package.
	Port AgentPacksAdminPort
	// ClassifyError is the runtime-owned sentinel mapper. Nil also accepts the
	// structural AgentPacksCodedError adapter for small embedders and tests.
	ClassifyError AgentPacksErrorClassifier
	// AdminScope reports whether the verified caller carries auth.ScopeAdmin.
	// Nil uses auth.HasScope.
	AdminScope ScopeChecker
	// AgentReach is the shared signed effective-Agent reach gate. Nil uses the
	// fail-closed canonical authorizer.
	AgentReach auth.AgentReachAuthorizer
	// AgentResolver is the authoritative same-runtime registration resolver.
	// Every addressed source and target must resolve in the caller's tenant;
	// signed reach alone is not registration authority. It is mandatory so an
	// unknown id can never reach the runtime port and synthesize state.
	AgentResolver AgentResolver
}

// ErrAgentPacksMisconfigured reports a missing mandatory runtime dependency.
var ErrAgentPacksMisconfigured = errors.New("protocol: AgentPacksSurface missing a mandatory dependency")

// AgentPacksSurface is the transport-independent admin Agent pack Protocol
// surface. Its fields are immutable after construction and Dispatch stores no
// per-request state, so one instance is safe for concurrent reuse.
type AgentPacksSurface struct {
	port       AgentPacksAdminPort
	classify   AgentPacksErrorClassifier
	adminScope ScopeChecker
	reach      auth.AgentReachAuthorizer
	agents     AgentResolver
}

// NewAgentPacksSurface builds an admin-only same-runtime Agent pack surface.
func NewAgentPacksSurface(deps AgentPacksDeps) (*AgentPacksSurface, error) {
	if deps.Port == nil {
		return nil, fmt.Errorf("%w: Port is nil", ErrAgentPacksMisconfigured)
	}
	if deps.AgentResolver == nil {
		return nil, fmt.Errorf("%w: AgentResolver is nil", ErrAgentPacksMisconfigured)
	}
	adminScope := deps.AdminScope
	if adminScope == nil {
		adminScope = auth.HasScope
	}
	reach := deps.AgentReach
	if reach == nil {
		reach = auth.NewAgentReachAuthorizer()
	}
	return &AgentPacksSurface{
		port: deps.Port, classify: deps.ClassifyError, adminScope: adminScope,
		reach: reach, agents: deps.AgentResolver,
	}, nil
}

// Dispatch handles agent_config.agent_packs.inspect and .copy. Caller
// identity, admin scope, signed reach, and same-runtime registration are
// checked before the runtime port is called. A copy collision must be
// returned by the port as a typed error; a successful response can contain
// only copied/noop outcomes.
func (s *AgentPacksSurface) Dispatch(ctx context.Context, method methods.Method, req any) (any, error) {
	if s == nil || s.port == nil {
		return nil, protoerrors.New(protoerrors.CodeRuntimeError, "agent-pack surface is not wired")
	}
	if !methods.IsAgentConfigAgentPacksMethod(method) {
		return nil, protoerrors.Newf(protoerrors.CodeUnknownMethod,
			"method %q is not an Agent pack inspection/copy method", string(method))
	}
	// Check the verified admin entitlement before inspecting any caller-owned
	// request data. The HTTP transport performs the same gate before decoding;
	// keeping it first here prevents an in-process caller from using malformed
	// body fields to distinguish an admin-only route.
	if !s.adminScope(ctx, auth.ScopeAdmin) {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityScopeRequired,
			"method %q: admin scope required", string(method))
	}
	scope, perr := agentPacksRequestScope(req)
	if perr != nil {
		return nil, perr
	}
	verifiedScope, perr := s.caller(ctx, scope)
	if perr != nil {
		return nil, perr
	}

	switch method {
	case methods.MethodAgentConfigAgentPacksInspect:
		r, ok := req.(*types.AgentConfigAgentPacksInspectRequest)
		if !ok || r == nil {
			return nil, invalidAgentPacksRequest(method, "AgentConfigAgentPacksInspectRequest")
		}
		if perr = validateAgentID(r.AgentID); perr != nil {
			return nil, perr
		}
		if _, admitErr := AdmitEffectiveAgent(ctx, string(method), identity.Identity{
			TenantID: verifiedScope.Tenant, UserID: verifiedScope.User, SessionID: verifiedScope.Session,
		}, r.AgentID, s.agents, s.reach); admitErr != nil {
			return nil, admitErr
		}
		forwarded := *r
		forwarded.Identity = verifiedScope
		resp, err := s.port.Inspect(ctx, forwarded)
		if err != nil {
			return nil, mapAgentPacksError(method, err, s.classify)
		}
		if perr = validateInspectResponse(resp, r.AgentID); perr != nil {
			return nil, perr
		}
		// The three layers are part of the complete inspection projection;
		// encode an empty layer as [] rather than null even when a runtime
		// implementation returns a nil slice.
		resp.BootPacks = nonNilAgentPackInspections(resp.BootPacks)
		resp.RevisionPacks = nonNilAgentPackInspections(resp.RevisionPacks)
		resp.EffectivePacks = nonNilAgentPackInspections(resp.EffectivePacks)
		resp.ProtocolVersion = types.ProtocolVersion
		return &resp, nil

	case methods.MethodAgentConfigAgentPacksCopy:
		r, ok := req.(*types.AgentConfigAgentPacksCopyRequest)
		if !ok || r == nil {
			return nil, invalidAgentPacksRequest(method, "AgentConfigAgentPacksCopyRequest")
		}
		if perr = validateCopyRequest(*r); perr != nil {
			return nil, perr
		}
		caller := identity.Identity{
			TenantID: verifiedScope.Tenant, UserID: verifiedScope.User, SessionID: verifiedScope.Session,
		}
		if _, admitErr := AdmitEffectiveAgent(ctx, string(method), caller, r.SourceAgentID, s.agents, s.reach); admitErr != nil {
			return nil, admitErr
		}
		if _, admitErr := AdmitEffectiveAgent(ctx, string(method), caller, r.TargetAgentID, s.agents, s.reach); admitErr != nil {
			return nil, admitErr
		}
		forwarded := *r
		forwarded.PackIDs = append([]string(nil), r.PackIDs...)
		forwarded.Identity = verifiedScope
		resp, err := s.port.Copy(ctx, forwarded)
		if err != nil {
			return nil, mapAgentPacksError(method, err, s.classify)
		}
		if perr = validateCopyResponse(resp, *r); perr != nil {
			return nil, perr
		}
		resp.ProtocolVersion = types.ProtocolVersion
		return &resp, nil
	default:
		return nil, protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: Agent pack dispatch table is incomplete", string(method))
	}
}

func (s *AgentPacksSurface) caller(ctx context.Context, scope types.IdentityScope) (types.IdentityScope, *protoerrors.Error) {
	if scope.Tenant == "" || scope.User == "" || scope.Session == "" {
		return types.IdentityScope{}, protoerrors.New(protoerrors.CodeIdentityRequired, "agent-pack identity is incomplete")
	}
	verified, ok := identity.FromVerified(ctx)
	if !ok {
		return types.IdentityScope{}, protoerrors.New(protoerrors.CodeIdentityRequired, "agent-pack caller identity is not verified")
	}
	requested := identity.Identity{TenantID: scope.Tenant, UserID: scope.User, SessionID: scope.Session}
	if requested != verified {
		return types.IdentityScope{}, protoerrors.New(protoerrors.CodeScopeMismatch, "agent-pack identity does not match the verified caller")
	}
	return types.IdentityScope{Tenant: verified.TenantID, User: verified.UserID, Session: verified.SessionID}, nil
}

func agentPacksRequestScope(req any) (types.IdentityScope, *protoerrors.Error) {
	switch r := req.(type) {
	case *types.AgentConfigAgentPacksInspectRequest:
		if r != nil {
			return r.Identity, nil
		}
	case *types.AgentConfigAgentPacksCopyRequest:
		if r != nil {
			return r.Identity, nil
		}
	}
	return types.IdentityScope{}, protoerrors.New(protoerrors.CodeInvalidRequest, "Agent pack request is nil or has the wrong wire type")
}

func invalidAgentPacksRequest(method methods.Method, want string) *protoerrors.Error {
	return protoerrors.Newf(protoerrors.CodeInvalidRequest,
		"method %q: request is nil or not a *types.%s", string(method), want)
}

func validateAgentID(agentID string) *protoerrors.Error {
	if strings.TrimSpace(agentID) == "" || len(agentID) > maxAgentPacksAgentIDBytes {
		return protoerrors.New(protoerrors.CodeInvalidRequest, "agent id is empty or exceeds the protocol bound")
	}
	return nil
}

func validateCopyRequest(req types.AgentConfigAgentPacksCopyRequest) *protoerrors.Error {
	if perr := validateAgentID(req.SourceAgentID); perr != nil {
		return perr
	}
	if perr := validateAgentID(req.TargetAgentID); perr != nil {
		return perr
	}
	if req.SourceAgentID == req.TargetAgentID {
		return protoerrors.New(protoerrors.CodeInvalidRequest, "source and target agent ids must differ")
	}
	if len(req.PackIDs) == 0 || len(req.PackIDs) > maxAgentPacksPackIDs {
		return protoerrors.New(protoerrors.CodeInvalidRequest, "pack_ids is empty or exceeds the protocol bound")
	}
	seen := make(map[string]struct{}, len(req.PackIDs))
	for _, packID := range req.PackIDs {
		canonicalPackID := strings.ToLower(strings.TrimSpace(packID))
		if canonicalPackID == "" || len(packID) > maxAgentPacksIDBytes || packID != canonicalPackID {
			return protoerrors.New(protoerrors.CodeInvalidRequest, "pack_ids contains an empty, non-canonical, or oversized id")
		}
		if _, exists := seen[canonicalPackID]; exists {
			return protoerrors.New(protoerrors.CodeInvalidRequest, "pack_ids contains a duplicate id")
		}
		seen[canonicalPackID] = struct{}{}
	}
	if strings.TrimSpace(req.IdempotencyKey) == "" || len(req.IdempotencyKey) > maxAgentPacksKeyBytes {
		return protoerrors.New(protoerrors.CodeInvalidRequest, "idempotency_key is empty or exceeds the protocol bound")
	}
	if !isCanonicalPackHash(req.ExpectedSourceCompositionHash) || !isCanonicalPackHash(req.ExpectedTargetCompositionHash) {
		return protoerrors.New(protoerrors.CodeInvalidRequest, "expected composition hashes must be canonical lowercase SHA-256 values")
	}
	return nil
}

func validateInspectResponse(resp types.AgentConfigAgentPacksInspectResponse, agentID string) *protoerrors.Error {
	if resp.AgentID != agentID {
		return protoerrors.New(protoerrors.CodeRuntimeError, "agent-pack inspection returned an unexpected agent")
	}
	if perr := validateRequiredPackHash(resp.CompositionHash); perr != nil {
		return perr
	}
	if perr := validateRequiredPackHash(resp.BootPackSetHash); perr != nil {
		return perr
	}
	if perr := validateInspectLayer(resp.BootPacks, "boot"); perr != nil {
		return perr
	}
	if perr := validateInspectLayer(resp.RevisionPacks, "revision"); perr != nil {
		return perr
	}
	return validateInspectLayer(resp.EffectivePacks, "effective")
}

func validateInspectLayer(items []types.AgentConfigAgentPackInspection, layer string) *protoerrors.Error {
	if len(items) > maxAgentPacksPackIDs {
		return protoerrors.New(protoerrors.CodeRuntimeError, "agent-pack inspection returned too many packs")
	}
	seen := make(map[string]struct{}, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.PackID) == "" || strings.TrimSpace(item.SemanticHash) == "" {
			return protoerrors.New(protoerrors.CodeRuntimeError, "agent-pack inspection returned incomplete metadata")
		}
		canonicalPackName := strings.ToLower(strings.TrimSpace(item.Pack.Name))
		if item.PackID != item.Pack.Name || item.Pack.Name != canonicalPackName || canonicalPackName == "" {
			return protoerrors.New(protoerrors.CodeRuntimeError, "agent-pack inspection returned a pack id that is not its canonical name")
		}
		if !isCanonicalPackHash(item.SemanticHash) {
			return protoerrors.New(protoerrors.CodeRuntimeError, "agent-pack inspection returned a non-canonical semantic hash")
		}
		if item.Source != "boot" && item.Source != "revision" && item.Source != "both" {
			return protoerrors.New(protoerrors.CodeRuntimeError, "agent-pack inspection returned an unknown source")
		}
		if layer == "boot" && item.Source != "boot" {
			return protoerrors.New(protoerrors.CodeRuntimeError, "agent-pack inspection returned an invalid boot source")
		}
		if layer == "revision" && item.Source != "revision" {
			return protoerrors.New(protoerrors.CodeRuntimeError, "agent-pack inspection returned an invalid revision source")
		}
		if _, exists := seen[item.PackID]; exists {
			return protoerrors.New(protoerrors.CodeRuntimeError, "agent-pack inspection returned duplicate pack ids")
		}
		seen[item.PackID] = struct{}{}
	}
	return nil
}

func validateCopyResponse(resp types.AgentConfigAgentPacksCopyResponse, req types.AgentConfigAgentPacksCopyRequest) *protoerrors.Error {
	if resp.SourceAgentID != req.SourceAgentID || resp.TargetAgentID != req.TargetAgentID {
		return protoerrors.New(protoerrors.CodeRuntimeError, "agent-pack copy returned unexpected agent ids")
	}
	if len(resp.Outcomes) != len(req.PackIDs) {
		return protoerrors.New(protoerrors.CodeRuntimeError, "agent-pack copy returned incomplete outcomes")
	}
	if perr := validateRequiredPackHash(resp.CompositionHash); perr != nil {
		return perr
	}
	if perr := validateRequiredPackHash(resp.BootPackSetHash); perr != nil {
		return perr
	}
	wanted := make(map[string]struct{}, len(req.PackIDs))
	for _, packID := range req.PackIDs {
		wanted[packID] = struct{}{}
	}
	seen := make(map[string]struct{}, len(resp.Outcomes))
	for _, outcome := range resp.Outcomes {
		if _, wanted := wanted[outcome.PackID]; !wanted {
			return protoerrors.New(protoerrors.CodeRuntimeError, "agent-pack copy returned an unexpected pack id")
		}
		if _, exists := seen[outcome.PackID]; exists {
			return protoerrors.New(protoerrors.CodeRuntimeError, "agent-pack copy returned duplicate pack ids")
		}
		if outcome.Outcome != "copied" && outcome.Outcome != "noop" {
			return protoerrors.New(protoerrors.CodeRuntimeError, "agent-pack copy returned a non-terminal outcome")
		}
		seen[outcome.PackID] = struct{}{}
	}
	return nil
}

func validateRequiredPackHash(value string) *protoerrors.Error {
	if !isCanonicalPackHash(value) {
		return protoerrors.New(protoerrors.CodeRuntimeError, "Agent pack response contained a missing or non-canonical hash")
	}
	return nil
}

func nonNilAgentPackInspections(items []types.AgentConfigAgentPackInspection) []types.AgentConfigAgentPackInspection {
	if items == nil {
		return []types.AgentConfigAgentPackInspection{}
	}
	return items
}

func isCanonicalPackHash(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func mapAgentPacksError(method methods.Method, err error, classify AgentPacksErrorClassifier) *protoerrors.Error {
	if err == nil {
		return nil
	}
	var code AgentPacksErrorCode
	if classify != nil {
		code = classify(err)
	}
	var coded AgentPacksCodedError
	if code == "" && errors.As(err, &coded) {
		code = coded.AgentPacksErrorCode()
	}
	if code != "" {
		switch code {
		case AgentPacksErrorInvalid:
			return protoerrors.Newf(protoerrors.CodeInvalidRequest, "method %q: Agent pack request rejected", string(method))
		case AgentPacksErrorNotFound:
			return protoerrors.Newf(protoerrors.CodeNotFound, "method %q: Agent pack was not found", string(method))
		case AgentPacksErrorStale:
			return protoerrors.Newf(protoerrors.CodeRevisionConflict, "method %q: Agent pack composition precondition is stale", string(method))
		case AgentPacksErrorConflict:
			return protoerrors.Newf(protoerrors.CodeAgentPackCopyConflict, "method %q: Agent pack copy collides with an independently authored target", string(method))
		case AgentPacksErrorIdempotencyConflict:
			return protoerrors.Newf(protoerrors.CodeAgentPackCopyIdempotencyConflict, "method %q: Agent pack copy idempotency key was reused", string(method))
		case AgentPacksErrorUnavailable:
			return protoerrors.Newf(protoerrors.CodeRuntimeError, "method %q: Agent pack runtime unavailable", string(method))
		}
	}
	return protoerrors.Newf(protoerrors.CodeRuntimeError, "method %q: Agent pack operation failed", string(method))
}

var _ interface {
	Dispatch(context.Context, methods.Method, any) (any, error)
} = (*AgentPacksSurface)(nil)
