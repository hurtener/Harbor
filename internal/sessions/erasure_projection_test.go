package sessions_test

// erasure_projection_test.go — the HA-64/65 erasure-cascade projection
// erasure seams: the turns projection (HA-64) is FENCED then its
// rows/checkpoint DELETED (FenceSession then DeleteScope — the HA-64
// fence persists without erasing rows, so a bare fence leaves the
// transcript readable through turns reads), the rollup store (HA-65) is
// fenced alone (its FenceSession erases rows AND fences in one
// transaction), a present-but-failing fence/erase capability fails the
// whole cascade loud right there (nothing deleted — retry-safe), and an
// unwired seam is a no-op (the cascade keeps its exact pre-baseline
// behavior).

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/sessions/turns/drivers/inmem"
	"github.com/hurtener/Harbor/internal/state"
)

// fenceRecorder is a ProjectionFencer (the HA-65 rollup seam) that
// records the fenced triples and can be switched to fail loud.
type fenceRecorder struct {
	mu   sync.Mutex
	ids  []identity.Identity
	fail error
}

var _ sessions.ProjectionFencer = (*fenceRecorder)(nil)

func (f *fenceRecorder) FenceSession(_ context.Context, id identity.Identity) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail != nil {
		return f.fail
	}
	f.ids = append(f.ids, id)
	return nil
}

func (f *fenceRecorder) fenced() []identity.Identity {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]identity.Identity(nil), f.ids...)
}

// turnsEraserRecorder is a TurnsProjectionEraser (the HA-64 turns seam)
// that records every FenceSession / DeleteScope invocation in call
// order and can be switched to fail either call loud.
type turnsEraserRecorder struct {
	mu         sync.Mutex
	calls      []string // ordered "fence:<SessionID>" / "delete:<SessionID>"
	fenceFail  error
	deleteFail error
}

var _ sessions.TurnsProjectionEraser = (*turnsEraserRecorder)(nil)

func (r *turnsEraserRecorder) FenceSession(_ context.Context, id identity.Identity) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "fence:"+id.SessionID)
	if r.fenceFail != nil {
		return r.fenceFail
	}
	return nil
}

func (r *turnsEraserRecorder) DeleteScope(_ context.Context, id identity.Identity) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, "delete:"+id.SessionID)
	if r.deleteFail != nil {
		return 0, r.deleteFail
	}
	return 0, nil
}

func (r *turnsEraserRecorder) log() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.calls...)
}

// TestCascadeEraser_ProjectionSeams_TurnsFencedThenDeletedRollupsFenced
// pins the ordering + erasure contract: when the HA-64 turns / HA-65
// rollups seams are wired, the cascade fences BOTH with the EXACT erased
// triple during the erase (after the bus fence, before any destructive
// step) — and additionally runs the turns row/checkpoint DeleteScope
// immediately AFTER the turns fence (fence-first, delete-second) — so a
// late event cannot resurrect the erased session's projection rows and
// persisted turn data is not merely fenced but gone.
func TestCascadeEraser_ProjectionSeams_TurnsFencedThenDeletedRollupsFenced(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t1", "u1", "s1")
	ictx := ctxFor(id)
	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}

	turnsSeam := &turnsEraserRecorder{}
	rollupsFence := &fenceRecorder{}
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry:          f.reg,
		State:             f.store,
		Memory:            f.mem,
		Artifacts:         f.arts,
		Skills:            f.skills,
		Bus:               busOf(t, f),
		TurnsProjection:   turnsSeam,
		RollupsProjection: rollupsFence,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	resp, derr := eraser.Erase(ctx, id)
	if derr != nil {
		t.Fatalf("Erase: %v", derr)
	}
	if !resp.Deleted {
		t.Fatalf("Erase response = %+v, want Deleted", resp)
	}

	// The turns seam saw the EXACT erased triple, fence FIRST, delete
	// SECOND — the HA-64 FenceSession persists the permanent fence but
	// does not erase rows, so the row/checkpoint DeleteScope must follow
	// it in the same cascade.
	want := []string{"fence:" + id.SessionID, "delete:" + id.SessionID}
	if got := turnsSeam.log(); !slices.Equal(got, want) {
		t.Fatalf("turns seam calls = %v, want exactly %v (fence before delete, both with the erased triple)", got, want)
	}
	rolls := rollupsFence.fenced()
	if len(rolls) != 1 || rolls[0] != id {
		t.Fatalf("rollups fencer fenced %v, want exactly [%v]", rolls, id)
	}

	// A converged erasure is terminal: re-erase of the erased session
	// refuses with the pre-existing not-found refusal (the projection
	// steps are never reached — an erased session is simply gone).
	if _, derr := eraser.Erase(ctx, id); !errors.Is(derr, sessions.ErrSessionNotFound) {
		t.Fatalf("re-erase after erasure: got %v, want ErrSessionNotFound", derr)
	}
}

