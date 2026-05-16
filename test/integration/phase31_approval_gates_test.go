// Phase 31 — Tool-side approval gates integration test
// (RFC §6.4 + §3.3; master-plan Phase 31 detail block; D-086).
//
// This test wires the REAL artifacts across the Phase 31 surface — no
// mocks at any seam (CLAUDE.md §17.3 #1):
//
//   - real `pauseresume.Coordinator`;
//   - real `audit.Redactor` (the patterns driver, the canonical V1 rule
//     set);
//   - real `events.EventBus` (in-mem driver);
//   - real `steering.Inbox` + `steering.Registry` — the Phase 53 inbox
//     is the wire APPROVE / REJECT control events flow through; this
//     test exercises that the gate's `ResolveApproval` matches the
//     contract a Phase 53 RunLoop will end up calling.
//
// It exercises (CLAUDE.md §17.3 #2 + #3 + final):
//
//   - the full APPROVE round-trip (identity propagates everywhere);
//   - the full REJECT round-trip (the typed `tool.rejected` event lands
//     with the verified identity in the envelope);
//   - the scope-gating failure mode — a non-admin / non-console-fleet
//     caller is rejected with `ErrApprovalScopeRequired`;
//   - cross-identity failure mode — a tenant-B admin cannot resolve a
//     tenant-A pause;
//   - initiate-then-cancel goroutine-leak;
//   - N=16 concurrency stress (CLAUDE.md §17.3 final — N>=10).
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/approval"
)

// phase31ID is the canonical test identity. Documented dummy values
// (CLAUDE.md §7 rule 2).
var phase31ID = identity.Identity{
	TenantID:  "tenant-phase31",
	UserID:    "user-phase31",
	SessionID: "session-phase31",
}

const phase31RunID = "run-phase31"

type phase31Env struct {
	bus         events.EventBus
	coordinator pauseresume.Coordinator
	gate        *approval.ApprovalGate
	registry    *steering.Registry
}

func buildPhase31Env(t *testing.T, policy approval.ApprovalPolicy) *phase31Env {
	t.Helper()
	red := patternsAudit.New()
	bus, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              2 * time.Second,
		DropWindow:               50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("eventsInmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	coord := pauseresume.New()
	gate, err := approval.NewApprovalGate(approval.GateDeps{
		Policy: policy, Coordinator: coord, Bus: bus, Redactor: red,
	})
	if err != nil {
		t.Fatalf("NewApprovalGate: %v", err)
	}
	t.Cleanup(func() { _ = gate.Close(context.Background()) })
	reg := steering.NewRegistry()
	return &phase31Env{bus: bus, coordinator: coord, gate: gate, registry: reg}
}

func phase31Ctx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func phase31AdminCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	return protocolauth.WithScopes(phase31Ctx(t, id),
		[]protocolauth.Scope{protocolauth.ScopeAdmin})
}

func phase31FleetCtx(t *testing.T, id identity.Identity) context.Context {
	t.Helper()
	return protocolauth.WithScopes(phase31Ctx(t, id),
		[]protocolauth.Scope{protocolauth.ScopeConsoleFleet})
}

// phase31SubFor builds a bus subscription for the test's identity +
// event-type filter.
func phase31SubFor(t *testing.T, bus events.EventBus, id identity.Identity, types ...events.EventType) (events.Subscription, func()) {
	t.Helper()
	subCtx, cancel := context.WithCancel(context.Background())
	sub, err := bus.Subscribe(subCtx, events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: types,
	})
	if err != nil {
		cancel()
		t.Fatalf("Subscribe: %v", err)
	}
	return sub, func() {
		sub.Cancel()
		cancel()
	}
}

func phase31WaitEv(t *testing.T, sub events.Subscription, d time.Duration) events.Event {
	t.Helper()
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription channel closed")
		}
		return ev
	case <-time.After(d):
		t.Fatal("timed out waiting for event")
		return events.Event{}
	}
}

