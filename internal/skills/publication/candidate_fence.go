package publication

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// CandidateMarkerKey records, inside a published revision body, which
// candidate produced it. Runtime-private bookkeeping (underscore prefix: never
// projected on the wire); the store uses it to make a candidate's publish
// idempotent across callers and to prove whether an acceptance applied.
const CandidateMarkerKey = "_publication.candidate_id"

const revokedFencePrefix = "revoked:"

// ErrCandidateFenced means the acceptance this publish belonged to was
// revoked (its claim was resolved as stale): the publish never lands. It is a
// conflict (errors.Is(err, ErrConflict) holds).
var ErrCandidateFenced = fmt.Errorf("%w: candidate acceptance was revoked", ErrConflict)

// CandidateFence names the candidate acceptance a publish belongs to. The
// publication store commits such a publish only while Token is the
// candidate's current fence, in the same compare-and-set as the publish.
type CandidateFence struct {
	CandidateID string
	Token       string
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
func checkFence(fences map[string]string, f *CandidateFence) error {
	if f == nil {
		return nil
	}
	if strings.TrimSpace(f.CandidateID) == "" || strings.TrimSpace(f.Token) == "" {
		return fmt.Errorf("%w: candidate fence is incomplete", ErrInvalidRequest)
	}
	if fences[f.CandidateID] != f.Token {
		return ErrCandidateFenced
	}
	return nil
}

func setFence(fences map[string]string, candidateID, token string) error {
	if strings.TrimSpace(candidateID) == "" || strings.TrimSpace(token) == "" {
		return fmt.Errorf("%w: candidate fence is incomplete", ErrInvalidRequest)
	}
	current := fences[candidateID]
	if current != "" && current != token && !strings.HasPrefix(current, revokedFencePrefix) {
		return ErrConflict
	}
	fences[candidateID] = token
	return nil
}

func resolveFence(pubs []Publication, fences map[string]string, candidateID, token string) (CandidateResolution, bool, error) {
	if res, ok := candidateRevision(pubs, candidateID); ok {
		return res, false, nil
	}
	current := fences[candidateID]
	if current != "" && current != token && !strings.HasPrefix(current, revokedFencePrefix) {
		return CandidateResolution{}, false, ErrConflict
	}
	changed := current != revokedFencePrefix+token
	fences[candidateID] = revokedFencePrefix + token
	return CandidateResolution{}, changed, nil
}

// FenceCandidate implements Store.FenceCandidate.
func (m *MemoryStore) FenceCandidate(ctx context.Context, caller identity.Quadruple, candidateID, token string) error {
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
	return setFence(m.fencesFor(caller.TenantID), candidateID, token)
}

// ResolveCandidate implements Store.ResolveCandidate.
func (m *MemoryStore) ResolveCandidate(ctx context.Context, caller identity.Quadruple, candidateID, token string) (CandidateResolution, error) {
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
	res, _, err := resolveFence(m.pubs[caller.TenantID], m.fencesFor(caller.TenantID), candidateID, token)
	return res, err
}

func (m *MemoryStore) fencesFor(tenant string) map[string]string {
	if m.fences == nil {
		m.fences = map[string]map[string]string{}
	}
	f := m.fences[tenant]
	if f == nil {
		f = map[string]string{}
		m.fences[tenant] = f
	}
	return f
}

// FenceCandidate implements Store.FenceCandidate.
func (d *StateStoreStore) FenceCandidate(ctx context.Context, c identity.Quadruple, candidateID, token string) error {
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
	if a.CandidateFences == nil {
		a.CandidateFences = map[string]string{}
	}
	if a.CandidateFences[candidateID] == token && token != "" {
		return nil
	}
	if err := setFence(a.CandidateFences, candidateID, token); err != nil {
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
func (d *StateStoreStore) ResolveCandidate(ctx context.Context, c identity.Quadruple, candidateID, token string) (CandidateResolution, error) {
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
	if a.CandidateFences == nil {
		a.CandidateFences = map[string]string{}
	}
	res, changed, err := resolveFence(a.Publications, a.CandidateFences, candidateID, token)
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
func (s *AuthorizedStore) FenceCandidate(ctx context.Context, caller identity.Quadruple, candidateID, token string) error {
	if err := s.adminCaller(ctx, caller); err != nil {
		return err
	}
	return s.store.FenceCandidate(ctx, caller, candidateID, token)
}

// ResolveCandidate resolves a candidate acceptance; admin-only.
func (s *AuthorizedStore) ResolveCandidate(ctx context.Context, caller identity.Quadruple, candidateID, token string) (CandidateResolution, error) {
	if err := s.adminCaller(ctx, caller); err != nil {
		return CandidateResolution{}, err
	}
	return s.store.ResolveCandidate(ctx, caller, candidateID, token)
}
