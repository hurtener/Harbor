package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tools"
)

const flowKindPrefix = "tools.auth.flow."

// PendingFlowRecord is the durable correlation record for one OAuth
// authorization-code flow. It contains PKCE and client credential material and
// therefore MUST only be persisted through FlowStore, whose StateStore-backed
// implementation seals the complete record at rest.
type PendingFlowRecord struct {
	State        string
	Source       tools.ToolSourceID
	BindingScope BindingScope
	SubjectID    string
	Identity     identity.Identity
	Verifier     string
	CreatedAt    time.Time
	ExpiresAt    time.Time
	PauseToken   pauseresume.Token
	ClientID     string
	ClientSecret string
	TokenURL     string
	RedirectURI  string
	// TerminalFailure is populated from the durable terminal marker after the
	// authorization code was spent but its token could not be persisted. A
	// retry must reject the parked pause and clean up without re-exchanging the
	// one-time code.
	TerminalFailure string
}

// CompletedFlowRecord is the durable, exact-state proof that one pending
// authorization code was exchanged and its token was persisted. It is kept
// after the secret-bearing PendingFlowRecord is deleted so callback retries can
// still route to the owning provider and converge the exact unified-pause
// decision without exchanging the one-time code again.
//
// The record contains no OAuth credential or PKCE material. FlowStore still
// seals it because the pause token and identity correlation are internal
// control-plane state.
type CompletedFlowRecord struct {
	State            string
	TokenMarker      string
	Source           tools.ToolSourceID
	BindingScope     BindingScope
	SubjectID        string
	Identity         identity.Identity
	PauseToken       pauseresume.Token
	ExpectedDecision pauseresume.Decision
	ExpiresAt        time.Time
}

// FlowStore persists the one-time OAuth correlation record required to finish
// a PKCE exchange and resume its unified pause after a Runtime restart.
// Implementations must be safe for concurrent reuse. Get is non-consuming;
// Claim establishes one durable completion owner across reconstructed stores.
type FlowStore interface {
	Put(ctx context.Context, flow PendingFlowRecord) error
	Get(ctx context.Context, stateToken string) (PendingFlowRecord, bool, error)
	GetCompleted(ctx context.Context, stateToken string) (CompletedFlowRecord, bool, error)
	Claim(ctx context.Context, stateToken string) (PendingFlowRecord, FlowClaim, bool, error)
	Release(ctx context.Context, claim FlowClaim) error
	Finish(ctx context.Context, claim FlowClaim) error
	MarkCompleted(ctx context.Context, claim FlowClaim, completed CompletedFlowRecord) error
	ForgetCompleted(ctx context.Context, completed CompletedFlowRecord) error
	MarkTerminal(ctx context.Context, claim FlowClaim, reason string) error
}

// FlowClaim is the opaque ownership receipt returned by FlowStore.Claim.
// Callers either Release it after a retryable failure or Finish it after the
// token is durable and the associated unified pause has resumed.
type FlowClaim struct {
	State    string
	Owner    state.EventID
	Identity identity.Identity
}

type stateStoreFlowStore struct {
	store  state.StateStore
	sealer Sealer
	mu     sync.Mutex
	now    func() time.Time
}

// NewFlowStore constructs the production FlowStore over Harbor's existing
// persistence seam. The same KEK used for OAuth token custody seals the entire
// pending-flow envelope; no PKCE verifier, client secret, or pause token is
// stored as plaintext.
func NewFlowStore(store state.StateStore, sealer Sealer) (FlowStore, error) {
	if store == nil {
		return nil, errors.New("auth: NewFlowStore: state.StateStore required")
	}
	if sealer == nil {
		return nil, errors.New("auth: NewFlowStore: Sealer required (encryption-at-rest is mandatory)")
	}
	return &stateStoreFlowStore{store: store, sealer: sealer, now: time.Now}, nil
}