// TestE2E_Phase31_FullApproveCycle is the master-plan headline
// acceptance criterion: APPROVE round-trip via the gate, against
// real Coordinator + real Bus + real Redactor.
func TestE2E_Phase31_FullApproveCycle(t *testing.T) {
	env := buildPhase31Env(t, approval.AlwaysDenyPolicy{Reason: "policy: deny-all"})

	requestedSub, cancelReq := phase31SubFor(t, env.bus, phase31ID,
		approval.EventTypeToolApprovalRequested)
	defer cancelReq()
	approvedSub, cancelApp := phase31SubFor(t, env.bus, phase31ID,
		approval.EventTypeToolApproved)
	defer cancelApp()

	args := json.RawMessage(`{"target":"team-doc","query":"summary"}`)
	req := &approval.ApprovalRequest{
		Tool: tools.Tool{
			Name:        "summarize_doc",
			Description: "Summarize a Harbor team document",
		},
		Args:     args,
		Identity: phase31ID,
		Tags:     []string{"sensitive"},
	}

	type outcome struct {
		out json.RawMessage
		err error
	}
	resCh := make(chan outcome, 1)
	go func() {
		out, err := env.gate.RunGuarded(phase31Ctx(t, phase31ID), req)
		resCh <- outcome{out: out, err: err}
	}()

	// Observe the request event, capture the pause token.
	requestedEv := phase31WaitEv(t, requestedSub, 2*time.Second)
	if requestedEv.Type != approval.EventTypeToolApprovalRequested {
		t.Fatalf("requested ev.Type: got %s", requestedEv.Type)
	}
	if requestedEv.Identity.TenantID != phase31ID.TenantID {
		t.Fatalf("event identity propagation: got %+v want %+v",
			requestedEv.Identity.Identity, phase31ID)
	}
	requestedPayload, ok := requestedEv.Payload.(approval.ToolApprovalRequestedPayload)
	if !ok {
		t.Fatalf("requested payload type: got %T", requestedEv.Payload)
	}
	token := pauseresume.Token(requestedPayload.PauseToken)
	if token == "" {
		t.Fatal("empty PauseToken on requested event")
	}

	// Coordinator.Status reports the pause as parked.
	status, err := env.coordinator.Status(phase31Ctx(t, phase31ID), token)
	if err != nil {
		t.Fatalf("Coordinator.Status: %v", err)
	}
	if status.State != pauseresume.StatusPaused {
		t.Fatalf("Status: got %s want paused", status.State)
	}
	if status.Reason != pauseresume.ReasonApprovalRequired {
		t.Fatalf("Status.Reason: got %s want %s",
			status.Reason, pauseresume.ReasonApprovalRequired)
	}

	// Resolve as admin.
	if err := env.gate.ResolveApproval(phase31AdminCtx(t, phase31ID),
		token, approval.DecisionApprove, "admin OK"); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}

	// tool.approved event lands.
	approvedEv := phase31WaitEv(t, approvedSub, 2*time.Second)
	approvedPayload, ok := approvedEv.Payload.(approval.ToolApprovedPayload)
	if !ok {
		t.Fatalf("approved payload type: got %T", approvedEv.Payload)
	}
	if approvedPayload.PauseToken != string(token) {
		t.Fatalf("approved PauseToken: got %q want %q",
			approvedPayload.PauseToken, string(token))
	}

	// RunGuarded returns the ORIGINAL args.
	select {
	case o := <-resCh:
		if o.err != nil {
			t.Fatalf("RunGuarded err: %v", o.err)
		}
		if string(o.out) != string(args) {
			t.Fatalf("RunGuarded out: got %s want %s", o.out, args)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunGuarded did not return")
	}

	// Coordinator.Status now reports resumed.
	status, err = env.coordinator.Status(phase31Ctx(t, phase31ID), token)
	if err != nil {
		t.Fatalf("Coordinator.Status post-resolve: %v", err)
	}
	if status.State != pauseresume.StatusResumed {
		t.Fatalf("Status post-resolve: got %s want resumed", status.State)
	}
}

