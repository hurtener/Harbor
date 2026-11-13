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

// fenceState is one candidate's fence in the publication aggregate. Tokens
// are integers allocated here, under the same compare-and-set that installs
// them: a new acceptance gets max(Live, Revoked)+1, a resume must present
// exactly Live, and a revoked token is permanent. Wall clocks never order
// them.
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

// installFence resumes the exact live token, or allocates a new one for a
// new acceptance (presented == 0).
func installFence(fences map[string]fenceState, candidateID string, presented uint64) (uint64, bool, error) {
	if strings.TrimSpace(candidateID) == "" {
		return 0, false, fmt.Errorf("%w: candidate fence is incomplete", ErrInvalidRequest)
	}
	st := fences[candidateID]
	if presented != 0 {
		if st.Live == presented {
			return presented, false, nil
		}
		// Revoked, superseded or never installed here: never re-installable.
		return 0, false, ErrCandidateFenced
	}
	if st.Live != 0 {
		// Another acceptance holds the live fence; it must be resolved first.
		return 0, false, ErrConflict
	}
	st.Live = max(st.Live, st.Revoked) + 1
	fences[candidateID] = st
	return st.Live, true, nil
}

// resolveFence reports the revision the candidate produced, or revokes the
// live fence (whatever acceptance holds it) so that publish can never land.
func resolveFence(pubs []Publication, fences map[string]fenceState, candidateID string) (CandidateResolution, bool) {
	if res, ok := candidateRevision(pubs, candidateID); ok {
		return res, false
	}
	st := fences[candidateID]
	if st.Live == 0 {
		return CandidateResolution{}, false
	}
	st.Revoked, st.Live = max(st.Revoked, st.Live), 0
	fences[candidateID] = st
	return CandidateResolution{}, true
}

// FenceCandidate implements Store.FenceCandidate.
func (m *MemoryStore) FenceCandidate(ctx context.Context, caller identity.Quadruple, candidateID string, presented uint64) (uint64, error) {
	if err := validateCaller(caller); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return 0, ErrStoreClosed
	}
	token, _, err := installFence(m.fencesFor(caller.TenantID), candidateID, presented)
	return token, err
}

// ResolveCandidate implements Store.ResolveCandidate.
func (m *MemoryStore) ResolveCandidate(ctx context.Context, caller identity.Quadruple, candidateID string) (CandidateResolution, error) {
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
	res, _ := resolveFence(m.pubs[caller.TenantID], m.fencesFor(caller.TenantID), candidateID)
	return res, nil
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
func (d *StateStoreStore) FenceCandidate(ctx context.Context, c identity.Quadruple, candidateID string, presented uint64) (uint64, error) {
	if err := validateCaller(c); err != nil {
		return 0, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.checkOpen(ctx); err != nil {
		return 0, err
	}
	a, old, present, err := d.loadAggregate(ctx, c.TenantID)
	if err != nil {
		return 0, err
	}
	token, changed, err := installFence(a.fences(), candidateID, presented)
	if err != nil || !changed {
		return token, err
	}
	if err := d.saveAggregate(ctx, c.TenantID, old, present, a); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return 0, ErrConflict
		}
		return 0, err
	}
	return token, nil
}

// ResolveCandidate implements Store.ResolveCandidate.
func (d *StateStoreStore) ResolveCandidate(ctx context.Context, c identity.Quadruple, candidateID string) (CandidateResolution, error) {
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
	res, changed := resolveFence(a.Publications, a.fences(), candidateID)
	if res.Applied || !changed {
		return res, nil
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
func (s *AuthorizedStore) FenceCandidate(ctx context.Context, caller identity.Quadruple, candidateID string, presented uint64) (uint64, error) {
	if err := s.adminCaller(ctx, caller); err != nil {
		return 0, err
	}
	return s.store.FenceCandidate(ctx, caller, candidateID, presented)
}

// ResolveCandidate resolves a candidate acceptance; admin-only.
func (s *AuthorizedStore) ResolveCandidate(ctx context.Context, caller identity.Quadruple, candidateID string) (CandidateResolution, error) {
	if err := s.adminCaller(ctx, caller); err != nil {
		return CandidateResolution{}, err
	}
	return s.store.ResolveCandidate(ctx, caller, candidateID)
}
