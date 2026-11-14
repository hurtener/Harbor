package protocol

import (
	"context"
	stderrors "errors"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/importer"
	"github.com/hurtener/Harbor/internal/skills/publication"
)

// read serves skills.publications.read. An admin caller reads any revision
// in the tenant; any other verified caller only the active catalog's current
// revision. Invisible revisions are not found.
func (s *SkillPublicationsSurface) read(ctx context.Context, method methods.Method, caller identity.Quadruple, r *types.SkillPublicationReadRequest) (any, error) {
	if strings.TrimSpace(r.PublicationID) == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest, "method %q: publication_id is required", string(method))
	}
	skill, meta, err := s.store.Read(ctx, caller, publication.ReadRequest{
		PublicationID: r.PublicationID, RevisionID: r.RevisionID, AnyState: s.isAdmin(ctx),
	})
	if err != nil {
		return nil, mapPublicationError(method, err)
	}
	doc, perr := renderDocument(method, skill)
	if perr != nil {
		return nil, perr
	}
	return &types.SkillPublicationReadResponse{
		Publication: metadataToWire(meta), RevisionID: meta.RevisionID, Skill: skillToWire(skill),
		Document: doc, ProtocolVersion: types.ProtocolVersion,
	}, nil
}

func (s *SkillPublicationsSurface) isAdmin(ctx context.Context) bool {
	return s.adminScope(ctx, auth.ScopeAdmin)
}

func (s *SkillPublicationsSurface) dispatchCandidate(ctx context.Context, method methods.Method, caller identity.Quadruple, req any) (any, error) {
	switch method {
	case methods.MethodSkillsPublicationsCandidatesSubmit:
		r, ok := req.(*types.SkillPublicationCandidateSubmitRequest)
		if !ok || r == nil {
			return nil, invalidPublicationRequest(method, "SkillPublicationCandidateSubmitRequest")
		}
		return s.submitCandidate(ctx, method, caller, r)
	case methods.MethodSkillsPublicationsCandidatesList:
		r, ok := req.(*types.SkillPublicationCandidateListRequest)
		if !ok || r == nil {
			return nil, invalidPublicationRequest(method, "SkillPublicationCandidateListRequest")
		}
		items, err := s.candidates.List(ctx, caller, publication.ListCandidatesRequest{State: r.State, All: s.isAdmin(ctx)})
		if err != nil {
			return nil, mapPublicationError(method, err)
		}
		out := make([]types.SkillPublicationCandidate, 0, len(items))
		for _, item := range items {
			out = append(out, candidateToWire(item))
		}
		return &types.SkillPublicationCandidateListResponse{Candidates: out, ProtocolVersion: types.ProtocolVersion}, nil
	case methods.MethodSkillsPublicationsCandidatesRead:
		r, ok := req.(*types.SkillPublicationCandidateReadRequest)
		if !ok || r == nil {
			return nil, invalidPublicationRequest(method, "SkillPublicationCandidateReadRequest")
		}
		c, err := s.candidates.Read(ctx, caller, r.CandidateID, s.isAdmin(ctx))
		if err != nil {
			return nil, mapPublicationError(method, err)
		}
		doc, perr := renderDocument(method, c.Skill)
		if perr != nil {
			return nil, perr
		}
		return &types.SkillPublicationCandidateReadResponse{Candidate: candidateToWire(candidateMeta(c)), Skill: skillToWire(c.Skill), Document: doc, ProtocolVersion: types.ProtocolVersion}, nil
	case methods.MethodSkillsPublicationsCandidatesAccept:
		if perr := s.requireAdmin(ctx, method); perr != nil {
			return nil, perr
		}
		r, ok := req.(*types.SkillPublicationCandidateAcceptRequest)
		if !ok || r == nil {
			return nil, invalidPublicationRequest(method, "SkillPublicationCandidateAcceptRequest")
		}
		return s.acceptCandidate(ctx, method, caller, r)
	case methods.MethodSkillsPublicationsCandidatesReject:
		if perr := s.requireAdmin(ctx, method); perr != nil {
			return nil, perr
		}
		r, ok := req.(*types.SkillPublicationCandidateRejectRequest)
		if !ok || r == nil {
			return nil, invalidPublicationRequest(method, "SkillPublicationCandidateRejectRequest")
		}
		meta, err := s.candidates.Decide(ctx, caller, publication.DecideCandidateRequest{
			IdempotencyKey: r.IdempotencyKey, CandidateID: r.CandidateID, ExpectedGeneration: r.ExpectedGeneration,
			Outcome: publication.CandidateRejected, DecisionNote: r.DecisionNote,
		}, true)
		if err != nil {
			return nil, mapPublicationError(method, err)
		}
		return &types.SkillPublicationCandidateRejectResponse{Candidate: candidateToWire(meta), ProtocolVersion: types.ProtocolVersion}, nil
	case methods.MethodSkillsPublicationsCandidatesWithdraw:
		r, ok := req.(*types.SkillPublicationCandidateWithdrawRequest)
		if !ok || r == nil {
			return nil, invalidPublicationRequest(method, "SkillPublicationCandidateWithdrawRequest")
		}
		meta, err := s.candidates.Decide(ctx, caller, publication.DecideCandidateRequest{
			IdempotencyKey: r.IdempotencyKey, CandidateID: r.CandidateID, ExpectedGeneration: r.ExpectedGeneration,
			Outcome: publication.CandidateWithdrawn,
		}, false)
		if err != nil {
			return nil, mapPublicationError(method, err)
		}
		return &types.SkillPublicationCandidateWithdrawResponse{Candidate: candidateToWire(meta), ProtocolVersion: types.ProtocolVersion}, nil
	}
	return nil, protoerrors.Newf(protoerrors.CodeRuntimeError, "method %q: candidate dispatch table is incomplete", string(method))
}

