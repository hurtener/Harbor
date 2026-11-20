package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tools"
)

// GateDeps bundles the collaborators an ApprovalGate needs. The
// production binary wires all five; tests may stub the bus / redactor
// with in-memory equivalents that satisfy the same interface.
//
// Every field is mandatory — the §13 amendment forbids silent default
// fallbacks on operator-facing seams. A nil field is rejected at
// construction (NewApprovalGate returns the matching sentinel).
type GateDeps struct {
	// Policy decides per-request whether approval is required.
	// Mandatory — there is no silent-auto-approve default
	// (CLAUDE.md §13 amendment).
	Policy ApprovalPolicy
	// Coordinator is the unified pause/resume primitive.
	// Mandatory — RunGuarded parks a pause record on it; an APPROVE /
	// REJECT control event resumes through it.
	Coordinator pauseresume.Coordinator
	// Bus is the event bus the gate emits
	// tool.approval_requested / tool.approved / tool.rejected on.
	// Mandatory — an unobservable gate is a design smell.
	Bus events.EventBus
	// Redactor processes the ToolApprovalRequestedPayload's
	// ArgsSummary before emission. Mandatory — CLAUDE.md §7 rule 6
	// requires audit redaction on every emit path.
	Redactor audit.Redactor
	// Authorizer is the injected resolve-privilege check.
	// Mandatory — an approval gate with no resolve privilege
	// check is a misconfiguration, not a permissive mode. Direct
	// construction wires the runtime-vocabulary default
	// (NewIdentityAuthorizer); wire-driven assembly injects the
	// Protocol-side adapter (`internal/server.ProtocolScopeAuthorizer`).
	Authorizer ResolveAuthorizer
}

// ApprovalGate is the V1 concrete approval-gate artifact.
//
// Concurrent reuse contract: every field below is either set
// once at construction (deps + closed flag) or is the pending map
// guarded by mu. There is no per-run state on the struct itself.
// One gate is safe to share across N goroutines; concurrent_test.go
// pins N≥128 under -race.
type ApprovalGate struct {
	policy      ApprovalPolicy
	coordinator pauseresume.Coordinator
	bus         events.EventBus
	redactor    audit.Redactor
	authorizer  ResolveAuthorizer

	// mu guards pending. Documented internally-synchronised per the
	// concurrent-reuse contract (CLAUDE.md §5).
	mu sync.Mutex
	// pending is the per-pause resolution channel registry. Keyed by
	// `pauseresume.Token` because that is what `ResolveApproval`
	// receives. The waitingEntry holds (a) the resolution channel the
	// RunGuarded caller blocks on, and (b) the original ApprovalRequest
	// so the gate can publish the right events without re-deriving
	// from the pause payload.
	pending map[pauseresume.Token]*waitingEntry

	closed atomic.Bool
}

// waitingEntry is the gate's per-pause record. Allocated fresh per
// RunGuarded invocation; lives on the gate's pending map until
// ResolveApproval delivers a decision or the waiter stops waiting.
type waitingEntry struct {
	// req is the original request, held so the gate can publish
	// tool.approved / tool.rejected with the right Tool name.
	req *ApprovalRequest
	// runID is the run that requested the approval. A resolver whose
	// context names a different run cannot resolve it.
	runID string
	// retired is set (under the gate's mu) when the waiter stops
	// waiting. A resolver never restores a retired entry, so the
	// original arguments are not retained for a waiter that is gone.
	retired bool
	// resolve is the per-entry signal channel. ResolveApproval sends
	// the decision; RunGuarded selects on it + ctx.Done(). Buffered
	// so a resolver that wins the race against a cancelled ctx does
	// not block forever sending.
	resolve chan resolution
}

// resolution carries the decision + reason from ResolveApproval to the
// RunGuarded goroutine. Internal — never exported.
type resolution struct {
	decision  ApprovalDecision
	reason    string
	decider   pauseresume.Decider
	decidedAt time.Time
}