func (s *stateStoreFlowStore) Put(ctx context.Context, flow PendingFlowRecord) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("auth: flow Put cancelled: %w", err)
	}
	if err := validatePendingFlow(flow); err != nil {
		return err
	}
	if err := validateFlowCaller(ctx, flow.Identity); err != nil {
		return err
	}
	// New flows are the natural bounded maintenance cadence for OAuth state.
	// Prune only this exact Harbor identity's expired completion tombstones;
	// ListKindForIdentity prevents a normal user flow from widening into another
	// tenant/user/session while ensuring completed states cannot accumulate when
	// browsers never replay their callback.
	if err := s.pruneExpiredCompleted(ctx, flow.Identity); err != nil {
		return err
	}
	plain, err := json.Marshal(flow) //nolint:gosec // The complete secret-bearing envelope is sealed before persistence.
	if err != nil {
		return fmt.Errorf("auth: marshal pending flow: %w", err)
	}
	sealed, err := s.sealer.Seal(plain)
	if err != nil {
		return fmt.Errorf("auth: seal pending flow: %w", err)
	}
	rec := state.StateRecord{
		// OAuth state is already a crypto-random, globally unique one-time
		// capability. Using it as EventID gives the callback a direct durable
		// lookup without an elevated cross-identity scan.
		ID:       state.EventID(flow.State),
		Identity: identity.Quadruple{Identity: flow.Identity},
		Kind:     flowKindPrefix + flow.State,
		Bytes:    sealed,
	}
	if err := s.store.Save(ctx, rec); err != nil {
		return fmt.Errorf("auth: save pending flow: %w", err)
	}
	return nil
}

func (s *stateStoreFlowStore) pruneExpiredCompleted(ctx context.Context, id identity.Identity) error {
	q := identity.Quadruple{Identity: id}
	records, err := s.store.ListKindForIdentity(ctx, q, flowCompletedKindPrefix)
	if err != nil {
		return fmt.Errorf("auth: list completed OAuth flows for identity: %w", err)
	}
	for _, rec := range records {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("auth: prune completed OAuth flows cancelled: %w", err)
		}
		if rec.Identity != q || len(rec.Kind) <= len(flowCompletedKindPrefix) || rec.Kind[:len(flowCompletedKindPrefix)] != flowCompletedKindPrefix {
			return errors.New("auth: identity-scoped completed OAuth flow enumeration returned mismatched record")
		}
		stateToken := rec.Kind[len(flowCompletedKindPrefix):]
		if rec.ID != flowCompletedEventID(stateToken) {
			return errors.New("auth: completed OAuth flow enumeration returned mismatched event id")
		}
		plain, openErr := s.sealer.Open(rec.Bytes)
		if openErr != nil {
			return fmt.Errorf("auth: open completed OAuth flow during prune: %w", openErr)
		}
		var completed CompletedFlowRecord
		if decodeErr := json.Unmarshal(plain, &completed); decodeErr != nil {
			return fmt.Errorf("auth: decode completed OAuth flow during prune: %w", decodeErr)
		}
		if validateErr := validateCompletedFlow(completed); validateErr != nil {
			return fmt.Errorf("auth: validate completed OAuth flow during prune: %w", validateErr)
		}
		if completed.State != stateToken || completed.Identity != id {
			return errors.New("auth: completed OAuth flow prune scope mismatch")
		}
		if !s.now().After(completed.ExpiresAt) {
			continue
		}
		if deleteErr := s.deleteCompletedArtifacts(ctx, completed); deleteErr != nil {
			return fmt.Errorf("auth: prune completed OAuth flow: %w", deleteErr)
		}
	}
	return nil
}

func (s *stateStoreFlowStore) Get(ctx context.Context, stateToken string) (PendingFlowRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getLocked(ctx, stateToken)
}

func (s *stateStoreFlowStore) GetCompleted(ctx context.Context, stateToken string) (CompletedFlowRecord, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getCompletedLocked(ctx, stateToken)
}

