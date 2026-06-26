package sessions_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
)

// lateTaskEvent fabricates a session-lifecycle event under the given triple
// — the stand-in for a lifecycle event a task that was still finishing
// concurrently emits to the durable event log AFTER the erasure cascade has
// already swept the session. session.touched is a registered, redacted
// event type; the bus persists it under the triple exactly like a real
// `task.*` lifecycle event.
func lateTaskEvent(id identity.Identity, seq int64) events.Event {
	return events.Event{
		Type:     sessions.EventTypeSessionTouched,
		Identity: identity.Quadruple{Identity: id},
		Payload:  sessions.SessionTouchedPayload{SessionID: id.SessionID, LastSeen: seq},
	}
}

func historyFilter(id identity.Identity) events.Filter {
	return events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
}

// TestCascadeEraser_FencesLateEvent_StateHistoryStaysEmpty is the binding
// regression for the live-found concurrency gap: `sessions.delete` must
// erase durably even when a task is finishing concurrently and emits its
// lifecycle events to the durable event log AFTER the cascade's State sweep.
// Without the erasure fence, the late event re-creates a `events.durable.head`
// record under the erased triple, so `state.history` returns a non-empty
// head/tail — contradicting the erasure-completeness guarantee. The test
// drives the REAL durable bus + the REAL StateStore, with no mocks at the
// seam.
func TestCascadeEraser_FencesLateEvent_StateHistoryStaysEmpty(t *testing.T) {
	f := newErasureFixture(t, nil) // no running probe ⇒ erasure is allowed
	ctx := context.Background()
	id := ident("t-fence", "u-fence", "s-fence")
	ictx := ctxFor(id)

	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.reg.Touch(ictx, id.SessionID); err != nil {
		t.Fatalf("Touch: %v", err)
	}

	hr, ok := f.bus.(events.HistoryReplayer)
	if !ok {
		t.Fatalf("durable bus must implement events.HistoryReplayer")
	}
	filter := historyFilter(id)

	// Pre-erasure: state.history is non-empty (the session has a stream).
	if _, _, _, err := hr.Bounds(ctx, filter); err != nil {
		t.Fatalf("pre-erasure Bounds = %v, want a non-empty history", err)
	}

	// Erase the session (the cascade fences the triple, then sweeps).
	if _, err := f.eraser.Erase(ctx, id); err != nil {
		t.Fatalf("Erase: %v", err)
	}

	// The in-flight task finishes AFTER the sweep and emits a lifecycle
	// event under the erased triple. This is the EXACT interleaving the live
	// test hit. The publish must be a graceful, fenced DROP — not an error,
	// and not a re-created history record.
	if err := f.bus.Publish(ctx, lateTaskEvent(id, 1)); err != nil {
		t.Fatalf("late event publish should be a graceful drop, got error: %v", err)
	}

	// GUARANTEE: post-erasure state.history for the erased triple is EMPTY
	// despite the late event.
	if _, _, _, err := hr.Bounds(ctx, filter); !errors.Is(err, events.ErrNoHistory) {
		t.Fatalf("post-erasure Bounds = %v, want events.ErrNoHistory (state.history must be empty)", err)
	}
	evs, err := hr.Window(ctx, 0, 100, filter)
	if err != nil {
		t.Fatalf("post-erasure Window: %v", err)
	}
	if len(evs) != 0 {
		t.Fatalf("post-erasure Window returned %d events, want 0", len(evs))
	}
	// The durable head was not re-created in the StateStore.
	if _, err := f.store.Load(ctx, identity.Quadruple{Identity: id}, "events.durable.head"); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("late event re-created the durable head under the erased triple: err=%v", err)
	}
}

