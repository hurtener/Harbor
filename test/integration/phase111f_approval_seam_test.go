// phase111f_approval_seam_test.go — Phase 111f (D-203) approval-half
// E2E: the injected resolve-authorizer seam exercised on the REAL
// production assembly (assemble.Assemble builds the gates from
// cfg.Tools.Entries through the catalog Builder; the Protocol-side
// ProtocolScopeAuthorizer is injected exactly as cmd/harbor /
// devstack inject it).
//
// What this pins:
//
//  1. Wire behaviour unchanged: a Protocol-scoped (admin) resolver
//     drives APPROVE through the assembled gate and the typed
//     `pause.resumed{Decision: approve}` lands (D-096 marker) — the
//     pre-seam admin/console:fleet acceptance preserved through the
//     server-side adapter.
//  2. The elevation-free runtime shape: the ORIGINATING identity
//     resolves a REJECT with no protocol scopes and no control-scope
//     claim — the steering bridge's post-D-203 path (the deleted
//     `protocolauth.WithScopes` self-elevation is not needed). The
//     full bridge-path E2E (control surface → steering drain → bridge
//     → gate → pause.resumed) is the D-097/D-192 suite in
//     approval_midstep_test.go, which runs the SAME seam.
//  3. Fail-closed: a foreign resolver (wrong identity, no scope, no
//     claim) is rejected loudly with approval.ErrResolveForbidden —
//     the seam's replacement for the pre-111f scope rejection — and
//     the pending approval survives for an authorized resolver.
//  4. Direction rule consequence: this test wires gates through
//     config + assembly without the test (or the gate) touching
//     `internal/protocol/auth` vocabulary anywhere except the
//     explicitly Protocol-side adapter injection.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/server"
	"github.com/hurtener/Harbor/internal/telemetry"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
)

// phase111fSeamID is the identity the gated calls run under.
var phase111fSeamID = identity.Identity{TenantID: "t-seam", UserID: "u-seam", SessionID: "s-seam"}

// assembleGatedStack assembles a production-shaped stack whose config
// declares ONE approval-gated tool ("guarded_echo", deny-all policy →
// every call requires approval), with the production wire-side
// authorizer composition injected.
func assembleGatedStack(t *testing.T) *assemble.Stack {
	t.Helper()
	cfg := headlessRecipeCfg(t)
	cfg.Tools.Entries = []config.ToolEntryConfig{
		{Name: "guarded_echo", Approval: &config.ToolApprovalConfig{Policy: "deny-all", Reason: "seam-e2e"}},
	}

	echo := tools.ToolDescriptor{
		Tool: tools.Tool{
			Name:        "guarded_echo",
			Description: "echo (phase 111f seam test)",
			Transport:   tools.TransportInProcess,
			Source:      "phase111f-test-source",
		},
		Invoke: func(_ context.Context, args json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: string(args)}, nil
		},
	}

	stack, err := assemble.Assemble(context.Background(), cfg, assemble.Options{
		PreRegisterTools: []tools.ToolDescriptor{echo},
		TelemetryOptions: []telemetry.Option{telemetry.WithWriter(io.Discard)},
		// The production composition cmd/harbor + devstack inject.
		ApprovalAuthorizer: server.NewProtocolScopeAuthorizer(approval.NewIdentityAuthorizer()),
	})
	if err != nil {
		if stack != nil {
			_ = stack.Close(context.Background())
		}
		t.Fatalf("assemble.Assemble: %v", err)
	}
	t.Cleanup(func() { _ = stack.Close(context.Background()) })
	if stack.Gates["guarded_echo"] == nil {
		t.Fatal("the catalog Builder did not surface the guarded_echo gate")
	}
	return stack
}

// parkGuardedCall invokes the wrapped tool on a goroutine and returns
// the minted pause token + the invocation's outcome channel.
func parkGuardedCall(t *testing.T, stack *assemble.Stack) (pauseresume.Token, chan error) {
	t.Helper()
	sub, err := stack.Bus.Subscribe(context.Background(), events.Filter{
		Tenant:  phase111fSeamID.TenantID,
		User:    phase111fSeamID.UserID,
		Session: phase111fSeamID.SessionID,
		Types:   []events.EventType{approval.EventTypeToolApprovalRequested},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	ctx, err := identity.With(context.Background(), phase111fSeamID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	d, ok := stack.Catalog.Resolve("guarded_echo")
	if !ok {
		t.Fatal("guarded_echo not in catalog")
	}
	outcome := make(chan error, 1)
	go func() {
		_, ierr := d.Invoke(ctx, json.RawMessage(`{"q":"hi"}`))
		outcome <- ierr
	}()

	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription closed before tool.approval_requested")
		}
		payload, ok := ev.Payload.(approval.ToolApprovalRequestedPayload)
		if !ok {
			t.Fatalf("payload type %T", ev.Payload)
		}
		return pauseresume.Token(payload.PauseToken), outcome
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for tool.approval_requested")
		return "", nil
	}
}

// awaitPauseResumed blocks until a typed pause.resumed event for the
// token lands and returns its payload.
func awaitPauseResumed(t *testing.T, sub events.Subscription, token pauseresume.Token) pauseresume.PauseResumedPayload {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				t.Fatal("subscription closed before pause.resumed")
			}
			payload, ok := ev.Payload.(pauseresume.PauseResumedPayload)
			if !ok || payload.Token != string(token) {
				continue
			}
			return payload
		case <-deadline:
			t.Fatalf("timed out waiting for pause.resumed(%s)", token)
		}
	}
}

