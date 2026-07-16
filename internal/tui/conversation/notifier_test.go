package conversation

import (
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tui/projection"
)

func notifierTestIdentity() types.IdentityScope {
	return types.IdentityScope{Tenant: "t", User: "u", Session: "s"}
}

// TestNotifier_CoalesceFlagsOverflowWithoutFalseReconciliation proves that
// loss-less batchable coalescing marks Overflow but never fabricates a
// ReconciliationRequired flag or a ReplayGap. Coalescing merges cumulative
// projections, so nothing is missing and no reconciliation is warranted.
func TestNotifier_CoalesceFlagsOverflowWithoutFalseReconciliation(t *testing.T) {
	id := notifierTestIdentity()
	cases := []struct {
		name       string
		first      Update
		second     Update
		wantMerged bool
	}{
		{
			name:       "second_coalesces_over_resident_first",
			first:      Update{Identity: id, Generation: 1, State: StateLive, Batchable: true, Projection: projection.Projection{Identity: id, Generation: 1, LastSequence: 5}},
			second:     Update{Identity: id, Generation: 1, State: StateLive, Batchable: true, Projection: projection.Projection{Identity: id, Generation: 1, LastSequence: 7}},
			wantMerged: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			n := NewNotifier(4)
			defer n.Close()
			n.Notify(tc.first)
			n.Notify(tc.second)

			update, ok := n.Next(t.Context())
			if !ok {
				t.Fatal("expected an update")
			}
			if !update.Batchable {
				t.Fatalf("expected batchable update, got %+v", update)
			}
			if update.Projection.LastSequence != tc.second.Projection.LastSequence {
				t.Fatalf("expected newest sequence %d, got %d", tc.second.Projection.LastSequence, update.Projection.LastSequence)
			}
			if !update.Overflow {
				t.Fatal("coalesced update must be flagged Overflow")
			}
			if update.Projection.ReconciliationRequired {
				t.Fatal("coalesced update must NOT set ReconciliationRequired (loss-less merge)")
			}
			if update.Projection.ReplayGap != nil {
				t.Fatalf("coalesced update must NOT fabricate a ReplayGap, got %+v", update.Projection.ReplayGap)
			}
		})
	}
}

// TestNotifier_SingleBatchableUpdateHasNoOverflow guards the non-coalescing
// path: a lone batchable update carries neither Overflow nor any reconciliation
// signal.
func TestNotifier_SingleBatchableUpdateHasNoOverflow(t *testing.T) {
	id := notifierTestIdentity()
	n := NewNotifier(4)
	defer n.Close()
	n.Notify(Update{Identity: id, Generation: 1, State: StateLive, Batchable: true, Projection: projection.Projection{Identity: id, Generation: 1, LastSequence: 3}})

	update, ok := n.Next(t.Context())
	if !ok {
		t.Fatal("expected an update")
	}
	if update.Overflow {
		t.Fatal("single batchable update must not set Overflow")
	}
	if update.Projection.ReconciliationRequired || update.Projection.ReplayGap != nil {
		t.Fatal("single batchable update must not carry reconciliation signals")
	}
}

// TestNotifier_CriticalUpdateDropsStaleResidentBatchFrame proves W2: after a
// critical frame at sequence N+1 is delivered, a subsequent Next must NOT return
// the still-resident older batch frame at sequence <= N+1 (which would regress
// observable state, e.g. a completed tool back to "running").
func TestNotifier_CriticalUpdateDropsStaleResidentBatchFrame(t *testing.T) {
	id := notifierTestIdentity()
	n := NewNotifier(4)
	defer n.Close()

	// Older batch frame at seq N stays resident (Next is not called yet).
	n.Notify(Update{Identity: id, Generation: 1, State: StateLive, Batchable: true, Projection: projection.Projection{Identity: id, Generation: 1, LastSequence: 10}})
	// Critical (non-batchable) frame at seq N+1 supersedes it.
	n.Notify(Update{Identity: id, Generation: 1, State: StateLive, Batchable: false, Projection: projection.Projection{Identity: id, Generation: 1, LastSequence: 11}})

	critical, ok := n.Next(t.Context())
	if !ok {
		t.Fatal("expected the critical update")
	}
	if critical.Batchable {
		t.Fatalf("expected critical (non-batchable) update first, got %+v", critical)
	}
	if critical.Projection.LastSequence != 11 {
		t.Fatalf("expected critical seq 11, got %d", critical.Projection.LastSequence)
	}

	// The stale seq-10 batch frame must have been dropped; a follow-up Next
	// must not hand it back and regress state.
	select {
	case leftover := <-n.wake:
		_ = leftover
	default:
	}
	n.mu.Lock()
	resident := n.latest
	n.mu.Unlock()
	if resident != nil {
		t.Fatalf("stale batch frame at seq %d was not dropped after critical seq 11", resident.Projection.LastSequence)
	}
}

// TestNotifier_CriticalUpdatePreservesStrictlyNewerBatchFrame guards the
// newest-wins boundary: when the resident batch frame is strictly newer than
// the critical frame, it must survive and be delivered on the next call.
func TestNotifier_CriticalUpdatePreservesStrictlyNewerBatchFrame(t *testing.T) {
	id := notifierTestIdentity()
	n := NewNotifier(4)
	defer n.Close()

	// Critical frame at seq 11 enqueued first.
	n.Notify(Update{Identity: id, Generation: 1, State: StateLive, Batchable: false, Projection: projection.Projection{Identity: id, Generation: 1, LastSequence: 11}})
	// Strictly newer batch frame at seq 12 stays resident.
	n.Notify(Update{Identity: id, Generation: 1, State: StateLive, Batchable: true, Projection: projection.Projection{Identity: id, Generation: 1, LastSequence: 12}})

	critical, ok := n.Next(t.Context())
	if !ok || critical.Batchable {
		t.Fatalf("expected critical update first, got ok=%v %+v", ok, critical)
	}

	next, ok := n.Next(t.Context())
	if !ok {
		t.Fatal("expected the strictly-newer batch frame")
	}
	if !next.Batchable || next.Projection.LastSequence != 12 {
		t.Fatalf("expected preserved batch frame at seq 12, got %+v", next)
	}
}
