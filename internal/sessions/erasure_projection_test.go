package sessions_test

// erasure_projection_test.go — the HA-64/65 erasure-cascade projection
// fences: the OPTIONAL ProjectionFencer seams (turns projection store,
// rollup store) are fenced with the exact erased triple BEFORE any
// destructive step, a present-but-failing fence capability fails the
// whole cascade loud right there (nothing deleted — retry-safe), and an
// unwired fencer is a no-op (the cascade keeps its exact pre-baseline
// behavior).

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions"
)

// fenceRecorder is a ProjectionFencer that records the fenced triples
// and can be switched to fail loud.
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

// TestCascadeEraser_ProjectionFencers_FencedWithExactTriple pins the
// fence ordering: when the HA-64 turns / HA-65 rollups fencers are
// wired, the cascade fences them with the EXACT erased triple during
// the erase (after the bus fence, before any destructive step) — so no
// late event can resurrect the erased session's projection rows.
func TestCascadeEraser_ProjectionFencers_FencedWithExactTriple(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t1", "u1", "s1")
	ictx := ctxFor(id)
	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}

	turnsFence := &fenceRecorder{}
	rollupsFence := &fenceRecorder{}
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry:          f.reg,
		State:             f.store,
		Memory:            f.mem,
		Artifacts:         f.arts,
		Skills:            f.skills,
		Bus:               busOf(t, f),
		TurnsProjection:   turnsFence,
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

	turns := turnsFence.fenced()
	if len(turns) != 1 || turns[0] != id {
		t.Fatalf("turns fencer fenced %v, want exactly [%v]", turns, id)
	}
	rolls := rollupsFence.fenced()
	if len(rolls) != 1 || rolls[0] != id {
		t.Fatalf("rollups fencer fenced %v, want exactly [%v]", rolls, id)
	}

	// A converged erasure is terminal: re-erase of the erased session
	// refuses with the pre-existing not-found refusal (the fence step is
	// never reached — an erased session is simply gone).
	if _, derr := eraser.Erase(ctx, id); !errors.Is(derr, sessions.ErrSessionNotFound) {
		t.Fatalf("re-erase after fencing: got %v, want ErrSessionNotFound", derr)
	}
}

// TestCascadeEraser_ProjectionFencerFailure_FailsLoudRetrySafe pins the
// fail-loud posture: a present-but-failing projection fence capability
// fails the WHOLE cascade right there — BEFORE any destructive step —
// so nothing has been deleted yet and the erase is safe to re-invoke
// once the fault clears. A projection fence failure must never degrade
// into a partial silent erasure.
func TestCascadeEraser_ProjectionFencerFailure_FailsLoudRetrySafe(t *testing.T) {
	f := newErasureFixture(t, nil)
	ctx := context.Background()
	id := ident("t1", "u1", "s1")
	ictx := ctxFor(id)
	if _, err := f.reg.Open(ictx, id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}

	turnsFence := &fenceRecorder{}
	rollupsFence := &fenceRecorder{fail: errors.New("rollups fence backend unavailable")}
	eraser, err := sessions.NewCascadeEraser(sessions.CascadeEraserDeps{
		Registry:          f.reg,
		State:             f.store,
		Memory:            f.mem,
		Artifacts:         f.arts,
		Skills:            f.skills,
		Bus:               busOf(t, f),
		TurnsProjection:   turnsFence,
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

	// Clear the fault and re-invoke — the cascade converges (retry-safe).
	rollupsFence.fail = nil
	resp, derr := eraser.Erase(ctx, id)
	if derr != nil {
		t.Fatalf("retry after fence fault cleared failed: %v", derr)
	}
	if !resp.Deleted {
		t.Errorf("retry response = %+v, want Deleted", resp)
	}
	// The turns fence fired once per attempt (the first fence runs before
	// the failing rollups fence aborts the attempt) — the last fenced
	// triple is the erased session either way.
	if got := turnsFence.fenced(); len(got) < 1 || got[len(got)-1] != id {
		t.Fatalf("turns fence after aborted+retry = %v, want last fenced triple %v", got, id)
	}
}

// TestCascadeEraser_UnwiredProjectionFencers_Noop pins the optional
// seam: with no fencers wired the cascade keeps its exact pre-baseline
// behavior (a full erase still converges) and never calls a nil fence.
func TestCascadeEraser_UnwiredProjectionFencers_Noop(t *testing.T) {
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
		t.Fatalf("Erase without fencers: %v", derr)
	}
	if !resp.Deleted {
		t.Fatalf("Erase response = %+v, want Deleted", resp)
	}
}
