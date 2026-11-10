package serve

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/telemetry"
)

// sessionPrepareLimits bounds ahead-of-time run-start preparation. They are
// runtime constants (see defaultSessionPrepareLimits), not operator knobs;
// tests construct tighter values.
type sessionPrepareLimits struct {
	// Freshness is how long a successful preparation may replace the
	// run-start reconcile (subject to an unchanged input stamp), and how
	// long a repeated prepare for the same principal is a no-op.
	Freshness time.Duration
	// FailedRetry is how long a failed or unstable preparation suppresses a
	// repeated prepare. It never replaces a run-start reconcile.
	FailedRetry time.Duration
	// JoinWait bounds how long run start waits for an in-flight preparation
	// before falling back to its own reconcile.
	JoinWait time.Duration
	// WorkBudget bounds one background preparation end to end.
	WorkBudget time.Duration
	// MaxEntries caps retained preparation records; Workers caps concurrent
	// background preparations.
	MaxEntries int
	Workers    int
}

func defaultSessionPrepareLimits() sessionPrepareLimits {
	return sessionPrepareLimits{
		Freshness:   90 * time.Second,
		FailedRetry: 10 * time.Second,
		JoinWait:    10 * time.Second,
		// Two reconcile passes, each with the bounded connection sweep, plus
		// the route selection.
		WorkBudget: 2*reconcileConnectionsSweepBudget + 15*time.Second,
		MaxEntries: 1024,
		Workers:    4,
	}
}

// sessionPrepareKey is the coalescing principal. Prepared reconcile state is
// durable per (tenant, Agent) and (tenant, user, Agent) slot, not per session,
// so the key deliberately omits the session; the input stamp still binds the
// session overlay when one exists.
type sessionPrepareKey struct {
	tenant, user, agent string
}

// sessionPrepareAdmission is the sealed reach-admission subject a record was
// prepared under, minus the session (reconcile state is slot-scoped). Only a
// run whose own restored admission names the same runtime, tenant, user and
// agent may consume the record.
type sessionPrepareAdmission struct {
	runtime, tenant, user, agent string
}

func admissionOf(subject tasks.AgentReachAdmissionSubject) sessionPrepareAdmission {
	return sessionPrepareAdmission{runtime: subject.RuntimeID, tenant: subject.TenantID, user: subject.UserID, agent: subject.EffectiveAgentID}
}

type sessionPrepareEntry struct {
	done      chan struct{}
	admission sessionPrepareAdmission // immutable after creation
	// The fields below are guarded by sessionPreparer.mu.
	completed   bool
	completedAt time.Time
	// observedAt is when the final pass's input observation (its leading
	// stamp read) began; freshness is measured from it, never from
	// completion, so a slow pass cannot extend the reuse window.
	observedAt time.Time
	ok         bool
	stamp      string
}

// ErrSessionPrepareUnadmitted is returned when the preparer is called without
// the admitted authority the control surface seats (verified identity, reach
// narrowed to the agent, sealed reach admission for exactly the subject).
var ErrSessionPrepareUnadmitted = errors.New("session prepare: request context carries no matching admitted authority")

// admittedSubject checks ctx carries the exact admitted authority for
// (id, agentID) and returns its admission subject.
func admittedSubject(ctx context.Context, id identity.Identity, agentID string) (sessionPrepareAdmission, bool) {
	verified, present := identity.FromVerified(ctx)
	if !present || verified != id {
		return sessionPrepareAdmission{}, false
	}
	subject, ok := tasks.AgentReachAdmissionSubjectFrom(ctx)
	if !ok || subject.TenantID != id.TenantID || subject.UserID != id.UserID ||
		subject.SessionID != id.SessionID || subject.EffectiveAgentID != agentID {
		return sessionPrepareAdmission{}, false
	}
	return admissionOf(subject), true
}

// sessionPreparer runs run-start preparation ahead of a `start` and lets the
// run start reuse it. It is a compiled artifact: every field except the
// mutex-guarded entries/closed state is immutable after construction, and all
// per-request state travels in ctx and arguments.
type sessionPreparer struct {
	logger *slog.Logger
	// reconcile runs one full run-start reconcile pass and reports whether
	// every leg completed without error.
	reconcile func(ctx context.Context, agentID string, q identity.Quadruple) bool
	// stamp returns the input-addressed reconcile/catalog stamp; ok=false
	// means the wired dependencies cannot report one (no fast path).
	stamp  func(ctx context.Context, id identity.Identity, agentID string) (string, bool, error)
	route  llm.ProviderRouteConfig
	now    func() time.Time
	limits sessionPrepareLimits

	sem     chan struct{}
	baseCtx context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	mu      sync.Mutex
	closed  bool
	entries map[sessionPrepareKey]*sessionPrepareEntry
}

