package approval

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tools"
)

func TestGate_InvocationWithdrawalAndResolvedRace(t *testing.T) {
	for _, resolvedFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("resolved-first-%v", resolvedFirst), func(t *testing.T) {
			g, bus := mkGate(t, AlwaysDenyPolicy{})
			sub, closeSub := subscribeTo(t, bus, testID, EventTypeToolApprovalRequested)
			defer closeSub()
			signal := make(chan struct{})
			ctx := tools.WithInvocationFence(mkPlainCtx(t, testID), signal)
			done := make(chan error, 1)
			go func() {
				_, err := g.RunGuarded(ctx, &ApprovalRequest{Tool: tools.Tool{Name: "guarded"}, Identity: testID})
				done <- err
			}()
			event := waitEvent(t, sub)
			payload, ok := event.Payload.(ToolApprovalRequestedPayload)
			if !ok {
				t.Fatalf("approval payload=%T", event.Payload)
			}
			token := pauseresume.Token(payload.PauseToken)
			if resolvedFirst {
				if err := g.coordinator.Resume(ctx, token, pauseresume.DecisionReject, nil); err != nil {
					t.Fatal(err)
				}
			}
			close(signal)
			if err := <-done; !errors.Is(err, tools.ErrInvocationSuperseded) || errors.Is(err, tools.ErrInvocationCleanupFailed) {
				t.Fatalf("withdrawal=%v", err)
			}
			status, err := g.coordinator.Status(ctx, token)
			if err != nil || status.State != pauseresume.StatusResumed || status.Decision != pauseresume.DecisionReject || g.pendingLen() != 0 {
				t.Fatalf("status=%+v pending=%d err=%v", status, g.pendingLen(), err)
			}
			if _, err := g.RunGuarded(ctx, &ApprovalRequest{Tool: tools.Tool{Name: "guarded"}, Identity: testID}); !errors.Is(err, tools.ErrInvocationSuperseded) {
				t.Fatalf("already-obsolete invocation admitted: %v", err)
			}
		})
	}
}

type failedWithdrawal struct{ pauseresume.Coordinator }

func (f failedWithdrawal) Resume(ctx context.Context, _ pauseresume.Token, _ pauseresume.Decision, _ map[string]any) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("required checkpoint delete failed")
}

func TestGate_InvocationWithdrawalFailureIsTerminal(t *testing.T) {
	g, bus := mkGate(t, TaggedPolicy{RequireTags: []string{"write"}})
	g.coordinator = failedWithdrawal{Coordinator: g.coordinator}
	sub, closeSub := subscribeTo(t, bus, testID, EventTypeToolApprovalRequested)
	defer closeSub()
	signal := make(chan struct{})
	ctx := tools.WithInvocationFence(mkPlainCtx(t, testID), signal)
	done := make(chan error, 1)
	go func() {
		_, err := g.RunGuarded(ctx, &ApprovalRequest{Tool: tools.Tool{Name: "guarded"}, Identity: testID, Tags: []string{"write"}})
		done <- err
	}()
	waitEvent(t, sub)
	close(signal)
	err := <-done
	if !errors.Is(err, tools.ErrInvocationSuperseded) || !errors.Is(err, tools.ErrInvocationCleanupFailed) {
		t.Fatalf("required cleanup failure lost: %v", err)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.pending) != 0 {
		t.Fatal("obsolete in-process approval remains invocable")
	}
}