// TestRegistry_Unfence_OnReusedSessionID asserts the fence is LIFTED when a
// brand-new session reuses an erased session id, so the new session retains
// its events normally (a fence that outlived the erased session would
// silently swallow the reused session's stream).
func TestRegistry_Unfence_OnReusedSessionID(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t-reuse", "u-reuse", "s-reuse")
	ictx := ctxFor(id)

	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("first Open: %v", err)
	}
	if _, err := f.eraser.Erase(ctx, id); err != nil {
		t.Fatalf("Erase: %v", err)
	}

	hr := f.bus.(events.HistoryReplayer)
	filter := historyFilter(id)

	// Sanity: the triple is fenced — a late event is dropped and history is
	// empty.
	if err := f.bus.Publish(ctx, lateTaskEvent(id, 1)); err != nil {
		t.Fatalf("fenced publish should drop gracefully: %v", err)
	}
	if _, _, _, err := hr.Bounds(ctx, filter); !errors.Is(err, events.ErrNoHistory) {
		t.Fatalf("between erase and reopen Bounds = %v, want ErrNoHistory", err)
	}

	// Reopen a NEW session with the SAME id (a freshly-reused conversation
	// id). Open lifts the fence before emitting session.opened.
	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("reopen reused id: %v", err)
	}
	// The new session retains its events again: state.history is non-empty.
	if _, _, _, err := hr.Bounds(ctx, filter); err != nil {
		t.Fatalf("post-reopen Bounds = %v, want history retained (fence lifted)", err)
	}
	// And a fresh event is now retained rather than dropped.
	if err := f.bus.Publish(ctx, lateTaskEvent(id, 2)); err != nil {
		t.Fatalf("post-reopen publish: %v", err)
	}
	evs, err := hr.Window(ctx, 0, 100, filter)
	if err != nil {
		t.Fatalf("post-reopen Window: %v", err)
	}
	if len(evs) == 0 {
		t.Fatalf("post-reopen Window returned 0 events, want the reused session's stream retained")
	}
}

// TestCascadeEraser_FenceConcurrent_NoLateHistory stresses the fence under
// the race detector: N sessions are each erased CONCURRENTLY with a task
// goroutine publishing late lifecycle events to the same triple. Every
// erased session must read empty history afterwards, with no data race, no
// cross-session bleed, and no late head re-created. Run under `go test
// -race` (the package gate).
func TestCascadeEraser_FenceConcurrent_NoLateHistory(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	const n = 120

	ids := make([]identity.Identity, n)
	for i := range ids {
		ids[i] = ident("t-c", "u-c", fmt.Sprintf("s-c-%d", i))
		if _, err := f.reg.Open(ctxFor(ids[i]), ids[i].SessionID, ids[i]); err != nil {
			t.Fatalf("Open %d: %v", i, err)
		}
	}

	var wg sync.WaitGroup
	for i := range ids {
		id := ids[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			// A "task" goroutine emits lifecycle events that race the erasure.
			var pub sync.WaitGroup
			pub.Add(1)
			go func() {
				defer pub.Done()
				for j := int64(1); j <= 6; j++ {
					// A fenced drop returns nil; a pre-fence persist is swept by
					// DeleteScope. Either way the post-erasure history must be empty.
					_ = f.bus.Publish(ctx, lateTaskEvent(id, j))
				}
			}()
			if _, err := f.eraser.Erase(ctx, id); err != nil {
				t.Errorf("Erase %s: %v", id.SessionID, err)
			}
			pub.Wait()
			// A few more strictly-after-erase emissions (definitely fenced).
			for j := int64(7); j <= 9; j++ {
				_ = f.bus.Publish(ctx, lateTaskEvent(id, j))
			}
		}()
	}
	wg.Wait()

	hr := f.bus.(events.HistoryReplayer)
	for i := range ids {
		id := ids[i]
		if _, _, _, err := hr.Bounds(ctx, historyFilter(id)); !errors.Is(err, events.ErrNoHistory) {
			t.Errorf("session %s: post-erasure Bounds = %v, want ErrNoHistory", id.SessionID, err)
		}
		if _, err := f.store.Load(ctx, identity.Quadruple{Identity: id}, "events.durable.head"); !errors.Is(err, state.ErrNotFound) {
			t.Errorf("session %s: durable head survived erasure under concurrency: %v", id.SessionID, err)
		}
	}
}