// TestCascadeEraser_ProjectionFencerFailure_FailsLoudRetrySafe pins the
// fail-loud posture: a present-but-failing projection fence capability
// fails the WHOLE cascade right there — BEFORE any destructive step —
// so nothing has been deleted yet and the erase is safe to re-invoke
// once the fault clears. A projection fence failure must never degrade
// into a partial silent erasure. (The turns fence + delete pair runs
// BEFORE the failing rollups fence, so both fired on the aborted
// attempt too.)
func TestCascadeEraser_ProjectionFencerFailure_FailsLoudRetrySafe(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t1", "u1", "s1")
	ictx := ctxFor(id)
	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}

	turnsSeam := &turnsEraserRecorder{}
	rollupsFence := &fenceRecorder{fail: errors.New("rollups fence backend unavailable")}
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry:          f.reg,
		State:             f.store,
		Memory:            f.mem,
		Artifacts:         f.arts,
		Skills:            f.skills,
		Bus:               busOf(t, f),
		TurnsProjection:   turnsSeam,
		RollupsProjection: rollupsFence,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	if _, derr := eraser.Erase(ctx, id); derr == nil {
		t.Fatal("projection fence failure did not surface loudly")
	} else if !strings.Contains(derr.Error(), "rollups projection fence") {
		t.Fatalf("fence failure error %q does not name the rollups projection fence", derr)
	}

	// Nothing was deleted: the session-lifecycle record (the first
	// destructive step's target) survived the aborted cascade.
	if _, lerr := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); lerr != nil {
		t.Fatalf("aborted cascade deleted the session record: %v", lerr)
	}

	// The turns seam fired its fence+delete pair before the rollups
	// failure aborted the attempt — the pair ran with the erased triple.
	if got := turnsSeam.log(); !slices.Equal(got, []string{"fence:" + id.SessionID, "delete:" + id.SessionID}) {
		t.Fatalf("turns seam calls on the aborted attempt = %v, want [fence, delete] for %v", got, id)
	}

	// Clear the fault and re-invoke — the cascade converges (retry-safe).
	rollupsFence.fail = nil
	resp, derr := eraser.Erase(ctx, id)
	if derr != nil {
		t.Fatalf("retry after fence fault cleared failed: %v", derr)
	}
	if !resp.Deleted {
		t.Errorf("retry response = %+v, want Deleted", resp)
	}
	// The turns fence+delete pair ran again on the retry (fence and
	// delete both idempotent — the pair ran on BOTH attempts).
	if got := turnsSeam.log(); len(got) != 4 {
		t.Fatalf("turns seam calls after aborted+retry = %v, want the fence/delete pair on each attempt", got)
	}
}