// TestE2E_Phase31_FullRejectCycle is the master-plan acceptance
// criterion: "reject path raises typed tool.rejected events."
func TestE2E_Phase31_FullRejectCycle(t *testing.T) {
	env := buildPhase31Env(t, approval.AlwaysDenyPolicy{Reason: "policy: review-required"})

	requestedSub, cancelReq := phase31SubFor(t, env.bus, phase31ID,
		approval.EventTypeToolApprovalRequested)
	defer cancelReq()
	rejectedSub, cancelRej := phase31SubFor(t, env.bus, phase31ID,
		approval.EventTypeToolRejected)
	defer cancelRej()

	args := json.RawMessage(`{"target":"team-doc"}`)
	req := &approval.ApprovalRequest{
		Tool:     tools.Tool{Name: "delete_doc"},
		Args:     args,
		Identity: phase31ID,
		Tags:     []string{"sensitive", "write:prod"},
	}

	type outcome struct {
		out json.RawMessage
		err error
	}
	resCh := make(chan outcome, 1)
	go func() {
		out, err := env.gate.RunGuarded(phase31Ctx(t, phase31ID), req)
		resCh <- outcome{out: out, err: err}
	}()

	requestedEv := phase31WaitEv(t, requestedSub, 2*time.Second)
	requestedPayload, _ := requestedEv.Payload.(approval.ToolApprovalRequestedPayload)
	token := pauseresume.Token(requestedPayload.PauseToken)

	// Resolve as console:fleet (the second accepted scope).
	if err := env.gate.ResolveApproval(phase31FleetCtx(t, phase31ID),
		token, approval.DecisionReject, "policy: bad target"); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}

	// tool.rejected event lands with the verified identity in the
	// envelope (the master-plan acceptance criterion shape).
	rejectedEv := phase31WaitEv(t, rejectedSub, 2*time.Second)
	if rejectedEv.Identity.TenantID != phase31ID.TenantID {
		t.Fatalf("rejected event identity: got %+v want %+v",
			rejectedEv.Identity.Identity, phase31ID)
	}
	rejectedPayload, ok := rejectedEv.Payload.(approval.ToolRejectedPayload)
	if !ok {
		t.Fatalf("rejected payload type: got %T", rejectedEv.Payload)
	}
	if rejectedPayload.Tool != "delete_doc" {
		t.Fatalf("rejected payload Tool: got %q", rejectedPayload.Tool)
	}
	if rejectedPayload.Reason != "policy: bad target" {
		t.Fatalf("rejected payload Reason: got %q", rejectedPayload.Reason)
	}

	// RunGuarded returns *ErrToolRejected.
	select {
	case o := <-resCh:
		var rejErr *approval.ErrToolRejected
		if !errors.As(o.err, &rejErr) {
			t.Fatalf("RunGuarded err: want *ErrToolRejected, got %v", o.err)
		}
		if !errors.Is(o.err, approval.ErrToolRejectedSentinel) {
			t.Fatal("errors.Is against sentinel: want true")
		}
		if rejErr.Identity != phase31ID {
			t.Fatalf("rejErr.Identity: got %+v want %+v",
				rejErr.Identity, phase31ID)
		}
		if o.out != nil {
			t.Fatalf("RunGuarded out on reject: want nil, got %s", o.out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("RunGuarded did not return")
	}
}

// TestE2E_Phase31_ScopeGating_RejectsUnscoped is the §17.3 failure-
// mode coverage: a caller without admin / console:fleet scope is
// rejected at ResolveApproval. Defence in depth — the Phase 54
// Protocol edge also enforces this at the JWT boundary; this is the
// in-process layer.
func TestE2E_Phase31_ScopeGating_RejectsUnscoped(t *testing.T) {
	env := buildPhase31Env(t, approval.AlwaysDenyPolicy{})
	requestedSub, cancelReq := phase31SubFor(t, env.bus, phase31ID,
		approval.EventTypeToolApprovalRequested)
	defer cancelReq()

	go func() {
		_, _ = env.gate.RunGuarded(phase31Ctx(t, phase31ID),
			&approval.ApprovalRequest{
				Tool: tools.Tool{Name: "x"}, Args: json.RawMessage(`{}`),
				Identity: phase31ID,
			})
	}()
	ev := phase31WaitEv(t, requestedSub, 2*time.Second)
	payload, _ := ev.Payload.(approval.ToolApprovalRequestedPayload)
	token := pauseresume.Token(payload.PauseToken)

	// Unscoped ctx — auth middleware never ran, no scopes attached.
	err := env.gate.ResolveApproval(phase31Ctx(t, phase31ID), token,
		approval.DecisionApprove, "")
	if !errors.Is(err, approval.ErrApprovalScopeRequired) {
		t.Fatalf("unscoped Resolve: got %v want ErrApprovalScopeRequired", err)
	}

	// Cleanup with admin.
	if err := env.gate.ResolveApproval(phase31AdminCtx(t, phase31ID),
		token, approval.DecisionApprove, ""); err != nil {
		t.Fatalf("cleanup admin Resolve: %v", err)
	}
}

// TestE2E_Phase31_CrossIdentity_Rejected — a tenant-B admin cannot
// resolve a tenant-A pause. The Coordinator's scope check fires
// (ErrScopeMismatch propagates through ResolveApproval).
func TestE2E_Phase31_CrossIdentity_Rejected(t *testing.T) {
	env := buildPhase31Env(t, approval.AlwaysDenyPolicy{})

	idA := identity.Identity{TenantID: "tA", UserID: "uA", SessionID: "sA"}
	idB := identity.Identity{TenantID: "tB", UserID: "uB", SessionID: "sB"}

	// Subscribe Admin so we can observe both identities.
	adminSubCtx, cancelSub := context.WithCancel(context.Background())
	defer cancelSub()
	sub, err := env.bus.Subscribe(adminSubCtx, events.Filter{
		Admin: true,
		Types: []events.EventType{approval.EventTypeToolApprovalRequested},
	})
	if err != nil {
		t.Fatalf("Subscribe(admin): %v", err)
	}
	defer sub.Cancel()

	go func() {
		_, _ = env.gate.RunGuarded(phase31Ctx(t, idA),
			&approval.ApprovalRequest{
				Tool: tools.Tool{Name: "x"}, Args: json.RawMessage(`{}`),
				Identity: idA,
			})
	}()
	ev := phase31WaitEv(t, sub, 2*time.Second)
	payload, _ := ev.Payload.(approval.ToolApprovalRequestedPayload)
	token := pauseresume.Token(payload.PauseToken)

	// Tenant-B admin tries to resolve — Coordinator scope check
	// rejects via ErrScopeMismatch.
	err = env.gate.ResolveApproval(phase31AdminCtx(t, idB), token,
		approval.DecisionApprove, "")
	if !errors.Is(err, pauseresume.ErrScopeMismatch) {
		t.Fatalf("cross-identity Resolve: got %v want ErrScopeMismatch", err)
	}

	// Clean up with correct-identity admin.
	if err := env.gate.ResolveApproval(phase31AdminCtx(t, idA), token,
		approval.DecisionApprove, ""); err != nil {
		t.Fatalf("correct-identity Resolve: %v", err)
	}
}

// TestE2E_Phase31_AdminCtx_UnblocksGate_ResolveApproval — contract
// test that the gate's pending-resolution channel responds correctly
// when ResolveApproval is invoked from the admin-scope identity ctx
// that the Phase 53 RunLoop would hand to the gate's resolve path.
// This is NOT a steering-inbox round-trip test (the inbox-drain →
// gate.ResolveApproval wiring is tracked in issue #112); it pins the
// gate's resolve-from-outside-the-package shape that Phase 53 relies
// on.
func TestE2E_Phase31_AdminCtx_UnblocksGate_ResolveApproval(t *testing.T) {
	env := buildPhase31Env(t, approval.AlwaysDenyPolicy{})

	requestedSub, cancelReq := phase31SubFor(t, env.bus, phase31ID,
		approval.EventTypeToolApprovalRequested)
	defer cancelReq()

	args := json.RawMessage(`{"do":"thing"}`)
	done := make(chan struct{})
	go func() {
		defer close(done)
		out, err := env.gate.RunGuarded(phase31Ctx(t, phase31ID),
			&approval.ApprovalRequest{
				Tool:     tools.Tool{Name: "do_thing"},
				Args:     args,
				Identity: phase31ID,
			})
		if err != nil {
			t.Errorf("RunGuarded: %v", err)
		}
		if string(out) != string(args) {
			t.Errorf("args mismatch: got %s", out)
		}
	}()

	ev := phase31WaitEv(t, requestedSub, 2*time.Second)
	payload, _ := ev.Payload.(approval.ToolApprovalRequestedPayload)
	token := pauseresume.Token(payload.PauseToken)

	// Build the admin ctx the Phase 53 RunLoop would hand to the
	// gate's resolve path (a steering ControlApprove arrives → the
	// applier ctx carries the run quadruple identity + the admin
	// scope claim).
	adminCtx := phase31AdminCtx(t, phase31ID)
	if err := env.gate.ResolveApproval(adminCtx, token,
		approval.DecisionApprove, "steering APPROVE"); err != nil {
		t.Fatalf("ResolveApproval (steering-shape): %v", err)
	}
	// Synchronous join — the RunGuarded goroutine returns when the
	// resolution lands (Wave 11 §17.5 audit, finding W3: replaced
	// time.Sleep-as-sync with a done channel).
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("RunGuarded did not return after ResolveApproval")
	}
}

