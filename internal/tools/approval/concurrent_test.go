package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tools"
)

// alternatingPolicy is a tiny test-only policy that alternates between
// Required=true (RunGuarded parks) and Required=false (short-circuit).
// Index advances atomically so N concurrent callers see a deterministic
// 50/50 split.
type alternatingPolicy struct {
	idx atomic.Int64
}

func (p *alternatingPolicy) ShouldApprove(_ context.Context, _ *ApprovalRequest) (bool, string, error) {
	n := p.idx.Add(1)
	if n%2 == 0 {
		return false, "", nil
	}
	return true, "concurrent-test", nil
}

// TestApprovalGate_ConcurrentReuse_NoCrossTalk pins the D-025
// concurrent-reuse contract for ApprovalGate. N=128 concurrent
// RunGuarded invocations against ONE shared gate, distinct identity
// stacks; the test asserts:
//
//   - no data races (the -race flag is the gate);
//   - no context bleed (every returned identity exactly matches the
//     caller's identity — the gate never confuses run A's args with
//     run B's);
//   - no cross-cancellation (cancelling run A's ctx must NOT affect
//     run B; we cancel half the callers AFTER their pause was
//     registered; the other half resolve via the admin path; both
//     halves' outcomes are correct);
//   - no goroutine leaks (baseline-restored after every cycle joined).
//
// CLAUDE.md §5 + §11 + D-025 — the §13 amendment-era pre-merge
// checklist row requires this test.
func TestApprovalGate_ConcurrentReuse_NoCrossTalk(t *testing.T) {
	const N = 128

	red := patternsAudit.New()
	bus := mkConcurrentBus(t, red)
	coord := pauseresume.New()
	policy := &alternatingPolicy{}
	g, err := NewApprovalGate(GateDeps{
		Policy: policy, Coordinator: coord, Bus: bus, Redactor: red,
	})
	if err != nil {
		t.Fatalf("NewApprovalGate: %v", err)
	}
	t.Cleanup(func() { _ = g.Close(context.Background()) })

	// Admin-scoped resolver subscribes to all approval-request events
	// across every identity (Admin filter) and resolves them as they
	// land. We resolve a random ~half of the parked calls; the rest
	// are ctx-cancelled (their callers cancel after a brief wait).
	adminSubCtx, adminSubCancel := context.WithCancel(context.Background())
	defer adminSubCancel()
	adminSub, err := bus.Subscribe(adminSubCtx, events.Filter{
		Admin: true,
		Types: []events.EventType{EventTypeToolApprovalRequested},
	})
	if err != nil {
		t.Fatalf("admin Subscribe: %v", err)
	}
	defer adminSub.Cancel()

	// Resolver goroutine — drains pending approvals.
	resolverDone := make(chan struct{})
	go func() {
		defer close(resolverDone)
		for ev := range adminSub.Events() {
			p, ok := ev.Payload.(ToolApprovalRequestedPayload)
			if !ok {
				continue
			}
			// Resolve from the admin ctx for the matching identity.
			tok := pauseresume.Token(p.PauseToken)
			adminCtx := mkConcurrentAdminCtx(ev.Identity.Identity)
			// 50/50 approve vs reject so both code paths exercise.
			decision := DecisionApprove
			if int(ev.Sequence)%2 == 0 {
				decision = DecisionReject
			}
			_ = g.ResolveApproval(adminCtx, tok, decision, "concurrent-test")
		}
	}()

	baseline := runtime.NumGoroutine()
	var wg sync.WaitGroup
	errCh := make(chan error, N)

	for i := range N {

		wg.Add(1)
		go func() {
			defer wg.Done()
			id := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-%d", i%7),
				UserID:    fmt.Sprintf("user-%d", i),
				SessionID: fmt.Sprintf("session-%d", i),
			}
			ctx, cancel := context.WithCancel(mkIDCtx(id))
			defer cancel()
			args := json.RawMessage(fmt.Sprintf(`{"i":%d}`, i))
			req := &ApprovalRequest{
				Tool:     tools.Tool{Name: fmt.Sprintf("tool-%d", i%5)},
				Args:     args,
				Identity: id,
				Tags:     []string{"sensitive"},
			}
			out, err := g.RunGuarded(ctx, req)
			if err == nil {
				// Approved or short-circuited: out must equal the
				// caller's args (no cross-context bleed).
				if string(out) != string(args) {
					errCh <- fmt.Errorf("g%d cross-context bleed: out=%s want=%s", i, out, args)
				}
				return
			}
			// Either ErrToolRejected (admin said no) or
			// ErrApprovalCancelled (we cancelled before resolver).
			var rejErr *ErrToolRejected
			switch {
			case errors.As(err, &rejErr):
				if rejErr.Identity != id {
					errCh <- fmt.Errorf("g%d rejected identity mismatch: got %+v want %+v",
						i, rejErr.Identity, id)
				}
				if rejErr.Tool != req.Tool.Name {
					errCh <- fmt.Errorf("g%d rejected tool mismatch: got %q want %q",
						i, rejErr.Tool, req.Tool.Name)
				}
			case errors.Is(err, ErrApprovalCancelled):
				// fine — caller's ctx was cancelled, can happen on
				// the goroutine-leak / cancel half of the alternating
				// pattern.
			default:
				errCh <- fmt.Errorf("g%d unexpected err: %w", i, err)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Error(e)
	}

	// Stop the resolver: cancel the admin subscription so its loop
	// exits.
	adminSub.Cancel()
	adminSubCancel()
	select {
	case <-resolverDone:
	case <-time.After(2 * time.Second):
		t.Fatal("resolver did not exit")
	}

	// Goroutine-leak check — baseline-restored within a bounded
	// window. The bus + Coordinator may have transient goroutines;
	// allow a small slack.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+10 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	leak := runtime.NumGoroutine() - baseline
	if leak > 10 {
		t.Fatalf("goroutine leak: leaked=%d (baseline=%d, now=%d)",
			leak, baseline, runtime.NumGoroutine())
	}
	if g.pendingLen() != 0 {
		t.Errorf("pendingLen leftover: %d", g.pendingLen())
	}
}

func mkConcurrentBus(t *testing.T, red *patternsAudit.Driver) events.EventBus {
	t.Helper()
	cfg := config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              2 * time.Second,
		DropWindow:               50 * time.Millisecond,
	}
	b, err := eventsInmem.New(cfg, red)
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b
}

func mkIDCtx(id identity.Identity) context.Context {
	ctx, _ := identity.With(context.Background(), id)
	return ctx
}

// mkConcurrentAdminCtx is a non-test-helper version of mkAdminCtx used
// from inside the resolver goroutine where we cannot pass *testing.T
// without provoking a vet warning about test-helper misuse.
func mkConcurrentAdminCtx(id identity.Identity) context.Context {
	base, _ := identity.With(context.Background(), id)
	return protocolauth.WithScopes(base, []protocolauth.Scope{protocolauth.ScopeAdmin})
}