// NewApprovalGate constructs an ApprovalGate. Every dep is mandatory;
// a nil dep returns the matching sentinel error (no silent stub
// default — CLAUDE.md §13 amendment).
func NewApprovalGate(deps GateDeps) (*ApprovalGate, error) {
	if deps.Policy == nil {
		return nil, ErrPolicyRequired
	}
	if deps.Coordinator == nil {
		return nil, ErrCoordinatorRequired
	}
	if deps.Bus == nil {
		return nil, ErrBusRequired
	}
	if deps.Redactor == nil {
		return nil, ErrRedactorRequired
	}
	if deps.Authorizer == nil {
		return nil, ErrAuthorizerRequired
	}
	return &ApprovalGate{
		policy:      deps.Policy,
		coordinator: deps.Coordinator,
		bus:         deps.Bus,
		redactor:    deps.Redactor,
		authorizer:  deps.Authorizer,
		pending:     make(map[pauseresume.Token]*waitingEntry),
	}, nil
}

// Close idempotently retires the gate. Any in-flight RunGuarded calls
// see their pending entries dropped from the map; the Coordinator's
// pause records remain (the gate is not the source of truth for the
// pause record — the Coordinator is). After Close, RunGuarded /
// ResolveApproval return ErrGateClosed.
//
// Close is safe to call concurrently with RunGuarded — atomic flag +
// the mutex on map operations cover the race. Mirrors the
// Provider.Close pattern.
func (g *ApprovalGate) Close(_ context.Context) error {
	if g.closed.Swap(true) {
		// Already closed — idempotent no-op.
		return nil
	}
	g.mu.Lock()
	for tok, entry := range g.pending {
		// Drop the resolve channel — any RunGuarded waiter sees
		// ctx.Done() OR the close (since we are about to never send
		// to resolve). We do not send a synthetic resolution; the
		// RunGuarded waiter falls through to ErrGateClosed via its
		// post-select check.
		entry.retired = true
		delete(g.pending, tok)
	}
	g.mu.Unlock()
	return nil
}