func newSessionPreparer(logger *slog.Logger, limits sessionPrepareLimits, route llm.ProviderRouteConfig,
	reconcile func(context.Context, string, identity.Quadruple) bool,
	stamp func(context.Context, identity.Identity, string) (string, bool, error),
) *sessionPreparer {
	if logger == nil {
		logger = slog.Default()
	}
	if limits.Workers <= 0 {
		limits.Workers = 1
	}
	if limits.MaxEntries <= 0 {
		limits.MaxEntries = 1
	}
	// The workers outlive the request that scheduled them; they are bounded
	// by WorkBudget and cancelled by Close, never by a caller disconnect.
	base, cancel := context.WithCancel(context.Background())
	return &sessionPreparer{
		logger: logger, reconcile: reconcile, stamp: stamp, route: route, now: time.Now, limits: limits,
		sem: make(chan struct{}, limits.Workers), baseCtx: base, cancel: cancel,
		entries: make(map[sessionPrepareKey]*sessionPrepareEntry),
	}
}

var _ protocol.SessionPreparer = (*sessionPreparer)(nil)

// PrepareSession implements protocol.SessionPreparer. ctx must be the admitted
// request context the control surface seats: the verified identity equal to
// req.Identity, Agent reach narrowed to exactly the target agent, and a sealed
// reach admission for exactly that subject (only the admission authority can
// mint one). Anything else is refused with ErrSessionPrepareUnadmitted and
// creates no record. The background work runs under a detached copy of
// exactly that authority, and the record carries the admission subject.
func (p *sessionPreparer) PrepareSession(ctx context.Context, req protocol.SessionPrepareRequest) (types.PrepareState, error) {
	if err := identity.Validate(req.Identity); err != nil {
		return "", fmt.Errorf("session prepare: %w", err)
	}
	if strings.TrimSpace(req.EffectiveAgentID) == "" {
		return "", errors.New("session prepare: effective agent is empty")
	}
	admission, admitted := admittedSubject(ctx, req.Identity, req.EffectiveAgentID)
	if reach, present := auth.AgentReachFrom(ctx); !admitted || !present || len(reach) != 1 || reach[0] != req.EffectiveAgentID {
		return "", ErrSessionPrepareUnadmitted
	}
	key := sessionPrepareKey{tenant: req.Identity.TenantID, user: req.Identity.UserID, agent: req.EffectiveAgentID}
	routeReq, needRoute := p.routeRequest(req)
	if needRoute {
		if _, cached := p.route.SelectionCache.Lookup(llm.ProviderRouteSelectionKeyFor(routeReq), p.now()); cached {
			needRoute = false
		}
	}

	now := p.now()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return "", protocol.ErrSessionPrepareUnavailable
	}
	doReconcile, state := true, types.PrepareStateScheduled
	if entry := p.entries[key]; entry != nil {
		if !entry.completed {
			p.mu.Unlock()
			return types.PrepareStateInFlight, nil
		}
		fresh := entry.ok && entry.admission == admission && now.Before(entry.observedAt.Add(p.limits.Freshness))
		if fresh || (!entry.ok && now.Sub(entry.completedAt) < p.limits.FailedRetry) {
			doReconcile, state = false, types.PrepareStateFresh
		}
	}
	if !doReconcile && !needRoute {
		p.mu.Unlock()
		return state, nil
	}
	select {
	case p.sem <- struct{}{}:
	default:
		p.mu.Unlock()
		if state == types.PrepareStateFresh {
			return state, nil
		}
		return types.PrepareStateBusy, nil
	}
	var entry *sessionPrepareEntry
	if doReconcile {
		p.evictLocked(now)
		if len(p.entries) >= p.limits.MaxEntries {
			<-p.sem
			p.mu.Unlock()
			return types.PrepareStateBusy, nil
		}
		entry = &sessionPrepareEntry{done: make(chan struct{}), admission: admission}
		p.entries[key] = entry
	}
	p.wg.Add(1)
	p.mu.Unlock()

	runID := routeReq.LogicalRunID
	workCtx, err := identity.WithRun(context.WithoutCancel(ctx), req.Identity, runID)
	if err != nil {
		p.finish(entry, false, "", time.Time{})
		<-p.sem
		p.wg.Done()
		return "", fmt.Errorf("session prepare: run identity: %w", err)
	}
	go p.work(workCtx, entry, identity.Quadruple{Identity: req.Identity, RunID: runID}, req.EffectiveAgentID, routeReq, needRoute)
	return state, nil
}

