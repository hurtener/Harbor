package publication

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/state"
)

// CandidateMarkerKey records, inside a published revision body, which
// candidate produced it. Runtime-private bookkeeping (underscore prefix: never
// projected on the wire); the store uses it to make a candidate's publish
// idempotent across callers and to prove whether an acceptance applied.
const CandidateMarkerKey = "_publication.candidate_id"

// checkReservedExtra refuses a body that plants the candidate marker: an
// ordinary publish may not carry it, and a candidate publish carries exactly
// its own. Only CandidateMarkerKey is reserved; any other underscore key
// (including ones earlier releases accepted from callers) passes unchanged.
func checkReservedExtra(skill skills.Skill, f *CandidateFence) error {
	value, ok := skill.Extra[CandidateMarkerKey]
	if !ok {
		return nil
	}
	if f == nil {
		return fmt.Errorf("%w: extra key %q is reserved", ErrInvalidRequest, CandidateMarkerKey)
	}
	if marker, _ := value.(string); marker != f.CandidateID {
		return fmt.Errorf("%w: candidate marker does not name this candidate", ErrInvalidRequest)
	}
	return nil
}

// ErrCandidateFenced means the acceptance this publish belonged to was
// revoked (its claim was resolved as stale): the publish never lands. It is a
// conflict (errors.Is(err, ErrConflict) holds).
var ErrCandidateFenced = fmt.Errorf("%w: candidate acceptance was revoked", ErrConflict)

// CandidateFence names the candidate acceptance a publish belongs to. The
// publication store commits such a publish only while Token is the
// candidate's live fence, in the same compare-and-set as the publish.
type CandidateFence struct {
	CandidateID string
	Token       uint64
}

// fenceState is one candidate's fence in the publication aggregate. The
// token of an acceptance is its claim's sequence number (per candidate,
// monotonic, allocated under the candidate CAS), so the caller always knows
// exactly which token it holds. A fence never moves backwards: a token at or
// below Revoked, or below Live, can never be installed; installing a newer
// token revokes the older live one in the same write. Wall clocks never
// order tokens.
type fenceState struct {
	Live    uint64 `json:"live,omitempty"`
	Revoked uint64 `json:"revoked,omitempty"`
}

// CandidateResolution is the atomic outcome of resolving an acceptance:
// either its publish applied (the revision it produced), or it did not and
// its fence is now revoked so it never can.
type CandidateResolution struct {
	Applied          bool
	Publication      Metadata
	PriorContentHash string
	Receipt          Receipt
}

// candidateRevision finds the revision a candidate produced, if any.
func candidateRevision(pubs []Publication, candidateID string) (CandidateResolution, bool) {
	if candidateID == "" {
		return CandidateResolution{}, false
	}
	for _, p := range pubs {
		for i, rev := range p.Revisions {
			if marker, _ := rev.Skill.Extra[CandidateMarkerKey].(string); marker == candidateID {
				res := CandidateResolution{Applied: true, Publication: revisionMetadata(p, rev)}
				op := "publish"
				if i > 0 {
					res.PriorContentHash = p.Revisions[i-1].ContentHash
					op = "publish_successor"
				}
				res.Receipt = Receipt{Operation: op, PublicationID: p.PublicationID, RevisionID: rev.RevisionID, Generation: rev.Generation, State: p.State, BeforeHash: res.PriorContentHash, AfterHash: rev.ContentHash}
				return res, true
			}
		}
	}
	return CandidateResolution{}, false
}

// checkFence admits a fenced publish only under the candidate's live token.
func checkFence(fences map[string]fenceState, f *CandidateFence) error {
	if f == nil {
		return nil
	}
	if strings.TrimSpace(f.CandidateID) == "" || f.Token == 0 {
		return fmt.Errorf("%w: candidate fence is incomplete", ErrInvalidRequest)
	}
	if fences[f.CandidateID].Live != f.Token {
		return ErrCandidateFenced
	}
	return nil
}

// installFence installs (or resumes) token as the candidate's live fence.
func installFence(fences map[string]fenceState, candidateID string, token uint64) (bool, error) {
	if strings.TrimSpace(candidateID) == "" || token == 0 {
		return false, fmt.Errorf("%w: candidate fence is incomplete", ErrInvalidRequest)
	}
	st := fences[candidateID]
	switch {
	case st.Live == token:
		return false, nil
	case token <= st.Revoked || token < st.Live:
		// Revoked or superseded: never re-installable.
		return false, ErrCandidateFenced
	}
	// A newer acceptance: the older live fence (whose claim was already
	// released) is revoked in this same write.
	st.Revoked, st.Live = max(st.Revoked, st.Live), token
	fences[candidateID] = st
	return true, nil
}

