package sessions_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
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

// ---------------------------------------------------------------------------
// D-274 — fenceSession fail-loud posture.
// ---------------------------------------------------------------------------

// flakyFencerBus wraps a real Fencer-capable bus and forces Fence to fail
// while its toggle is set — the seam for forcing the fenceSession
// fail-loud path (D-274). Delegation only, mirroring erasure_test.go's
// flakyArts: the embedded events.EventBus promotes Publish / Subscribe /
// Close to the real bus, and Fence / Unfence explicitly delegate to the
// real bus's Fencer (captured separately) once the toggle clears — so a
// retry after the transient fault exercises the REAL fence semantics.
type flakyFencerBus struct {
	events.EventBus
	fencer events.Fencer
	fail   *atomic.Bool
}

func (b *flakyFencerBus) Fence(ctx context.Context, id identity.Identity) error {
	if b.fail.Load() {
		return errors.New("flaky bus: forced fence failure")
	}
	return b.fencer.Fence(ctx, id)
}

func (b *flakyFencerBus) Unfence(ctx context.Context, id identity.Identity) error {
	return b.fencer.Unfence(ctx, id)
}

// TestCascadeEraser_FenceError_FailsLoud_NothingDeleted_RetrySafe pins the
// D-274 fail-loud posture: when the configured bus DOES implement
// events.Fencer but Fence(ctx, id) errors, Erase fails loud, no store is
// touched (fenceSession runs BEFORE any destructive step), and a retry
// after the transient fault clears converges to a full, successful
// erasure — the same idempotent-retry contract a mid-cascade store error
// already gets.
func TestCascadeEraser_FenceError_FailsLoud_NothingDeleted_RetrySafe(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t-fenceerr", "u-fenceerr", "s-fenceerr")
	ictx := ctxFor(id)

	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := f.mem.AddTurn(ictx, identity.Quadruple{Identity: id}, memory.ConversationTurn{
		UserMessage: "hello", AssistantResponse: "world",
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	if _, err := f.arts.PutBytes(ctx, scope, []byte("blob"), artifacts.PutOpts{Namespace: "test"}); err != nil {
		t.Fatalf("PutBytes: %v", err)
	}

	realFencer, ok := f.bus.(events.Fencer)
	if !ok {
		t.Fatalf("fixture bus must implement events.Fencer")
	}
	var fail atomic.Bool
	fail.Store(true)
	flaky := &flakyFencerBus{EventBus: f.bus, fencer: realFencer, fail: &fail}

	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: f.reg, State: f.store, Memory: f.mem, Artifacts: f.arts, Bus: flaky,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	// First attempt: the fence fails → the WHOLE erasure fails loud.
	if _, ferr := eraser.Erase(ctx, id); ferr == nil {
		t.Fatal("fence failure did not surface loudly")
	}

	// Nothing was deleted — the fence runs before any destructive step.
	if _, lerr := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); lerr != nil {
		t.Errorf("fence failure deleted the session record: %v", lerr)
	}
	patch, gerr := f.mem.GetLLMContext(ctx, identity.Quadruple{Identity: id})
	if gerr != nil {
		t.Fatalf("GetLLMContext: %v", gerr)
	}
	if len(patch.RecentTurns) == 0 {
		t.Error("fence failure purged memory before any destructive step ran")
	}
	refs, lerr := f.arts.List(ctx, scope)
	if lerr != nil {
		t.Fatalf("List: %v", lerr)
	}
	if len(refs) != 1 {
		t.Errorf("fence failure touched artifacts: %d, want 1", len(refs))
	}
	// The registry's catalog is untouched too (clearErased never ran):
	// the session still resolves through the registry's read surface.
	if _, gerr := f.reg.Get(ictx, id.SessionID); gerr != nil {
		t.Errorf("fence failure cleared the registry catalog: Get = %v, want the session still resolvable", gerr)
	}

	// Clear the transient fault and re-invoke — the cascade converges to a
	// full, successful erasure.
	fail.Store(false)
	resp, rerr := eraser.Erase(ctx, id)
	if rerr != nil {
		t.Fatalf("retry after fence recovers failed: %v", rerr)
	}
	if !resp.Deleted {
		t.Errorf("retry response = %+v, want Deleted", resp)
	}
	if _, lerr := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); !errors.Is(lerr, state.ErrNotFound) {
		t.Errorf("session.lifecycle survived the retry: err=%v", lerr)
	}
	refs, lerr = f.arts.List(ctx, scope)
	if lerr != nil {
		t.Fatalf("List after retry: %v", lerr)
	}
	if len(refs) != 0 {
		t.Errorf("artifacts survived the retry: %d", len(refs))
	}
	// And the registry catalog is cleared by the successful retry.
	if _, gerr := f.reg.Get(ictx, id.SessionID); gerr == nil {
		t.Error("registry catalog still resolves the session after a successful erasure")
	}
}

// nonFencerBus wraps a real EventBus but deliberately does NOT implement
// events.Fencer — delegation only, never embedding the underlying bus
// (embedding would promote its Fence/Unfence methods and defeat the
// point of the test). Exercises the CascadeEraser's not-a-Fencer
// capability-downgrade branch (warn + proceed, D-274).
type nonFencerBus struct {
	inner events.EventBus
}

func (b *nonFencerBus) Publish(ctx context.Context, ev events.Event) error {
	return b.inner.Publish(ctx, ev)
}

func (b *nonFencerBus) Subscribe(ctx context.Context, f events.Filter) (events.Subscription, error) {
	return b.inner.Subscribe(ctx, f)
}

func (b *nonFencerBus) Close(ctx context.Context) error {
	return b.inner.Close(ctx)
}

// TestCascadeEraser_NonFencerBus_Erase_Succeeds_WarnOnly pins the OTHER
// half of the D-274 posture: a bus that does not implement events.Fencer
// at all is a documented capability downgrade (warn + proceed on the
// primary State sweep alone), never an error — Erase still completes
// with Deleted: true.
func TestCascadeEraser_NonFencerBus_Erase_Succeeds_WarnOnly(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t-nofence", "u-nofence", "s-nofence")
	ictx := ctxFor(id)

	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}

	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: f.reg, State: f.store, Memory: f.mem, Artifacts: f.arts,
		Bus: &nonFencerBus{inner: f.bus},
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	resp, err := eraser.Erase(ctx, id)
	if err != nil {
		t.Fatalf("Erase over a non-Fencer bus should succeed (warn + proceed), got: %v", err)
	}
	if !resp.Deleted {
		t.Errorf("response = %+v, want Deleted", resp)
	}
	if _, lerr := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); !errors.Is(lerr, state.ErrNotFound) {
		t.Errorf("session.lifecycle survived erasure: err=%v", lerr)
	}
}