// TestE2E_Phase31_InitiateThenCancel_NoGoroutineLeak is the
// master-plan-style goroutine-leak fence: 25 cycles of
// pause-then-cancel-ctx-without-resolution → baseline restored.
func TestE2E_Phase31_InitiateThenCancel_NoGoroutineLeak(t *testing.T) {
	env := buildPhase31Env(t, approval.AlwaysDenyPolicy{})
	requestedSub, cancelReq := phase31SubFor(t, env.bus, phase31ID,
		approval.EventTypeToolApprovalRequested)
	defer cancelReq()

	baseline := runtime.NumGoroutine()
	for i := 0; i < 25; i++ {
		ctx, cancel := context.WithCancel(phase31Ctx(t, phase31ID))
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = env.gate.RunGuarded(ctx, &approval.ApprovalRequest{
				Tool: tools.Tool{Name: "x"}, Args: json.RawMessage(`{}`),
				Identity: phase31ID,
			})
		}()
		_ = phase31WaitEv(t, requestedSub, 2*time.Second)
		cancel()
		<-done
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+5 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	leak := runtime.NumGoroutine() - baseline
	if leak > 5 {
		t.Fatalf("goroutine leak: leaked=%d (baseline=%d, now=%d)",
			leak, baseline, runtime.NumGoroutine())
	}
}

