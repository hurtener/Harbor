// Package approval ships Harbor's tool-side synchronous approval-gate
// subsystem — the second consumer of the unified pause/resume primitive
// (Phase 50 / D-067), layered on the same Coordinator + bus + steering
// inbox seams Phase 30 (tool-side OAuth) built. Where Phase 30's gate
// is "we need a bearer token from an authorization server," Phase 31's
// gate is "we need a human to say yes" — same primitive, simpler
// payload (no token, no URL, no third-party flow).
//
// # The flow
//
//  1. A caller (the planner via the runtime dispatcher in a later phase;
//     or a test today) builds an `ApprovalRequest{Tool, Args, Identity,
//     Tags}` describing the tool call about to fire.
//
//  2. The runtime calls `ApprovalGate.RunGuarded(ctx, req)`.
//
//  3. The gate asks the configured `ApprovalPolicy.ShouldApprove(ctx,
//     req)`. When the policy returns `Required=false`, the gate returns
//     `(req.Args, nil)` immediately and the caller proceeds — there is
//     NO pause, NO bus emit. When `Required=true`, the gate parks via
//     `Coordinator.Request(ApprovalRequired)` and publishes
//     `tool.approval_requested` (audit-redacted) onto the bus.
//
//  4. An admin / fleet-control caller resolves the approval by
//     submitting APPROVE / REJECT through the Phase 53 steering inbox
//     (or, in-process, via `ApprovalGate.ResolveApproval`). The
//     Coordinator resumes the parked run; the gate's per-pause channel
//     unblocks.
//
//  5. APPROVE → the gate publishes `tool.approved` and returns
//     `(req.Args, nil)`. The original args are held in the gate's
//     pending map throughout — they were NEVER on the bus — so a
//     redactor that elides a secret-shaped field does not corrupt the
//     executed tool call.
//
//  6. REJECT → the gate publishes `tool.rejected` (the master-plan
//     acceptance event) and returns `(nil, *ErrToolRejected)`.
//
// # Distinct from Phase 30 OAuth
//
// Phase 30's gate uses `ReasonExternalEvent` (waiting on an
// out-of-band callback). Phase 31's gate uses
// `ReasonApprovalRequired` (the textbook RFC §6.3 reason for a HITL
// approval gate — brief 02 §"Pause-reason taxonomy"). The two paths
// share the Coordinator but emit different events with different
// shapes:
//
//   - OAuth: `tool.auth_required{Source, AuthorizeURL, State, ...}`
//   - Approval: `tool.approval_requested{Tool, Identity, Tags, ...}`
//
// CLAUDE.md §13 forbids "two parallel implementations of the same
// conceptual feature" — the approval gate is NOT another pause
// primitive; it is another CONSUMER of the one primitive.
//
// # Scope-gating
//
// Approval resolution is privileged. A non-admin user cannot approve
// their own tool call (a self-approval would defeat the gate's
// purpose). `ResolveApproval` enforces
// `auth.HasScope(ctx, ScopeAdmin) || auth.HasScope(ctx,
// ScopeConsoleFleet)` — Phase 61's verified-JWT scope claims — and
// rejects unscoped callers with `ErrApprovalScopeRequired`. The Phase
// 54 Protocol edge also enforces this at the JWT boundary; the
// in-process helper is the second line of defence.
//
// # Audit redaction
//
// `tool.approval_requested` carries Tool name + identity triple +
// caller-supplied Tags + a REDACTED summary of args — never the
// original arg bytes. The gate runs the summary through
// `audit.Redactor.Redact` BEFORE publish; a redactor error fails the
// gate's Request loud (the §3.4 fail-loudly principle — there is no
// "emit anyway with raw args" fallback). The ORIGINAL args are held
// in the gate's per-pause map and used to drive the post-APPROVE tool
// invocation, so a redactor that elides a secret does NOT corrupt the
// executed call.
//
// # Concurrent reuse (D-025)
//
// `*ApprovalGate` is a compiled artifact: immutable after construction
// except for a mutex-guarded pending-resolutions map. One gate is
// safe to share across N concurrent goroutines; concurrent_test.go
// pins N≥128 under -race.
//
// # §13 amendment — no silent stub default
//
// `NewApprovalGate(GateDeps{})` with a nil `Policy` fails loud with
// `ErrPolicyRequired`. Approval gates with no policy attached are dead
// code at best, a privilege-escalation footgun at worst — the binary
// REFUSES to construct one. Compare Phase 61's `NewMux` requiring
// `WithValidator` after PR #95.
//
// # §13 primitive-with-consumer
//
// The primitive Phase 31 ships is `ApprovalGate` + the typed event
// triple `(tool.approval_requested, tool.approved, tool.rejected)` +
// the `*ErrToolRejected` sentinel. The first consumers are:
//
//   - `test/integration/phase31_approval_gates_test.go` —
//     end-to-end APPROVE and REJECT cycles against real
//     `pauseresume.Coordinator` + real `events.EventBus` + real
//     `audit.Redactor` + real `steering.Inbox`.
//   - `concurrent_test.go` — N≥128 concurrent invocations under
//     `-race` (the D-025 obligation).
//
// A later phase wires the gate into the runtime dispatcher so every
// gated tool call routes through it automatically; Phase 31 ships
// the gate as the explicit middleware tools opt into.
package approval

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// ApprovalDecision is the resolution any approval gate eventually
// receives. `DecisionPending` is the implicit state while a gate is
// parked; callers never construct it.
type ApprovalDecision string