// RunGuarded is the gate's entry point. Caller path:
//
//  1. Build an `ApprovalRequest{Tool, Args, Identity, Tags}`.
//  2. Call `args, err := gate.RunGuarded(ctx, req)`.
//  3. On nil err: proceed to invoke the tool with `args`.
//  4. On `*ErrToolRejected`: do NOT invoke the tool; surface the
//     rejection to the planner (the runtime translates this into the
//     Finish{ConstraintsConflict} outcome a later phase will wire).
//
// The gate consults the ApprovalPolicy. When `Required=false`, it
// short-circuits — no pause, no bus emit, returns `(req.Args, nil)`.
// When `Required=true`:
//
//   - The gate parks the run via `Coordinator.Request` with
//     `ReasonApprovalRequired` (the textbook RFC §6.3 reason).
//   - The gate publishes `tool.approval_requested` (audit-redacted).
//   - The gate blocks on the per-pause resolution channel.
//   - On APPROVE: returns `(req.Args, nil)`. Publishes
//     `tool.approved`.
//   - On REJECT: returns `(nil, *ErrToolRejected)`. Publishes
//     `tool.rejected`.
//   - On ctx cancellation before resolution: returns
//     `ErrApprovalCancelled` and resolves the pause as interrupted if
//     no other decision has committed. A failed approval request
//     publication also interrupts the pause.
//
// Concurrent-safe: one ApprovalGate is shared by N runs.
//
//nolint:gocyclo // gate orchestration is naturally branchy — splitting it would only hide the flow.
func (g *ApprovalGate) RunGuarded(ctx context.Context, req *ApprovalRequest) (json.RawMessage, error) {
	if g.closed.Load() {
		return nil, ErrGateClosed
	}
	if err := tools.CheckInvocationFence(ctx); err != nil {
		return nil, fmt.Errorf("approval: RunGuarded cancelled: %w", err)
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	// Consult the policy. A policy error is loud — there is no silent
	// auto-approve fallback (§13 amendment).
	required, reason, err := g.policy.ShouldApprove(ctx, req)
	if err != nil {
		// Double-wrap so callers can errors.Is against BOTH the
		// approval-package sentinel AND the underlying policy error.
		return nil, fmt.Errorf("%w: %w", ErrPolicyFailed, err)
	}
	if !required {
		// Short-circuit: no pause, no bus emit, return original args.
		return req.Args, nil
	}

	// Build the audit-redacted args summary BEFORE we mint a pause —
	// a redactor error fails the gate loud (CLAUDE.md §13 amendment;
	// CLAUDE.md §7 rule 6).
	summary, err := g.buildRedactedSummary(ctx, req)
	if err != nil {
		return nil, err
	}

	// Park via the unified Coordinator. The pause payload carries the
	// gate-side bookkeeping the Coordinator needs to scope the
	// resolver's identity AND a small classification footprint
	// observers can correlate against. The ORIGINAL args stay in the
	// gate's pending map — never on the pause record's payload.
	// The waiter is registered before the pause becomes visible: the gate
	// lock is held across Coordinator.Request, so a resolver that learns
	// the token (pause.requested, pause.list) blocks until the entry is in
	// the pending map instead of missing it and deferring the decision
	// behind the step that waits for it.
	entry := &waitingEntry{
		req:     req,
		resolve: make(chan resolution, 1),
	}
	if q, ok := identity.QuadrupleFrom(ctx); ok {
		entry.runID = q.RunID
	}
	g.mu.Lock()
	if g.closed.Load() {
		g.mu.Unlock()
		return nil, ErrGateClosed
	}
	pause, err := g.coordinator.Request(ctx, pauseresume.PauseRequest{
		Identity: req.Identity,
		Reason:   pauseresume.ReasonApprovalRequired,
		Payload: map[string]any{
			"tool":   req.Tool.Name,
			"reason": reason,
			// The same redacted summary the event carries, size-bounded;
			// never the original arguments.
			"args_summary": boundSummary(summary),
		},
	})
	if err != nil {
		g.mu.Unlock()
		// Coordinator sentinels propagate verbatim — caller reaches
		// them via errors.Is.
		return nil, fmt.Errorf("approval: coordinator.Request: %w", err)
	}
	entry.runID = pause.RunID
	g.pending[pause.Token] = entry
	g.mu.Unlock()
	defer g.retire(pause.Token, entry)

	// Publish tool.approval_requested. A bus publish failure is
	// loud-and-surfaced — the caller does NOT proceed to invoke a
	// tool whose approval observers might have missed (§13 fail-
	// loudly). Resolve the pause since this waiter cannot continue.
	if err := g.publishApprovalRequested(ctx, req, pause.RunID, pause.Token, reason, summary); err != nil {
		g.retire(pause.Token, entry)
		return nil, errors.Join(err, g.interrupt(req, pause))
	}

	// Block on resolution. The waiter selects on:
	//   - resolve: a ResolveApproval / steering-inbox dispatched
	//     decision arrived.
	//   - resolved: the Coordinator resolved this pause out of band; a
	//     typed `timeout` Decision (the max-park sweeper) means the
	//     approval expired.
	//   - ctx.Done(): the caller's ctx was cancelled (e.g. the run
	//     itself was cancelled).
	//
	// The Coordinator's resolution signal fires for every resolution of
	// this pause; only a timeout ends the wait there — approve / reject
	// arrive on entry.resolve.
	resolved := pauseresume.Resolved(g.coordinator, pause.Token)
	for {
		select {
		case res := <-entry.resolve:
			// Remove the entry — the Coordinator's pause has been resumed
			// (ResolveApproval did the Coordinator.Resume already; this
			// is the post-resume bookkeeping).
			g.retire(pause.Token, entry)
			switch res.decision {
			case DecisionApprove:
				if err := g.publishApproved(ctx, req, pause.RunID, pause.Token, res); err != nil {
					// Approval landed but the observability emit failed.
					// The gate's correctness path (return original args
					// to the caller) still proceeds — observability is
					// not the correctness path. Surface the err wrapped
					// so callers that care can branch.
					return req.Args, fmt.Errorf("approval: approved but emit failed: %w", err)
				}
				return req.Args, nil
			case DecisionReject:
				if err := g.publishRejected(ctx, req, pause.RunID, pause.Token, pauseresume.DecisionReject, res); err != nil {
					// Same posture as approved: the rejection has
					// happened in the Coordinator; the emit is
					// best-effort.
					return nil, fmt.Errorf("approval: rejected but emit failed: %w; original rejection reason: %s",
						err, res.reason)
				}
				return nil, &ErrToolRejected{
					Tool:     req.Tool.Name,
					Reason:   res.reason,
					Identity: req.Identity,
				}
			default:
				// resolve channel buffered to 1; we wrote a real
				// decision; an empty / pending here would be a
				// programmer error.
				g.retire(pause.Token, entry)
				return nil, fmt.Errorf("%w: %q", ErrInvalidDecision, res.decision)
			}
		case <-resolved:
			resolved = nil
			if g.timedOut(ctx, pause.Token) {
				return g.expire(ctx, req, pause)
			}
		case <-tools.InvocationInvalidated(ctx):
			g.retire(pause.Token, entry)
			// Withdraw this run's obsolete request, never approve it. Required
			// persistence uses a bounded independent context; a failed withdrawal
			// remains terminal and cannot become an instruction to retry the action.
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			err := g.coordinator.Resume(cleanupCtx, pause.Token, pauseresume.DecisionReject, map[string]any{"reason": "plan_superseded"})
			cancel()
			if err != nil && !errors.Is(err, pauseresume.ErrAlreadyResumed) {
				return nil, fmt.Errorf("%w: %w: %w", tools.ErrInvocationSuperseded, tools.ErrInvocationCleanupFailed, err)
			}
			return nil, tools.ErrInvocationSuperseded
		case <-ctx.Done():
			g.retire(pause.Token, entry)
			return nil, errors.Join(ErrApprovalCancelled, ctx.Err(), g.interrupt(req, pause))
		}
	}
}

// interrupt resolves a pause after its waiter exits, under the pause's own
// identity. Its bounded context is independent of the failing invocation.
func (g *ApprovalGate) interrupt(req *ApprovalRequest, pause pauseresume.Pause) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ctx, err := identity.With(ctx, pause.Identity)
	if err != nil {
		return fmt.Errorf("approval: interrupted pause identity: %w", err)
	}
	const reason = "approval waiter stopped before a decision"
	if err := g.coordinator.Resume(ctx, pause.Token, pauseresume.DecisionInterrupted, map[string]any{"interrupted": true, "reason": reason}); err != nil {
		if errors.Is(err, pauseresume.ErrAlreadyResumed) {
			return nil
		}
		return fmt.Errorf("approval: interrupt pause: %w", err)
	}
	res := resolution{reason: reason, decidedAt: g.resolvedAt(ctx, pause.Token)}
	return g.publishRejected(ctx, req, pause.RunID, pause.Token, pauseresume.DecisionInterrupted, res)
}

