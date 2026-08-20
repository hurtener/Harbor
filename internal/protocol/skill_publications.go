package protocol

import (
	"context"
	stderrors "errors"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/publication"
)

// SkillPublicationsSurface is the transport-agnostic HA-68 Protocol surface.
// Organization verbs publish and retire reviewed immutable revisions under
// an admin scope. Caller verbs discover publications and pin an exact
// revision to one signed-reach Agent. No method other than Resolve (used by
// runtime composition) returns a skill body.
//
// The surface is a compiled artifact: its store and authorization gates are
// immutable after construction and Dispatch keeps all request state in ctx
// and the wire request, so one instance is safe for concurrent reuse.
type SkillPublicationsSurface struct {
	store      publication.Store
	adminScope ScopeChecker
	reach      auth.AgentReachAuthorizer
	runtimeID  string
}

// SkillPublicationsDeps are the runtime seams required by HA-68.
type SkillPublicationsDeps struct {
	// Store is the same-runtime publication store. Mandatory.
	Store publication.Store
	// AdminScope answers whether the verified caller carries the organization
	// admin authorization. Nil uses auth.HasScope.
	AdminScope ScopeChecker
	// AgentReach is the shared signed effective-Agent reach gate. Nil uses the
	// fail-closed canonical authorizer.
	AgentReach auth.AgentReachAuthorizer
	// RuntimeID binds resolution to one Harbor runtime/deployment. An empty
	// value leaves the store's construction-time binding authoritative.
	RuntimeID string
}

// ErrSkillPublicationsMisconfigured reports a missing mandatory dependency.
var ErrSkillPublicationsMisconfigured = stderrors.New("protocol: SkillPublicationsSurface missing a mandatory dependency")

// NewSkillPublicationsSurface builds the HA-68 Protocol surface.
func NewSkillPublicationsSurface(deps SkillPublicationsDeps) (*SkillPublicationsSurface, error) {
	if deps.Store == nil {
		return nil, fmt.Errorf("%w: Store is nil", ErrSkillPublicationsMisconfigured)
	}
	adminScope := deps.AdminScope
	if adminScope == nil {
		adminScope = auth.HasScope
	}
	reach := deps.AgentReach
	if reach == nil {
		reach = auth.NewAgentReachAuthorizer()
	}
	return &SkillPublicationsSurface{
		store:      deps.Store,
		adminScope: adminScope,
		reach:      reach,
		runtimeID:  strings.TrimSpace(deps.RuntimeID),
	}, nil
}

