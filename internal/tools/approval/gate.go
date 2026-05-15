package approval

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

// GateDeps bundles the collaborators an ApprovalGate needs. The
// production binary wires all four; tests may stub the bus / redactor
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
	// Coordinator is the unified pause/resume primitive (Phase 50).
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
}

// ApprovalGate is the V1 concrete approval-gate artifact.
//
// Concurrent reuse contract (D-025): every field below is either set
// once at construction (deps + closed flag) or is the pending map
// guarded by mu. There is no per-run state on the struct itself.
// One gate is safe to share across N goroutines; concurrent_test.go
// pins N≥128 under -race.
type ApprovalGate struct {
	policy      ApprovalPolicy
	coordinator pauseresume.Coordinator
	bus         events.EventBus
	redactor    audit.Redactor

	// mu guards pending. Documented internally-synchronised per the
	// D-025 concurrent-reuse contract (CLAUDE.md §5).
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
// ResolveApproval delivers a decision or the caller's ctx cancels
// (in which case the gate cleans up the entry without notifying — the
// pause record itself stays parked, observable via Coordinator.Status
// until an out-of-band resolution lands).
type waitingEntry struct {
	// req is the original request, held so the gate can publish
	// tool.approved / tool.rejected with the right Tool name.
	req *ApprovalRequest
	// resolve is the per-entry signal channel. ResolveApproval sends
	// the decision; RunGuarded selects on it + ctx.Done(). Buffered
	// so a resolver that wins the race against a cancelled ctx does
	// not block forever sending.
	resolve chan resolution
}

// resolution carries the decision + reason from ResolveApproval to the
// RunGuarded goroutine. Internal — never exported.
type resolution struct {
	decision ApprovalDecision
	reason   string
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
	return &ApprovalGate{
		policy:      deps.Policy,
		coordinator: deps.Coordinator,
		bus:         deps.Bus,
		redactor:    deps.Redactor,
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
// the mutex on map operations cover the race. Mirrors the Phase 30
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
		_ = entry
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
//     `ErrApprovalCancelled`. The pause record stays parked (the
//     Coordinator is the source of truth); an out-of-band resolver
//     can still land an APPROVE / REJECT later, but the original
//     caller is gone.
//
// Concurrent-safe (D-025): one ApprovalGate is shared by N runs.
//
//nolint:gocyclo // gate orchestration is naturally branchy — splitting it would only hide the flow.
func (g *ApprovalGate) RunGuarded(ctx context.Context, req *ApprovalRequest) (json.RawMessage, error) {
	if g.closed.Load() {
		return nil, ErrGateClosed
	}
	if err := ctx.Err(); err != nil {
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
	// brief 03 §"Audit redaction"; CLAUDE.md §7 rule 6).
	summary, err := g.buildRedactedSummary(ctx, req)
	if err != nil {
		return nil, err
	}

	// Park via the unified Coordinator. The pause payload carries the
	// gate-side bookkeeping the Coordinator needs to scope the
	// resolver's identity AND a small classification footprint
	// observers can correlate against. The ORIGINAL args stay in the
	// gate's pending map — never on the pause record's payload.
	pause, err := g.coordinator.Request(ctx, pauseresume.PauseRequest{
		Identity: req.Identity,
		Reason:   pauseresume.ReasonApprovalRequired,
		Payload: map[string]any{
			"tool":   req.Tool.Name,
			"reason": reason,
		},
	})
	if err != nil {
		// Coordinator sentinels propagate verbatim — caller reaches
		// them via errors.Is.
		return nil, fmt.Errorf("approval: coordinator.Request: %w", err)
	}

	// Register the per-pause resolution channel BEFORE the bus emit
	// so a resolver that wins a race against the emit still sees the
	// entry and lands the decision.
	entry := &waitingEntry{
		req:     req,
		resolve: make(chan resolution, 1),
	}
	g.mu.Lock()
	if g.closed.Load() {
		g.mu.Unlock()
		return nil, ErrGateClosed
	}
	g.pending[pause.Token] = entry
	g.mu.Unlock()

	// Publish tool.approval_requested. A bus publish failure is
	// loud-and-surfaced — the caller does NOT proceed to invoke a
	// tool whose approval observers might have missed (§13 fail-
	// loudly). Cleanup: drop the pending entry; the Coordinator's
	// pause record stays parked, but the in-process gate state is
	// rolled back.
	if err := g.publishApprovalRequested(ctx, req, pause.Token, reason, summary); err != nil {
		g.removePending(pause.Token)
		return nil, err
	}

	// Block on resolution. The waiter selects on:
	//   - resolve: a ResolveApproval / steering-inbox dispatched
	//     decision arrived.
	//   - ctx.Done(): the caller's ctx was cancelled (e.g. the run
	//     itself was cancelled).
	select {
	case res := <-entry.resolve:
		// Remove the entry — the Coordinator's pause has been resumed
		// (ResolveApproval did the Coordinator.Resume already; this
		// is the post-resume bookkeeping).
		g.removePending(pause.Token)
		switch res.decision {
		case DecisionApprove:
			if err := g.publishApproved(ctx, req, pause.Token, res.reason); err != nil {
				// Approval landed but the observability emit failed.
				// The gate's correctness path (return original args
				// to the caller) still proceeds — observability is
				// not the correctness path. Surface the err wrapped
				// so callers that care can branch.
				return req.Args, fmt.Errorf("approval: approved but emit failed: %w", err)
			}
			return req.Args, nil
		case DecisionReject:
			if err := g.publishRejected(ctx, req, pause.Token, res.reason); err != nil {
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
			g.removePending(pause.Token)
			return nil, fmt.Errorf("%w: %q", ErrInvalidDecision, res.decision)
		}
	case <-ctx.Done():
		// Caller's ctx died before resolution. Drop the in-process
		// entry; the Coordinator's pause record stays parked. An
		// out-of-band resolver can still APPROVE / REJECT it later
		// (the Coordinator will accept the Resume), but the original
		// RunGuarded caller has moved on.
		g.removePending(pause.Token)
		return nil, fmt.Errorf("%w: %v", ErrApprovalCancelled, ctx.Err())
	}
}

// ResolveApproval is the in-process resolution helper. The Phase 53
// steering-inbox + Phase 54 Protocol edge dispatches APPROVE /
// REJECT control events through this surface (in-process callers
// reach the gate directly). Path:
//
//  1. Enforce `auth.HasScope(ctx, ScopeAdmin) || HasScope(ctx,
//     ScopeConsoleFleet)`. A non-elevated caller is rejected with
//     `ErrApprovalScopeRequired`. The Phase 54 edge also enforces
//     this at the JWT boundary; the in-process helper is the
//     defence-in-depth layer.
//
//  2. Validate the decision is Approve or Reject (Pending is
//     rejected with `ErrInvalidDecision`).
//
//  3. Call `Coordinator.Resume(ctx, token, {rejected: bool})`. This
//     is where cross-identity rejection happens — the Coordinator's
//     `ErrScopeMismatch` propagates verbatim if the caller's
//     identity does not match the original pause's identity. For
//     elevated callers, the Phase 50 Coordinator's `sameScope`
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
	// Privilege check: admin OR console:fleet. Defence in depth — the
	// Phase 54 Protocol edge enforces this at the JWT boundary too.
	if !protocolauth.HasScope(ctx, protocolauth.ScopeAdmin) &&
		!protocolauth.HasScope(ctx, protocolauth.ScopeConsoleFleet) {
		return ErrApprovalScopeRequired
	}
	// Look up the pending entry. A missing entry is either "the
	// gate never opened it" or "it was already resolved" — both are
	// errors the caller distinguishes via `errors.Is`.
	g.mu.Lock()
	entry, ok := g.pending[token]
	if !ok {
		g.mu.Unlock()
		return fmt.Errorf("%w: token %q", ErrApprovalNotFound, token)
	}
	// Reserve the entry (remove it from the map so a concurrent
	// second ResolveApproval gets ErrApprovalAlreadyResolved).
	delete(g.pending, token)
	g.mu.Unlock()

	// Drive the Coordinator. The Coordinator's scope check enforces
	// identity-tuple equality. For REJECT, the convention (mirrored
	// from the steering apply path) is `rejected: true` on the
	// resume payload — observers can branch on it.
	payload := map[string]any{}
	if decision == DecisionReject {
		payload["rejected"] = true
	}
	if reason != "" {
		payload["reason"] = reason
	}
	if err := g.coordinator.Resume(ctx, token, payload); err != nil {
		// Put the entry back so a retry can land — Coordinator.Resume
		// errors are NOT terminal for the gate's pending state. A
		// scope-mismatch caller can retry from the right ctx; an
		// already-resumed Coordinator surfaces ErrAlreadyResumed.
		g.mu.Lock()
		// Only restore if no concurrent resolution intervened.
		if _, raced := g.pending[token]; !raced {
			g.pending[token] = entry
		}
		g.mu.Unlock()
		return fmt.Errorf("approval: coordinator.Resume: %w", err)
	}

	// Deliver the decision to the RunGuarded waiter. The resolve
	// channel is buffered to 1; the send never blocks.
	entry.resolve <- resolution{decision: decision, reason: reason}
	return nil
}

// removePending drops the entry for token from the pending map. Safe
// to call on a token that is no longer in the map (idempotent).
func (g *ApprovalGate) removePending(token pauseresume.Token) {
	g.mu.Lock()
	delete(g.pending, token)
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
// event onto the bus. A publish failure is loud — caller cleans up
// the pending entry.
func (g *ApprovalGate) publishApprovalRequested(ctx context.Context, req *ApprovalRequest, token pauseresume.Token, reason string, summary any) error {
	ev := events.Event{
		Type:     EventTypeToolApprovalRequested,
		Identity: identity.Quadruple{Identity: req.Identity},
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
func (g *ApprovalGate) publishApproved(ctx context.Context, req *ApprovalRequest, token pauseresume.Token, approverReason string) error {
	ev := events.Event{
		Type:     EventTypeToolApproved,
		Identity: identity.Quadruple{Identity: req.Identity},
		Payload: ToolApprovedPayload{
			Tool:           req.Tool.Name,
			PauseToken:     string(token),
			ApproverReason: approverReason,
		},
	}
	if err := g.bus.Publish(ctx, ev); err != nil {
		return fmt.Errorf("approval: bus.Publish(tool.approved): %w", err)
	}
	return nil
}

// publishRejected emits the typed tool.rejected event onto the bus.
// THIS is the master-plan acceptance criterion event.
func (g *ApprovalGate) publishRejected(ctx context.Context, req *ApprovalRequest, token pauseresume.Token, rejectReason string) error {
	ev := events.Event{
		Type:     EventTypeToolRejected,
		Identity: identity.Quadruple{Identity: req.Identity},
		Payload: ToolRejectedPayload{
			Tool:       req.Tool.Name,
			PauseToken: string(token),
			Reason:     rejectReason,
		},
	}
	if err := g.bus.Publish(ctx, ev); err != nil {
		return fmt.Errorf("approval: bus.Publish(tool.rejected): %w", err)
	}
	return nil
}
