package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools"
)

func mkStore(t *testing.T) state.StateStore {
	t.Helper()
	s, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close(context.Background()) })
	return s
}

type decisionEnv struct {
	gate  *ApprovalGate
	bus   events.EventBus
	coord pauseresume.Coordinator
	log   DecisionLog
	store state.StateStore
}

func mkDecisionEnv(t *testing.T) *decisionEnv {
	t.Helper()
	red := patternsAudit.New()
	bus := mkTestBus(t, red)
	store := mkStore(t)
	log, err := NewStateDecisionLog(store)
	if err != nil {
		t.Fatalf("NewStateDecisionLog: %v", err)
	}
	coord := pauseresume.New(pauseresume.WithBus(bus), pauseresume.WithCheckpointStore(store),
		pauseresume.WithResolutionRecorder(NewDecisionRecorder(log)))
	g, err := NewApprovalGate(GateDeps{
		Policy: AlwaysDenyPolicy{Reason: "needs review"}, Coordinator: coord, Bus: bus,
		Redactor: red, Authorizer: NewIdentityAuthorizer(),
	})
	if err != nil {
		t.Fatalf("NewApprovalGate: %v", err)
	}
	t.Cleanup(func() { _ = g.Close(context.Background()) })
	return &decisionEnv{gate: g, bus: bus, coord: coord, log: log, store: store}
}

type guardResult struct {
	args json.RawMessage
	err  error
}

func runGuardedAsync(t *testing.T, g *ApprovalGate, ctx context.Context, args string) <-chan guardResult {
	t.Helper()
	out := make(chan guardResult, 1)
	go func() {
		a, err := g.RunGuarded(ctx, &ApprovalRequest{Tool: tools.Tool{Name: "delete_doc"}, Args: json.RawMessage(args), Identity: testID})
		out <- guardResult{a, err}
	}()
	return out
}

func awaitResult(t *testing.T, ch <-chan guardResult) guardResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(waitEventTimeout):
		t.Fatal("RunGuarded did not return")
		return guardResult{}
	}
}

var testDecider = pauseresume.Decider{TenantID: testID.TenantID, UserID: "approver-1", SessionID: testID.SessionID, Speaker: "Approver One"}

func TestGate_ApproveAttributesDeciderAndRecords(t *testing.T) {
	env := mkDecisionEnv(t)
	sub, cancel := subscribeTo(t, env.bus, testID, EventTypeToolApprovalRequested, EventTypeToolApproved)
	defer cancel()
	ctx := mkPlainCtx(t, testID)
	res := runGuardedAsync(t, env.gate, ctx, `{"doc":"d-1","password":"hunter2"}`)
	req := waitEvent(t, sub).Payload.(ToolApprovalRequestedPayload)

	if err := env.gate.ResolveApproval(pauseresume.WithDecider(ctx, testDecider), pauseresume.Token(req.PauseToken), DecisionApprove, "ok"); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	if r := awaitResult(t, res); r.err != nil || string(r.args) != `{"doc":"d-1","password":"hunter2"}` {
		t.Fatalf("RunGuarded = %s, %v; want original args", r.args, r.err)
	}
	ap := waitEvent(t, sub).Payload.(ToolApprovedPayload)
	if ap.DecidedByUser != testDecider.UserID || ap.DecidedBySpeaker != testDecider.Speaker ||
		ap.DecidedByTenant != testDecider.TenantID || ap.DecidedBySession != testDecider.SessionID || ap.DecidedAt.IsZero() {
		t.Fatalf("tool.approved = %+v", ap)
	}
	recs, err := env.log.Recent(context.Background(), testID)
	if err != nil || len(recs) != 1 {
		t.Fatalf("Recent = %+v, %v", recs, err)
	}
	r := recs[0]
	if r.Outcome != OutcomeApproved || r.Tool != "delete_doc" || r.PolicyReason != "needs review" || r.DecisionReason != "ok" ||
		r.DecidedBy.UserID != testDecider.UserID || r.DecidedBy.Speaker != testDecider.Speaker || r.RequestedAt.IsZero() {
		t.Fatalf("record = %+v", r)
	}
	if b, _ := json.Marshal(r.ArgsSummary); strings.Contains(string(b), "hunter2") || !strings.Contains(string(b), "d-1") {
		t.Fatalf("record args summary = %s", b)
	}
}