func (s *stateStoreFlowStore) Claim(ctx context.Context, stateToken string) (PendingFlowRecord, FlowClaim, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	flow, ok, err := s.getLocked(ctx, stateToken)
	if err != nil || !ok {
		return PendingFlowRecord{}, FlowClaim{}, ok, err
	}
	if err := validateFlowCaller(ctx, flow.Identity); err != nil {
		return PendingFlowRecord{}, FlowClaim{}, false, err
	}

	claimEventID := flowClaimEventID(stateToken)
	existing, loadErr := s.store.LoadByEventID(ctx, claimEventID)
	if loadErr == nil {
		var held flowClaimEnvelope
		if err := json.Unmarshal(existing.Bytes, &held); err != nil {
			return PendingFlowRecord{}, FlowClaim{}, false, fmt.Errorf("auth: decode OAuth flow claim: %w", err)
		}
		if s.now().Before(held.ExpiresAt) {
			return PendingFlowRecord{}, FlowClaim{}, false, nil
		}
		if err := s.store.Delete(ctx, existing.Identity, existing.Kind); err != nil {
			return PendingFlowRecord{}, FlowClaim{}, false, fmt.Errorf("auth: delete expired OAuth flow claim: %w", err)
		}
	} else if !errors.Is(loadErr, state.ErrNotFound) {
		return PendingFlowRecord{}, FlowClaim{}, false, fmt.Errorf("auth: load OAuth flow claim: %w", loadErr)
	}

	claim := FlowClaim{State: stateToken, Owner: state.NewEventID(), Identity: flow.Identity}
	// A claim cannot expire while the authorization flow itself remains
	// callback-able. This deliberately favors at-most-once exchange over
	// lease stealing: if a process dies during completion, the operator safely
	// re-initiates after the flow TTL instead of risking a second code exchange.
	claimBytes, err := json.Marshal(flowClaimEnvelope{Owner: claim.Owner, ExpiresAt: flow.ExpiresAt})
	if err != nil {
		return PendingFlowRecord{}, FlowClaim{}, false, fmt.Errorf("auth: marshal OAuth flow claim: %w", err)
	}
	claimRec := state.StateRecord{
		ID:       claimEventID,
		Identity: identity.Quadruple{Identity: flow.Identity},
		Kind:     flowClaimKindPrefix + stateToken,
		Bytes:    claimBytes,
	}
	if err := s.store.Save(ctx, claimRec); err != nil {
		if errors.Is(err, state.ErrIdempotencyConflict) {
			return PendingFlowRecord{}, FlowClaim{}, false, nil
		}
		return PendingFlowRecord{}, FlowClaim{}, false, fmt.Errorf("auth: claim pending flow: %w", err)
	}
	return flow, claim, true, nil
}

func (s *stateStoreFlowStore) Release(ctx context.Context, claim FlowClaim) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deleteClaimLocked(ctx, claim)
}

func (s *stateStoreFlowStore) Finish(ctx context.Context, claim FlowClaim) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.verifyClaimLocked(ctx, claim); err != nil {
		return err
	}
	q := identity.Quadruple{Identity: claim.Identity}
	if err := s.store.Delete(ctx, q, flowClaimKindPrefix+claim.State); err != nil {
		return fmt.Errorf("auth: finish pending flow claim: %w", err)
	}
	if err := s.store.Delete(ctx, q, flowKindPrefix+claim.State); err != nil {
		return fmt.Errorf("auth: finish pending flow: %w", err)
	}
	if err := s.store.Delete(ctx, q, flowTerminalKindPrefix+claim.State); err != nil {
		return fmt.Errorf("auth: finish pending flow terminal marker: %w", err)
	}
	// The completed-flow tombstone deliberately survives destructive cleanup.
	// It is the callback-routing and exact-state idempotency oracle through the
	// original flow's retry horizon.
	return nil
}