// resolveFence reports the revision the candidate produced, or revokes the
// fence of the acceptance the caller saw (expected) — and anything older —
// so no publish under it can ever land, even one that has not fenced yet. A
// live fence NEWER than expected belongs to a fresher acceptance the caller
// did not see: ErrConflict, and the caller must re-read.
func resolveFence(pubs []Publication, fences map[string]fenceState, candidateID string, expected uint64) (CandidateResolution, bool, error) {
	if res, ok := candidateRevision(pubs, candidateID); ok {
		return res, false, nil
	}
	st := fences[candidateID]
	if st.Live > expected {
		return CandidateResolution{}, false, ErrConflict
	}
	next := fenceState{Revoked: max(st.Revoked, st.Live, expected)}
	if next == st {
		return CandidateResolution{}, false, nil
	}
	fences[candidateID] = next
	return CandidateResolution{}, true, nil
}

// FenceCandidate implements Store.FenceCandidate.
func (m *MemoryStore) FenceCandidate(ctx context.Context, caller identity.Quadruple, candidateID string, token uint64) error {
	if err := validateCaller(caller); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return ErrStoreClosed
	}
	_, err := installFence(m.fencesFor(caller.TenantID), candidateID, token)
	return err
}

// ResolveCandidate implements Store.ResolveCandidate.
func (m *MemoryStore) ResolveCandidate(ctx context.Context, caller identity.Quadruple, candidateID string, expected uint64) (CandidateResolution, error) {
	if err := validateCaller(caller); err != nil {
		return CandidateResolution{}, err
	}
	if err := ctx.Err(); err != nil {
		return CandidateResolution{}, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return CandidateResolution{}, ErrStoreClosed
	}
	res, _, err := resolveFence(m.pubs[caller.TenantID], m.fencesFor(caller.TenantID), candidateID, expected)
	return res, err
}

func (m *MemoryStore) fencesFor(tenant string) map[string]fenceState {
	if m.fences == nil {
		m.fences = map[string]map[string]fenceState{}
	}
	f := m.fences[tenant]
	if f == nil {
		f = map[string]fenceState{}
		m.fences[tenant] = f
	}
	return f
}

func (a *durableAggregate) fences() map[string]fenceState {
	if a.CandidateFenceTokens == nil {
		a.CandidateFenceTokens = map[string]fenceState{}
	}
	return a.CandidateFenceTokens
}

// FenceCandidate implements Store.FenceCandidate.
func (d *StateStoreStore) FenceCandidate(ctx context.Context, c identity.Quadruple, candidateID string, token uint64) error {
	if err := validateCaller(c); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpen(ctx); err != nil {
		return err
	}
	a, old, present, err := d.loadAggregate(ctx, c.TenantID)
	if err != nil {
		return err
	}
	changed, err := installFence(a.fences(), candidateID, token)
	if err != nil || !changed {
		return err
	}
	if err := d.saveAggregate(ctx, c.TenantID, old, present, a); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return ErrConflict
		}
		return err
	}
	return nil
}

// ResolveCandidate implements Store.ResolveCandidate.
func (d *StateStoreStore) ResolveCandidate(ctx context.Context, c identity.Quadruple, candidateID string, expected uint64) (CandidateResolution, error) {
	if err := validateCaller(c); err != nil {
		return CandidateResolution{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpen(ctx); err != nil {
		return CandidateResolution{}, err
	}
	a, old, present, err := d.loadAggregate(ctx, c.TenantID)
	if err != nil {
		return CandidateResolution{}, err
	}
	res, changed, err := resolveFence(a.Publications, a.fences(), candidateID, expected)
	if err != nil || res.Applied || !changed {
		return res, err
	}
	if err := d.saveAggregate(ctx, c.TenantID, old, present, a); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			// Another write raced (perhaps the publish itself): unknown, retry.
			return CandidateResolution{}, ErrConflict
		}
		return CandidateResolution{}, err
	}
	return res, nil
}

// FenceCandidate gates a candidate acceptance; admin-only.
func (s *AuthorizedStore) FenceCandidate(ctx context.Context, caller identity.Quadruple, candidateID string, token uint64) error {
	if err := s.adminCaller(ctx, caller); err != nil {
		return err
	}
	return s.store.FenceCandidate(ctx, caller, candidateID, token)
}

// ResolveCandidate resolves a candidate acceptance; admin-only.
func (s *AuthorizedStore) ResolveCandidate(ctx context.Context, caller identity.Quadruple, candidateID string, expected uint64) (CandidateResolution, error) {
	if err := s.adminCaller(ctx, caller); err != nil {
		return CandidateResolution{}, err
	}
	return s.store.ResolveCandidate(ctx, caller, candidateID, expected)
}