// The max-park sweeper's timeout resolution ends the wait with a typed
// expiry: the tool does not run, tool.rejected carries Decision
// "timeout", and the expiry is recorded.
func TestGate_ExpiryProducesTypedOutcome(t *testing.T) {
	env := mkDecisionEnv(t)
	sub, cancel := subscribeTo(t, env.bus, testID, EventTypeToolApprovalRequested, EventTypeToolRejected)
	defer cancel()
	ctx := mkPlainCtx(t, testID)
	res := runGuardedAsync(t, env.gate, ctx, `{"doc":"d-2"}`)
	req := waitEvent(t, sub).Payload.(ToolApprovalRequestedPayload)

	// What the sweeper does on a deadline: Resume with DecisionTimeout
	// under the pause's own identity.
	if err := env.coord.Resume(ctx, pauseresume.Token(req.PauseToken), pauseresume.DecisionTimeout, map[string]any{"timed_out": true}); err != nil {
		t.Fatalf("timeout Resume: %v", err)
	}
	r := awaitResult(t, res)
	if !errors.Is(r.err, ErrApprovalExpired) || !errors.Is(r.err, ErrToolRejectedSentinel) {
		t.Fatalf("RunGuarded err = %v, want ErrApprovalExpired joined with a rejection", r.err)
	}
	var rej *ErrToolRejected
	if !errors.As(r.err, &rej) || rej.Reason != ExpiredReason {
		t.Fatalf("typed rejection = %+v", rej)
	}
	ev := waitEvent(t, sub).Payload.(ToolRejectedPayload)
	if ev.Decision != string(pauseresume.DecisionTimeout) || ev.Reason != ExpiredReason || ev.DecidedByUser != "" || ev.DecidedAt.IsZero() {
		t.Fatalf("tool.rejected = %+v", ev)
	}
	recs, err := env.log.Recent(context.Background(), testID)
	if err != nil || len(recs) != 1 || recs[0].Outcome != OutcomeExpired {
		t.Fatalf("Recent = %+v, %v", recs, err)
	}
	if env.gate.pendingLen() != 0 {
		t.Fatalf("pending entries = %d after expiry", env.gate.pendingLen())
	}
}

// A restart leaves the approval's checkpoint without a waiter; the boot
// pass resolves it with the typed `interrupted` outcome and event, and
// records it.
func TestResolveInterruptedApprovals_AfterRestart(t *testing.T) {
	env := mkDecisionEnv(t)
	sub, cancel := subscribeTo(t, env.bus, testID, EventTypeToolApprovalRequested, EventTypeToolRejected)
	defer cancel()
	waitCtx, stopWaiter := context.WithCancel(mkPlainCtx(t, testID))
	res := runGuardedAsync(t, env.gate, waitCtx, `{"doc":"d-3"}`)
	req := waitEvent(t, sub).Payload.(ToolApprovalRequestedPayload)
	// The process "dies": its waiter goes away, the checkpoint stays.
	stopWaiter()
	if r := awaitResult(t, res); !errors.Is(r.err, ErrApprovalCancelled) {
		t.Fatalf("waiter err = %v", r.err)
	}

	restarted := pauseresume.New(pauseresume.WithCheckpointStore(env.store), pauseresume.WithResolutionRecorder(NewDecisionRecorder(env.log)))
	n, err := ResolveInterruptedApprovals(context.Background(), restarted, env.bus, nil)
	if err != nil || n != 1 {
		t.Fatalf("ResolveInterruptedApprovals = %d, %v; want 1", n, err)
	}
	ev := waitEvent(t, sub).Payload.(ToolRejectedPayload)
	if ev.PauseToken != req.PauseToken || ev.Tool != "delete_doc" || ev.Decision != string(pauseresume.DecisionInterrupted) || ev.Reason != InterruptedReason {
		t.Fatalf("tool.rejected = %+v", ev)
	}
	recs, err := env.log.Recent(context.Background(), testID)
	if err != nil || len(recs) != 1 || recs[0].Outcome != OutcomeInterrupted || recs[0].PolicyReason != "needs review" {
		t.Fatalf("Recent = %+v, %v", recs, err)
	}
	list, err := restarted.List(mkPlainCtx(t, testID), pauseresume.ListRequest{Identity: testID})
	if err != nil || len(list.Snapshots) != 0 {
		t.Fatalf("pause.list after resolution = %+v, %v", list.Snapshots, err)
	}
}

