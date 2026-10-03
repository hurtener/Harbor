// Package sessionadmission serializes externally authorized session mutations
// with durable enrollment changes. It owns an acceptance fence, not execution,
// task recovery, or a permit queue. There is at most one unresolved acceptance
// per full session identity; uncertainty is never resolved by a timer.
package sessionadmission

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tools"
)

// Admission errors are intentionally independent of wire transports.
var (
	// ErrDenied rejects missing, stale or foreign session authority.
	ErrDenied = errors.New("session admission: authority denied")
	// ErrBusy preserves one unresolved acceptance without expiry.
	ErrBusy = errors.New("session admission: acceptance unresolved")
	// ErrConflict reports an epoch/CAS transition that did not match.
	ErrConflict = errors.New("session admission: enrollment changed")
	// ErrUnavailable refuses an unwired or downgraded admission boundary.
	ErrUnavailable = errors.New("session admission: durable gate unavailable")
	// ErrCorrupt refuses unsupported or internally inconsistent durable state.
	ErrCorrupt = errors.New("session admission: invalid durable record")
)

const kindPrefix = state.InternalKindPrefix + "session-admission/v1/"

// Policy is the immutable enrollment authority and its monotonic epoch.
// Epoch zero is legacy, unenrolled state; enrollment cannot be removed.
type Policy struct {
	Identity    identity.Identity `json:"identity"`
	Issuer      string            `json:"issuer,omitempty"`
	Coordinator string            `json:"coordinator,omitempty"`
	Epoch       uint64            `json:"epoch"`
}

type record struct {
	Schema    int            `json:"schema"`
	Policy    Policy         `json:"policy"`
	Pending   string         `json:"pending,omitempty"`
	Method    methods.Method `json:"method,omitempty"`
	ReplayKey string         `json:"replay_key,omitempty"`
}

// Gate is immutable and safe to share. Independent Gates using the same durable
// store serialize through SaveIf, never a process-local mutex.
type Gate struct{ store state.StateStore }

// New builds a gate over the runtime's existing mandatory StateStore.
func New(store state.StateStore) (*Gate, error) {
	if store == nil {
		return nil, ErrUnavailable
	}
	return &Gate{store: store}, nil
}

// RequireEnabled refuses a same-version configuration downgrade once this
// StateStore has recorded admission state. An older binary cannot enforce this
// check, which is why fleet cutover and downgrade prevention remain required.
func RequireEnabled(ctx context.Context, store state.StateStore, enabled bool) error {
	if store == nil {
		if enabled {
			return ErrUnavailable
		}
		return nil
	}
	if enabled {
		return nil
	}
	rows, err := store.ListKindForIdentityBounded(ctx, identity.InternalCoordinationQuadruple(), kindPrefix, 1)
	if err != nil {
		return fmt.Errorf("session admission: check downgrade: %w", err)
	}
	if len(rows) > 0 {
		return fmt.Errorf("%w: recorded admission state requires scoped audience configuration", ErrUnavailable)
	}
	return nil
}

type gateKey struct{}
type replayKey struct{}
type nativeResumeKey struct{}

// WithReplayKey lets an exact keyed Start or user-message retry wait briefly
// for its first acceptance to finish. Only a digest is persisted; the existing
// task idempotency and input receipts remain the authoritative replay stores.
func WithReplayKey(ctx context.Context, key string) context.Context {
	if key == "" {
		return ctx
	}
	digest := sha256.Sum256([]byte(key))
	return context.WithValue(ctx, replayKey{}, hex.EncodeToString(digest[:]))
}

// WithGate supplies the runtime-owned gate to transport-independent services.
func WithGate(ctx context.Context, gate *Gate) context.Context {
	return context.WithValue(ctx, gateKey{}, gate)
}

// From returns the runtime-owned gate, if assembled.
func From(ctx context.Context) (*Gate, bool) {
	gate, ok := ctx.Value(gateKey{}).(*Gate)
	return gate, ok && gate != nil
}

type acceptancePhase uint8

const (
	acceptanceActive acceptancePhase = iota
	acceptanceParked
	acceptanceFinished
	acceptanceUncertain
)

// Acceptance is a single durable reservation. Finish must run after the existing
// mutation path has returned and can no longer accept new effects. A crash leaves
// the record blocked: there is no lease, blind expiry, or automatic retry.
type Acceptance struct {
	effectStarted bool
	mu            sync.Mutex
	phase         acceptancePhase
	method        methods.Method
	gate          *Gate
	target        identity.Identity
	kind          string
	event         state.EventID
	record        record
	noop          bool
}