// Dispatch handles one of the ten canonical skills.publications.* methods.
// It returns the corresponding Protocol response or a typed Protocol error.
func (s *SkillPublicationsSurface) Dispatch(ctx context.Context, method methods.Method, req any) (any, error) {
	if !methods.IsSkillPublicationMethod(method) {
		return nil, protoerrors.Newf(protoerrors.CodeUnknownMethod,
			"method %q is not a canonical skill-publication method", string(method))
	}
	scope, perr := publicationScope(req)
	if perr != nil {
		return nil, perr
	}
	caller, perr := s.caller(ctx, scope)
	if perr != nil {
		return nil, perr
	}
	if s.runtimeID != "" {
		ctx = publication.WithRuntimeID(ctx, s.runtimeID)
	}

	switch method {
	case methods.MethodSkillsPublicationsPublish:
		if perr = s.requireAdmin(ctx, method); perr != nil {
			return nil, perr
		}
		r, ok := req.(*types.SkillPublicationPublishRequest)
		if !ok || r == nil {
			return nil, invalidPublicationRequest(method, "SkillPublicationPublishRequest")
		}
		skill, err := skillFromWire(r.Skill)
		if err != nil {
			return nil, mapPublicationError(method, err)
		}
		meta, receipt, err := s.store.Publish(ctx, caller, publication.PublishRequest{
			IdempotencyKey: r.IdempotencyKey, Name: r.Name, Skill: skill, ExpectedAbsent: r.ExpectedAbsent,
		})
		if err != nil {
			return nil, mapPublicationError(method, err)
		}
		return &types.SkillPublicationPublishResponse{Publication: metadataToWire(meta), Receipt: receiptToWire(receipt), ProtocolVersion: types.ProtocolVersion}, nil

	case methods.MethodSkillsPublicationsList:
		if perr = s.requireAdmin(ctx, method); perr != nil {
			return nil, perr
		}
		r, ok := req.(*types.SkillPublicationListRequest)
		if !ok || r == nil {
			return nil, invalidPublicationRequest(method, "SkillPublicationListRequest")
		}
		items, err := s.store.List(ctx, caller)
		if err != nil {
			return nil, mapPublicationError(method, err)
		}
		return &types.SkillPublicationListResponse{Publications: metadataSliceToWire(items), ProtocolVersion: types.ProtocolVersion}, nil

	case methods.MethodSkillsPublicationsGet:
		if perr = s.requireAdmin(ctx, method); perr != nil {
			return nil, perr
		}
		r, ok := req.(*types.SkillPublicationGetRequest)
		if !ok || r == nil {
			return nil, invalidPublicationRequest(method, "SkillPublicationGetRequest")
		}
		item, err := s.store.Get(ctx, caller, r.PublicationID)
		if err != nil {
			return nil, mapPublicationError(method, err)
		}
		return &types.SkillPublicationGetResponse{Publication: metadataToWire(item), ProtocolVersion: types.ProtocolVersion}, nil

	case methods.MethodSkillsPublicationsSuccessor:
		if perr = s.requireAdmin(ctx, method); perr != nil {
			return nil, perr
		}
		r, ok := req.(*types.SkillPublicationSuccessorRequest)
		if !ok || r == nil {
			return nil, invalidPublicationRequest(method, "SkillPublicationSuccessorRequest")
		}
		skill, err := skillFromWire(r.Skill)
		if err != nil {
			return nil, mapPublicationError(method, err)
		}
		meta, receipt, err := s.store.PublishSuccessor(ctx, caller, publication.SuccessorRequest{
			IdempotencyKey: r.IdempotencyKey, PublicationID: r.PublicationID,
			ExpectedGeneration: r.ExpectedGeneration, ExpectedContentHash: r.ExpectedContentHash, Skill: skill,
		})
		if err != nil {
			return nil, mapPublicationError(method, err)
		}
		return &types.SkillPublicationSuccessorResponse{Publication: metadataToWire(meta), Receipt: receiptToWire(receipt), ProtocolVersion: types.ProtocolVersion}, nil

	case methods.MethodSkillsPublicationsRetire:
		if perr = s.requireAdmin(ctx, method); perr != nil {
			return nil, perr
		}
		r, ok := req.(*types.SkillPublicationRetireRequest)
		if !ok || r == nil {
			return nil, invalidPublicationRequest(method, "SkillPublicationRetireRequest")
		}
		meta, receipt, err := s.store.Retire(ctx, caller, publication.RetireRequest{
			IdempotencyKey: r.IdempotencyKey, PublicationID: r.PublicationID,
			ExpectedGeneration: r.ExpectedGeneration, ExpectedContentHash: r.ExpectedContentHash,
		})
		if err != nil {
			return nil, mapPublicationError(method, err)
		}
		return &types.SkillPublicationRetireResponse{Publication: metadataToWire(meta), Receipt: receiptToWire(receipt), ProtocolVersion: types.ProtocolVersion}, nil

	case methods.MethodSkillsPublicationsAvailable:
		r, ok := req.(*types.SkillPublicationAvailableRequest)
		if !ok || r == nil {
			return nil, invalidPublicationRequest(method, "SkillPublicationAvailableRequest")
		}
		items, err := s.store.ListAvailable(ctx, caller)
		if err != nil {
			return nil, mapPublicationError(method, err)
		}
		return &types.SkillPublicationAvailableResponse{Publications: metadataSliceToWire(items), ProtocolVersion: types.ProtocolVersion}, nil

	case methods.MethodSkillsPublicationsInstall:
		r, ok := req.(*types.SkillPublicationInstallRequest)
		if !ok || r == nil {
			return nil, invalidPublicationRequest(method, "SkillPublicationInstallRequest")
		}
		if perr = s.requireReach(ctx, method, r.AgentID); perr != nil {
			return nil, perr
		}
		ref, receipt, err := s.store.Install(ctx, caller, publication.InstallRequest{
			IdempotencyKey: r.IdempotencyKey, AgentID: r.AgentID, PublicationID: r.PublicationID,
			RevisionID: r.RevisionID, ExpectedAbsent: r.ExpectedAbsent,
		})
		if err != nil {
			return nil, mapPublicationError(method, err)
		}
		return &types.SkillPublicationInstallResponse{Reference: referenceToWire(ref), Receipt: receiptToWire(receipt), ProtocolVersion: types.ProtocolVersion}, nil

	case methods.MethodSkillsPublicationsUpdate:
		r, ok := req.(*types.SkillPublicationUpdateRequest)
		if !ok || r == nil {
			return nil, invalidPublicationRequest(method, "SkillPublicationUpdateRequest")
		}
		if perr = s.requireReach(ctx, method, r.AgentID); perr != nil {
			return nil, perr
		}
		ref, receipt, err := s.store.Update(ctx, caller, publication.UpdateRequest{
			IdempotencyKey: r.IdempotencyKey, AgentID: r.AgentID, PublicationID: r.PublicationID,
			RevisionID: r.RevisionID, ExpectedGeneration: r.ExpectedGeneration, ExpectedContentHash: r.ExpectedContentHash,
		})
		if err != nil {
			return nil, mapPublicationError(method, err)
		}
		return &types.SkillPublicationUpdateResponse{Reference: referenceToWire(ref), Receipt: receiptToWire(receipt), ProtocolVersion: types.ProtocolVersion}, nil

	case methods.MethodSkillsPublicationsRemove:
		r, ok := req.(*types.SkillPublicationRemoveRequest)
		if !ok || r == nil {
			return nil, invalidPublicationRequest(method, "SkillPublicationRemoveRequest")
		}
		if perr = s.requireReach(ctx, method, r.AgentID); perr != nil {
			return nil, perr
		}
		receipt, err := s.store.Remove(ctx, caller, publication.RemoveRequest{
			IdempotencyKey: r.IdempotencyKey, AgentID: r.AgentID,
			ExpectedGeneration: r.ExpectedGeneration, ExpectedContentHash: r.ExpectedContentHash,
		})
		if err != nil {
			return nil, mapPublicationError(method, err)
		}
		return &types.SkillPublicationRemoveResponse{Receipt: receiptToWire(receipt), ProtocolVersion: types.ProtocolVersion}, nil

	case methods.MethodSkillsPublicationsReferencesList:
		r, ok := req.(*types.SkillPublicationReferencesListRequest)
		if !ok || r == nil {
			return nil, invalidPublicationRequest(method, "SkillPublicationReferencesListRequest")
		}
		refs, err := s.store.ListReferences(ctx, caller)
		if err != nil {
			return nil, mapPublicationError(method, err)
		}
		out := make([]types.SkillPublicationReference, 0, len(refs))
		for _, ref := range refs {
			out = append(out, referenceToWire(ref))
		}
		return &types.SkillPublicationReferencesListResponse{References: out, ProtocolVersion: types.ProtocolVersion}, nil
	default:
		return nil, protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: skill-publication dispatch table is incomplete", string(method))
	}
}