// TestCascadeEraser_TurnsProjectionDeleteFailure_FailsLoud_FenceSurvivesRetry
// pins acceptance criterion 2 for the HA-64 leg: a turns DeleteScope
// failure fails the WHOLE cascade loud (never a partial silent success),
// does NOT undo the already-set permanent fence (the fence ran FIRST),
// and a re-invoke converges — the fence stays set and the delete lands.
func TestCascadeEraser_TurnsProjectionDeleteFailure_FailsLoud_FenceSurvivesRetry(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t1", "u1", "s1")
	ictx := ctxFor(id)
	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}

	turnsSeam := &turnsEraserRecorder{deleteFail: errors.New("turns delete backend unavailable")}
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: f.reg, State: f.store, Memory: f.mem, Artifacts: f.arts,
		Skills: f.skills, Bus: busOf(t, f),
		TurnsProjection: turnsSeam,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}

	// First attempt: the turns DeleteScope fails → loud error naming the
	// turns projection scope, and the fence was set BEFORE the delete
	// (fence-first ordering) — the fence survives the failed attempt.
	if _, derr := eraser.Erase(ctx, id); derr == nil {
		t.Fatal("turns DeleteScope failure did not surface loudly")
	} else if !strings.Contains(derr.Error(), "turns projection scope") {
		t.Fatalf("delete failure error %q does not name the turns projection scope", derr)
	}
	if got := turnsSeam.log(); !slices.Equal(got, []string{"fence:" + id.SessionID, "delete:" + id.SessionID}) {
		t.Fatalf("turns seam calls on the failed attempt = %v, want fence BEFORE delete for %v", got, id)
	}
	// Nothing else was deleted: the session-lifecycle record survived.
	if _, lerr := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); lerr != nil {
		t.Fatalf("aborted cascade deleted the session record: %v", lerr)
	}

	// Clear the fault and re-invoke — the cascade converges; the fence
	// fires again (idempotent) and the delete lands.
	turnsSeam.deleteFail = nil
	resp, derr := eraser.Erase(ctx, id)
	if derr != nil {
		t.Fatalf("retry after delete fault cleared failed: %v", derr)
	}
	if !resp.Deleted {
		t.Errorf("retry response = %+v, want Deleted", resp)
	}
	if _, lerr := f.store.Load(ctx, identity.Quadruple{Identity: id}, "session.lifecycle"); !errors.Is(lerr, state.ErrNotFound) {
		t.Errorf("session.lifecycle survived the retry: err=%v", lerr)
	}
	// The pair ran on BOTH attempts — fence, delete, fence, delete.
	if got := turnsSeam.log(); len(got) != 4 {
		t.Fatalf("turns seam calls after aborted+retry = %v, want the fence/delete pair on each attempt", got)
	}
}