// Begin checks the exact resolved target, signed method reach, and enrolled
// authority before reserving acceptance. Every exposed mutation calls this after
// target resolution and before its first mutation. Enrollment cannot succeed
// while an acceptance is reserved.
func Begin(ctx context.Context, target identity.Identity, method methods.Method) (context.Context, *Acceptance, error) {
	if err := identity.Validate(target); err != nil {
		return ctx, nil, fmt.Errorf("session admission: target identity: %w", err)
	}
	nativeTarget, native := ctx.Value(nativeResumeKey{}).(identity.Identity)
	native = native && nativeTarget == target && method == methods.MethodResume
	if !native {
		if err := auth.AuthorizeMethod(ctx, method); err != nil {
			return ctx, nil, err
		}
		if err := auth.NewSessionReachAuthorizer().AuthorizeSessionReach(ctx, target.SessionID); err != nil {
			return ctx, nil, err
		}
	}
	gate, ok := From(ctx)
	if !ok {
		_, restricted := auth.MethodReachFrom(ctx)
		_, admitted := auth.SessionAdmissionFrom(ctx)
		if restricted || admitted {
			return ctx, nil, ErrUnavailable
		}
		return ctx, &Acceptance{noop: true}, nil // trusted legacy embedder; no enrollment capability
	}

	replay, hasReplay := ctx.Value(replayKey{}).(string)
	if !hasReplay {
		replay = ""
	}
	waitForReplay := (method == methods.MethodStart || method == methods.MethodUserMessage) && replay != ""
	waitUntil := time.Now().Add(5 * time.Second)
	for attempt := 0; attempt < 8; attempt++ {
		r, event, kind, err := gate.load(ctx, target)
		if err != nil {
			return ctx, nil, err
		}
		if !native {
			if err := authorize(ctx, target, r.Policy); err != nil {
				return ctx, nil, err
			}
		}
		if r.Pending != "" {
			if waitForReplay && r.Method == method && r.ReplayKey == replay && time.Now().Before(waitUntil) {
				if err := waitReplayContention(ctx); err != nil {
					return ctx, nil, err
				}
				attempt--
				continue
			}
			return ctx, nil, ErrBusy
		}
		r.Pending = string(state.NewEventID())
		r.Method = method
		r.ReplayKey = replay
		next := state.NewEventID()
		if err := gate.save(ctx, kind, event, next, r); err != nil {
			if errors.Is(err, state.ErrConditionFailed) {
				// A definitive CAS loss accepted no effect. Exact keyed retries
				// use the same bounded wait for a raced reservation as for an
				// observed pending one; eight fast races must not exhaust replay
				// while its original acceptance is making progress. Every retry
				// reloads and reauthorizes; unknown commits never enter this path.
				if waitForReplay && time.Now().Before(waitUntil) {
					if err := waitReplayContention(ctx); err != nil {
						return ctx, nil, err
					}
					attempt--
				}
				continue
			}
			return ctx, nil, err
		}
		a := &Acceptance{gate: gate, target: target, kind: kind, event: next, record: r, method: method}
		accepted := ctx
		if method == methods.MethodMCPAppsCallTool {
			accepted = tools.WithInvocationAdmission(accepted, a)
		}
		return accepted, a, nil
	}
	return ctx, nil, ErrConflict
}