func (s *stateStoreFlowStore) MarkCompleted(ctx context.Context, claim FlowClaim, completed CompletedFlowRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.verifyClaimLocked(ctx, claim); err != nil {
		return err
	}
	if err := validateCompletedFlow(completed); err != nil {
		return err
	}
	if completed.State != claim.State || completed.Identity != claim.Identity {
		return ErrStateMismatch
	}
	pending, ok, err := s.getLocked(ctx, claim.State)
	if err != nil {
		return err
	}
	if !ok {
		return ErrFlowNotFound
	}
	if completed.Source != pending.Source || completed.BindingScope != pending.BindingScope ||
		completed.SubjectID != pending.SubjectID || completed.PauseToken != pending.PauseToken ||
		!completed.ExpiresAt.Equal(pending.ExpiresAt) {
		return errors.New("auth: completed OAuth flow does not match pending flow")
	}

	// A fixed EventID makes the completion write idempotent. Because sealing
	// uses a fresh nonce, read and validate an existing record rather than
	// attempting to reproduce its ciphertext on retry after an ambiguous Save.
	if existing, ok, err := s.getCompletedLocked(ctx, completed.State); err != nil {
		return err
	} else if ok {
		if existing != completed {
			return errors.New("auth: completed OAuth flow marker mismatch")
		}
		return nil
	}

	plain, err := json.Marshal(completed)
	if err != nil {
		return fmt.Errorf("auth: marshal completed OAuth flow: %w", err)
	}
	sealed, err := s.sealer.Seal(plain)
	if err != nil {
		return fmt.Errorf("auth: seal completed OAuth flow: %w", err)
	}
	rec := state.StateRecord{
		ID:       flowCompletedEventID(completed.State),
		Identity: identity.Quadruple{Identity: completed.Identity},
		Kind:     flowCompletedKindPrefix + completed.State,
		Bytes:    sealed,
	}
	if err := s.store.Save(ctx, rec); err != nil {
		// Save may have landed while its acknowledgement was lost. Resolve the
		// fixed EventID and accept only the exact sealed logical record.
		existing, ok, loadErr := s.getCompletedLocked(ctx, completed.State)
		if loadErr == nil && ok && existing == completed {
			return nil
		}
		return errors.Join(fmt.Errorf("auth: mark completed OAuth flow: %w", err), loadErr)
	}
	return nil
}

func (s *stateStoreFlowStore) ForgetCompleted(ctx context.Context, completed CompletedFlowRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := validateCompletedFlow(completed); err != nil {
		return err
	}
	if err := validateFlowCaller(ctx, completed.Identity); err != nil {
		return err
	}
	existing, ok, err := s.getCompletedLocked(ctx, completed.State)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	if existing != completed {
		return errors.New("auth: completed OAuth flow marker mismatch during expiry cleanup")
	}
	return s.deleteCompletedArtifacts(ctx, completed)
}

func (s *stateStoreFlowStore) deleteCompletedArtifacts(ctx context.Context, completed CompletedFlowRecord) error {
	q := identity.Quadruple{Identity: completed.Identity}
	// Delete the exact residual secret/control records first and the tombstone
	// last. Any mid-sequence failure therefore retains the durable proof needed
	// for the next identity-scoped Put or callback retry to converge cleanup.
	targets := []struct {
		kind string
		id   state.EventID
	}{
		{kind: flowKindPrefix + completed.State, id: state.EventID(completed.State)},
		{kind: flowClaimKindPrefix + completed.State, id: flowClaimEventID(completed.State)},
		{kind: flowTerminalKindPrefix + completed.State, id: flowTerminalEventID(completed.State)},
		{kind: flowCompletedKindPrefix + completed.State, id: flowCompletedEventID(completed.State)},
	}
	for _, target := range targets {
		if err := s.store.Delete(ctx, q, target.kind); err != nil {
			// Delete is idempotent, but a driver may apply it and lose the
			// acknowledgement. Resolve the exact EventID before surfacing failure.
			_, loadErr := s.store.LoadByEventID(ctx, target.id)
			if errors.Is(loadErr, state.ErrNotFound) {
				continue
			}
			return errors.Join(fmt.Errorf("auth: delete completed OAuth flow artifact %q: %w", target.kind, err), loadErr)
		}
	}
	return nil
}

func (s *stateStoreFlowStore) MarkTerminal(ctx context.Context, claim FlowClaim, reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if reason != flowTerminalWriteFailed {
		return fmt.Errorf("auth: unsupported pending flow terminal reason %q", reason)
	}
	if err := s.verifyClaimLocked(ctx, claim); err != nil {
		return err
	}
	rec := state.StateRecord{
		ID:       flowTerminalEventID(claim.State),
		Identity: identity.Quadruple{Identity: claim.Identity},
		Kind:     flowTerminalKindPrefix + claim.State,
		Bytes:    []byte(reason),
	}
	if err := s.store.Save(ctx, rec); err != nil {
		return fmt.Errorf("auth: mark pending flow terminal: %w", err)
	}
	return nil
}

