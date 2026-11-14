package publication

// candidates.go — the reviewed submission path for organization
// publications.
//
// A candidate is an immutable snapshot of one caller's own personal skill,
// frozen at submission for organization-admin review. The body stays inside
// the runtime: projections are content-free apart from short listing text
// (name, title, description), and the frozen body is returned only by
// ReadCandidate. An admin accepts a candidate as a new publication or as a
// successor revision of an existing one (through the ordinary publication
// store, so every publication invariant still applies), or rejects it; the
// submitter may withdraw a pending one.
//
// Acceptance spans two aggregates (candidate and publication), so it is a
// claim → publish → finalize sequence. The claim records the exact
// acceptance parameters on the pending candidate; any admin retrying with
// the same parameters resumes the same claim, and a different request (or a
// reject) conflicts while it is outstanding. The publication's own
// name-uniqueness and generation/hash CAS make a resumed publish unable to
// produce a second publication; a resume that finds the claimed publish
// already applied adopts it instead of publishing again.
//
// Two implementations share one aggregate: NewMemoryCandidateStore (tests,
// embedded use) and NewStateStoreCandidateStore over Harbor's mandatory
// StateStore, inheriting the in-memory, SQLite and PostgreSQL driver triad.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
)

// CandidateState is the lifecycle state of a candidate.
type CandidateState string

// Candidate states. Pending is the only non-terminal state.
const (
	CandidatePending   CandidateState = "pending"
	CandidateAccepted  CandidateState = "accepted"
	CandidateRejected  CandidateState = "rejected"
	CandidateWithdrawn CandidateState = "withdrawn"
)

// Accept modes.
const (
	AcceptModeNew       = "new"
	AcceptModeSuccessor = "successor"
)

const (
	candidateKind = "skills.publications.candidates.v1"
	// maxPendingCandidates bounds a tenant's open review queue; only pending
	// candidates count. maxPendingPerUser keeps one person from filling it.
	maxPendingCandidates = 1024
	maxPendingPerUser    = 64
	// maxRetainedTerminal bounds decided candidates kept for history; the
	// oldest are pruned first (publications themselves are unaffected).
	maxRetainedTerminal    = 1024
	maxCandidateNoteRunes  = 2000
	maxCandidateListText   = 1024
	candidateIDPrefix      = "cand"
	candidateAcceptKeyBase = "candidate-accept"
)

// Candidate sentinel errors.
var (
	// ErrCandidateNotFound means the candidate does not exist or is not
	// visible to the caller.
	ErrCandidateNotFound = errors.New("skills/publication: candidate not found")
	// ErrCandidatePendingExists means the submitter already has a pending
	// candidate for the same skill name.
	ErrCandidatePendingExists = errors.New("skills/publication: a pending candidate already exists for this skill")
	// ErrCandidateSupportFilesUnsupported means the skill carries support
	// files, which publications cannot carry.
	ErrCandidateSupportFilesUnsupported = errors.New("skills/publication: skills with support files cannot become publications")
	// ErrClaimOutstanding means another acceptance of the candidate, with a
	// different target, is recorded and not yet finalized or released. It is
	// a conflict (errors.Is(err, ErrConflict) holds).
	ErrClaimOutstanding = fmt.Errorf("%w: another acceptance of this candidate is in progress", ErrConflict)
)

// AcceptClaim is the exact acceptance an admin requested. It is recorded on
// the pending candidate before publishing so a retry resumes the same
// publish and a divergent request conflicts.
type AcceptClaim struct {
	Mode                           string `json:"mode"`
	Name                           string `json:"name,omitempty"`
	PublicationID                  string `json:"publication_id,omitempty"`
	ExpectedPublicationGeneration  uint64 `json:"expected_publication_generation,omitempty"`
	ExpectedPublicationContentHash string `json:"expected_publication_content_hash,omitempty"`
	DecisionNote                   string `json:"decision_note,omitempty"`
	ClaimedBy                      string `json:"claimed_by"`
	// ClaimedAt is the claim's age for the stale window only; it never
	// orders or identifies claims.
	ClaimedAt time.Time `json:"claimed_at,omitempty"`
	// Seq identifies this claim (per-candidate, monotonic): a release names
	// it, so a release never clears a newer claim, and it is the claim's
	// publication fence token.
	Seq uint64 `json:"seq,omitempty"`
}

