package protocol

import (
	"context"
	stderrors "errors"
	"fmt"
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
	var (
		c     publication.Candidate
		claim publication.AcceptClaim
	)
	// At most two rounds: a claim whose fence was revoked but which was
	// never released (a crash or CAS conflict between resolve and release)
	// is released and replaced by a fresh claim once.
	for round := 0; ; round++ {
		var err error
		c, err = s.candidates.Claim(ctx, caller, r.CandidateID, r.ExpectedGeneration, want)
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
		claim = *c.Claim
		// Fence the publish with this claim: its token is the claim's
		// sequence number. The publication store commits the publish only
		// while that token is the live fence, in the same compare-and-set as
		// the publish, and returns the revision the candidate already produced
		// (any admin, any session, any generation) instead of writing again.
		err = s.store.FenceCandidate(ctx, caller, c.CandidateID, claim.Seq)
		if stderrors.Is(err, publication.ErrCandidateFenced) && round == 0 {
			// This claim was resolved (its fence revoked) but is still
			// recorded: it can never publish. Release it — idempotently, by
			// its sequence number — and take a fresh claim.
			if rerr := s.releaseClaim(ctx, caller, c.CandidateID, claim.Seq); rerr != nil {
				return nil, mapPublicationError(method, rerr)
			}
			continue
		}
		if err != nil {
			if stderrors.Is(err, publication.ErrCandidateFenced) {
				return nil, mapPublicationError(method, err)
			}
			if stderrors.Is(err, publication.ErrConflict) {
				return nil, mapPublicationError(method, publication.ErrClaimOutstanding)
			}
			return nil, mapPublicationError(method, err)
		}
		break
	}
	// The fence must belong to the claim that is still recorded: if a
	// rejection, withdrawal or another acceptance resolved this claim while it
	// paused, revoke this claim's fence (and only it) and stop.
	if current, rerr := s.candidates.Read(ctx, caller, c.CandidateID, true); rerr != nil {
		return nil, mapPublicationError(method, rerr)
	} else if current.State != publication.CandidatePending || current.Claim == nil || current.Claim.Seq != claim.Seq {
		if _, rerr := s.store.ResolveCandidate(ctx, caller, c.CandidateID, claim.Seq); rerr != nil && !stderrors.Is(rerr, publication.ErrConflict) {
			return nil, mapPublicationError(method, rerr)
		}
		return nil, mapPublicationError(method, publication.ErrCandidateFenced)
	}
	token := claim.Seq
	var err error
	skill := markCandidate(c.Skill, c.CandidateID)
	key := publication.CandidateAcceptKey(c.CandidateID)
	fence := &publication.CandidateFence{CandidateID: c.CandidateID, Token: token}
	var (
		meta    publication.Metadata
		receipt publication.Receipt
		prior   string
	)
	// The publish runs under a deadline shorter than StaleClaimAfter; the
	// fence, not the deadline, is what guarantees a resolved claim's publish
	// never lands.
	publishCtx, cancelPublish := context.WithTimeout(ctx, AcceptPublishDeadline)
	defer cancelPublish()
	switch claim.Mode {
	case publication.AcceptModeNew:
		skill.Name = claim.Name
		meta, receipt, err = s.store.Publish(publishCtx, caller, publication.PublishRequest{IdempotencyKey: key, Name: claim.Name, Skill: skill, ExpectedAbsent: true, Candidate: fence})
	case publication.AcceptModeSuccessor:
		current, gerr := s.store.Get(ctx, caller, claim.PublicationID)
		if gerr != nil {
			err = gerr
			break
		}
		skill.Name = current.Name
		meta, receipt, err = s.store.PublishSuccessor(publishCtx, caller, publication.SuccessorRequest{
			IdempotencyKey: key, PublicationID: claim.PublicationID, ExpectedGeneration: claim.ExpectedPublicationGeneration,
			ExpectedContentHash: claim.ExpectedPublicationContentHash, Skill: skill, Candidate: fence,
		})
	}
	if err == nil {
		prior = receipt.BeforeHash
	}
	if err != nil {
		if definitivePublishFailure(err) {
			// Release only once the store has PROVEN nothing applied (and
			// revoked this claim's fence); otherwise keep the claim.
			res, rerr := s.store.ResolveCandidate(ctx, caller, c.CandidateID, claim.Seq)
			if rerr != nil {
				return nil, mapPublicationError(method, err)
			}
			if res.Applied {
				meta, prior, receipt, err = res.Publication, res.PriorContentHash, res.Receipt, nil
			} else if rerr := s.releaseClaim(ctx, caller, c.CandidateID, claim.Seq); rerr != nil {
				return nil, mapPublicationError(method, rerr)
			}
		}
		if err != nil {
			return nil, mapPublicationError(method, err)
		}
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

// errStaleResolutionUnknown means a stale acceptance could not be proven
// applied or not applied (for example a catalog read failed). The claim is
// kept and the caller may retry.
var errStaleResolutionUnknown = stderrors.New("protocol: stale acceptance could not be resolved; retry")

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
	if !privileged {
		// The submitter cannot resolve the catalog side; an admin decision
		// (accept or reject) resolves it.
		return publication.ErrClaimOutstanding
	}
	// Atomically: the revision the claim produced, or a revoked fence so its
	// publish can never land. Any error is "unknown": keep the claim.
	// Resolve exactly the claim read above: if a fresher acceptance fenced
	// meanwhile, this is a conflict and nothing is revoked or released.
	res, err := s.store.ResolveCandidate(ctx, caller, c.CandidateID, claim.Seq)
	if err != nil {
		return fmt.Errorf("%w: resolving a stale acceptance: %w", errStaleResolutionUnknown, err)
	}
	if res.Applied {
		if _, ferr := s.candidates.FinalizeAccept(ctx, caller, c.CandidateID, publication.AcceptResult{
			PublicationID: res.Publication.PublicationID, RevisionID: res.Publication.RevisionID, PriorContentHash: res.PriorContentHash, Receipt: res.Receipt,
		}); ferr != nil {
			return ferr
		}
		return publication.ErrConflict
	}
	return s.releaseClaim(ctx, caller, c.CandidateID, claim.Seq)
}

// releaseClaim releases the claim numbered seq after its fence was resolved.
// It is idempotent (a release of a claim that is no longer recorded is a
// no-op) and retried a few times on transient store conflicts.
func (s *SkillPublicationsSurface) releaseClaim(ctx context.Context, caller identity.Quadruple, candidateID string, seq uint64) error {
	var err error
	for range 3 {
		if err = s.candidates.ReleaseClaim(ctx, caller, candidateID, seq); err == nil || ctx.Err() != nil {
			return err
		}
	}
	return err
}

func markCandidate(skill skills.Skill, candidateID string) skills.Skill {
	extra := make(map[string]any, len(skill.Extra)+1)
	for k, v := range skill.Extra {
		extra[k] = v
	}
	extra[publication.CandidateMarkerKey] = candidateID
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