// TestCascadeEraser_TurnsProjection_PersistedRowsGone_FenceSurvives is
// the HA-64 P1 regression at cascade level: with a REAL turns store
// wired as TurnsProjection, a session's persisted turn transcript is
// GONE (not merely fenced) after sessions.delete — GetTurn reports
// ErrTurnNotFound, ListTurns is an empty page on the advanced snapshot,
// and the checkpoint reads 0 — while the permanent store-local fence
// survives, so a late replay (AppendTurnIf / SaveCheckpoint) is refused
// with ErrErasureFenced and repeated erasure stays safe (terminal
// not-found path).
func TestCascadeEraser_TurnsProjection_PersistedRowsGone_FenceSurvives(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t1", "u1", "s1")
	ictx := ctxFor(id)
	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}

	turnsStore, err := inmem.New()
	if err != nil {
		t.Fatalf("inmem turns store: %v", err)
	}
	t.Cleanup(func() { _ = turnsStore.Close(ctx) })

	// Persist a durable turn row + a checkpoint under the exact session
	// triple — the transcript a session deletion must erase.
	appended, aerr := turnsStore.AppendTurnIf(ctx, id, turns.TurnRow{TurnID: turns.TurnID("run-1")})
	if aerr != nil {
		t.Fatalf("AppendTurnIf: %v", aerr)
	}
	if appended.Sequence != 1 {
		t.Fatalf("appended sequence = %d, want 1", appended.Sequence)
	}
	if err := turnsStore.SaveCheckpoint(ctx, id, 42); err != nil {
		t.Fatalf("SaveCheckpoint: %v", err)
	}
	// Sanity: the transcript is readable pre-erasure.
	if _, gerr := turnsStore.GetTurn(ctx, id, turns.TurnID("run-1")); gerr != nil {
		t.Fatalf("pre-erasure GetTurn: %v", gerr)
	}

	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: f.reg, State: f.store, Memory: f.mem, Artifacts: f.arts,
		Skills: f.skills, Bus: busOf(t, f),
		TurnsProjection: turnsStore,
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}
	if _, derr := eraser.Erase(ctx, id); derr != nil {
		t.Fatalf("Erase: %v", derr)
	}

	// The persisted turn transcript is GONE — not merely fenced (the
	// HA-64 defect: a bare FenceSession leaves rows readable through
	// turns reads; DeleteScope removes them while the fence survives).
	if _, gerr := turnsStore.GetTurn(ctx, id, turns.TurnID("run-1")); !errors.Is(gerr, turns.ErrTurnNotFound) {
		t.Fatalf("GetTurn after erasure = %v, want ErrTurnNotFound", gerr)
	}
	rows, next, info, lerr := turnsStore.ListTurns(ctx, id, nil, 10)
	if lerr != nil {
		t.Fatalf("ListTurns after erasure: %v", lerr)
	}
	if len(rows) != 0 || next != nil {
		t.Fatalf("ListTurns after erasure = %d rows, next %v; want an empty page", len(rows), next)
	}
	if info.Snapshot == 0 {
		t.Fatalf("ListTurns after erasure snapshot = 0 — the DeleteScope snapshot advance is missing")
	}
	if cp, cerr := turnsStore.LoadCheckpoint(ctx, id); cerr != nil || cp != 0 {
		t.Fatalf("checkpoint after erasure = %d, err %v; want 0", cp, cerr)
	}

	// Late replay stays FENCED: the permanent store-local fence survives
	// the DeleteScope, so no write can resurrect the erased transcript.
	if _, aerr := turnsStore.AppendTurnIf(ctx, id, turns.TurnRow{TurnID: turns.TurnID("run-2")}); !errors.Is(aerr, turns.ErrErasureFenced) {
		t.Fatalf("AppendTurnIf after erasure = %v, want ErrErasureFenced", aerr)
	}
	if serr := turnsStore.SaveCheckpoint(ctx, id, 99); !errors.Is(serr, turns.ErrErasureFenced) {
		t.Fatalf("SaveCheckpoint after erasure = %v, want ErrErasureFenced", serr)
	}

	// Idempotent repeated erasure stays safe: the converged session is
	// terminal — a re-invoke refuses with ErrSessionNotFound and does
	// not re-touch the (already empty) projection.
	if _, derr := eraser.Erase(ctx, id); !errors.Is(derr, sessions.ErrSessionNotFound) {
		t.Fatalf("re-erase after converged erasure = %v, want ErrSessionNotFound", derr)
	}
}

// TestCascadeEraser_UnwiredProjectionSeams_Noop pins the optional seam:
// with no seams wired the cascade keeps its exact pre-baseline behavior
// (a full erase still converges) and never calls a nil seam.
func TestCascadeEraser_UnwiredProjectionSeams_Noop(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t1", "u1", "s1")
	ictx := ctxFor(id)
	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry: f.reg, State: f.store, Memory: f.mem, Artifacts: f.arts,
		Skills: f.skills, Bus: busOf(t, f),
	})
	if err != nil {
		t.Fatalf("NewCascadeEraser: %v", err)
	}
	resp, derr := eraser.Erase(ctx, id)
	if derr != nil {
		t.Fatalf("Erase without seams: %v", derr)
	}
	if !resp.Deleted {
		t.Fatalf("Erase response = %+v, want Deleted", resp)
	}
}