// SameTarget reports whether two acceptances publish to the same place.
// A retry naming the same target resumes the recorded claim with its
// original compare-and-swap predicates, even if the caller re-read the
// target publication after an earlier attempt already advanced it.
func (a AcceptClaim) SameTarget(b AcceptClaim) bool {
	return a.Mode == b.Mode && a.Name == b.Name && a.PublicationID == b.PublicationID
}

// Candidate is the durable candidate record. Skill is the frozen body and is
// never part of CandidateMetadata.
type Candidate struct {
	CandidateID         string         `json:"candidate_id"`
	Name                string         `json:"name"`
	Title               string         `json:"title,omitempty"`
	Description         string         `json:"description,omitempty"`
	SubmitterUser       string         `json:"submitter_user"`
	AgentID             string         `json:"agent_id"`
	PackageHash         string         `json:"package_hash,omitempty"`
	ContentHash         string         `json:"content_hash"`
	TargetPublicationID string         `json:"target_publication_id,omitempty"`
	Note                string         `json:"note,omitempty"`
	State               CandidateState `json:"state"`
	DecisionNote        string         `json:"decision_note,omitempty"`
	DecidedBy           string         `json:"decided_by,omitempty"`
	PublicationID       string         `json:"publication_id,omitempty"`
	RevisionID          string         `json:"revision_id,omitempty"`
	PriorContentHash    string         `json:"prior_content_hash,omitempty"`
	Generation          uint64         `json:"generation"`
	CreatedAt           time.Time      `json:"created_at"`
	UpdatedAt           time.Time      `json:"updated_at"`
	RuntimeID           string         `json:"runtime_id"`
	Skill               skills.Skill   `json:"skill"`
	Claim               *AcceptClaim   `json:"claim,omitempty"`
	// ClaimSeq numbers this candidate's claims monotonically.
	ClaimSeq      uint64   `json:"claim_seq,omitempty"`
	AcceptReceipt *Receipt `json:"accept_receipt,omitempty"`
}

// CandidateMetadata is the content-free candidate projection.
type CandidateMetadata struct {
	CandidateID         string
	Name                string
	Title               string
	Description         string
	SubmitterUser       string
	AgentID             string
	PackageHash         string
	ContentHash         string
	TargetPublicationID string
	Note                string
	State               CandidateState
	DecisionNote        string
	DecidedBy           string
	PublicationID       string
	RevisionID          string
	PriorContentHash    string
	Generation          uint64
	CreatedAt           time.Time
	UpdatedAt           time.Time
	RuntimeID           string
}

func candidateMetadata(c Candidate) CandidateMetadata {
	return CandidateMetadata{
		CandidateID: c.CandidateID, Name: c.Name, Title: c.Title, Description: c.Description,
		SubmitterUser: c.SubmitterUser, AgentID: c.AgentID, PackageHash: c.PackageHash, ContentHash: c.ContentHash,
		TargetPublicationID: c.TargetPublicationID, Note: c.Note, State: c.State, DecisionNote: c.DecisionNote,
		DecidedBy: c.DecidedBy, PublicationID: c.PublicationID, RevisionID: c.RevisionID, PriorContentHash: c.PriorContentHash,
		Generation: c.Generation, CreatedAt: c.CreatedAt, UpdatedAt: c.UpdatedAt, RuntimeID: c.RuntimeID,
	}
}

func cloneCandidate(c Candidate) Candidate {
	c.Skill = cloneSkill(c.Skill)
	if c.Claim != nil {
		claim := *c.Claim
		c.Claim = &claim
	}
	if c.AcceptReceipt != nil {
		receipt := *c.AcceptReceipt
		c.AcceptReceipt = &receipt
	}
	return c
}