// timedOut reports whether the Coordinator resolved token with the
// typed timeout Decision. A Status error reads as "not a timeout".
func (g *ApprovalGate) timedOut(ctx context.Context, token pauseresume.Token) bool {
	st, err := g.coordinator.Status(ctx, token)
	return err == nil && st.State == pauseresume.StatusResumed && st.Decision == pauseresume.DecisionTimeout
}

// expire finishes a wait whose pause the max-park deadline resolved
// with the typed timeout Decision: the tool never runs, `tool.rejected`
// carries Decision "timeout", and the caller gets ErrApprovalExpired
// joined with the typed *ErrToolRejected. The Coordinator's resolution
// recorder wrote the outcome. RunGuarded retires the entry on return.
func (g *ApprovalGate) expire(ctx context.Context, req *ApprovalRequest, pause pauseresume.Pause) (json.RawMessage, error) {
	res := resolution{reason: ExpiredReason, decidedAt: g.resolvedAt(ctx, pause.Token)}
	rejected := &ErrToolRejected{Tool: req.Tool.Name, Reason: ExpiredReason, Identity: req.Identity}
	if err := g.publishRejected(ctx, req, pause.RunID, pause.Token, pauseresume.DecisionTimeout, res); err != nil {
		return nil, fmt.Errorf("%w: %w (emit failed: %w)", ErrApprovalExpired, rejected, err)
	}
	return nil, fmt.Errorf("%w: %w", ErrApprovalExpired, rejected)
}

// resolvedAt is the Coordinator's resolution time for token (the
// instant pause.resumed and the decision record carry), falling back to
// now when the Coordinator cannot report it.
func (g *ApprovalGate) resolvedAt(ctx context.Context, token pauseresume.Token) time.Time {
	if st, err := g.coordinator.Status(ctx, token); err == nil && !st.ResumedAt.IsZero() {
		return st.ResumedAt.UTC()
	}
	return time.Now().UTC()
}

// maxDecisionNoteBytes bounds the approver's free-form note.
const maxDecisionNoteBytes = 512

// redactNote runs the approver's note through the audit redactor (the
// one that redacts arguments) and bounds it, before it reaches the
// pause record, the event and the decision record. A redactor error
// fails closed.
func (g *ApprovalGate) redactNote(ctx context.Context, note string) (string, error) {
	if note == "" {
		return "", nil
	}
	out, err := g.redactor.Redact(ctx, map[string]any{"note": note})
	if err != nil {
		return "", fmt.Errorf("approval: redact decision note: %w", err)
	}
	var redacted string
	if m, ok := out.(map[string]any); ok {
		redacted = payloadString(m, "note")
	}
	return truncateUTF8(redacted, maxDecisionNoteBytes), nil
}