func (s *SkillPublicationsSurface) caller(ctx context.Context, scope types.IdentityScope) (identity.Quadruple, *protoerrors.Error) {
	if scope.Tenant == "" || scope.User == "" || scope.Session == "" {
		return identity.Quadruple{}, protoerrors.New(protoerrors.CodeIdentityRequired, "skill-publication identity is incomplete")
	}
	verified, ok := identity.FromVerified(ctx)
	if !ok {
		return identity.Quadruple{}, protoerrors.New(protoerrors.CodeIdentityRequired, "skill-publication caller identity is not verified")
	}
	requested := identity.Identity{TenantID: scope.Tenant, UserID: scope.User, SessionID: scope.Session}
	if requested != verified {
		return identity.Quadruple{}, protoerrors.New(protoerrors.CodeScopeMismatch, "skill-publication identity does not match the verified caller")
	}
	return identity.Quadruple{Identity: verified, RunID: scope.Run}, nil
}

func (s *SkillPublicationsSurface) requireAdmin(ctx context.Context, method methods.Method) *protoerrors.Error {
	if !s.adminScope(ctx, auth.ScopeAdmin) {
		return protoerrors.Newf(protoerrors.CodeIdentityScopeRequired,
			"method %q: organization skill-publication admin scope required", string(method))
	}
	return nil
}

func (s *SkillPublicationsSurface) requireReach(ctx context.Context, method methods.Method, agentID string) *protoerrors.Error {
	if err := s.reach.AuthorizeAgentReach(ctx, agentID); err != nil {
		return protoerrors.Newf(protoerrors.CodeIdentityScopeRequired,
			"method %q: signed effective-Agent reach denied", string(method))
	}
	return nil
}

func publicationScope(req any) (types.IdentityScope, *protoerrors.Error) {
	switch r := req.(type) {
	case *types.SkillPublicationPublishRequest:
		if r != nil {
			return r.Identity, nil
		}
	case *types.SkillPublicationListRequest:
		if r != nil {
			return r.Identity, nil
		}
	case *types.SkillPublicationGetRequest:
		if r != nil {
			return r.Identity, nil
		}
	case *types.SkillPublicationSuccessorRequest:
		if r != nil {
			return r.Identity, nil
		}
	case *types.SkillPublicationRetireRequest:
		if r != nil {
			return r.Identity, nil
		}
	case *types.SkillPublicationAvailableRequest:
		if r != nil {
			return r.Identity, nil
		}
	case *types.SkillPublicationInstallRequest:
		if r != nil {
			return r.Identity, nil
		}
	case *types.SkillPublicationUpdateRequest:
		if r != nil {
			return r.Identity, nil
		}
	case *types.SkillPublicationRemoveRequest:
		if r != nil {
			return r.Identity, nil
		}
	case *types.SkillPublicationReferencesListRequest:
		if r != nil {
			return r.Identity, nil
		}
	}
	return types.IdentityScope{}, protoerrors.New(protoerrors.CodeInvalidRequest, "skill-publication request is nil or has the wrong wire type")
}