// SubmitCandidateRequest freezes one snapshot. The caller (the protocol
// surface) supplies the snapshot it read under the verified identity and
// signed Agent reach; the store never trusts a caller-named owner.
type SubmitCandidateRequest struct {
	IdempotencyKey      string
	AgentID             string
	Skill               skills.Skill
	PackageHash         string
	TargetPublicationID string
	Note                string
}

// ListCandidatesRequest filters a listing. All=false restricts the listing to
// the caller's own submissions; All=true is admin-only at the edge.
type ListCandidatesRequest struct {
	State string
	All   bool
}

// DecideCandidateRequest is a terminal reject/withdraw transition.
type DecideCandidateRequest struct {
	IdempotencyKey     string
	CandidateID        string
	ExpectedGeneration uint64
	Outcome            CandidateState
	DecisionNote       string
}

// AcceptResult is what the publication store produced for a claim.
type AcceptResult struct {
	PublicationID    string
	RevisionID       string
	PriorContentHash string
	Receipt          Receipt
}

// CandidateStore persists candidates. Visibility (admin vs submitter) is a
// parameter decided at the authorization edge; tenant isolation is
// structural (one aggregate per tenant).
type CandidateStore interface {
	Submit(ctx context.Context, caller identity.Quadruple, req SubmitCandidateRequest) (CandidateMetadata, error)
	List(ctx context.Context, caller identity.Quadruple, req ListCandidatesRequest) ([]CandidateMetadata, error)
	// Read returns a candidate and its frozen body. privileged=false
	// restricts it to the caller's own candidates.
	Read(ctx context.Context, caller identity.Quadruple, candidateID string, privileged bool) (Candidate, error)
	// Claim records an acceptance on a pending candidate (or returns the
	// identical outstanding claim for a resume, or the finalized candidate
	// for an identical replay).
	Claim(ctx context.Context, caller identity.Quadruple, candidateID string, expectedGeneration uint64, claim AcceptClaim) (Candidate, error)
	// ReleaseClaim clears the claim numbered claimSeq from a pending
	// candidate after a publish that definitively did not apply, so the
	// candidate can be accepted differently, rejected or withdrawn. It is a
	// no-op when that claim is no longer the recorded one.
	ReleaseClaim(ctx context.Context, caller identity.Quadruple, candidateID string, claimSeq uint64) error
	// FinalizeAccept marks a claimed candidate accepted with the result.
	FinalizeAccept(ctx context.Context, caller identity.Quadruple, candidateID string, result AcceptResult) (CandidateMetadata, error)
	// Decide applies a reject (privileged) or withdraw (submitter) transition.
	Decide(ctx context.Context, caller identity.Quadruple, req DecideCandidateRequest, privileged bool) (CandidateMetadata, error)
	Close(ctx context.Context) error
}

type candidateOp struct {
	Fingerprint string `json:"fingerprint"`
	CandidateID string `json:"candidate_id"`
}

type candidateAggregate struct {
	Candidates []Candidate            `json:"candidates"`
	Operations map[string]candidateOp `json:"operations,omitempty"`
	// Seq numbers candidates monotonically, so pruning never reuses an id.
	Seq uint64 `json:"seq,omitempty"`
}