func phase111fResumeSub(t *testing.T, stack *assemble.Stack) events.Subscription {
	t.Helper()
	sub, err := stack.Bus.Subscribe(context.Background(), events.Filter{
		Tenant:  phase111fSeamID.TenantID,
		User:    phase111fSeamID.UserID,
		Session: phase111fSeamID.SessionID,
		Types:   []events.EventType{pauseresume.EventTypePauseResumed},
	})
	if err != nil {
		t.Fatalf("Subscribe(pause.resumed): %v", err)
	}
	t.Cleanup(sub.Cancel)
	return sub
}

// TestE2E_Phase111f_WireScopedApprove_BehaviourUnchanged — the
// Protocol-scoped resolver path: an admin-scope ctx (the Phase 61
// middleware shape) resolves the assembled gate exactly as before the
// seam, and the typed pause.resumed{approve} marker lands.
func TestE2E_Phase111f_WireScopedApprove_BehaviourUnchanged(t *testing.T) {
	stack := assembleGatedStack(t)
	resumed := phase111fResumeSub(t, stack)
	token, outcome := parkGuardedCall(t, stack)

	// Wire shape: verified Protocol scopes + the caller's identity
	// (the middleware injects both; the Coordinator's identity check
	// requires the matching triple).
	base, err := identity.With(context.Background(), phase111fSeamID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	wireCtx := protocolauth.WithScopes(base, []protocolauth.Scope{protocolauth.ScopeAdmin})

	if err := stack.Gates["guarded_echo"].ResolveApproval(wireCtx, token, approval.DecisionApprove, "lgtm"); err != nil {
		t.Fatalf("wire-scoped ResolveApproval: %v", err)
	}
	payload := awaitPauseResumed(t, resumed, token)
	if payload.Decision != pauseresume.DecisionApprove {
		t.Fatalf("pause.resumed Decision = %q, want approve", payload.Decision)
	}
	if err := <-outcome; err != nil {
		t.Fatalf("approved invocation failed: %v", err)
	}
}

// TestE2E_Phase111f_OriginatingIdentityReject_NoElevation — the
// runtime shape the deleted self-elevation papered over: the
// originating identity (no protocol scopes, no control-scope claim)
// resolves a REJECT directly; the typed pause.resumed{reject} marker
// + the *ErrToolRejected surface land.
func TestE2E_Phase111f_OriginatingIdentityReject_NoElevation(t *testing.T) {
	stack := assembleGatedStack(t)
	resumed := phase111fResumeSub(t, stack)
	token, outcome := parkGuardedCall(t, stack)

	plainCtx, err := identity.With(context.Background(), phase111fSeamID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if err := stack.Gates["guarded_echo"].ResolveApproval(plainCtx, token, approval.DecisionReject, "not today"); err != nil {
		t.Fatalf("originating-identity ResolveApproval: %v", err)
	}
	payload := awaitPauseResumed(t, resumed, token)
	if payload.Decision != pauseresume.DecisionReject {
		t.Fatalf("pause.resumed Decision = %q, want reject", payload.Decision)
	}
	ierr := <-outcome
	var rejected *approval.ErrToolRejected
	if !errors.As(ierr, &rejected) {
		t.Fatalf("rejected invocation error = %v, want *ErrToolRejected", ierr)
	}
	if rejected.Reason != "not today" {
		t.Fatalf("rejection reason = %q, want %q", rejected.Reason, "not today")
	}
}

// TestE2E_Phase111f_ForeignResolver_FailsClosed — the seam's failure
// mode: a resolver with the WRONG identity and no scope/claim is
// rejected with ErrResolveForbidden; the pending approval survives
// and the originating identity still resolves it.
func TestE2E_Phase111f_ForeignResolver_FailsClosed(t *testing.T) {
	stack := assembleGatedStack(t)
	resumed := phase111fResumeSub(t, stack)
	token, outcome := parkGuardedCall(t, stack)

	foreignCtx, err := identity.With(context.Background(), identity.Identity{
		TenantID: "t-evil", UserID: "u-evil", SessionID: "s-evil",
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	gate := stack.Gates["guarded_echo"]
	if rerr := gate.ResolveApproval(foreignCtx, token, approval.DecisionApprove, ""); !errors.Is(rerr, approval.ErrResolveForbidden) {
		t.Fatalf("foreign resolve: got %v, want ErrResolveForbidden", rerr)
	}
	// Identity-free ctx is rejected too.
	if rerr := gate.ResolveApproval(context.Background(), token, approval.DecisionApprove, ""); !errors.Is(rerr, approval.ErrResolveForbidden) {
		t.Fatalf("identity-free resolve: got %v, want ErrResolveForbidden", rerr)
	}

	// The pending approval is intact: the originating identity lands
	// the approval and the run unblocks.
	plainCtx, err := identity.With(context.Background(), phase111fSeamID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if err := gate.ResolveApproval(plainCtx, token, approval.DecisionApprove, ""); err != nil {
		t.Fatalf("post-rejection authorized resolve: %v", err)
	}
	if payload := awaitPauseResumed(t, resumed, token); payload.Decision != pauseresume.DecisionApprove {
		t.Fatalf("pause.resumed Decision = %q, want approve", payload.Decision)
	}
	if err := <-outcome; err != nil {
		t.Fatalf("approved invocation failed: %v", err)
	}
}
