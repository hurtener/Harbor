package publication

// candidates.go — the reviewed submission path for organization
// publications.
//
// A candidate is an immutable snapshot of one caller's own personal skill,
// frozen at submission for organization-admin review. The body stays inside
// the runtime: projections are content-free apart from short listing text
// (name, title, description), and the frozen body is returned only by
// ReadCandidate. An admin accepts a candidate as a new publication or as a
// successor revision of an existing one (through the same publish mutation
// the publication store applies, so every publication invariant still
// applies), or rejects it; the submitter may withdraw a pending one.
//
// Acceptance is one atomic step (D-512). The candidate and publication
// aggregates are both slots of the tenant's organization identity in one
// StateStore, so Accept loads both, applies the publish or successor mutation
// and the accepted transition, and writes both in one SaveBatchIf. A
// concurrent accept, reject or withdraw that loses the compare-and-set gets an
// ordinary conflict and re-reads; nothing is ever half applied.
//
// The store is backed by Harbor's mandatory StateStore (in-memory, SQLite and
// PostgreSQL drivers) and must share it with the publication store.

import (
	"context"
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
	maxRetainedTerminal   = 1024
	maxCandidateNoteRunes = 2000
	maxCandidateListText  = 1024
	candidateIDPrefix     = "cand"
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
)

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
	// AcceptReceipt is the publication receipt of the accept; its
	// OperationID is the accept's idempotency key. Releases before 1.55 also
	// wrote claim and claim_seq here; decoding ignores them.
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

// AcceptCandidateRequest accepts a pending candidate. Mode new publishes it
// under Name (default: the candidate's name); mode successor appends it to
// PublicationID under the exact expected generation and content hash.
type AcceptCandidateRequest struct {
	IdempotencyKey                 string
	CandidateID                    string
	ExpectedGeneration             uint64
	Mode                           string
	Name                           string
	PublicationID                  string
	ExpectedPublicationGeneration  uint64
	ExpectedPublicationContentHash string
	DecisionNote                   string
}