// pruneTerminal keeps at most maxRetainedTerminal decided candidates,
// dropping the least recently updated and their idempotency records.
func pruneTerminal(a *candidateAggregate) {
	terminal := 0
	for _, c := range a.Candidates {
		if c.State != CandidatePending {
			terminal++
		}
	}
	excess := terminal - maxRetainedTerminal
	if excess <= 0 {
		return
	}
	order := make([]int, 0, terminal)
	for i, c := range a.Candidates {
		if c.State != CandidatePending {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(x, y int) bool {
		return a.Candidates[order[x]].UpdatedAt.Before(a.Candidates[order[y]].UpdatedAt)
	})
	drop := make(map[string]bool, excess)
	for _, i := range order[:excess] {
		drop[a.Candidates[i].CandidateID] = true
	}
	kept := a.Candidates[:0]
	for _, c := range a.Candidates {
		if !drop[c.CandidateID] {
			kept = append(kept, c)
		}
	}
	a.Candidates = kept
	for key, op := range a.Operations {
		if drop[op.CandidateID] {
			delete(a.Operations, key)
		}
	}
}

// candidateBackend loads and saves one tenant's aggregate. token is an
// opaque CAS witness for save.
type candidateBackend interface {
	load(ctx context.Context, tenant string) (candidateAggregate, any, error)
	save(ctx context.Context, tenant string, token any, a candidateAggregate) error
}

type candidateStore struct {
	mu      sync.Mutex
	backend candidateBackend
	runtime string
	clock   func() time.Time
	closed  bool
}

// NewMemoryCandidateStore returns an in-memory CandidateStore bound to one
// runtime/deployment.
func NewMemoryCandidateStore(runtimeID string) CandidateStore {
	return NewMemoryCandidateStoreWithClock(runtimeID, time.Now)
}

// NewMemoryCandidateStoreWithClock is NewMemoryCandidateStore with an
// injected clock (claims are timestamped with it), for deterministic tests.
func NewMemoryCandidateStoreWithClock(runtimeID string, clock func() time.Time) CandidateStore {
	if clock == nil {
		clock = time.Now
	}
	return &candidateStore{backend: &memoryCandidateBackend{tenants: map[string][]byte{}}, runtime: strings.TrimSpace(runtimeID), clock: clock}
}

// NewStateStoreCandidateStore returns a durable CandidateStore over the
// shared StateStore.
func NewStateStoreCandidateStore(st state.StateStore, runtimeID string) (CandidateStore, error) {
	return NewStateStoreCandidateStoreWithClock(st, runtimeID, time.Now)
}

// NewStateStoreCandidateStoreWithClock is NewStateStoreCandidateStore with an
// injected clock (claim age only), for deterministic tests.
func NewStateStoreCandidateStoreWithClock(st state.StateStore, runtimeID string, clock func() time.Time) (CandidateStore, error) {
	if st == nil {
		return nil, ErrStoreClosed
	}
	if clock == nil {
		clock = time.Now
	}
	return &candidateStore{backend: &stateCandidateBackend{state: st}, runtime: strings.TrimSpace(runtimeID), clock: clock}, nil
}

type memoryCandidateBackend struct {
	tenants map[string][]byte
}

func (m *memoryCandidateBackend) load(_ context.Context, tenant string) (candidateAggregate, any, error) {
	raw, ok := m.tenants[tenant]
	if !ok {
		return candidateAggregate{Operations: map[string]candidateOp{}}, nil, nil
	}
	a, err := decodeCandidates(raw)
	return a, nil, err
}

func (m *memoryCandidateBackend) save(_ context.Context, tenant string, _ any, a candidateAggregate) error {
	b, err := encode(a)
	if err != nil {
		return err
	}
	m.tenants[tenant] = b
	return nil
}

type stateCandidateBackend struct {
	state state.StateStore
}

type stateToken struct {
	record  state.StateRecord
	present bool
}

func (s *stateCandidateBackend) load(ctx context.Context, tenant string) (candidateAggregate, any, error) {
	r, err := s.state.Load(ctx, orgIdentity(tenant), candidateKind)
	if errors.Is(err, state.ErrNotFound) {
		return candidateAggregate{Operations: map[string]candidateOp{}}, stateToken{}, nil
	}
	if err != nil {
		return candidateAggregate{}, nil, err
	}
	a, err := decodeCandidates(r.Bytes)
	return a, stateToken{record: r, present: true}, err
}

func (s *stateCandidateBackend) save(ctx context.Context, tenant string, token any, a candidateAggregate) error {
	b, err := encode(a)
	if err != nil {
		return err
	}
	tok, isToken := token.(stateToken)
	next := state.StateRecord{ID: state.NewEventID(), Identity: orgIdentity(tenant), Kind: candidateKind, Bytes: b}
	expect := state.SlotExpectation{Identity: next.Identity, Kind: next.Kind}
	if isToken && tok.present {
		expect.ExpectedEventID = tok.record.ID
	}
	if err := s.state.SaveIf(ctx, []state.SlotExpectation{expect}, next); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return ErrConflict
		}
		return err
	}
	return nil
}