// truncateUTF8 cuts s to at most n bytes on a rune boundary.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	for n > 0 && !utf8.RuneStart(s[n]) {
		n--
	}
	return s[:n]
}

// ResolveApproval is the in-process resolution helper. The run-loop
// steering-inbox + Protocol edge dispatches APPROVE /
// REJECT control events through this surface (in-process callers
// reach the gate directly). Path:
//
//  1. Validate the decision is Approve or Reject (Pending is
//     rejected with `ErrInvalidDecision`).
//
//  2. Locate the pending entry and consult the injected
//     `ResolveAuthorizer` with the entry\'s
//     PendingInfo. An unauthorized resolver is rejected with a
//     wrapped `ErrResolveForbidden` BEFORE any pause state mutates.
//     The edge also enforces the RFC §6.3 steering scopes at
//     the JWT boundary; the in-process check is the defence-in-depth
//     layer — in runtime vocabulary, never protocol scopes (the
//     pre-seam `internal/protocol/auth` check lives on as the
//     server-side `ProtocolScopeAuthorizer` adapter, injected at
//     wire-driven gate assembly).
//
//  3. Call `Coordinator.Resume(ctx, token, decision, {rejected: bool})`
//     with the typed `pauseresume.Decision` (Approve or Reject) so the
//     emitted `pause.resumed` event carries a typed marker wire
//     consumers branch on (issue #113). This is where
//     cross-identity rejection happens — the Coordinator's
//     `ErrScopeMismatch` propagates verbatim if the caller's
//     identity does not match the original pause's identity. For
//     elevated callers, the Coordinator's `sameScope`
//     check applies the same `(tenant, user, session)` equality —
//     so a console-fleet admin resolver MUST present a ctx whose
//     triple matches the original pause's triple. (The
//     Coordinator's scope check is identity-based; the gate's
//     scope check is privilege-based; both fire.)
//
//  4. Dispatch the resolution to the RunGuarded waiter via the
//     pending entry's channel. The waiter unblocks and returns
//     APPROVE / REJECT to the original caller.
//
// Idempotency: a second ResolveApproval for the same Token returns
// `ErrApprovalAlreadyResolved` (the first call removed the entry
// from the pending map). Mirrors `pauseresume.ErrAlreadyResumed`.
func (g *ApprovalGate) ResolveApproval(ctx context.Context, token pauseresume.Token, decision ApprovalDecision, reason string) error {
	if g.closed.Load() {
		return ErrGateClosed
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("approval: ResolveApproval cancelled: %w", err)
	}
	if !IsValidDecision(decision) {
		return fmt.Errorf("%w: %q", ErrInvalidDecision, decision)
	}
	// Look up the pending entry. A missing entry is either "the
	// gate never opened it" or "it was already resolved" — both are
	// errors the caller distinguishes via `errors.Is`. (The lookup
	// precedes the privilege check because the authorizer needs the
	// entry\'s PendingInfo; pause tokens are unguessable
	// Coordinator-minted handles, so the not-found answer leaks no
	// pending state to an unauthorized prober.)
	g.mu.Lock()
	entry, ok := g.pending[token]
	g.mu.Unlock()
	if !ok {
		return fmt.Errorf("%w: token %q", ErrApprovalNotFound, token)
	}
	// Run binding: a resolver acting for a run (the steering bridge
	// resolves under the run that received the control) can only resolve
	// that run's own approvals — a control authorized for run A never
	// resolves run B's approval in the same shared session. Answered as
	// not-found so a foreign token's existence is not revealed.
	if q, hasRun := identity.QuadrupleFrom(ctx); hasRun && q.RunID != "" && entry.runID != "" && q.RunID != entry.runID {
		return fmt.Errorf("%w: token %q", ErrApprovalNotFound, token)
	}
	// The approver's note is redacted and bounded once, before it reaches
	// the pause record, the event and the decision record.
	note, err := g.redactNote(ctx, reason)
	if err != nil {
		return err
	}
	reason = note

	// Privilege check via the injected seam:
	// originating identity / control scope by default, protocol scopes
	// via the server-side adapter. Defence in depth — the
	// Protocol edge enforces the RFC §6.3 steering scopes at the JWT
	// boundary too. Authorize BEFORE reserving the entry so a rejected
	// resolver leaves the pending approval untouched.
	if err := g.authorizer.AuthorizeResolve(ctx, PendingInfo{
		Tool:     entry.req.Tool.Name,
		Token:    token,
		Identity: entry.req.Identity,
		Tags:     append([]string(nil), entry.req.Tags...),
	}); err != nil {
		return fmt.Errorf("approval: resolve authorization: %w", err)
	}

	// Reserve the entry (remove it from the map so a concurrent
	// second ResolveApproval gets ErrApprovalAlreadyResolved). The
	// entry may have been resolved between the authorize call and the
	// reservation — that race surfaces as ErrApprovalNotFound, the
	// same already-resolved answer a late second resolver gets.
	g.mu.Lock()
	current, ok := g.pending[token]
	if !ok || current != entry {
		g.mu.Unlock()
		return fmt.Errorf("%w: token %q", ErrApprovalNotFound, token)
	}
	delete(g.pending, token)
	g.mu.Unlock()

	// Drive the Coordinator. The Coordinator's scope check enforces
	// identity-tuple equality. For REJECT, the convention (mirrored
	// from the steering apply path) is `rejected: true` on the
	// resume payload — observers can branch on it; the typed
	// `pauseresume.Decision` (issue #113) is the load-bearing
	// channel wire consumers actually switch on.
	payload := map[string]any{}
	if decision == DecisionReject {
		payload["rejected"] = true
	}
	if reason != "" {
		payload["reason"] = reason
	}
	// Map the gate-internal ApprovalDecision (approve/reject/pending)
	// onto the broader pauseresume.Decision (approve/reject/resume/
	// timeout). The IsValidDecision pre-check above already rejected
	// Pending; only Approve and Reject reach this point.
	prDecision := pauseresume.DecisionApprove
	if decision == DecisionReject {
		prDecision = pauseresume.DecisionReject
	}
	if err := g.coordinator.Resume(ctx, token, prDecision, payload); err != nil {
		// Put the entry back so a retry can land — but only for a
		// retryable failure and only while its waiter still waits. A pause
		// that already resolved (a timeout won, or the deadline passed)
		// is terminal: restoring it would retain the original arguments
		// for a waiter that is gone.
		// (ErrPauseNotFound is a scope mismatch here — retryable from the
		// right identity.)
		terminal := errors.Is(err, pauseresume.ErrAlreadyResumed)
		g.mu.Lock()
		if _, raced := g.pending[token]; !raced && !terminal && !entry.retired {
			g.pending[token] = entry
		}
		g.mu.Unlock()
		return fmt.Errorf("approval: coordinator.Resume: %w", err)
	}

	// Deliver the decision to the RunGuarded waiter. The resolve
	// channel is buffered to 1; the send never blocks. The deciding
	// principal is the one the steering edge seated on ctx from the
	// verified control caller (zero for an in-process resolver).
	entry.resolve <- resolution{
		decision:  decision,
		reason:    reason,
		decider:   pauseresume.DeciderFrom(ctx),
		decidedAt: g.resolvedAt(ctx, token),
	}
	return nil
}

