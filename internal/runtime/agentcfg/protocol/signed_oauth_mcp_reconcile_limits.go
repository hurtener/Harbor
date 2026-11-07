package protocol

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
)

// ErrSignedCapabilityAttachSuppressed reports that one pair's run-start
// attachment was skipped because its previous private preparation failed and
// its bounded retry window has not elapsed. It is not a new failure: nothing
// durable changed and no credential exchange or downstream request was made.
// Callers compare with errors.Is to log it below error level.
var ErrSignedCapabilityAttachSuppressed = errors.New("signed capability attachment suppressed by its bounded retry window")

const (
	// signedAttachBackoffBase is the first retry delay after a failed private
	// preparation of one pair.
	signedAttachBackoffBase = 30 * time.Second
	// signedAttachBackoffMax caps exponential growth so a persistently
	// failing pair is still retried periodically without operator action.
	signedAttachBackoffMax = 5 * time.Minute
	// signedAttachBackoffEntries bounds the per-pair failure memory. At the cap
	// the entry closest to (or past) its retry time is evicted, which can only
	// make a pair retry sooner, never suppress it longer.
	signedAttachBackoffEntries = 1024
	// signedReconcileContinuationEntries bounds the per-cursor scan
	// continuations. Losing one restarts that cursor's bounded scan at the
	// first page, which is always correct.
	signedReconcileContinuationEntries = 4096
)

// signedPreparationError marks a failure of the external private-preparation
// steps (provider construction, credential exchange, connect and discovery).
// Only these failures open a retry window: local revalidation and CAS races
// must converge on the very next pass.
type signedPreparationError struct{ err error }

func (e *signedPreparationError) Error() string { return e.err.Error() }
func (e *signedPreparationError) Unwrap() error { return e.err }

type signedAttachBackoffKey struct {
	scope    agentcfg.ConfigScope
	tenant   string
	agent    string
	user     string
	session  string
	provider string
}

type signedAttachBackoffState struct {
	fingerprint string
	failures    int
	suppressed  int
	nextAttempt time.Time
}

// signedAttachBackoff is a bounded, process-local negative cache keyed by one
// exact pair slot. The fingerprint binds the durable revision, the pair's
// authority receipt and the operation phase, so any registration, rotation,
// removal or phase change retries immediately. It never grants anything: a
// suppressed attempt simply does not run.
type signedAttachBackoff struct {
	mu      sync.Mutex
	now     func() time.Time
	entries map[signedAttachBackoffKey]*signedAttachBackoffState
}

func newSignedAttachBackoff() *signedAttachBackoff {
	return &signedAttachBackoff{now: time.Now, entries: make(map[signedAttachBackoffKey]*signedAttachBackoffState)}
}

func signedAttachBackoffDelay(failures int) time.Duration {
	d := signedAttachBackoffBase
	for i := 1; i < failures; i++ {
		d *= 2
		if d >= signedAttachBackoffMax {
			return signedAttachBackoffMax
		}
	}
	return d
}

// admit returns a wrapped ErrSignedCapabilityAttachSuppressed while the exact
// slot and fingerprint are inside their retry window.
func (b *signedAttachBackoff) admit(key signedAttachBackoffKey, fingerprint string) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.entries[key]
	if st == nil {
		return nil
	}
	if st.fingerprint != fingerprint {
		delete(b.entries, key)
		return nil
	}
	if !b.now().Before(st.nextAttempt) {
		return nil
	}
	st.suppressed++
	return fmt.Errorf("%w: failures=%d suppressed_attempts=%d", ErrSignedCapabilityAttachSuppressed, st.failures, st.suppressed)
}

func (b *signedAttachBackoff) fail(key signedAttachBackoffKey, fingerprint string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	st := b.entries[key]
	if st == nil || st.fingerprint != fingerprint {
		if st == nil && len(b.entries) >= signedAttachBackoffEntries {
			b.evictLocked(now)
		}
		st = &signedAttachBackoffState{fingerprint: fingerprint}
		b.entries[key] = st
	}
	st.failures++
	st.suppressed = 0
	st.nextAttempt = now.Add(signedAttachBackoffDelay(st.failures))
}

func (b *signedAttachBackoff) succeed(key signedAttachBackoffKey) {
	if b == nil {
		return
	}
	b.mu.Lock()
	delete(b.entries, key)
	b.mu.Unlock()
}

// ownerPrepared clears every retry window of the same owner slot after a
// successful private preparation for any of its pairs.
func (b *signedAttachBackoff) ownerPrepared(slot signedAttachBackoffKey) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	for key := range b.entries {
		if key.scope == slot.scope && key.tenant == slot.tenant && key.agent == slot.agent && key.user == slot.user && key.session == slot.session {
			delete(b.entries, key)
		}
	}
}

// evictLocked drops every expired entry, or failing that the entry whose retry
// is due soonest. Eviction only shortens a suppression.
func (b *signedAttachBackoff) evictLocked(now time.Time) {
	var (
		victim     signedAttachBackoffKey
		victimAt   time.Time
		haveVictim bool
	)
	for key, st := range b.entries {
		if !now.Before(st.nextAttempt) {
			delete(b.entries, key)
			continue
		}
		if !haveVictim || st.nextAttempt.Before(victimAt) {
			victim, victimAt, haveVictim = key, st.nextAttempt, true
		}
	}
	if len(b.entries) >= signedAttachBackoffEntries && haveVictim {
		delete(b.entries, victim)
	}
}