const flowClaimKindPrefix = "tools.auth.flow.claim."
const flowTerminalKindPrefix = "tools.auth.flow.terminal."
const flowCompletedKindPrefix = "tools.auth.flow.completed."
const flowTerminalWriteFailed = "oauth_token_persistence_failed"

type flowClaimEnvelope struct {
	Owner     state.EventID `json:"owner"`
	ExpiresAt time.Time     `json:"expires_at"`
}

func flowClaimEventID(stateToken string) state.EventID {
	return state.EventID("oauth-flow-claim:" + stateToken)
}

func flowTerminalEventID(stateToken string) state.EventID {
	return state.EventID("oauth-flow-terminal:" + stateToken)
}

func flowCompletedEventID(stateToken string) state.EventID {
	return state.EventID("oauth-flow-completed:" + stateToken)
}

func (s *stateStoreFlowStore) deleteClaimLocked(ctx context.Context, claim FlowClaim) error {
	if err := validateFlowCaller(ctx, claim.Identity); err != nil {
		return err
	}
	if err := s.verifyClaimLocked(ctx, claim); err != nil {
		return err
	}
	q := identity.Quadruple{Identity: claim.Identity}
	if err := s.store.Delete(ctx, q, flowClaimKindPrefix+claim.State); err != nil {
		return fmt.Errorf("auth: release pending flow claim: %w", err)
	}
	return nil
}

func (s *stateStoreFlowStore) verifyClaimLocked(ctx context.Context, claim FlowClaim) error {
	if err := validateFlowCaller(ctx, claim.Identity); err != nil {
		return err
	}
	rec, err := s.store.LoadByEventID(ctx, flowClaimEventID(claim.State))
	if err != nil {
		return fmt.Errorf("auth: load pending flow claim receipt: %w", err)
	}
	if rec.Kind != flowClaimKindPrefix+claim.State || rec.Identity.Identity != claim.Identity {
		return errors.New("auth: pending flow claim scope mismatch")
	}
	var held flowClaimEnvelope
	if err := json.Unmarshal(rec.Bytes, &held); err != nil {
		return fmt.Errorf("auth: decode pending flow claim receipt: %w", err)
	}
	if held.Owner != claim.Owner {
		return errors.New("auth: pending flow claim owner mismatch")
	}
	return nil
}

func validateFlowCaller(ctx context.Context, target identity.Identity) error {
	caller, err := identityFromCtx(ctx)
	if err != nil {
		return err
	}
	if caller != target {
		return ErrStateMismatch
	}
	return nil
}

func (s *stateStoreFlowStore) getLocked(ctx context.Context, stateToken string) (PendingFlowRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return PendingFlowRecord{}, false, fmt.Errorf("auth: flow Get cancelled: %w", err)
	}
	if stateToken == "" {
		return PendingFlowRecord{}, false, nil
	}
	rec, err := s.store.LoadByEventID(ctx, state.EventID(stateToken))
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return PendingFlowRecord{}, false, nil
		}
		return PendingFlowRecord{}, false, fmt.Errorf("auth: load pending flow: %w", err)
	}
	if rec.Kind != flowKindPrefix+stateToken {
		return PendingFlowRecord{}, false, fmt.Errorf("auth: pending flow event id collision for state %q", stateToken)
	}
	plain, err := s.sealer.Open(rec.Bytes)
	if err != nil {
		return PendingFlowRecord{}, false, fmt.Errorf("auth: open pending flow: %w", err)
	}
	var flow PendingFlowRecord
	if err := json.Unmarshal(plain, &flow); err != nil {
		return PendingFlowRecord{}, false, fmt.Errorf("auth: decode pending flow: %w", err)
	}
	if err := validatePendingFlow(flow); err != nil {
		return PendingFlowRecord{}, false, fmt.Errorf("auth: persisted pending flow invalid: %w", err)
	}
	if flow.State != stateToken || flow.Identity != rec.Identity.Identity {
		return PendingFlowRecord{}, false, errors.New("auth: persisted pending flow scope mismatch")
	}
	terminal, terminalErr := s.store.LoadByEventID(ctx, flowTerminalEventID(stateToken))
	if terminalErr == nil {
		if terminal.Kind != flowTerminalKindPrefix+stateToken || terminal.Identity.Identity != flow.Identity {
			return PendingFlowRecord{}, false, errors.New("auth: pending flow terminal marker scope mismatch")
		}
		if string(terminal.Bytes) != flowTerminalWriteFailed {
			return PendingFlowRecord{}, false, errors.New("auth: pending flow terminal marker invalid")
		}
		flow.TerminalFailure = string(terminal.Bytes)
	} else if !errors.Is(terminalErr, state.ErrNotFound) {
		return PendingFlowRecord{}, false, fmt.Errorf("auth: load pending flow terminal marker: %w", terminalErr)
	}
	return flow, true, nil
}