// TestE2E_Phase31_Concurrency_NoCrossTalk runs N=16 distinct identity
// stacks concurrently — each parks via the same shared gate, each
// resolves under its own admin ctx; no cross-talk on identity or args
// (CLAUDE.md §17.3 final).
func TestE2E_Phase31_Concurrency_NoCrossTalk(t *testing.T) {
	env := buildPhase31Env(t, approval.AlwaysDenyPolicy{})

	// Admin-scope resolver subscribes via Admin filter so it can see
	// every identity's approval-request event.
	resolverCtx, cancelResolver := context.WithCancel(context.Background())
	defer cancelResolver()
	sub, err := env.bus.Subscribe(resolverCtx, events.Filter{
		Admin: true,
		Types: []events.EventType{approval.EventTypeToolApprovalRequested},
	})
	if err != nil {
		t.Fatalf("admin Subscribe: %v", err)
	}
	defer sub.Cancel()

	const N = 16
	var wg sync.WaitGroup
	errCh := make(chan error, N)

	// Spawn N callers.
	for i := 0; i < N; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", i%3),
				UserID:    fmt.Sprintf("user-%d", i),
				SessionID: fmt.Sprintf("session-%d", i),
			}
			args := json.RawMessage(fmt.Sprintf(`{"i":%d}`, i))
			out, err := env.gate.RunGuarded(phase31Ctx(t, id),
				&approval.ApprovalRequest{
					Tool: tools.Tool{Name: fmt.Sprintf("tool-%d", i)},
					Args: args, Identity: id,
				})
			if err != nil {
				errCh <- fmt.Errorf("g%d RunGuarded: %v", i, err)
				return
			}
			if string(out) != string(args) {
				errCh <- fmt.Errorf("g%d cross-context bleed: out=%s want=%s",
					i, out, args)
			}
		}()
	}

	// Resolver: receive N approval-request events, resolve each one
	// from the matching identity's admin ctx.
	resolverDone := make(chan struct{})
	go func() {
		defer close(resolverDone)
		seen := 0
		for ev := range sub.Events() {
			payload, ok := ev.Payload.(approval.ToolApprovalRequestedPayload)
			if !ok {
				continue
			}
			tok := pauseresume.Token(payload.PauseToken)
			adminCtx := protocolauth.WithScopes(
				func() context.Context {
					c, _ := identity.With(context.Background(), ev.Identity.Identity)
					return c
				}(),
				[]protocolauth.Scope{protocolauth.ScopeAdmin},
			)
			if err := env.gate.ResolveApproval(adminCtx, tok,
				approval.DecisionApprove, ""); err != nil {
				errCh <- fmt.Errorf("resolver: %v", err)
				return
			}
			seen++
			if seen == N {
				return
			}
		}
	}()

	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}
	<-resolverDone
}