func decodeCandidates(raw []byte) (candidateAggregate, error) {
	var a candidateAggregate
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &a); err != nil {
			return candidateAggregate{}, fmt.Errorf("%w: candidate aggregate: %w", ErrInvalidRequest, err)
		}
	}
	if a.Operations == nil {
		a.Operations = map[string]candidateOp{}
	}
	return a, nil
}

func (s *candidateStore) open(ctx context.Context, caller identity.Quadruple) error {
	if err := validateCaller(caller); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed {
		return ErrStoreClosed
	}
	return nil
}

func validNote(note string) (string, error) {
	note = strings.TrimSpace(note)
	if !utf8.ValidString(note) || utf8.RuneCountInString(note) > maxCandidateNoteRunes {
		return "", fmt.Errorf("%w: note must be valid UTF-8 of at most %d characters", ErrInvalidRequest, maxCandidateNoteRunes)
	}
	return note, nil
}

func clipListText(v string) string {
	v = strings.TrimSpace(v)
	if utf8.RuneCountInString(v) <= maxCandidateListText {
		return v
	}
	return string([]rune(v)[:maxCandidateListText])
}

func candidateOpKey(caller identity.Quadruple, key string) string {
	return caller.UserID + "\x00" + strings.TrimSpace(key)
}

func findCandidate(a candidateAggregate, id string) (int, bool) {
	for i, c := range a.Candidates {
		if c.CandidateID == id {
			return i, true
		}
	}
	return -1, false
}

// Submit implements CandidateStore.
func (s *candidateStore) Submit(ctx context.Context, caller identity.Quadruple, req SubmitCandidateRequest) (CandidateMetadata, error) {
	if err := validateKey(req.IdempotencyKey); err != nil {
		return CandidateMetadata{}, err
	}
	agentID := strings.TrimSpace(req.AgentID)
	if agentID == "" {
		return CandidateMetadata{}, fmt.Errorf("%w: agent id is required", ErrInvalidRequest)
	}
	note, err := validNote(req.Note)
	if err != nil {
		return CandidateMetadata{}, err
	}
	skill, err := validateSkill(req.Skill, "")
	if err != nil {
		return CandidateMetadata{}, err
	}
	target := strings.TrimSpace(req.TargetPublicationID)
	fp := strings.Join([]string{"submit", agentID, skill.Name, skill.ContentHash, req.PackageHash, target, note}, "\x00")
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.open(ctx, caller); err != nil {
		return CandidateMetadata{}, err
	}
	a, token, err := s.backend.load(ctx, caller.TenantID)
	if err != nil {
		return CandidateMetadata{}, err
	}
	opKey := candidateOpKey(caller, req.IdempotencyKey)
	if prior, ok := a.Operations[opKey]; ok {
		if prior.Fingerprint != fp {
			return CandidateMetadata{}, ErrIdempotencyConflict
		}
		if i, found := findCandidate(a, prior.CandidateID); found {
			return candidateMetadata(a.Candidates[i]), nil
		}
		return CandidateMetadata{}, ErrCandidateNotFound
	}
	for _, c := range a.Candidates {
		if c.State == CandidatePending && c.SubmitterUser == caller.UserID && c.Name == skill.Name {
			return CandidateMetadata{}, ErrCandidatePendingExists
		}
	}
	pending, mine := 0, 0
	for _, c := range a.Candidates {
		if c.State == CandidatePending {
			pending++
			if c.SubmitterUser == caller.UserID {
				mine++
			}
		}
	}
	if pending >= maxPendingCandidates || mine >= maxPendingPerUser {
		return CandidateMetadata{}, fmt.Errorf("%w: pending candidate limit reached", ErrConflict)
	}
	pruneTerminal(&a)
	if a.Seq < uint64(len(a.Candidates)) {
		a.Seq = uint64(len(a.Candidates))
	}
	a.Seq++
	now := s.clock().UTC()
	c := Candidate{
		CandidateID: newOpaqueID(candidateIDPrefix, caller.TenantID+"\x00"+caller.UserID+"\x00"+skill.Name, a.Seq),
		Name:        skill.Name, Title: clipListText(skill.Title), Description: clipListText(skill.Description),
		SubmitterUser: caller.UserID, AgentID: agentID, PackageHash: strings.TrimSpace(req.PackageHash),
		ContentHash: skill.ContentHash, TargetPublicationID: target, Note: note, State: CandidatePending,
		Generation: 1, CreatedAt: now, UpdatedAt: now, RuntimeID: s.runtime, Skill: cloneSkill(skill),
	}
	a.Candidates = append(a.Candidates, c)
	a.Operations[opKey] = candidateOp{Fingerprint: fp, CandidateID: c.CandidateID}
	if err := s.backend.save(ctx, caller.TenantID, token, a); err != nil {
		return CandidateMetadata{}, err
	}
	return candidateMetadata(c), nil
}