func invalidPublicationRequest(method methods.Method, want string) *protoerrors.Error {
	return protoerrors.Newf(protoerrors.CodeInvalidRequest,
		"method %q: request is nil or not a *types.%s", string(method), want)
}

func skillFromWire(in types.SkillPublicationSkill) (skills.Skill, error) {
	item := skills.AgentPackItem{
		Name: in.Name, Title: in.Title, Description: in.Description, Trigger: in.Trigger,
		TaskType: in.TaskType, Tags: append([]string(nil), in.Tags...), Steps: append([]string(nil), in.Steps...),
		Preconditions: append([]string(nil), in.Preconditions...), FailureModes: append([]string(nil), in.FailureModes...),
		RequiredTools: append([]string(nil), in.RequiredTools...), RequiredNS: append([]string(nil), in.RequiredNS...),
		RequiredTags: append([]string(nil), in.RequiredTags...), OriginRef: in.OriginRef,
		Extra: cloneStringMap(in.Extra),
	}
	return item.Skill()
}

func metadataToWire(m publication.Metadata) types.SkillPublicationMetadata {
	return types.SkillPublicationMetadata{PublicationID: m.PublicationID, RevisionID: m.RevisionID, Name: m.Name, ContentHash: m.ContentHash, State: string(m.State), Generation: m.Generation, RuntimeID: m.RuntimeID}
}

func metadataSliceToWire(items []publication.Metadata) []types.SkillPublicationMetadata {
	out := make([]types.SkillPublicationMetadata, 0, len(items))
	for _, item := range items {
		out = append(out, metadataToWire(item))
	}
	return out
}

func referenceToWire(r publication.Reference) types.SkillPublicationReference {
	return types.SkillPublicationReference{AgentID: r.AgentID, PublicationID: r.PublicationID, RevisionID: r.RevisionID, ContentHash: r.ContentHash, Generation: r.Generation, RuntimeID: r.RuntimeID, State: string(r.State)}
}

func receiptToWire(r publication.Receipt) types.SkillPublicationReceipt {
	return types.SkillPublicationReceipt{OperationID: r.OperationID, Operation: r.Operation, PublicationID: r.PublicationID, RevisionID: r.RevisionID, Generation: r.Generation, State: string(r.State), BeforeHash: r.BeforeHash, AfterHash: r.AfterHash}
}

func cloneStringMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func mapPublicationError(method methods.Method, err error) *protoerrors.Error {
	if err == nil {
		return nil
	}
	switch {
	case stderrors.Is(err, publication.ErrIdentityRequired):
		return protoerrors.Newf(protoerrors.CodeIdentityRequired, "method %q: identity scope incomplete", string(method))
	case stderrors.Is(err, publication.ErrInvalidRequest):
		return protoerrors.Newf(protoerrors.CodeInvalidRequest, "method %q: request failed validation", string(method))
	case stderrors.Is(err, publication.ErrNotFound), stderrors.Is(err, publication.ErrReferenceNotFound):
		return protoerrors.Newf(protoerrors.CodeSkillPublicationNotFound, "method %q: publication or reference not found", string(method))
	case stderrors.Is(err, publication.ErrConflict):
		return protoerrors.Newf(protoerrors.CodeSkillPublicationConflict, "method %q: compare-and-swap precondition failed", string(method))
	case stderrors.Is(err, publication.ErrIdempotencyConflict):
		return protoerrors.Newf(protoerrors.CodeSkillPublicationIdempotencyConflict, "method %q: idempotency key was reused with a divergent request", string(method))
	case stderrors.Is(err, publication.ErrRetired):
		return protoerrors.Newf(protoerrors.CodeSkillPublicationRetired, "method %q: publication is retired", string(method))
	case stderrors.Is(err, publication.ErrRuntimeMismatch):
		return protoerrors.Newf(protoerrors.CodeSkillPublicationRuntimeMismatch, "method %q: publication belongs to another runtime", string(method))
	case stderrors.Is(err, publication.ErrContentHashMismatch):
		return protoerrors.Newf(protoerrors.CodeSkillPublicationConflict, "method %q: pinned content hash no longer matches", string(method))
	case stderrors.Is(err, publication.ErrStoreClosed):
		return protoerrors.Newf(protoerrors.CodeRuntimeError, "method %q: publication store is closed", string(method))
	default:
		return protoerrors.Newf(protoerrors.CodeRuntimeError, "method %q: publication operation failed", string(method))
	}
}

var _ interface {
	Dispatch(context.Context, methods.Method, any) (any, error)
} = (*SkillPublicationsSurface)(nil)