// retire marks the waiter's entry retired and drops it from the pending
// map, under the same lock a resolver uses to restore an entry, so a
// losing resolver can never re-insert it. Idempotent.
func (g *ApprovalGate) retire(token pauseresume.Token, entry *waitingEntry) {
	g.mu.Lock()
	entry.retired = true
	if current, ok := g.pending[token]; ok && current == entry {
		delete(g.pending, token)
	}
	g.mu.Unlock()
}

// pendingLen returns the number of currently-pending approvals.
// Internal — exposed for tests via the same-package
// (concurrent_test.go) only.
func (g *ApprovalGate) pendingLen() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.pending)
}

// buildRedactedSummary builds the ToolApprovalRequestedPayload's
// ArgsSummary field. The raw args go through the redactor; the
// redactor's output is what lands on the bus. A redactor error is
// loud — there is no silent fallback to raw args (CLAUDE.md §7
// rule 6 + §13 fail-loudly).
//
// The summary shape is a `map[string]any` with the args as a JSON-
// decoded value under "args". A redactor that elides secret-shaped
// values in the map returns a similarly-shaped map; the gate carries
// the redactor's output verbatim.
func (g *ApprovalGate) buildRedactedSummary(ctx context.Context, req *ApprovalRequest) (any, error) {
	// Build the input shape the redactor walks. A nil / empty Args is
	// fine — the redactor sees an empty map. The redactor accepts
	// `any`; the gate hands it a `map[string]any` so reflective
	// pattern rules apply.
	input := map[string]any{}
	if len(req.Args) > 0 {
		var decoded any
		if err := json.Unmarshal(req.Args, &decoded); err != nil {
			// Args not JSON: pass it through as a base64-ish opaque
			// blob via length-only summary. We do NOT carry the raw
			// bytes; the redactor would not know how to walk them.
			input["args"] = map[string]any{
				"_nonjson_length": len(req.Args),
			}
		} else {
			input["args"] = decoded
		}
	}
	input["tool"] = req.Tool.Name
	out, err := g.redactor.Redact(ctx, input)
	if err != nil {
		// Fail loud — do not emit raw args as a fallback.
		return nil, fmt.Errorf("approval: redactor.Redact: %w", err)
	}
	return out, nil
}