const (
	// DecisionPending — the implicit state of a parked approval. A
	// caller that submits `DecisionPending` to `ResolveApproval` is
	// rejected with `ErrInvalidDecision` (a no-op resolution is
	// nonsensical).
	DecisionPending ApprovalDecision = "pending"
	// DecisionApprove — the approver said yes; the gate proceeds.
	DecisionApprove ApprovalDecision = "approve"
	// DecisionReject — the approver said no; the gate returns
	// `*ErrToolRejected`.
	DecisionReject ApprovalDecision = "reject"
)

// IsValidDecision reports whether d is a callable resolution
// (i.e. Approve or Reject; Pending is the implicit parked state).
func IsValidDecision(d ApprovalDecision) bool {
	return d == DecisionApprove || d == DecisionReject
}

// ApprovalRequest is the gate's input: a description of the tool call
// about to fire. The gate forwards the request to the
// `ApprovalPolicy`, which returns whether approval is required + a
// caller-facing Reason. The request's Args field is the ORIGINAL tool
// args; they NEVER appear on the bus (the redactor sees only the
// gate's summary payload).
type ApprovalRequest struct {
	// Tool is the planner-visible tool description. The gate emits
	// Tool.Name on `tool.approval_requested` so the Console can
	// render "Approve call to <Tool.Name>?".
	Tool tools.Tool
	// Args is the original argument blob the gate will return to the
	// caller post-APPROVE. NEVER published on the bus.
	Args json.RawMessage
	// Identity is the (tenant, user, session) triple the call is
	// running under. The Coordinator's pause record scopes against
	// it; a cross-tenant resolver is rejected with
	// `pauseresume.ErrScopeMismatch`.
	Identity identity.Identity
	// Tags is the caller-supplied classification surface. Operators
	// reach for Tags in `TaggedPolicy` to decide "this is a write to
	// a sensitive endpoint — require approval."
	Tags []string
}

// ApprovalPolicy decides, per ApprovalRequest, whether approval is
// required. A `Required=false` return short-circuits the gate (no
// pause, no bus emit). `Required=true` parks the call until the
// approver resolves it.
//
// `Reason` is the operator-facing classification the gate carries on
// `tool.approval_requested` so the Console can render
// "Approval required: <Reason>." It is NOT raw user data — operators
// keep Reason values to a small, stable set (the Phase 56 cardinality
// rule). The gate trusts the policy here; if Reason ever needs
// redaction, the policy is the bug.
//
// `Err` is the loud-failure path. A policy that cannot decide (e.g.
// missing operator configuration; a corrupt rule table) returns a
// non-nil Err and the gate refuses to invoke — there is NO silent
// auto-approve fallback (CLAUDE.md §13 amendment).
type ApprovalPolicy interface {
	ShouldApprove(ctx context.Context, req *ApprovalRequest) (Required bool, Reason string, Err error)
}

// ErrToolRejected is the typed sentinel `RunGuarded` returns on a
// REJECT resolution. Callers reach it via `errors.As`. Field set is
// SafePayload by construction — Tool name + classification reason +
// identity triple, no caller-controlled arg bytes.
type ErrToolRejected struct {
	// Tool is the name of the tool whose call was rejected.
	Tool string
	// Reason is the operator-facing classification the approver
	// supplied (or the policy's pre-emptive reject reason). Free-form
	// but low-cardinality by convention; never raw user data.
	Reason string
	// Identity is the (tenant, user, session) triple the rejected
	// call was running under — preserved so logs / audits can
	// correlate the rejection back to the originating run.
	Identity identity.Identity
}

// Error implements the error interface.
func (e *ErrToolRejected) Error() string {
	if e == nil {
		return "approval: <nil ErrToolRejected>"
	}
	if e.Reason != "" {
		return "approval: tool " + e.Tool + " rejected: " + e.Reason
	}
	return "approval: tool " + e.Tool + " rejected"
}