func waitReplayContention(ctx context.Context) error {
	timer := time.NewTimer(10 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// Run keeps the durable reservation until the existing synchronous acceptance
// path returns. The callback must not escape asynchronous acceptance work; task
// execution already accepted by Spawn is deliberately outside this boundary.
func Run[T any](ctx context.Context, target identity.Identity, method methods.Method, fn func(context.Context) (T, error)) (T, error) {
	var zero T
	if _, assembled := From(ctx); !assembled {
		_, restricted := auth.MethodReachFrom(ctx)
		_, enrolled := auth.SessionAdmissionFrom(ctx)
		if !restricted && !enrolled {
			return fn(ctx)
		}
	}
	accepted, permit, err := Begin(ctx, target, method)
	if err != nil {
		return zero, ProtocolError(err)
	}
	result, runErr := fn(accepted)
	if runErr != nil && !isRejected(runErr) {
		// A returned error does not prove that no mutation was accepted.
		// Leave the exact durable reservation pending until its outcome is
		// reconciled; neither retry nor an epoch transition may erase it.
		if permit.parked() {
			// The native primitive owns a proved pause and no descriptor is running.
			// Closing this caller cannot discard an uncertain execution outcome.
			if err := permit.Finish(ctx); err != nil {
				return zero, ProtocolError(err)
			}
		} else {
			permit.freeze()
		}
		return zero, runErr
	}
	if err := permit.Finish(ctx); err != nil {
		return zero, ProtocolError(err)
	}
	return result, runErr
}

// Rejected marks an error whose originating service has established that no
// mutation was accepted. Use it only around validation/authorization failures
// before the acceptance side effect, never around an arbitrary driver failure.
// It preserves errors.Is/errors.As and leaves nil unchanged.
func Rejected(err error) error {
	if err == nil {
		return nil
	}
	return &rejectedError{cause: err}
}

type rejectedError struct{ cause error }

func (e *rejectedError) Error() string { return e.cause.Error() }
func (e *rejectedError) Unwrap() error { return e.cause }

func isRejected(err error) bool {
	switch failure := err.(type) { //nolint:errorlint // Inspect every unwrap edge so a joined unknown outcome cannot be hidden.
	case *rejectedError:
		return true
	case *protoerrors.Error:
		switch failure.Code {
		case protoerrors.CodeInvalidRequest, protoerrors.CodeIdentityRequired,
			protoerrors.CodeScopeMismatch, protoerrors.CodeIdentityScopeRequired,
			protoerrors.CodeAuthRejected, protoerrors.CodePayloadInvalid,
			protoerrors.CodeUnknownMethod, protoerrors.CodeNotFound,
			protoerrors.CodeRevisionConflict, protoerrors.CodeControlReceiptConflict:
			return true
		}
	case interface{ Unwrap() []error }:
		// One recognized rejection must never hide a joined unknown outcome.
		causes := failure.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !isRejected(cause) {
				return false
			}
		}
		return true
	case interface{ Unwrap() error }:
		return isRejected(failure.Unwrap())
	}
	return false
}

// RunNativeResume is for the existing OAuth callback after its opaque state and
// exact owner/pause binding have been verified by the OAuth provider. It grants
// no bearer method or general control authority. The native flow's own signed
// binding remains authoritative, while its acceptance serializes with enrollment.
func RunNativeResume[T any](ctx context.Context, target identity.Identity, fn func(context.Context) (T, error)) (T, error) {
	return Run(context.WithValue(ctx, nativeResumeKey{}, target), target, methods.MethodResume, fn)
}

// CheckMethod enforces signed method reach at a transport-independent service
// boundary without granting any identity, scope, or session admission authority.
func CheckMethod(ctx context.Context, method methods.Method) error {
	return ProtocolError(auth.AuthorizeMethod(ctx, method))
}

// ProtocolError maps admission refusals without leaking durable record details.
func ProtocolError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, identity.ErrIdentityIncomplete) {
		return protoerrors.New(protoerrors.CodeIdentityRequired, "session admission requires a complete target identity")
	}
	if errors.Is(err, ErrDenied) || errors.Is(err, auth.ErrMethodReachDenied) || errors.Is(err, auth.ErrSessionReachDenied) {
		return protoerrors.New(protoerrors.CodeScopeMismatch, "session method authority denied")
	}
	if errors.Is(err, ErrBusy) || errors.Is(err, ErrConflict) {
		return protoerrors.New(protoerrors.CodeRevisionConflict, "session admission changed or has unresolved acceptance")
	}
	var perr *protoerrors.Error
	if errors.As(err, &perr) {
		return perr
	}
	return protoerrors.New(protoerrors.CodeRuntimeError, "session admission is unavailable; acceptance must be reconciled")
}

func authorize(ctx context.Context, target identity.Identity, policy Policy) error {
	claim, ok := auth.SessionAdmissionFrom(ctx)
	if policy.Epoch == 0 {
		if ok {
			// Signed admission is authority for one existing enrollment; it
			// cannot fall back to legacy authority for an unenrolled target.
			return ErrDenied
		}
		return nil
	}
	owner, hasOwner := auth.TokenAuthorityFrom(ctx)
	_, hasMethods := auth.MethodReachFrom(ctx)
	if !ok || !hasOwner || !hasMethods || claim.Identity != target || claim.Epoch != policy.Epoch || claim.Coordinator != policy.Coordinator || owner.Issuer != policy.Issuer {
		return ErrDenied
	}
	return nil
}

