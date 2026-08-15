package materializer

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/tasks"
)

// sealBarrierStore deterministically places two projector instances after
// their optimistic reads but before either conditional seal reaches the real
// store. One wins; the other therefore observes ErrTurnSealed from the store,
// reproducing the macOS CI interleaving without scheduler luck.
type sealBarrierStore struct {
	turns.Store

	mu       sync.Mutex
	arrivals int
	release  chan struct{}
}

func newSealBarrierStore(store turns.Store) *sealBarrierStore {
	return &sealBarrierStore{Store: store, release: make(chan struct{})}
}

func (s *sealBarrierStore) SealTurnIf(ctx context.Context, id identity.Identity, turnID turns.TurnID, expectedVersion int, row turns.TurnRow) (turns.TurnRow, error) {
	s.mu.Lock()
	s.arrivals++
	if s.arrivals == 2 {
		close(s.release)
	}
	s.mu.Unlock()

	select {
	case <-s.release:
	case <-ctx.Done():
		return turns.TurnRow{}, ctx.Err()
	case <-time.After(5 * time.Second):
		return turns.TurnRow{}, errors.New("test seal barrier: second materializer did not reach the conditional seal")
	}
	return s.Store.SealTurnIf(ctx, id, turnID, expectedVersion, row)
}

// TestMaterialize_N100Identities_IsolatedUnderRace pins the mandatory
// cross-session isolation gate at N>=100 mixed identities under the
// race detector: one shared materializer + one shared projector/store,
// every identity's projection contains EXACTLY its own turns, a foreign
// identity's turn is a typed not-found (non-oracular denial), and
// concurrent readers running against the shared projection during
// concurrent (idempotent) materialize passes neither race nor leak
// goroutines.
func TestMaterialize_N100Identities_IsolatedUnderRace(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)

	const n = 120
	ids := make([]identity.Identity, n)
	quads := make([]identity.Quadruple, n)
	taskIDs := make([]string, n)
	for i := range n {
		id := identity.Identity{
			TenantID:  fmt.Sprintf("tenant-%d", i%3),
			UserID:    fmt.Sprintf("user-%d", i),
			SessionID: fmt.Sprintf("sess-%d", i),
		}
		ids[i] = id
		quads[i] = testQuad(id, fmt.Sprintf("run-%d", i))
		taskIDs[i] = fmt.Sprintf("task-%d", i)
	}

	// One full canonical lifecycle per identity — mixed tenants, users,
	// and sessions on ONE shared projection.
	for i := range n {
		h.lifecycle(t, quads[i], taskIDs[i])
	}
	if _, err := m.Materialize(context.Background()); err != nil {
		t.Fatalf("materialize: %v", err)
	}

	// Same-user/session reach: every identity pages exactly its own
	// turn and nothing else.
	for i := range n {
		page, err := h.proj.List(context.Background(), ids[i], turns.ListOptions{Limit: 50})
		if err != nil {
			t.Fatalf("list identity %d: %v", i, err)
		}
		if len(page.Rows) != 1 || string(page.Rows[0].TurnID) != taskIDs[i] {
			t.Fatalf("identity %d rows = %d (turn %q), want exactly its own turn %q",
				i, len(page.Rows), page.Rows[0].TurnID, taskIDs[i])
		}
		row, err := h.proj.Get(context.Background(), ids[i], turns.TurnID(taskIDs[i]))
		if err != nil || !row.Sealed {
			t.Fatalf("get identity %d: err=%v sealed=%v", i, err, row.Sealed)
		}
	}

	// Cross-identity denial is NON-ORACULAR: asking for another
	// identity's turn under this triple is a typed not-found, never a
	// cross-session row and never an invented one.
	for i := range n {
		other := (i + 1) % n
		if _, err := h.proj.Get(context.Background(), ids[i], turns.TurnID(taskIDs[other])); !errors.Is(err, turns.ErrTurnNotFound) {
			t.Fatalf("cross-identity get = %v, want ErrTurnNotFound (identity %d read identity %d)", err, i, other)
		}
	}

	// Concurrent readers against the shared projection while two
	// idempotent materialize passes run concurrently on the same
	// materializer (the internal state mutex serializes them).
	baseline := runtime.NumGoroutine()
	ctx := context.Background()
	var wg sync.WaitGroup
	const readers = 16
	const readsPerReader = 200
	for w := range readers {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for k := range readsPerReader {
				i := (w*31 + k*7) % n
				if _, err := h.proj.List(context.Background(), ids[i], turns.ListOptions{Limit: 50}); err != nil {
					t.Errorf("concurrent list identity %d: %v", i, err)
					return
				}
				if _, err := h.proj.Get(context.Background(), ids[i], turns.TurnID(taskIDs[i])); err != nil {
					t.Errorf("concurrent get identity %d: %v", i, err)
					return
				}
			}
		}(w)
	}
	matDone := make(chan struct{})
	go func() {
		defer close(matDone)
		for range 2 {
			if _, err := m.Materialize(ctx); err != nil {
				t.Errorf("concurrent materialize: %v", err)
				return
			}
		}
	}()
	<-matDone
	wg.Wait()

	// The concurrent idempotent passes did not mutate the rows.
	for i := range n {
		row, err := h.proj.Get(context.Background(), ids[i], turns.TurnID(taskIDs[i]))
		if err != nil || !row.Sealed {
			t.Fatalf("post-stress get identity %d: err=%v sealed=%v", i, err, row.Sealed)
		}
	}

	// Goroutine-leak gate: the shared instance spawns no goroutines per
	// invocation; the count returns to baseline.
	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Errorf("goroutine leak: baseline %d, after %d", baseline, got)
	}
}