// routeRequest builds the trusted, credential-free selection request for the
// prepared route. Run, task and call IDs are opaque preparation correlation
// values minted here; they are never task identities. The returned request
// always carries a fresh LogicalRunID, which also names the preparation's run
// scope.
func (p *sessionPreparer) routeRequest(req protocol.SessionPrepareRequest) (llm.ProviderRouteRequest, bool) {
	token := newSessionPrepareToken()
	out := llm.ProviderRouteRequest{
		TenantID: req.Identity.TenantID, UserID: req.Identity.UserID, SessionID: req.Identity.SessionID,
		LogicalRunID: "prepare-run-" + token, EffectiveAgentID: req.EffectiveAgentID,
		TaskID: "prepare-task-" + token, LogicalCallID: "prepare-call-" + token,
		ModelProfileSupported: true, Purpose: llm.ProviderRoutePurposeRun,
	}
	if req.ProviderRoute == nil || p.route.Resolver == nil || p.route.SelectionCache == nil {
		return out, false
	}
	route := *req.ProviderRoute
	out.RouteID, out.RouteGeneration = route.RouteID, route.RouteGeneration
	out.ProviderConnectionID, out.ProviderConnectionGeneration = route.ProviderConnectionID, route.ProviderConnectionGeneration
	out.CredentialAssetGeneration, out.ModelSelector = route.CredentialAssetGeneration, route.ModelSelector
	return out, true
}

func (p *sessionPreparer) work(ctx context.Context, entry *sessionPrepareEntry, q identity.Quadruple,
	agentID string, routeReq llm.ProviderRouteRequest, needRoute bool,
) {
	defer p.wg.Done()
	defer func() { <-p.sem }()
	ctx, cancel := context.WithTimeout(ctx, p.limits.WorkBudget)
	defer cancel()
	stop := context.AfterFunc(p.baseCtx, cancel)
	defer stop()

	started := p.now()
	routeOutcome := "none"
	if needRoute {
		switch cached, err := llm.PrepareProviderRouteSelection(ctx, p.route, routeReq); {
		case err != nil:
			routeOutcome = "failed"
		case cached:
			routeOutcome = "cached"
		default:
			routeOutcome = "selected"
		}
	}
	routeElapsed := p.now().Sub(started)

	outcome, passes := "skipped", 0
	if entry != nil {
		var ok bool
		var stamp string
		var observedAt time.Time
		outcome, passes, ok, stamp, observedAt = p.converge(ctx, agentID, q)
		p.finish(entry, ok, stamp, observedAt)
	}
	p.logger.LogAttrs(ctx, slog.LevelInfo, "session.prepare",
		slog.String("tenant_id", q.TenantID), slog.String("user_id", q.UserID), slog.String("session_id", q.SessionID),
		slog.String("agent_id", agentID), slog.String("outcome", outcome), slog.Int("reconcile_passes", passes),
		slog.String("route", routeOutcome),
		slog.Float64("route_ms", float64(routeElapsed)/float64(time.Millisecond)),
		slog.Float64("duration_ms", float64(p.now().Sub(started))/float64(time.Millisecond)))
}

// converge runs the run-start reconcile until its inputs are observed stable.
// A pass whose before/after stamps agree ran on exactly the inputs the stamp
// names (write identities are unique and generations monotonic, so no ABA),
// which is the condition a later run start may rely on. A first pass that
// moves the stamp (its own attaches) is verified by one more pass; a second
// move means concurrent change and the result is not retained. observedAt is
// the instant the accepted pass's leading stamp read began: the inputs a
// reusing run relies on are those observed from that instant on.
func (p *sessionPreparer) converge(ctx context.Context, agentID string, q identity.Quadruple) (outcome string, passes int, ok bool, stamp string, observedAt time.Time) {
	if p.stamp == nil {
		p.reconcile(ctx, agentID, q)
		return "warmed", 1, false, "", time.Time{}
	}
	observed := p.now()
	before, stampOK, err := p.stamp(ctx, q.Identity, agentID)
	if err != nil {
		return "failed", 0, false, "", time.Time{}
	}
	for passes < 2 {
		passes++
		clean := p.reconcile(ctx, agentID, q)
		if ctx.Err() != nil {
			return "cancelled", passes, false, "", time.Time{}
		}
		if !clean {
			return "failed", passes, false, "", time.Time{}
		}
		if !stampOK {
			return "warmed", passes, false, "", time.Time{}
		}
		nextObserved := p.now()
		after, afterOK, err := p.stamp(ctx, q.Identity, agentID)
		if err != nil || !afterOK {
			return "failed", passes, false, "", time.Time{}
		}
		if after == before {
			return "ready", passes, true, after, observed
		}
		before, observed = after, nextObserved
	}
	return "unstable", passes, false, "", time.Time{}
}

func (p *sessionPreparer) finish(entry *sessionPrepareEntry, ok bool, stamp string, observedAt time.Time) {
	if entry == nil {
		return
	}
	// A failed record stays retained too: it suppresses repeated prepares
	// for FailedRetry and is reclaimed by eviction; consume never reuses it.
	p.mu.Lock()
	entry.completed, entry.completedAt, entry.ok, entry.stamp, entry.observedAt = true, p.now(), ok, stamp, observedAt
	p.mu.Unlock()
	close(entry.done)
}