func TestDecisionLog_PrunesAndBounds(t *testing.T) {
	t.Parallel()
	log, err := NewStateDecisionLog(mkStore(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	base := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	for i := range MaxRecordedDecisions + 5 {
		if err := log.Record(ctx, testID, DecisionRecord{
			Tool: "t", PauseToken: fmt.Sprintf("tok-%02d", i), Outcome: OutcomeApproved,
			DecidedAt: base.Add(time.Duration(i) * time.Minute), ArgsSummary: map[string]any{"big": strings.Repeat("x", 4096)},
		}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	recs, err := log.Recent(ctx, testID)
	if err != nil || len(recs) != MaxRecordedDecisions {
		t.Fatalf("Recent len = %d, %v; want %d", len(recs), err, MaxRecordedDecisions)
	}
	if recs[0].PauseToken != fmt.Sprintf("tok-%02d", MaxRecordedDecisions+4) {
		t.Fatalf("newest first: got %q", recs[0].PauseToken)
	}
	summary, _ := recs[0].ArgsSummary.(map[string]any)
	if summary["omitted"] != "too_large" {
		t.Fatalf("oversized summary not bounded: %v", recs[0].ArgsSummary)
	}
	// Another session sees nothing (identity-scoped).
	other := identity.Identity{TenantID: testID.TenantID, UserID: testID.UserID, SessionID: "other"}
	if recs, err := log.Recent(ctx, other); err != nil || len(recs) != 0 {
		t.Fatalf("other session = %+v, %v", recs, err)
	}
	if _, err := log.Recent(ctx, identity.Identity{}); err == nil {
		t.Fatal("missing identity must fail closed")
	}
	// The wire projection caps at the inspect limit and maps attribution.
	out, err := InterventionReader{Log: log}.RecentInterventions(ctx, testID)
	if err != nil || len(out) != 5 || out[0].Type != InterventionTypeToolApproval || out[0].DecidedBy != nil {
		t.Fatalf("RecentInterventions = %+v, %v", out, err)
	}
}

func runScopedCtx(t *testing.T, runID string) context.Context {
	t.Helper()
	ctx, err := identity.WithRun(mkPlainCtx(t, testID), testID, runID)
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

// Two writers' runs in one shared session: a resolver acting for run A
// cannot resolve run B's approval; run B's own resolver can.
func TestResolveApproval_BoundToOriginatingRun(t *testing.T) {
	env := mkDecisionEnv(t)
	sub, cancel := subscribeTo(t, env.bus, testID, EventTypeToolApprovalRequested)
	defer cancel()
	res := runGuardedAsync(t, env.gate, runScopedCtx(t, "run-B"), `{"doc":"b"}`)
	req := waitEvent(t, sub).Payload.(ToolApprovalRequestedPayload)

	err := env.gate.ResolveApproval(runScopedCtx(t, "run-A"), pauseresume.Token(req.PauseToken), DecisionApprove, "")
	if !errors.Is(err, ErrApprovalNotFound) {
		t.Fatalf("foreign-run resolve = %v, want ErrApprovalNotFound", err)
	}
	if env.gate.pendingLen() != 1 {
		t.Fatal("foreign-run attempt consumed the pending approval")
	}
	if err := env.gate.ResolveApproval(runScopedCtx(t, "run-B"), pauseresume.Token(req.PauseToken), DecisionReject, ""); err != nil {
		t.Fatalf("own-run resolve: %v", err)
	}
	if r := awaitResult(t, res); !errors.Is(r.err, ErrToolRejectedSentinel) {
		t.Fatalf("RunGuarded = %v", r.err)
	}
}

// The approver's note is redacted with the argument redactor and bounded
// before it reaches the event and the durable record.
func TestResolveApproval_RedactsAndBoundsNote(t *testing.T) {
	env := mkDecisionEnv(t)
	sub, cancel := subscribeTo(t, env.bus, testID, EventTypeToolApprovalRequested, EventTypeToolApproved)
	defer cancel()
	ctx := mkPlainCtx(t, testID)
	res := runGuardedAsync(t, env.gate, ctx, `{}`)
	req := waitEvent(t, sub).Payload.(ToolApprovalRequestedPayload)
	secret := "Bearer abcdefghijklmnopqrstuvwxyz0123456789"
	note := "ok, use " + secret + " " + strings.Repeat("y", 2000)
	if err := env.gate.ResolveApproval(ctx, pauseresume.Token(req.PauseToken), DecisionApprove, note); err != nil {
		t.Fatalf("ResolveApproval: %v", err)
	}
	awaitResult(t, res)
	ap := waitEvent(t, sub).Payload.(ToolApprovedPayload)
	if strings.Contains(ap.ApproverReason, "abcdefghijklmnop") || len(ap.ApproverReason) > maxDecisionNoteBytes {
		t.Fatalf("event note not redacted/bounded: %d bytes %q", len(ap.ApproverReason), ap.ApproverReason[:min(80, len(ap.ApproverReason))])
	}
	recs, _ := env.log.Recent(context.Background(), testID)
	if len(recs) != 1 || strings.Contains(recs[0].DecisionReason, "abcdefghijklmnop") || recs[0].DecisionReason != ap.ApproverReason {
		t.Fatalf("record note = %+v", recs)
	}
	if !recs[0].DecidedAt.Equal(ap.DecidedAt) {
		t.Fatalf("record and event disagree on decision time: %v vs %v", recs[0].DecidedAt, ap.DecidedAt)
	}
}

// An approval arriving after the park deadline loses to the typed
// timeout inside the Coordinator; the gated call expires and the pending
// entry (with the original arguments) is not restored.
func TestResolveApproval_AfterDeadlineLosesToTimeout(t *testing.T) {
	red := patternsAudit.New()
	bus := mkTestBus(t, red)
	store := mkStore(t)
	log, _ := NewStateDecisionLog(store)
	var now atomicTime
	now.set(time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC))
	coord := pauseresume.New(pauseresume.WithBus(bus), pauseresume.WithCheckpointStore(store),
		pauseresume.WithMaxParkDuration(time.Minute), pauseresume.WithClock(now.get),
		pauseresume.WithResolutionRecorder(NewDecisionRecorder(log)))
	g, err := NewApprovalGate(GateDeps{Policy: AlwaysDenyPolicy{}, Coordinator: coord, Bus: bus, Redactor: red, Authorizer: NewIdentityAuthorizer()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close(context.Background()) })
	sub, cancel := subscribeTo(t, bus, testID, EventTypeToolApprovalRequested, EventTypeToolRejected)
	defer cancel()
	ctx := mkPlainCtx(t, testID)
	res := runGuardedAsync(t, g, ctx, `{"doc":"late"}`)
	req := waitEvent(t, sub).Payload.(ToolApprovalRequestedPayload)

	now.set(now.get().Add(2 * time.Minute))
	err = g.ResolveApproval(ctx, pauseresume.Token(req.PauseToken), DecisionApprove, "too late")
	if !errors.Is(err, pauseresume.ErrPauseExpired) {
		t.Fatalf("late approve = %v, want ErrPauseExpired", err)
	}
	if r := awaitResult(t, res); !errors.Is(r.err, ErrApprovalExpired) {
		t.Fatalf("RunGuarded = %v, want ErrApprovalExpired", r.err)
	}
	if ev := waitEvent(t, sub).Payload.(ToolRejectedPayload); ev.Decision != string(pauseresume.DecisionTimeout) {
		t.Fatalf("tool.rejected = %+v", ev)
	}
	if g.pendingLen() != 0 {
		t.Fatal("expired approval restored to the pending map")
	}
	recs, _ := log.Recent(context.Background(), testID)
	if len(recs) != 1 || recs[0].Outcome != OutcomeExpired || recs[0].DecidedBy != (DecidedBy{}) {
		t.Fatalf("record = %+v", recs)
	}
}

type atomicTime struct {
	mu sync.Mutex
	t  time.Time
}

func (a *atomicTime) set(t time.Time) { a.mu.Lock(); a.t = t; a.mu.Unlock() }
func (a *atomicTime) get() time.Time  { a.mu.Lock(); defer a.mu.Unlock(); return a.t }

// blockingResumeCoord blocks Resume until released, then fails with a
// retryable error — the window in which a waiter can retire.
type blockingResumeCoord struct {
	pauseresume.Coordinator
	entered chan struct{}
	release chan struct{}
}

func (c *blockingResumeCoord) Resume(context.Context, pauseresume.Token, pauseresume.Decision, map[string]any) error {
	close(c.entered)
	<-c.release
	return errors.New("transient store error")
}

// A losing resolver cannot re-insert an entry (and its arguments) the
// waiter retired while the resolver's Resume was in flight.
func TestResolveApproval_RetiredEntryIsNeverRestored(t *testing.T) {
	red := patternsAudit.New()
	bus := mkTestBus(t, red)
	coord := &blockingResumeCoord{Coordinator: pauseresume.New(pauseresume.WithBus(bus)), entered: make(chan struct{}), release: make(chan struct{})}
	g, err := NewApprovalGate(GateDeps{Policy: AlwaysDenyPolicy{}, Coordinator: coord, Bus: bus, Redactor: red, Authorizer: NewIdentityAuthorizer()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = g.Close(context.Background()) })
	sub, cancelSub := subscribeTo(t, bus, testID, EventTypeToolApprovalRequested)
	defer cancelSub()
	waitCtx, stopWaiter := context.WithCancel(mkPlainCtx(t, testID))
	res := runGuardedAsync(t, g, waitCtx, `{"secret_arg":"x"}`)
	req := waitEvent(t, sub).Payload.(ToolApprovalRequestedPayload)

	resolved := make(chan error, 1)
	go func() {
		resolved <- g.ResolveApproval(mkPlainCtx(t, testID), pauseresume.Token(req.PauseToken), DecisionApprove, "")
	}()
	<-coord.entered
	stopWaiter()
	if r := awaitResult(t, res); !errors.Is(r.err, ErrApprovalCancelled) {
		t.Fatalf("waiter = %v", r.err)
	}
	close(coord.release)
	if err := <-resolved; err == nil {
		t.Fatal("resolver should report the failed resume")
	}
	if g.pendingLen() != 0 {
		t.Fatal("a retired entry was restored to the pending map")
	}
}

// The waiter is registered before the pause is visible: a resolver that
// acts on pause.requested immediately always finds it.
func TestRunGuarded_WaiterRegisteredBeforePauseIsVisible(t *testing.T) {
	for i := range 20 {
		red := patternsAudit.New()
		bus := mkTestBus(t, red)
		coord := pauseresume.New(pauseresume.WithBus(bus))
		g, err := NewApprovalGate(GateDeps{Policy: AlwaysDenyPolicy{}, Coordinator: coord, Bus: bus, Redactor: red, Authorizer: NewIdentityAuthorizer()})
		if err != nil {
			t.Fatal(err)
		}
		sub, cancelSub := subscribeTo(t, bus, testID, pauseresume.EventTypePauseRequested)
		ctx := mkPlainCtx(t, testID)
		res := runGuardedAsync(t, g, ctx, `{}`)
		tok := waitEvent(t, sub).Payload.(pauseresume.PauseRequestedPayload).Token
		if err := g.ResolveApproval(ctx, pauseresume.Token(tok), DecisionApprove, ""); err != nil {
			t.Fatalf("iteration %d: immediate resolve on pause.requested: %v", i, err)
		}
		if r := awaitResult(t, res); r.err != nil {
			t.Fatalf("iteration %d: RunGuarded = %v", i, r.err)
		}
		cancelSub()
		_ = g.Close(context.Background())
	}
}