// TestMaterialize_ConcurrentMaterializers_Converge pins the
// at-least-once idempotency contract under -race with TWO materializer
// instances replaying the same retained source concurrently against one
// shared projection (a response-loss replay racing a live pass): the
// row-level monotonic guards and version retries converge to identical
// rows with no double-application.
func TestMaterialize_ConcurrentMaterializers_Converge(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	barrier := newSealBarrierStore(h.store)
	proj, err := turns.New(barrier)
	if err != nil {
		t.Fatalf("new barrier projector: %v", err)
	}
	h.proj = proj
	m1 := h.newMaterializer(t)
	m2 := h.newMaterializer(t)

	evs := h.lifecycle(t, testQuad(h.id, "run-1"), "task-1")

	ctx := context.Background()
	start := make(chan struct{})
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, m := range []*Materializer{m1, m2} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for range 3 {
				if _, err := m.Materialize(ctx); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent materialize: %v", err)
	}

	row, err := h.proj.Get(ctx, h.id, "task-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !row.Sealed || row.Status != turns.StatusFailed || row.ErrorClass != turns.ErrorClassTimeout {
		t.Fatalf("converged row = %+v", row)
	}
	if len(row.Activity.Rows) != 1 || row.Activity.Rows[0].Status != turns.ActivitySucceeded {
		t.Fatalf("converged activity = %+v (no double application)", row.Activity.Rows)
	}
	if usageValue(t, row.Usage.PromptTokens) != 100 {
		t.Errorf("usage not converged: %+v", row.Usage.PromptTokens)
	}
	cp, err := h.proj.Checkpoint(ctx, h.id)
	if err != nil {
		t.Fatalf("checkpoint: %v", err)
	}
	if cp != evs[len(evs)-1].Sequence {
		t.Errorf("checkpoint = %d, want terminal sequence %d", cp, evs[len(evs)-1].Sequence)
	}

	// Same status is not enough for convergence: a different closed error
	// class is a genuinely conflicting immutable terminal row and stays loud.
	conflictingState := &turnState{taskID: "task-1"}
	if _, err := m1.sealTurn(ctx, &sessionState{id: h.id}, conflictingState, turns.Seal{
		Status:     turns.StatusFailed,
		ErrorClass: turns.ErrorClass5xx,
		EventSeq:   row.LastAppliedEventSeq,
	}); !errors.Is(err, turns.ErrTurnSealed) {
		t.Fatalf("conflicting same-status seal = %v, want ErrTurnSealed", err)
	}
	if conflictingState.sealed {
		t.Error("conflicting terminal observation marked local state sealed; retry would skip the loud conflict")
	}
}

// TestMaterialize_EraseDuringPass_NoResurrectionUnderRace pins the
// erasure-fence contract under -race: erasing a session while a pass is
// applying its events leaves that projection permanently empty (the
// store-local fence refuses every subsequent write) and never errors
// the pass; unrelated sessions' projections are untouched.
func TestMaterialize_EraseDuringPass_NoResurrectionUnderRace(t *testing.T) {
	h := newHarness(t, "")
	defer h.closeStore()
	m := h.newMaterializer(t)

	erased := identity.Identity{TenantID: "tenant-x", UserID: "user-x", SessionID: "sess-x"}

	// Publish a batch for OTHER identities, then the erased session's
	// events, so the erase lands while the pass is mid-application.
	for i := range 40 {
		oid := identity.Identity{TenantID: "tenant-y", UserID: fmt.Sprintf("user-%d", i), SessionID: fmt.Sprintf("sess-%d", i)}
		h.src.publish(t, spawnEv(oid, fmt.Sprintf("run-%d", i), fmt.Sprintf("task-%d", i), tasks.KindForeground, ""))
		h.src.publish(t, startedEv(oid, fmt.Sprintf("task-%d", i)))
	}
	quad := testQuad(erased, "run-e")
	h.src.publish(t, spawnEv(erased, quad.RunID, "task-e", tasks.KindForeground, ""))
	h.src.publish(t, startedEv(erased, "task-e"))
	h.src.publish(t, toolInvokedEv(erased, quad.RunID, "e.tool", time.Now()))

	ctx := context.Background()
	matDone := make(chan struct{})
	var matErr error
	go func() {
		defer close(matDone)
		_, matErr = m.Materialize(ctx)
	}()

	// Erase the erased session mid-pass (its events are in the source;
	// the store-local fence refuses the writes).
	if _, err := h.proj.Erase(ctx, erased); err != nil {
		t.Fatalf("erase: %v", err)
	}
	<-matDone
	if matErr != nil && !errors.Is(matErr, turns.ErrErasureFenced) {
		t.Fatalf("materialize after concurrent erase = %v (must be a fence skip, never a hard failure)", matErr)
	}
	if _, err := h.proj.Get(ctx, erased, "task-e"); !errors.Is(err, turns.ErrTurnNotFound) {
		t.Fatalf("erased turn = %v, want ErrTurnNotFound (no resurrection)", err)
	}
	// An unrelated session's projection is untouched by the erase.
	other := identity.Identity{TenantID: "tenant-y", UserID: "user-2", SessionID: "sess-2"}
	if _, err := h.proj.Get(ctx, other, "task-2"); err != nil {
		t.Fatalf("unrelated turn: %v", err)
	}
}