func (b *signedAttachBackoff) len() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.entries)
}

func signedAttachSlot(ctx context.Context, q identity.Quadruple, agentID string, pair *agentcfg.SignedOAuthMCPPair) signedAttachBackoffKey {
	key := signedAttachBackoffKey{scope: signedOAuthMCPConfigScope(ctx), tenant: q.TenantID, agent: agentID, provider: pair.ProviderName}
	if key.scope == agentcfg.ConfigScopeUser {
		// A user-tier preparation is the acting session's exchange: a refusal
		// for one session is no evidence about a freshly authenticated other.
		key.user, key.session = q.UserID, q.SessionID
	}
	return key
}

func signedAttachFingerprint(pair *agentcfg.SignedOAuthMCPPair, revision agentcfg.Revision, op agentcfg.SignedOAuthMCPOperation) string {
	return strings.Join([]string{
		revision.RevisionID, revision.ContentHash, pair.AuthorityOperationKind, pair.AuthorityJTIHash,
		op.Fingerprint, string(op.Phase), fmt.Sprint(op.AuthorityGeneration),
	}, "\x00")
}

// signedReconcileGates serializes reconciliation per exact owner slot instead
// of runtime-wide. Entries are reference counted and removed when the last
// waiter releases, so memory is bounded by concurrent callers.
type signedReconcileGates struct {
	mu    sync.Mutex
	gates map[string]*signedReconcileGate
}

type signedReconcileGate struct {
	ch   chan struct{}
	refs int
}

func newSignedReconcileGates() *signedReconcileGates {
	return &signedReconcileGates{gates: make(map[string]*signedReconcileGate)}
}

func (g *signedReconcileGates) acquire(ctx context.Context, key string) (func(), error) {
	g.mu.Lock()
	gate := g.gates[key]
	if gate == nil {
		gate = &signedReconcileGate{ch: make(chan struct{}, 1)}
		g.gates[key] = gate
	}
	gate.refs++
	g.mu.Unlock()
	select {
	case gate.ch <- struct{}{}:
		return func() {
			<-gate.ch
			g.put(key, gate)
		}, nil
	case <-ctx.Done():
		g.put(key, gate)
		return nil, ctx.Err()
	}
}

func (g *signedReconcileGates) put(key string, gate *signedReconcileGate) {
	g.mu.Lock()
	gate.refs--
	if gate.refs == 0 && g.gates[key] == gate {
		delete(g.gates, key)
	}
	g.mu.Unlock()
}

func (g *signedReconcileGates) len() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.gates)
}

// signedReconcileGateKey names the exact slot whose durable receipts one pass
// mutates: agent-tier pairs are (tenant, agent) scoped and user-tier pairs are
// (tenant, user, agent) scoped. Different owners never share a gate; the two
// tiers of one agent touch disjoint receipts and are filtered accordingly.
func signedReconcileGateKey(scope agentcfg.ConfigScope, q identity.Quadruple, agentID string) string {
	parts := []string{string(scope), q.TenantID, agentID}
	if scope == agentcfg.ConfigScopeUser {
		parts = append(parts, q.UserID)
	}
	return strings.Join(parts, "\x00")
}

// signedReconcileContinuations is the bounded, synchronized cursor store for
// the per-slot maintenance scan.
type signedReconcileContinuations struct {
	mu      sync.Mutex
	entries map[string]string
}

func newSignedReconcileContinuations() *signedReconcileContinuations {
	return &signedReconcileContinuations{entries: make(map[string]string)}
}

func (c *signedReconcileContinuations) get(key string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[key]
}

func (c *signedReconcileContinuations) set(key, continuation string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if continuation == "" {
		delete(c.entries, key)
		return
	}
	if _, exists := c.entries[key]; !exists && len(c.entries) >= signedReconcileContinuationEntries {
		for victim := range c.entries {
			delete(c.entries, victim)
			break
		}
	}
	c.entries[key] = continuation
}

// signedOwnerPreparationContext returns a context whose only identity is the
// frozen pair owner. It is detached from the triggering run's values (its
// verified identity, run quadruple and per-call capabilities) but keeps the
// caller's cancellation, deadline, trace span and signed config scope.
func signedOwnerPreparationContext(ctx context.Context, owner identity.Identity) (context.Context, context.CancelFunc, error) {
	base := trace.ContextWithSpan(context.Background(), trace.SpanFromContext(ctx))
	base = withSignedOAuthMCPConfigScope(base, signedOAuthMCPConfigScope(ctx))
	var cancel context.CancelFunc
	if deadline, ok := ctx.Deadline(); ok {
		base, cancel = context.WithDeadline(base, deadline)
	} else {
		base, cancel = context.WithCancel(base)
	}
	stop := context.AfterFunc(ctx, cancel)
	release := func() {
		stop()
		cancel()
	}
	ownerCtx, err := identity.With(base, owner)
	if err != nil {
		release()
		return nil, nil, err
	}
	return ownerCtx, release, nil
}
