package publication

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/skills"
)

// AuthorizedStore is the request-edge authorization wrapper for a Store.
//
// Publication storage deliberately accepts an explicit caller quadruple so it
// can remain usable by the runtime and by StateStore conformance tests. The
// wrapper is the service boundary: it reconciles that caller with the
// transport-established verified identity, applies the organization-admin
// gate, and applies the signed effective-Agent reach gate to every operation
// that names an Agent. It is immutable and safe for concurrent use.
type AuthorizedStore struct {
	store Store
	reach auth.AgentReachAuthorizer
}

// Sentinel errors returned by AuthorizedStore. Callers should use errors.Is;
// the wire layer owns mapping these typed outcomes to protocol errors.
var (
	// ErrVerifiedIdentityRequired means the request had no verified identity,
	// or the explicit storage caller did not match it exactly.
	ErrVerifiedIdentityRequired = errors.New("skills/publication: verified identity required")
	// ErrAgentReachDenied means the target Agent was not in the caller's
	// signed effective-Agent reach set.
	ErrAgentReachDenied = errors.New("skills/publication: agent reach denied")
	// ErrAdminRequired means an organization-owned mutation was attempted by a
	// caller without the verified admin scope.
	ErrAdminRequired = errors.New("skills/publication: admin scope required")
	// ErrAuthorizedStoreMisconfigured means a mandatory authorization seam was
	// omitted at construction.
	ErrAuthorizedStoreMisconfigured = errors.New("skills/publication: authorized store misconfigured")
)

// NewAuthorizedStore wraps store with verified-identity, signed-agent-reach,
// and the canonical verified admin-scope checks. The privilege decision is
// deliberately not injectable: construction cannot accidentally create a
// broad admin bypass while wiring a test or an alternate runtime.
func NewAuthorizedStore(store Store, reach auth.AgentReachAuthorizer) (*AuthorizedStore, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: store is nil", ErrAuthorizedStoreMisconfigured)
	}
	if reach == nil {
		return nil, fmt.Errorf("%w: agent reach authorizer is nil", ErrAuthorizedStoreMisconfigured)
	}
	return &AuthorizedStore{store: store, reach: reach}, nil
}

