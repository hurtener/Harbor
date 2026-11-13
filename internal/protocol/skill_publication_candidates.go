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
		if err := s.clearStaleClaim(ctx, caller, r.CandidateID, true); err != nil {
			return nil, mapPublicationError(method, err)
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
		if err := s.clearStaleClaim(ctx, caller, r.CandidateID, false); err != nil {
			return nil, mapPublicationError(method, err)
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

// acceptCandidate runs claim → publish → finalize. Every resume of the same
// claim uses the candidate-derived publication idempotency key; when the
// claimed publish already applied under another caller, the existing
// publication is adopted rather than published twice.
func (s *SkillPublicationsSurface) acceptCandidate(ctx context.Context, method methods.Method, caller identity.Quadruple, r *types.SkillPublicationCandidateAcceptRequest) (any, error) {
	want := publication.AcceptClaim{
		Mode: r.Mode, Name: r.Name, PublicationID: r.PublicationID,
		ExpectedPublicationGeneration: r.ExpectedPublicationGeneration, ExpectedPublicationContentHash: r.ExpectedPublicationContentHash,
		DecisionNote: r.DecisionNote,
	}
	c, err := s.candidates.Claim(ctx, caller, r.CandidateID, r.ExpectedGeneration, want)
	if stderrors.Is(err, publication.ErrClaimOutstanding) {
		// A different acceptance is recorded. If it is stale (its attempt
		// died mid-flight) and published nothing, release it and claim again.
		if cerr := s.clearStaleClaim(ctx, caller, r.CandidateID, true); cerr != nil {
			return nil, mapPublicationError(method, cerr)
		}
		c, err = s.candidates.Claim(ctx, caller, r.CandidateID, r.ExpectedGeneration, want)
	}
	if err != nil {
		return nil, mapPublicationError(method, err)
	}
	if c.State == publication.CandidateAccepted {
		meta, gerr := s.store.Get(ctx, caller, c.PublicationID)
		if gerr != nil {
			return nil, mapPublicationError(method, gerr)
		}
		receipt := publication.Receipt{}
		if c.AcceptReceipt != nil {
			receipt = *c.AcceptReceipt
		}
		return &types.SkillPublicationCandidateAcceptResponse{Candidate: candidateToWire(candidateMeta(c)), Publication: metadataToWire(meta), Receipt: receiptToWire(receipt), ProtocolVersion: types.ProtocolVersion}, nil
	}
	claim := *c.Claim
	skill := markCandidate(c.Skill, c.CandidateID)
	key := publication.CandidateAcceptKey(c.CandidateID)
	var (
		meta    publication.Metadata
		receipt publication.Receipt
		prior   string
	)
	// A publish this candidate already produced (any admin, any session,
	// whatever the publication's current generation) is adopted from the
	// durable marker instead of being published again.
	// The publish runs under a deadline shorter than staleClaimAfter, so a
	// claim old enough to be resolved by another decision can no longer have
	// a publish in flight.
	publishCtx, cancelPublish := context.WithTimeout(ctx, AcceptPublishDeadline)
	defer cancelPublish()
	if found, foundPrior, ok := s.findCandidateRevision(ctx, caller, c.CandidateID, claim); ok {
		meta, prior = found, foundPrior
		receipt = adoptedReceipt(claim, found, prior)
	} else {
		switch claim.Mode {
		case publication.AcceptModeNew:
			skill.Name = claim.Name
			meta, receipt, err = s.store.Publish(publishCtx, caller, publication.PublishRequest{IdempotencyKey: key, Name: claim.Name, Skill: skill, ExpectedAbsent: true})
		case publication.AcceptModeSuccessor:
			current, gerr := s.store.Get(ctx, caller, claim.PublicationID)
			if gerr != nil {
				err = gerr
				break
			}
			skill.Name = current.Name
			prior = claim.ExpectedPublicationContentHash
			meta, receipt, err = s.store.PublishSuccessor(publishCtx, caller, publication.SuccessorRequest{
				IdempotencyKey: key, PublicationID: claim.PublicationID, ExpectedGeneration: claim.ExpectedPublicationGeneration,
				ExpectedContentHash: claim.ExpectedPublicationContentHash, Skill: skill,
			})
			if err == nil && receipt.BeforeHash != "" {
				prior = receipt.BeforeHash
			}
		}
		if err != nil {
			// A concurrent attempt may have published between the check and
			// this publish: re-check the durable marker before deciding.
			if found, foundPrior, ok := s.findCandidateRevision(ctx, caller, c.CandidateID, claim); ok {
				meta, prior, err = found, foundPrior, nil
				receipt = adoptedReceipt(claim, found, prior)
			}
		}
	}
	if err != nil {
		if definitivePublishFailure(err) {
			// Nothing was published under this claim (an applied publish
			// replays or is adopted above), so release it: the candidate can
			// be accepted under another name or target, rejected or withdrawn.
			if rerr := s.candidates.ReleaseClaim(ctx, caller, c.CandidateID, claim.ClaimedAt); rerr != nil {
				return nil, mapPublicationError(method, rerr)
			}
		}
		return nil, mapPublicationError(method, err)
	}
	final, err := s.candidates.FinalizeAccept(ctx, caller, c.CandidateID, publication.AcceptResult{
		PublicationID: meta.PublicationID, RevisionID: meta.RevisionID, PriorContentHash: prior, Receipt: receipt,
	})
	if err != nil {
		return nil, mapPublicationError(method, err)
	}
	return &types.SkillPublicationCandidateAcceptResponse{Candidate: candidateToWire(final), Publication: metadataToWire(meta), Receipt: receiptToWire(receipt), ProtocolVersion: types.ProtocolVersion}, nil
}

// staleClaimAfter is how long a recorded acceptance may stay unfinalized
// before another acceptance, a rejection or a withdrawal may resolve it. An
// in-flight publish finishes well within it.
const staleClaimAfter = StaleClaimAfter

// StaleClaimAfter is how long a recorded acceptance may stay unfinalized
// before another decision may resolve it.
const StaleClaimAfter = 2 * time.Minute

// AcceptPublishDeadline bounds the publish step of an acceptance. It must stay
// shorter than StaleClaimAfter so a stale claim never has a publish in flight.
const AcceptPublishDeadline = 60 * time.Second

// definitivePublishFailure reports whether a publish error proves the
// publication store did not apply the claimed publish.
func definitivePublishFailure(err error) bool {
	return stderrors.Is(err, publication.ErrConflict) || stderrors.Is(err, publication.ErrNotFound) ||
		stderrors.Is(err, publication.ErrRetired) || stderrors.Is(err, publication.ErrInvalidRequest) ||
		stderrors.Is(err, publication.ErrRuntimeMismatch) || stderrors.Is(err, publication.ErrContentHashMismatch) ||
		stderrors.Is(err, publication.ErrIdempotencyConflict)
}

// clearStaleClaim resolves a recorded acceptance before another decision.
// A claim younger than staleClaimAfter may still be publishing: conflict. An
// older claim whose publish applied is finalized (the candidate becomes
// accepted, so the caller's decision then conflicts); one that published
// nothing is released.
func (s *SkillPublicationsSurface) clearStaleClaim(ctx context.Context, caller identity.Quadruple, candidateID string, privileged bool) error {
	c, err := s.candidates.Read(ctx, caller, candidateID, privileged)
	if err != nil {
		return err
	}
	if c.State != publication.CandidatePending || c.Claim == nil {
		return nil
	}
	claim := *c.Claim
	if time.Since(claim.ClaimedAt) < staleClaimAfter {
		return publication.ErrClaimOutstanding
	}
	if privileged {
		if meta, prior, ok := s.findCandidateRevision(ctx, caller, c.CandidateID, claim); ok {
			if _, ferr := s.candidates.FinalizeAccept(ctx, caller, c.CandidateID, publication.AcceptResult{
				PublicationID: meta.PublicationID, RevisionID: meta.RevisionID, PriorContentHash: prior,
				Receipt: publication.Receipt{Operation: "publish", PublicationID: meta.PublicationID, RevisionID: meta.RevisionID, Generation: meta.Generation, State: meta.State, BeforeHash: prior, AfterHash: meta.ContentHash},
			}); ferr != nil {
				return ferr
			}
			return publication.ErrConflict
		}
	} else {
		// The submitter cannot read the catalog to prove the claim published
		// nothing; an admin decision (accept or reject) resolves it.
		return publication.ErrClaimOutstanding
	}
	return s.candidates.ReleaseClaim(ctx, caller, c.CandidateID, claim.ClaimedAt)
}

// findCandidateRevision finds the publication revision this candidate's
// claim produced, from the marker in the revision body. It is independent of
// caller-scoped idempotency receipts and of the publication's current
// generation, so any admin or session recovers an applied acceptance. prior is
// the content hash of the revision it replaced (successor) or empty.
func (s *SkillPublicationsSurface) findCandidateRevision(ctx context.Context, caller identity.Quadruple, candidateID string, claim publication.AcceptClaim) (publication.Metadata, string, bool) {
	var ids []string
	switch claim.Mode {
	case publication.AcceptModeNew:
		items, err := s.store.List(ctx, caller)
		if err != nil {
			return publication.Metadata{}, "", false
		}
		for _, item := range items {
			if item.Name == strings.ToLower(strings.TrimSpace(claim.Name)) {
				ids = append(ids, item.PublicationID)
			}
		}
	case publication.AcceptModeSuccessor:
		ids = []string{claim.PublicationID}
	}
	for _, id := range ids {
		revisions, err := s.store.Revisions(ctx, caller, id)
		if err != nil {
			continue
		}
		for i, rev := range revisions {
			if claim.Mode == publication.AcceptModeNew && i > 0 {
				break
			}
			body, _, err := s.store.Read(ctx, caller, publication.ReadRequest{PublicationID: id, RevisionID: rev.RevisionID, AnyState: true})
			if err != nil {
				continue
			}
			if marker, _ := body.Extra[candidateMarkerKey].(string); marker == candidateID {
				prior := ""
				if i > 0 {
					prior = revisions[i-1].ContentHash
				}
				return rev, prior, true
			}
		}
	}
	return publication.Metadata{}, "", false
}

func adoptedReceipt(claim publication.AcceptClaim, meta publication.Metadata, prior string) publication.Receipt {
	op := "publish"
	if claim.Mode == publication.AcceptModeSuccessor {
		op = "publish_successor"
	}
	return publication.Receipt{Operation: op, PublicationID: meta.PublicationID, RevisionID: meta.RevisionID, Generation: meta.Generation, State: meta.State, BeforeHash: prior, AfterHash: meta.ContentHash}
}

// candidateMarkerKey records, inside the published revision body, which
// candidate produced it. It is runtime-private bookkeeping (underscore
// prefix: never projected on the wire) and is the proof a resumed
// acceptance uses to adopt a publish that already applied.
const candidateMarkerKey = "_publication.candidate_id"

func markCandidate(skill skills.Skill, candidateID string) skills.Skill {
	extra := make(map[string]any, len(skill.Extra)+1)
	for k, v := range skill.Extra {
		extra[k] = v
	}
	extra[candidateMarkerKey] = candidateID
	skill.Extra = extra
	return skill
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