// evictLocked reclaims expired records, then the oldest completed record when
// the cap is still reached. In-flight records are never evicted; they are
// bounded by Workers. Callers hold p.mu.
func (p *sessionPreparer) evictLocked(now time.Time) {
	if len(p.entries) < p.limits.MaxEntries {
		return
	}
	retain := max(p.limits.Freshness, p.limits.FailedRetry)
	var oldestKey sessionPrepareKey
	var oldest time.Time
	found := false
	for k, e := range p.entries {
		if !e.completed {
			continue
		}
		if now.Sub(e.completedAt) >= retain {
			delete(p.entries, k)
			continue
		}
		if !found || e.completedAt.Before(oldest) {
			oldestKey, oldest, found = k, e.completedAt, true
		}
	}
	if len(p.entries) >= p.limits.MaxEntries && found {
		delete(p.entries, oldestKey)
	}
}

// consume decides whether run start may skip its reconcile. It returns Hit
// or Joined only for a successful preparation of the same (tenant, user,
// Agent), prepared under the same sealed admission subject (runtime, tenant,
// user, agent) as the run's own restored admission on ctx, whose input stamp
// is unchanged now, and whose freshness deadline — observedAt + Freshness —
// has not passed when the decision is taken (checked again after the stamp
// read). Everything else falls back to the normal reconcile.
func (p *sessionPreparer) consume(ctx context.Context, id identity.Identity, agentID string) telemetry.TurnPrepared {
	if p == nil || p.stamp == nil {
		return telemetry.TurnPreparedNone
	}
	key := sessionPrepareKey{tenant: id.TenantID, user: id.UserID, agent: agentID}
	p.mu.Lock()
	entry := p.entries[key]
	p.mu.Unlock()
	if entry == nil {
		return telemetry.TurnPreparedMiss
	}
	joined := false
	select {
	case <-entry.done:
	default:
		joined = true
		timer := time.NewTimer(p.limits.JoinWait)
		defer timer.Stop()
		select {
		case <-entry.done:
		case <-timer.C:
			return telemetry.TurnPreparedStale
		case <-ctx.Done():
			return telemetry.TurnPreparedStale
		}
	}
	runAdmission, admitted := admittedSubject(ctx, id, agentID)
	if !admitted || runAdmission != entry.admission {
		return telemetry.TurnPreparedStale
	}
	p.mu.Lock()
	ok, stamp, observedAt := entry.ok, entry.stamp, entry.observedAt
	p.mu.Unlock()
	deadline := observedAt.Add(p.limits.Freshness)
	if !ok || !p.now().Before(deadline) {
		return telemetry.TurnPreparedStale
	}
	current, stampOK, err := p.stamp(ctx, id, agentID)
	if err != nil || !stampOK || current != stamp {
		return telemetry.TurnPreparedStale
	}
	// The stamp read may itself be slow: the reuse decision is taken now,
	// so the deadline must still hold now.
	if !p.now().Before(deadline) {
		return telemetry.TurnPreparedStale
	}
	if joined {
		return telemetry.TurnPreparedJoined
	}
	return telemetry.TurnPreparedHit
}

// Close refuses new work, cancels in-flight preparations and joins them.
func (p *sessionPreparer) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.cancel()
	p.wg.Wait()
}

func (p *sessionPreparer) entryCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

func newSessionPrepareToken() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand never fails on supported platforms; a time-derived
		// token keeps the correlation value unique enough if it ever does.
		return fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// SessionPrepareBridge late-binds the run-loop driver's preparer to the
// control surface, which assembly constructs before the driver. It is
// internally synchronized: Bind is called once during boot, before serving.
// An unbound bridge answers ErrSessionPrepareUnavailable (the route reads as
// absent).
type SessionPrepareBridge struct {
	mu     sync.RWMutex
	target protocol.SessionPreparer
}

// NewSessionPrepareBridge returns an unbound bridge.
func NewSessionPrepareBridge() *SessionPrepareBridge { return &SessionPrepareBridge{} }

// Bind sets the preparer the bridge forwards to.
func (b *SessionPrepareBridge) Bind(target protocol.SessionPreparer) {
	b.mu.Lock()
	b.target = target
	b.mu.Unlock()
}

// PrepareSession implements protocol.SessionPreparer.
func (b *SessionPrepareBridge) PrepareSession(ctx context.Context, req protocol.SessionPrepareRequest) (types.PrepareState, error) {
	b.mu.RLock()
	target := b.target
	b.mu.RUnlock()
	if target == nil {
		return "", protocol.ErrSessionPrepareUnavailable
	}
	return target.PrepareSession(ctx, req)
}