func (s *AuthorizedStore) caller(ctx context.Context, caller identity.Quadruple) error {
	if s == nil || s.store == nil || s.reach == nil {
		return ErrAuthorizedStoreMisconfigured
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	verified, ok := identity.FromVerified(ctx)
	if !ok || identity.Validate(verified) != nil || verified != caller.Identity {
		return ErrVerifiedIdentityRequired
	}
	if err := identity.Validate(caller.Identity); err != nil {
		return fmt.Errorf("%w: caller: %w", ErrVerifiedIdentityRequired, err)
	}
	return nil
}

func (s *AuthorizedStore) adminCaller(ctx context.Context, caller identity.Quadruple) error {
	if err := s.caller(ctx, caller); err != nil {
		return err
	}
	if !auth.HasScope(ctx, auth.ScopeAdmin) {
		return ErrAdminRequired
	}
	return nil
}

func (s *AuthorizedStore) agentCaller(ctx context.Context, caller identity.Quadruple, agentID string) error {
	if err := s.caller(ctx, caller); err != nil {
		return err
	}
	if strings.TrimSpace(agentID) == "" {
		return fmt.Errorf("%w: effective agent id is empty", ErrAgentReachDenied)
	}
	if err := s.reach.AuthorizeAgentReach(ctx, agentID); err != nil {
		return fmt.Errorf("%w: %s", ErrAgentReachDenied, err)
	}
	return nil
}

// Publish creates an organization publication and requires the verified
// admin scope.
func (s *AuthorizedStore) Publish(ctx context.Context, caller identity.Quadruple, req PublishRequest) (Metadata, Receipt, error) {
	if err := s.adminCaller(ctx, caller); err != nil {
		return Metadata{}, Receipt{}, err
	}
	return s.store.Publish(ctx, caller, req)
}

// List lists organization publication metadata for the verified caller's
// tenant. Listing is content-free but remains admin-owned.
func (s *AuthorizedStore) List(ctx context.Context, caller identity.Quadruple) ([]Metadata, error) {
	if err := s.adminCaller(ctx, caller); err != nil {
		return nil, err
	}
	return s.store.List(ctx, caller)
}

// Get returns one content-free organization publication projection.
func (s *AuthorizedStore) Get(ctx context.Context, caller identity.Quadruple, publicationID string) (Metadata, error) {
	if err := s.adminCaller(ctx, caller); err != nil {
		return Metadata{}, err
	}
	return s.store.Get(ctx, caller, publicationID)
}

// PublishSuccessor appends an exact immutable organization revision.
func (s *AuthorizedStore) PublishSuccessor(ctx context.Context, caller identity.Quadruple, req SuccessorRequest) (Metadata, Receipt, error) {
	if err := s.adminCaller(ctx, caller); err != nil {
		return Metadata{}, Receipt{}, err
	}
	return s.store.PublishSuccessor(ctx, caller, req)
}

// Retire terminally retires an organization publication.
func (s *AuthorizedStore) Retire(ctx context.Context, caller identity.Quadruple, req RetireRequest) (Metadata, Receipt, error) {
	if err := s.adminCaller(ctx, caller); err != nil {
		return Metadata{}, Receipt{}, err
	}
	return s.store.Retire(ctx, caller, req)
}

// ListAvailable returns content-free active publication metadata to an
// authenticated caller in the caller's tenant.
func (s *AuthorizedStore) ListAvailable(ctx context.Context, caller identity.Quadruple) ([]Metadata, error) {
	if err := s.caller(ctx, caller); err != nil {
		return nil, err
	}
	return s.store.ListAvailable(ctx, caller)
}

// Install pins one exact publication revision to the target Agent. The
// target is authorized by signed reach; ScopeAgentConfigUser is deliberately
// not treated as a substitute for this gate.
func (s *AuthorizedStore) Install(ctx context.Context, caller identity.Quadruple, req InstallRequest) (Reference, Receipt, error) {
	if err := s.agentCaller(ctx, caller, req.AgentID); err != nil {
		return Reference{}, Receipt{}, err
	}
	return s.store.Install(ctx, caller, req)
}

// Update swaps a reference to an exact revision under the reference CAS.
func (s *AuthorizedStore) Update(ctx context.Context, caller identity.Quadruple, req UpdateRequest) (Reference, Receipt, error) {
	if err := s.agentCaller(ctx, caller, req.AgentID); err != nil {
		return Reference{}, Receipt{}, err
	}
	return s.store.Update(ctx, caller, req)
}

// Remove removes an exact Agent reference under the reference CAS.
func (s *AuthorizedStore) Remove(ctx context.Context, caller identity.Quadruple, req RemoveRequest) (Receipt, error) {
	if err := s.agentCaller(ctx, caller, req.AgentID); err != nil {
		return Receipt{}, err
	}
	return s.store.Remove(ctx, caller, req)
}

// ListReferences returns the verified user's reference metadata. It has no
// target Agent parameter, so it does not invent a reach grant merely to list
// content-free state.
func (s *AuthorizedStore) ListReferences(ctx context.Context, caller identity.Quadruple) ([]Reference, error) {
	if err := s.caller(ctx, caller); err != nil {
		return nil, err
	}
	return s.store.ListReferences(ctx, caller)
}

// Resolve obtains the exact body pinned to agentID and re-applies signed
// reach immediately before the body-bearing read.
func (s *AuthorizedStore) Resolve(ctx context.Context, caller identity.Quadruple, agentID string) (skills.Skill, Metadata, error) {
	if err := s.agentCaller(ctx, caller, agentID); err != nil {
		return skills.Skill{}, Metadata{}, err
	}
	return s.store.Resolve(ctx, caller, agentID)
}

// Close delegates closure to the wrapped store.
func (s *AuthorizedStore) Close(ctx context.Context) error {
	if s == nil || s.store == nil {
		return ErrAuthorizedStoreMisconfigured
	}
	return s.store.Close(ctx)
}

var _ Store = (*AuthorizedStore)(nil)