// List implements CandidateStore.
func (s *candidateStore) List(ctx context.Context, caller identity.Quadruple, req ListCandidatesRequest) ([]CandidateMetadata, error) {
	stateFilter := CandidateState(strings.TrimSpace(req.State))
	switch stateFilter {
	case "", CandidatePending, CandidateAccepted, CandidateRejected, CandidateWithdrawn:
	default:
		return nil, fmt.Errorf("%w: unknown candidate state %q", ErrInvalidRequest, req.State)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.open(ctx, caller); err != nil {
		return nil, err
	}
	a, _, err := s.backend.load(ctx, caller.TenantID)
	if err != nil {
		return nil, err
	}
	out := make([]CandidateMetadata, 0, len(a.Candidates))
	for _, c := range a.Candidates {
		if !req.All && c.SubmitterUser != caller.UserID {
			continue
		}
		if stateFilter != "" && c.State != stateFilter {
			continue
		}
		out = append(out, candidateMetadata(c))
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].CandidateID > out[j].CandidateID
	})
	return out, nil
}

// Read implements CandidateStore.
func (s *candidateStore) Read(ctx context.Context, caller identity.Quadruple, candidateID string, privileged bool) (Candidate, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.open(ctx, caller); err != nil {
		return Candidate{}, err
	}
	a, _, err := s.backend.load(ctx, caller.TenantID)
	if err != nil {
		return Candidate{}, err
	}
	i, ok := findCandidate(a, strings.TrimSpace(candidateID))
	if !ok || (!privileged && a.Candidates[i].SubmitterUser != caller.UserID) {
		return Candidate{}, ErrCandidateNotFound
	}
	if skills.CanonicalContentHash(a.Candidates[i].Skill) != a.Candidates[i].ContentHash {
		return Candidate{}, ErrContentHashMismatch
	}
	return cloneCandidate(a.Candidates[i]), nil
}