// Is supports errors.Is comparisons against the sentinel
// `ErrToolRejectedSentinel`.
func (e *ErrToolRejected) Is(target error) bool {
	return errors.Is(target, ErrToolRejectedSentinel)
}

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrToolRejectedSentinel — comparison target for
	// `errors.Is(err, approval.ErrToolRejectedSentinel)`. The typed
	// `*ErrToolRejected`'s Is() method matches this sentinel.
	ErrToolRejectedSentinel = errors.New("approval: tool call rejected")

	// ErrPolicyRequired — `NewApprovalGate` was called with a nil
	// `GateDeps.Policy`. The §13 amendment trip-wire — silent
	// auto-approve is forbidden.
	ErrPolicyRequired = errors.New("approval: ApprovalPolicy required at construction")

	// ErrCoordinatorRequired — `NewApprovalGate` was called with a
	// nil `GateDeps.Coordinator`.
	ErrCoordinatorRequired = errors.New("approval: pauseresume.Coordinator required at construction")

	// ErrBusRequired — `NewApprovalGate` was called with a nil
	// `GateDeps.Bus`. The bus is mandatory because every gate
	// publishes lifecycle events; an unobservable gate is a design
	// smell.
	ErrBusRequired = errors.New("approval: events.EventBus required at construction")

	// ErrRedactorRequired — `NewApprovalGate` was called with a nil
	// `GateDeps.Redactor`. Audit redaction is mandatory on the
	// approval-request payload (CLAUDE.md §7 rule 6).
	ErrRedactorRequired = errors.New("approval: audit.Redactor required at construction")

	// ErrIdentityRequired — `RunGuarded` was called with a request
	// whose Identity is incomplete. Identity is mandatory
	// (CLAUDE.md §6 rule 9).
	ErrIdentityRequired = errors.New("approval: identity triple incomplete")

	// ErrInvalidDecision — `ResolveApproval` was called with a
	// decision that is not one of Approve / Reject. A pending
	// no-op resolution is rejected.
	ErrInvalidDecision = errors.New("approval: decision must be approve or reject")

	// ErrApprovalScopeRequired — `ResolveApproval` was called from a
	// ctx that does NOT carry `auth.ScopeAdmin` OR
	// `auth.ScopeConsoleFleet`. The gate enforces the scope check in
	// addition to the Phase 54 Protocol edge's JWT check; defence in
	// depth.
	ErrApprovalScopeRequired = errors.New("approval: admin or console:fleet scope required")

	// ErrApprovalNotFound — `ResolveApproval` was called with a
	// Token the gate has no pending record for. Either the gate
	// never opened it, or it was already resolved / cancelled.
	ErrApprovalNotFound = errors.New("approval: no pending approval for token")

	// ErrApprovalAlreadyResolved — a second `ResolveApproval` for the
	// same Token (idempotency surface). The first resolution wins;
	// the second is rejected loud. Mirrors
	// `pauseresume.ErrAlreadyResumed`.
	ErrApprovalAlreadyResolved = errors.New("approval: already resolved")

	// ErrApprovalCancelled — the caller's ctx was cancelled before
	// the approver resolved the gate. Returned by `RunGuarded` so
	// the runtime distinguishes "approver said no" from "ctx died."
	ErrApprovalCancelled = errors.New("approval: cancelled before resolution")

	// ErrGateClosed — any operation called after `Close`.
	ErrGateClosed = errors.New("approval: gate closed")

	// ErrPolicyFailed — the configured `ApprovalPolicy.ShouldApprove`
	// returned a non-nil Err. The gate refuses to invoke — there is
	// no silent auto-approve fallback (§13 amendment).
	ErrPolicyFailed = errors.New("approval: policy decision failed")
)

// Validate reports whether the ApprovalRequest is structurally valid.
// The gate calls this on every RunGuarded entry — a missing identity
// or empty Tool name is a programmer error, not a recoverable runtime
// state.
func (r *ApprovalRequest) Validate() error {
	if r == nil {
		return errors.New("approval: nil ApprovalRequest")
	}
	if r.Tool.Name == "" {
		return errors.New("approval: ApprovalRequest.Tool.Name empty")
	}
	if err := identity.Validate(r.Identity); err != nil {
		// Wrap so callers reach ErrIdentityRequired via errors.Is.
		return joinIdentityErr(err)
	}
	return nil
}

// joinIdentityErr wraps an identity validation error with the
// approval-package sentinel for errors.Is.
func joinIdentityErr(err error) error {
	if err == nil {
		return nil
	}
	return errors.Join(ErrIdentityRequired, err)
}