// AcceptCandidateResult is the accepted candidate, the publication revision
// it produced and the publication receipt.
type AcceptCandidateResult struct {
	Candidate   CandidateMetadata
	Publication Metadata
	Receipt     Receipt
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
	// Accept publishes a pending candidate and marks it accepted in one
	// atomic write. It applies the verified admin gate itself. An exact
	// replay of the idempotency key returns the original result.
	Accept(ctx context.Context, caller identity.Quadruple, req AcceptCandidateRequest) (AcceptCandidateResult, error)
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

// candidateStore keeps one tenant's candidates in a single StateStore slot.
type candidateStore struct {
	mu      sync.Mutex
	state   state.StateStore
	runtime string
	clock   func() time.Time
	closed  bool
}

// NewStateStoreCandidateStore returns a durable CandidateStore over the
// StateStore the publication store uses.
func NewStateStoreCandidateStore(st state.StateStore, runtimeID string) (CandidateStore, error) {
	return NewStateStoreCandidateStoreWithClock(st, runtimeID, time.Now)
}

// NewStateStoreCandidateStoreWithClock is NewStateStoreCandidateStore with an
// injected clock, for deterministic tests.
func NewStateStoreCandidateStoreWithClock(st state.StateStore, runtimeID string, clock func() time.Time) (CandidateStore, error) {
	if st == nil {
		return nil, ErrStoreClosed
	}
	if clock == nil {
		clock = time.Now
	}
	return &candidateStore{state: st, runtime: strings.TrimSpace(runtimeID), clock: clock}, nil
}

// orgSlot is one loaded organization slot and its compare-and-set witness.
type orgSlot struct {
	kind    string
	id      state.EventID // the loaded generation; empty when absent
	present bool
}

func (s *candidateStore) loadSlot(ctx context.Context, tenant, kind string) ([]byte, orgSlot, error) {
	r, err := s.state.Load(ctx, orgIdentity(tenant), kind)
	if errors.Is(err, state.ErrNotFound) {
		return nil, orgSlot{kind: kind}, nil
	}
	if err != nil {
		return nil, orgSlot{}, err
	}
	return r.Bytes, orgSlot{kind: kind, id: r.ID, present: true}, nil
}

func (s *candidateStore) load(ctx context.Context, tenant string) (candidateAggregate, orgSlot, error) {
	raw, sl, err := s.loadSlot(ctx, tenant, candidateKind)
	if err != nil {
		return candidateAggregate{}, orgSlot{}, err
	}
	a, err := decodeCandidates(raw)
	return a, sl, err
}

// saveSlots writes every value to its slot in one conditional batch: each
// slot must still hold the generation it was loaded at (or still be absent).
func (s *candidateStore) saveSlots(ctx context.Context, tenant string, slots []orgSlot, values []any) error {
	expect := make([]state.SlotExpectation, 0, len(slots))
	writes := make([]state.StateRecord, 0, len(slots))
	for i, sl := range slots {
		b, err := encode(values[i])
		if err != nil {
			return err
		}
		e := state.SlotExpectation{Identity: orgIdentity(tenant), Kind: sl.kind}
		if sl.present {
			e.ExpectedEventID = sl.id
		}
		expect = append(expect, e)
		writes = append(writes, state.StateRecord{ID: state.NewEventID(), Identity: e.Identity, Kind: sl.kind, Bytes: b})
	}
	var err error
	if len(writes) == 1 {
		err = s.state.SaveIf(ctx, expect, writes[0])
	} else {
		err = s.state.SaveBatchIf(ctx, expect, writes)
	}
	if errors.Is(err, state.ErrConditionFailed) {
		return ErrConflict
	}
	return err
}

func (s *candidateStore) save(ctx context.Context, tenant string, sl orgSlot, a candidateAggregate) error {
	return s.saveSlots(ctx, tenant, []orgSlot{sl}, []any{a})
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
	a, sl, err := s.load(ctx, caller.TenantID)
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
	if err := s.save(ctx, caller.TenantID, sl, a); err != nil {
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
	a, _, err := s.load(ctx, caller.TenantID)
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
	a, _, err := s.load(ctx, caller.TenantID)
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
	a, sl, err := s.load(ctx, caller.TenantID)
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
	if c.State != CandidatePending || c.Generation != req.ExpectedGeneration {
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
	if err := s.save(ctx, caller.TenantID, sl, a); err != nil {
		return CandidateMetadata{}, err
	}
	return candidateMetadata(c), nil
}

// Accept implements CandidateStore.
func (s *candidateStore) Accept(ctx context.Context, caller identity.Quadruple, req AcceptCandidateRequest) (AcceptCandidateResult, error) {
	if err := verifiedAdmin(ctx, caller); err != nil {
		return AcceptCandidateResult{}, err
	}
	if err := validateKey(req.IdempotencyKey); err != nil {
		return AcceptCandidateResult{}, err
	}
	note, err := validNote(req.DecisionNote)
	if err != nil {
		return AcceptCandidateResult{}, err
	}
	id := strings.TrimSpace(req.CandidateID)
	name, pubID := canonicalName(req.Name), strings.TrimSpace(req.PublicationID)
	switch req.Mode {
	case AcceptModeNew:
		if pubID != "" || req.ExpectedPublicationGeneration != 0 || req.ExpectedPublicationContentHash != "" {
			return AcceptCandidateResult{}, fmt.Errorf("%w: mode new takes no publication target", ErrInvalidRequest)
		}
	case AcceptModeSuccessor:
		if pubID == "" || req.ExpectedPublicationGeneration == 0 || req.ExpectedPublicationContentHash == "" || name != "" {
			return AcceptCandidateResult{}, fmt.Errorf("%w: mode successor requires publication id, generation and content hash", ErrInvalidRequest)
		}
	default:
		return AcceptCandidateResult{}, fmt.Errorf("%w: mode must be %q or %q", ErrInvalidRequest, AcceptModeNew, AcceptModeSuccessor)
	}
	fp := strings.Join([]string{"accept", id, fmt.Sprint(req.ExpectedGeneration), req.Mode, name, pubID,
		fmt.Sprint(req.ExpectedPublicationGeneration), req.ExpectedPublicationContentHash, note}, "\x00")
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.open(ctx, caller); err != nil {
		return AcceptCandidateResult{}, err
	}
	cands, candSlot, err := s.load(ctx, caller.TenantID)
	if err != nil {
		return AcceptCandidateResult{}, err
	}
	rawPubs, pubSlot, err := s.loadSlot(ctx, caller.TenantID, publicationOrgKind)
	if err != nil {
		return AcceptCandidateResult{}, err
	}
	pubs, err := decodeAggregate(state.StateRecord{Bytes: rawPubs})
	if err != nil {
		return AcceptCandidateResult{}, err
	}
	if pubs.Operations == nil {
		pubs.Operations = map[string]operation{}
	}
	i, ok := findCandidate(cands, id)
	if !ok {
		return AcceptCandidateResult{}, ErrCandidateNotFound
	}
	c := cands.Candidates[i]
	opKey := candidateOpKey(caller, req.IdempotencyKey)
	if prior, seen := cands.Operations[opKey]; seen {
		// An exact replay answers the recorded acceptance; it never
		// publishes again.
		if prior.Fingerprint != fp || prior.CandidateID != id || c.State != CandidateAccepted || c.AcceptReceipt == nil {
			return AcceptCandidateResult{}, ErrIdempotencyConflict
		}
		p, ferr := pubs.find(c.PublicationID)
		if ferr != nil {
			return AcceptCandidateResult{}, ferr
		}
		rev, rerr := revisionFor(p, c.RevisionID)
		if rerr != nil {
			return AcceptCandidateResult{}, rerr
		}
		return AcceptCandidateResult{Candidate: candidateMetadata(c), Publication: revisionMetadata(p, rev), Receipt: *c.AcceptReceipt}, nil
	}
	if c.State != CandidatePending || c.Generation != req.ExpectedGeneration {
		return AcceptCandidateResult{}, ErrConflict
	}
	if skills.CanonicalContentHash(c.Skill) != c.ContentHash {
		return AcceptCandidateResult{}, ErrContentHashMismatch
	}
	now := s.clock().UTC()
	skill := cloneSkill(c.Skill)
	var (
		meta    Metadata
		receipt Receipt
	)
	if req.Mode == AcceptModeNew {
		if name == "" {
			name = c.Name
		}
		skill.Name = name
		if skill, err = validateSkill(skill, name); err == nil {
			meta, receipt, err = pubs.publishNew(s.runtime, now, caller.TenantID, name, skill, req.IdempotencyKey)
		}
	} else {
		p, ferr := pubs.find(pubID)
		if ferr != nil {
			return AcceptCandidateResult{}, ferr
		}
		skill.Name = p.Name
		if skill, err = validateSkill(skill, ""); err == nil {
			meta, receipt, err = pubs.publishSuccessor(now, pubID, req.ExpectedPublicationGeneration, req.ExpectedPublicationContentHash, skill, req.IdempotencyKey)
		}
	}
	if err != nil {
		return AcceptCandidateResult{}, err
	}
	c.State = CandidateAccepted
	c.PublicationID, c.RevisionID, c.PriorContentHash = meta.PublicationID, meta.RevisionID, receipt.BeforeHash
	c.DecisionNote, c.DecidedBy = note, caller.UserID
	c.AcceptReceipt = &receipt
	c.Generation++
	c.UpdatedAt = now
	cands.Candidates[i] = c
	cands.Operations[opKey] = candidateOp{Fingerprint: fp, CandidateID: id}
	if err := s.saveSlots(ctx, caller.TenantID, []orgSlot{candSlot, pubSlot}, []any{cands, pubs}); err != nil {
		return AcceptCandidateResult{}, err
	}
	return AcceptCandidateResult{Candidate: candidateMetadata(c), Publication: meta, Receipt: receipt}, nil
}

// Close implements CandidateStore.
func (s *candidateStore) Close(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	return nil
}