// Claim implements CandidateStore.
func (s *candidateStore) Claim(ctx context.Context, caller identity.Quadruple, candidateID string, expectedGeneration uint64, claim AcceptClaim) (Candidate, error) {
	note, err := validNote(claim.DecisionNote)
	if err != nil {
		return Candidate{}, err
	}
	claim.DecisionNote = note
	claim.Name = canonicalName(claim.Name)
	claim.PublicationID = strings.TrimSpace(claim.PublicationID)
	claim.ClaimedBy = caller.UserID
	claim.ClaimedAt = s.clock().UTC()
	switch claim.Mode {
	case AcceptModeNew:
		if claim.PublicationID != "" || claim.ExpectedPublicationGeneration != 0 || claim.ExpectedPublicationContentHash != "" {
			return Candidate{}, fmt.Errorf("%w: mode new takes no publication target", ErrInvalidRequest)
		}
	case AcceptModeSuccessor:
		if claim.PublicationID == "" || claim.ExpectedPublicationGeneration == 0 || claim.ExpectedPublicationContentHash == "" || claim.Name != "" {
			return Candidate{}, fmt.Errorf("%w: mode successor requires publication id, generation and content hash", ErrInvalidRequest)
		}
	default:
		return Candidate{}, fmt.Errorf("%w: mode must be %q or %q", ErrInvalidRequest, AcceptModeNew, AcceptModeSuccessor)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.open(ctx, caller); err != nil {
		return Candidate{}, err
	}
	a, token, err := s.backend.load(ctx, caller.TenantID)
	if err != nil {
		return Candidate{}, err
	}
	i, ok := findCandidate(a, strings.TrimSpace(candidateID))
	if !ok {
		return Candidate{}, ErrCandidateNotFound
	}
	c := a.Candidates[i]
	if claim.Mode == AcceptModeNew && claim.Name == "" {
		claim.Name = c.Name
	}
	if c.Claim != nil && c.Claim.SameTarget(claim) && ((c.State == CandidatePending && c.Generation == expectedGeneration) || (c.State == CandidateAccepted && c.Generation == expectedGeneration+1)) {
		// Resume an outstanding claim with its original predicates, or
		// replay a finalized acceptance. A resumed pending claim is in flight
		// again: refresh its age (never its sequence) so it does not look
		// stale to a concurrent decision.
		if c.State == CandidatePending {
			resumed := *c.Claim
			resumed.ClaimedAt = claim.ClaimedAt
			c.Claim = &resumed
			a.Candidates[i] = c
			if err := s.backend.save(ctx, caller.TenantID, token, a); err != nil {
				return Candidate{}, err
			}
		}
		return cloneCandidate(c), nil
	}
	if c.State == CandidatePending && c.Generation == expectedGeneration && c.Claim != nil {
		return Candidate{}, ErrClaimOutstanding
	}
	if c.State != CandidatePending || c.Generation != expectedGeneration {
		return Candidate{}, ErrConflict
	}
	c.ClaimSeq++
	claim.Seq = c.ClaimSeq
	c.Claim = &claim
	a.Candidates[i] = c
	if err := s.backend.save(ctx, caller.TenantID, token, a); err != nil {
		return Candidate{}, err
	}
	return cloneCandidate(c), nil
}

// ReleaseClaim implements CandidateStore.
func (s *candidateStore) ReleaseClaim(ctx context.Context, caller identity.Quadruple, candidateID string, claimSeq uint64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.open(ctx, caller); err != nil {
		return err
	}
	a, token, err := s.backend.load(ctx, caller.TenantID)
	if err != nil {
		return err
	}
	i, ok := findCandidate(a, strings.TrimSpace(candidateID))
	if !ok {
		return ErrCandidateNotFound
	}
	c := a.Candidates[i]
	if c.State != CandidatePending || c.Claim == nil || c.Claim.Seq != claimSeq {
		// Already released, superseded or finalized: nothing to release.
		return nil
	}
	c.Claim = nil
	c.UpdatedAt = s.clock().UTC()
	a.Candidates[i] = c
	return s.backend.save(ctx, caller.TenantID, token, a)
}

// FinalizeAccept implements CandidateStore.
func (s *candidateStore) FinalizeAccept(ctx context.Context, caller identity.Quadruple, candidateID string, result AcceptResult) (CandidateMetadata, error) {
	if strings.TrimSpace(result.PublicationID) == "" || strings.TrimSpace(result.RevisionID) == "" {
		return CandidateMetadata{}, fmt.Errorf("%w: accept result is incomplete", ErrInvalidRequest)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.open(ctx, caller); err != nil {
		return CandidateMetadata{}, err
	}
	a, token, err := s.backend.load(ctx, caller.TenantID)
	if err != nil {
		return CandidateMetadata{}, err
	}
	i, ok := findCandidate(a, strings.TrimSpace(candidateID))
	if !ok {
		return CandidateMetadata{}, ErrCandidateNotFound
	}
	c := a.Candidates[i]
	if c.State == CandidateAccepted && c.PublicationID == result.PublicationID && c.RevisionID == result.RevisionID {
		return candidateMetadata(c), nil
	}
	if c.State != CandidatePending || c.Claim == nil {
		return CandidateMetadata{}, ErrConflict
	}
	receipt := result.Receipt
	c.State = CandidateAccepted
	c.PublicationID = result.PublicationID
	c.RevisionID = result.RevisionID
	c.PriorContentHash = result.PriorContentHash
	c.DecisionNote = c.Claim.DecisionNote
	c.DecidedBy = caller.UserID
	c.AcceptReceipt = &receipt
	c.Generation++
	c.UpdatedAt = s.clock().UTC()
	a.Candidates[i] = c
	if err := s.backend.save(ctx, caller.TenantID, token, a); err != nil {
		return CandidateMetadata{}, err
	}
	return candidateMetadata(c), nil
}

// Decide implements CandidateStore.
func (s *candidateStore) Decide(ctx context.Context, caller identity.Quadruple, req DecideCandidateRequest, privileged bool) (CandidateMetadata, error) {
	if err := validateKey(req.IdempotencyKey); err != nil {
		return CandidateMetadata{}, err
	}
	note, err := validNote(req.DecisionNote)
	if err != nil {
		return CandidateMetadata{}, err
	}
	switch req.Outcome {
	case CandidateRejected:
		if !privileged {
			return CandidateMetadata{}, fmt.Errorf("%w: reject requires the admin path", ErrInvalidRequest)
		}
	case CandidateWithdrawn:
		if note != "" {
			return CandidateMetadata{}, fmt.Errorf("%w: withdraw takes no decision note", ErrInvalidRequest)
		}
	default:
		return CandidateMetadata{}, fmt.Errorf("%w: unsupported outcome %q", ErrInvalidRequest, req.Outcome)
	}
	id := strings.TrimSpace(req.CandidateID)
	fp := strings.Join([]string{"decide", id, string(req.Outcome), fmt.Sprint(req.ExpectedGeneration), note}, "\x00")
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.open(ctx, caller); err != nil {
		return CandidateMetadata{}, err
	}
	a, token, err := s.backend.load(ctx, caller.TenantID)
	if err != nil {
		return CandidateMetadata{}, err
	}
	i, ok := findCandidate(a, id)
	if !ok || (req.Outcome == CandidateWithdrawn && a.Candidates[i].SubmitterUser != caller.UserID) {
		return CandidateMetadata{}, ErrCandidateNotFound
	}
	opKey := candidateOpKey(caller, req.IdempotencyKey)
	if prior, seen := a.Operations[opKey]; seen {
		if prior.Fingerprint != fp || prior.CandidateID != id {
			return CandidateMetadata{}, ErrIdempotencyConflict
		}
		return candidateMetadata(a.Candidates[i]), nil
	}
	c := a.Candidates[i]
	if c.State != CandidatePending || c.Generation != req.ExpectedGeneration || c.Claim != nil {
		return CandidateMetadata{}, ErrConflict
	}
	c.State = req.Outcome
	c.DecisionNote = note
	if req.Outcome == CandidateRejected {
		c.DecidedBy = caller.UserID
	}
	c.Generation++
	c.UpdatedAt = s.clock().UTC()
	a.Candidates[i] = c
	a.Operations[opKey] = candidateOp{Fingerprint: fp, CandidateID: id}
	if err := s.backend.save(ctx, caller.TenantID, token, a); err != nil {
		return CandidateMetadata{}, err
	}
	return candidateMetadata(c), nil
}

// Close implements CandidateStore.
func (s *candidateStore) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}

// CandidateAcceptKey derives the publication idempotency key for one
// candidate acceptance. It is a pure function of the candidate so every
// resume of the same claim names the same publication operation.
func CandidateAcceptKey(candidateID string) string {
	sum := sha256.Sum256([]byte(candidateAcceptKeyBase + "\x00" + candidateID))
	return candidateAcceptKeyBase + "-" + hex.EncodeToString(sum[:16])
}