// Finish clears only this reservation. Cancellation does not mean an already
// accepted mutation failed; a bounded detached cleanup attempts exact CAS. A
// failed or lost commit reply is returned as uncertainty, never inferred failure.
func (a *Acceptance) Finish(ctx context.Context) error {
	if a == nil || a.noop {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	switch a.phase {
	case acceptanceFinished:
		return nil
	case acceptanceParked:
		a.phase = acceptanceFinished
		return nil
	case acceptanceUncertain:
		return ErrBusy
	}
	a.phase = acceptanceUncertain
	if err := a.release(ctx); err != nil {
		return err
	}
	a.phase = acceptanceFinished
	return nil
}

func (a *Acceptance) release(ctx context.Context) error {
	cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	r := a.record
	r.Pending = ""
	r.Method = ""
	r.ReplayKey = ""
	if err := a.gate.save(cleanup, a.kind, a.event, state.NewEventID(), r); err != nil {
		return fmt.Errorf("session admission: release acceptance: %w", err)
	}
	return nil
}

func (a *Acceptance) parked() bool {
	if a == nil || a.noop {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.phase == acceptanceParked
}
func (a *Acceptance) freeze() {
	if a == nil {
		return
	}
	if a.noop {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.phase = acceptanceUncertain
}

// ParkForNative hands an App's acceptance back only after the existing native
// primitive has established its pause. The pause owner remains unchanged.
func (a *Acceptance) ParkForNative(ctx context.Context) error {
	if a == nil || a.method != methods.MethodMCPAppsCallTool {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.phase == acceptanceParked {
		return nil
	}
	if a.phase != acceptanceActive || a.effectStarted {
		return ErrDenied
	}
	a.phase = acceptanceUncertain
	if err := a.release(ctx); err != nil {
		return ProtocolError(err)
	}
	a.phase = acceptanceParked
	return nil
}

// EffectStarted records that an external request may now be accepted. The
// marker survives retries, preventing a later challenge from refunding unknown
// prior work. It contains no body or credential data.
func (a *Acceptance) EffectStarted(context.Context) error {
	if a == nil || a.method != methods.MethodMCPAppsCallTool {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.phase != acceptanceActive {
		return ErrDenied
	}
	a.effectStarted = true
	return nil
}

// ResumeAfterNative reacquires acceptance with the original signed authority.
// Enrollment can change while parked; stale authority then stops the original
// descriptor before any invocation. A busy native-control acceptance is waited
// on briefly, but is never cleared or considered expired.
func (a *Acceptance) ResumeAfterNative(ctx context.Context) error {
	if a == nil || a.method != methods.MethodMCPAppsCallTool {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.phase == acceptanceActive {
		return nil
	}
	if a.phase != acceptanceParked {
		return ErrDenied
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, next, err := Begin(ctx, a.target, a.method)
		if errors.Is(err, ErrBusy) && time.Now().Before(deadline) {
			timer := time.NewTimer(10 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
			continue
		}
		if err != nil {
			if !errors.Is(err, ErrBusy) && !errors.Is(err, ErrDenied) && !errors.Is(err, auth.ErrMethodReachDenied) && !errors.Is(err, auth.ErrSessionReachDenied) {
				a.phase = acceptanceUncertain
			}
			return ProtocolError(err)
		}
		a.event = next.event
		a.record = next.record
		a.phase = acceptanceActive
		return nil
	}
}

// Enroll installs or advances enrollment using the authenticated admin's issuer
// and subject. Target owner, expected epoch and +1 transition are exact. An
// existing authority can never be replaced, even by another administrator.
// This does not revoke effects already accepted before enrollment: callers must
// quiesce/reconcile those before relying on a new continuation authority.
func (g *Gate) Enroll(ctx context.Context, target identity.Identity, expected, next uint64) (Policy, error) {
	if g == nil {
		return Policy{}, ErrUnavailable
	}
	if err := auth.AuthorizeMethod(ctx, methods.MethodSessionsSetAdmission); err != nil {
		return Policy{}, err
	}
	if err := auth.NewSessionReachAuthorizer().AuthorizeSessionReach(ctx, target.SessionID); err != nil {
		return Policy{}, err
	}
	caller, verified := identity.FromVerified(ctx)
	owner, hasOwner := auth.TokenAuthorityFrom(ctx)
	if !verified || !auth.HasScope(ctx, auth.ScopeAdmin) || !hasOwner || auth.ValidateSessionAdmissionOwner(owner.Issuer, owner.Subject) != nil || caller.TenantID != target.TenantID || caller.UserID != target.UserID {
		return Policy{}, ErrDenied
	}
	if err := identity.Validate(target); err != nil {
		return Policy{}, ErrDenied
	}
	if expected == ^uint64(0) || next != expected+1 {
		return Policy{}, ErrConflict
	}
	r, event, kind, err := g.load(ctx, target)
	if err != nil {
		return Policy{}, err
	}
	if r.Policy.Epoch > 0 && (r.Policy.Issuer != owner.Issuer || r.Policy.Coordinator != owner.Subject) {
		return Policy{}, ErrDenied
	}
	if r.Policy.Epoch == next && r.Policy.Issuer == owner.Issuer && r.Policy.Coordinator == owner.Subject {
		return r.Policy, nil
	}
	if r.Pending != "" {
		return Policy{}, ErrBusy
	}
	if r.Policy.Epoch != expected {
		return Policy{}, ErrConflict
	}
	r.Policy = Policy{Identity: target, Issuer: owner.Issuer, Coordinator: owner.Subject, Epoch: next}
	if err := g.save(ctx, kind, event, state.NewEventID(), r); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return Policy{}, ErrConflict
		}
		return Policy{}, err
	}
	return r.Policy, nil
}

// PolicyFor reads exact session enrollment without reserving a mutation.
func (g *Gate) PolicyFor(ctx context.Context, target identity.Identity) (Policy, error) {
	r, _, _, err := g.load(ctx, target)
	return r.Policy, err
}

func slot(target identity.Identity) (string, error) {
	// Freeze the key encoding independently of Identity's field tags/schema.
	b, err := json.Marshal([3]string{target.TenantID, target.UserID, target.SessionID})
	if err != nil {
		return "", fmt.Errorf("session admission: encode slot: %w", err)
	}
	digest := sha256.Sum256(b)
	return kindPrefix + hex.EncodeToString(digest[:]), nil
}

func (g *Gate) load(ctx context.Context, target identity.Identity) (record, state.EventID, string, error) {
	if err := identity.Validate(target); err != nil {
		return record{}, "", "", ErrDenied
	}
	kind, err := slot(target)
	if err != nil {
		return record{}, "", "", err
	}
	stored, err := g.store.Load(ctx, identity.InternalCoordinationQuadruple(), kind)
	if errors.Is(err, state.ErrNotFound) {
		return record{Schema: 1, Policy: Policy{Identity: target}}, "", kind, nil
	}
	if err != nil {
		return record{}, "", kind, fmt.Errorf("session admission: load: %w", err)
	}
	var r record
	decoder := json.NewDecoder(bytes.NewReader(stored.Bytes))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&r); err != nil {
		return record{}, "", kind, ErrCorrupt
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return record{}, "", kind, ErrCorrupt
	}
	if r.Schema != 1 || r.Policy.Identity != target ||
		(r.Policy.Epoch == 0 && (r.Policy.Issuer != "" || r.Policy.Coordinator != "")) ||
		(r.Policy.Epoch > 0 && auth.ValidateSessionAdmissionOwner(r.Policy.Issuer, r.Policy.Coordinator) != nil) ||
		(r.Pending == "") != (r.Method == "") ||
		(r.Pending == "" && r.ReplayKey != "") ||
		(r.Pending != "" && !methods.IsValidMethod(r.Method)) {
		return record{}, "", kind, ErrCorrupt
	}
	return r, stored.ID, kind, nil
}

func (g *Gate) save(ctx context.Context, kind string, previous, next state.EventID, r record) error {
	b, err := json.Marshal(r)
	if err != nil {
		return fmt.Errorf("session admission: encode: %w", err)
	}
	q := identity.InternalCoordinationQuadruple()
	if err := g.store.SaveIf(ctx, []state.SlotExpectation{state.InternalSlotExpectation(q, kind, previous)}, state.NewInternalRecord(next, q, kind, b)); err != nil {
		return fmt.Errorf("session admission: save: %w", err)
	}
	return nil
}