func (s *SkillPublicationsSurface) submitCandidate(ctx context.Context, method methods.Method, caller identity.Quadruple, r *types.SkillPublicationCandidateSubmitRequest) (any, error) {
	agentID := strings.TrimSpace(r.AgentID)
	if agentID == "" || strings.TrimSpace(r.Name) == "" {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest, "method %q: agent_id and name are required", string(method))
	}
	if perr := s.requireReach(ctx, method, agentID); perr != nil {
		return nil, perr
	}
	skill, packageHash, supportFiles, err := s.snapshots.SnapshotPersonalSkill(ctx, agentID, r.Name)
	if err != nil {
		if stderrors.Is(err, skills.ErrSkillNotFound) || stderrors.Is(err, skills.ErrInstalledPackageNotFound) {
			return nil, protoerrors.Newf(protoerrors.CodeNotFound, "method %q: personal skill not found", string(method))
		}
		return nil, protoerrors.Newf(protoerrors.CodeIdentityScopeRequired, "method %q: personal skill could not be read under the caller's authority", string(method))
	}
	if supportFiles > 0 {
		return nil, mapPublicationError(method, publication.ErrCandidateSupportFilesUnsupported)
	}
	// Normalize through the bounded pack-item shape publications store.
	normalized, err := skills.PackItemFromSkill(skill).Skill()
	if err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest, "method %q: personal skill is not publishable: %v", string(method), err)
	}
	meta, err := s.candidates.Submit(ctx, caller, publication.SubmitCandidateRequest{
		IdempotencyKey: r.IdempotencyKey, AgentID: agentID, Skill: normalized, PackageHash: packageHash,
		TargetPublicationID: r.TargetPublicationID, Note: r.Note,
	})
	if err != nil {
		return nil, mapPublicationError(method, err)
	}
	return &types.SkillPublicationCandidateSubmitResponse{Candidate: candidateToWire(meta), ProtocolVersion: types.ProtocolVersion}, nil
}