// publishApprovalRequested emits the typed tool.approval_requested
// event onto the bus. A publish failure is loud — the caller interrupts
// the pause.
func (g *ApprovalGate) publishApprovalRequested(ctx context.Context, req *ApprovalRequest, runID string, token pauseresume.Token, reason string, summary any) error {
	ev := events.Event{
		Type:     EventTypeToolApprovalRequested,
		Identity: identity.Quadruple{Identity: req.Identity, RunID: runID},
		Payload: ToolApprovalRequestedPayload{
			Tool:        req.Tool.Name,
			PauseToken:  string(token),
			Reason:      reason,
			Tags:        append([]string(nil), req.Tags...),
			ArgsSummary: summary,
		},
	}
	if err := g.bus.Publish(ctx, ev); err != nil {
		return fmt.Errorf("approval: bus.Publish(tool.approval_requested): %w", err)
	}
	return nil
}

// publishApproved emits the typed tool.approved event onto the bus.
func (g *ApprovalGate) publishApproved(ctx context.Context, req *ApprovalRequest, runID string, token pauseresume.Token, res resolution) error {
	ev := events.Event{
		Type:     EventTypeToolApproved,
		Identity: identity.Quadruple{Identity: req.Identity, RunID: runID},
		Payload: ToolApprovedPayload{
			Tool:             req.Tool.Name,
			PauseToken:       string(token),
			ApproverReason:   res.reason,
			DecidedByTenant:  res.decider.TenantID,
			DecidedByUser:    res.decider.UserID,
			DecidedBySession: res.decider.SessionID,
			DecidedBySpeaker: res.decider.Speaker,
			DecidedAt:        res.decidedAt,
		},
	}
	if err := g.bus.Publish(ctx, ev); err != nil {
		return fmt.Errorf("approval: bus.Publish(tool.approved): %w", err)
	}
	return nil
}

// publishRejected emits the typed tool.rejected event onto the bus.
// THIS is the master-plan acceptance criterion event. decision is the
// typed outcome (reject / timeout / interrupted).
func (g *ApprovalGate) publishRejected(ctx context.Context, req *ApprovalRequest, runID string, token pauseresume.Token, decision pauseresume.Decision, res resolution) error {
	return publishRejectedEvent(ctx, g.bus, identity.Quadruple{Identity: req.Identity, RunID: runID}, req.Tool.Name, token, decision, res)
}

// publishRejectedEvent is the one tool.rejected emitter, shared by the
// live gate and the boot-time interrupted-approval pass.
func publishRejectedEvent(ctx context.Context, bus events.EventBus, q identity.Quadruple, tool string, token pauseresume.Token, decision pauseresume.Decision, res resolution) error {
	ev := events.Event{
		Type:     EventTypeToolRejected,
		Identity: q,
		Payload: ToolRejectedPayload{
			Tool:             tool,
			PauseToken:       string(token),
			Reason:           res.reason,
			Decision:         string(decision),
			DecidedByTenant:  res.decider.TenantID,
			DecidedByUser:    res.decider.UserID,
			DecidedBySession: res.decider.SessionID,
			DecidedBySpeaker: res.decider.Speaker,
			DecidedAt:        res.decidedAt,
		},
	}
	if err := bus.Publish(ctx, ev); err != nil {
		return fmt.Errorf("approval: bus.Publish(tool.rejected): %w", err)
	}
	return nil
}