func (s *stateStoreFlowStore) getCompletedLocked(ctx context.Context, stateToken string) (CompletedFlowRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return CompletedFlowRecord{}, false, fmt.Errorf("auth: completed flow Get cancelled: %w", err)
	}
	if stateToken == "" {
		return CompletedFlowRecord{}, false, nil
	}
	rec, err := s.store.LoadByEventID(ctx, flowCompletedEventID(stateToken))
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return CompletedFlowRecord{}, false, nil
		}
		return CompletedFlowRecord{}, false, fmt.Errorf("auth: load completed OAuth flow: %w", err)
	}
	if rec.Kind != flowCompletedKindPrefix+stateToken {
		return CompletedFlowRecord{}, false, fmt.Errorf("auth: completed OAuth flow event id collision for state %q", stateToken)
	}
	plain, err := s.sealer.Open(rec.Bytes)
	if err != nil {
		return CompletedFlowRecord{}, false, fmt.Errorf("auth: open completed OAuth flow: %w", err)
	}
	var completed CompletedFlowRecord
	if err := json.Unmarshal(plain, &completed); err != nil {
		return CompletedFlowRecord{}, false, fmt.Errorf("auth: decode completed OAuth flow: %w", err)
	}
	if err := validateCompletedFlow(completed); err != nil {
		return CompletedFlowRecord{}, false, fmt.Errorf("auth: persisted completed OAuth flow invalid: %w", err)
	}
	if completed.State != stateToken || completed.Identity != rec.Identity.Identity {
		return CompletedFlowRecord{}, false, errors.New("auth: persisted completed OAuth flow scope mismatch")
	}
	return completed, true, nil
}

func validatePendingFlow(flow PendingFlowRecord) error {
	if flow.State == "" || flow.Source == "" || flow.SubjectID == "" || flow.Verifier == "" ||
		flow.PauseToken == "" || flow.TokenURL == "" || flow.RedirectURI == "" || flow.CreatedAt.IsZero() || flow.ExpiresAt.IsZero() {
		return wrap(ErrConfigInvalid, "pending OAuth flow is incomplete")
	}
	if !IsValidBindingScope(flow.BindingScope) {
		return wrap(ErrInvalidBindingScope, "got %q", flow.BindingScope)
	}
	if err := state.ValidateIdentity(identity.Quadruple{Identity: flow.Identity}); err != nil {
		return fmt.Errorf("%w: pending OAuth flow identity: %w", ErrIdentityRequired, err)
	}
	return nil
}

func validateCompletedFlow(completed CompletedFlowRecord) error {
	if completed.State == "" || completed.TokenMarker == "" || completed.Source == "" || completed.SubjectID == "" ||
		completed.PauseToken == "" || completed.ExpiresAt.IsZero() {
		return wrap(ErrConfigInvalid, "completed OAuth flow is incomplete")
	}
	if completed.TokenMarker != completed.State {
		return ErrStateMismatch
	}
	if !IsValidBindingScope(completed.BindingScope) {
		return wrap(ErrInvalidBindingScope, "got %q", completed.BindingScope)
	}
	if completed.ExpectedDecision != pauseresume.DecisionResume {
		return fmt.Errorf("auth: completed OAuth flow expected decision %q is invalid", completed.ExpectedDecision)
	}
	if err := state.ValidateIdentity(identity.Quadruple{Identity: completed.Identity}); err != nil {
		return fmt.Errorf("%w: completed OAuth flow identity: %w", ErrIdentityRequired, err)
	}
	return nil
}