// acceptCandidate accepts a pending candidate in one atomic store write
// so the publication and the accepted candidate land together or not
// at all. A losing concurrent decision is an ordinary conflict; an exact
// retry with the same idempotency key returns the original result.
func (s *SkillPublicationsSurface) acceptCandidate(ctx context.Context, method methods.Method, caller identity.Quadruple, r *types.SkillPublicationCandidateAcceptRequest) (any, error) {
	res, err := s.candidates.Accept(ctx, caller, publication.AcceptCandidateRequest{
		IdempotencyKey: r.IdempotencyKey, CandidateID: r.CandidateID, ExpectedGeneration: r.ExpectedGeneration,
		Mode: r.Mode, Name: r.Name, PublicationID: r.PublicationID,
		ExpectedPublicationGeneration: r.ExpectedPublicationGeneration, ExpectedPublicationContentHash: r.ExpectedPublicationContentHash,
		DecisionNote: r.DecisionNote,
	})
	if err != nil {
		return nil, mapPublicationError(method, err)
	}
	return &types.SkillPublicationCandidateAcceptResponse{Candidate: candidateToWire(res.Candidate), Publication: metadataToWire(res.Publication), Receipt: receiptToWire(res.Receipt), ProtocolVersion: types.ProtocolVersion}, nil
}

func renderDocument(method methods.Method, skill skills.Skill) (string, *protoerrors.Error) {
	doc, err := importer.RenderDocument(skill)
	if err != nil {
		return "", protoerrors.Newf(protoerrors.CodeRuntimeError, "method %q: skill document could not be rendered", string(method))
	}
	return string(doc), nil
}

func skillToWire(sk skills.Skill) types.SkillPublicationSkill {
	item := skills.PackItemFromSkill(sk)
	// Importer bookkeeping (underscore-prefixed keys) is runtime-private.
	for key := range item.Extra {
		if strings.HasPrefix(key, "_") {
			delete(item.Extra, key)
		}
	}
	if len(item.Extra) == 0 {
		item.Extra = nil
	}
	return types.SkillPublicationSkill{
		Name: item.Name, Title: item.Title, Description: item.Description, Trigger: item.Trigger, TaskType: item.TaskType,
		Tags: item.Tags, Steps: item.Steps, Preconditions: item.Preconditions, FailureModes: item.FailureModes,
		RequiredTools: item.RequiredTools, RequiredNS: item.RequiredNS, RequiredTags: item.RequiredTags,
		OriginRef: item.OriginRef, Extra: item.Extra,
	}
}

func candidateMeta(c publication.Candidate) publication.CandidateMetadata {
	return publication.CandidateMetadata{
		CandidateID: c.CandidateID, Name: c.Name, Title: c.Title, Description: c.Description, SubmitterUser: c.SubmitterUser,
		AgentID: c.AgentID, PackageHash: c.PackageHash, ContentHash: c.ContentHash, TargetPublicationID: c.TargetPublicationID,
		Note: c.Note, State: c.State, DecisionNote: c.DecisionNote, DecidedBy: c.DecidedBy, PublicationID: c.PublicationID,
		RevisionID: c.RevisionID, PriorContentHash: c.PriorContentHash, Generation: c.Generation, CreatedAt: c.CreatedAt,
		UpdatedAt: c.UpdatedAt, RuntimeID: c.RuntimeID,
	}
}

func candidateToWire(m publication.CandidateMetadata) types.SkillPublicationCandidate {
	return types.SkillPublicationCandidate{
		CandidateID: m.CandidateID, Name: m.Name, Title: m.Title, Description: m.Description, SubmitterUser: m.SubmitterUser,
		AgentID: m.AgentID, PackageHash: m.PackageHash, ContentHash: m.ContentHash, TargetPublicationID: m.TargetPublicationID,
		Note: m.Note, State: string(m.State), DecisionNote: m.DecisionNote, DecidedBy: m.DecidedBy,
		PublicationID: m.PublicationID, RevisionID: m.RevisionID, PriorContentHash: m.PriorContentHash,
		Generation: m.Generation, CreatedAt: wireTime(m.CreatedAt), UpdatedAt: wireTime(m.UpdatedAt), RuntimeID: m.RuntimeID,
	}
}

func wireTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
