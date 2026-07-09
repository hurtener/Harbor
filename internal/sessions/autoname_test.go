package sessions_test

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions"
)

// autoname_test.go covers the auto-naming registry surface (D-289):
// RecordCompletedTurn, SetTitleAuto (manual-wins refusal + atomic
// counters+title in one save + defensive clamp), and AutoNamingState.

func openForNaming(t *testing.T, reg *sessions.Registry, id identity.Identity) {
	t.Helper()
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
}

func TestRegistry_RecordCompletedTurn_Increments(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	openForNaming(t, reg, id)

	for want := 1; want <= 3; want++ {
		got, err := reg.RecordCompletedTurn(ctxFor(id), id.SessionID, id)
		if err != nil {
			t.Fatalf("RecordCompletedTurn: %v", err)
		}
		if got != want {
			t.Errorf("RecordCompletedTurn = %d, want %d", got, want)
		}
	}
	st, err := reg.AutoNamingState(ctxFor(id), id.SessionID, id)
	if err != nil {
		t.Fatalf("AutoNamingState: %v", err)
	}
	if st.TurnCount != 3 {
		t.Errorf("TurnCount = %d, want 3", st.TurnCount)
	}
}

func TestRegistry_RecordCompletedTurn_UnknownID_NotFound(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "ghost")
	if _, err := reg.RecordCompletedTurn(ctxFor(id), id.SessionID, id); !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Errorf("RecordCompletedTurn(unknown) = %v, want ErrSessionNotFound", err)
	}
}

func TestRegistry_SetTitleAuto_SetsAutoAndBumpsCountersInOneSave(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	openForNaming(t, reg, id)

	// Two completed turns, then an auto-name at turn 2.
	if _, err := reg.RecordCompletedTurn(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if _, err := reg.RecordCompletedTurn(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if err := reg.SetTitleAuto(ctxFor(id), id.SessionID, id, "Weather chat"); err != nil {
		t.Fatalf("SetTitleAuto: %v", err)
	}
	st, err := reg.AutoNamingState(ctxFor(id), id.SessionID, id)
	if err != nil {
		t.Fatalf("AutoNamingState: %v", err)
	}
	if st.TitleSource != sessions.TitleSourceAuto {
		t.Errorf("TitleSource = %q, want auto", st.TitleSource)
	}
	if st.CurrentTitle != "Weather chat" {
		t.Errorf("CurrentTitle = %q, want %q", st.CurrentTitle, "Weather chat")
	}
	if st.AutoNameCount != 1 {
		t.Errorf("AutoNameCount = %d, want 1", st.AutoNameCount)
	}
	if st.LastAutoNamedTurn != 2 {
		t.Errorf("LastAutoNamedTurn = %d, want 2 (the turn at naming time)", st.LastAutoNamedTurn)
	}
	// The title is visible through the normal read projection.
	snap, err := reg.Inspect(ctxFor(id), id.SessionID)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if snap.Title != "Weather chat" || snap.TitleSource != sessions.TitleSourceAuto {
		t.Errorf("Inspect title = %q/%q, want auto", snap.Title, snap.TitleSource)
	}
}

// TestRegistry_SetTitle_ClearResetsAutoNamingCounters_ReArms pins FIX-3:
// a manual clear (SetTitle with an empty value) zeroes AutoNameCount +
// LastAutoNamedTurn in the same record save so auto-naming RE-ARMS. Before
// the fix, clear reset only Title/TitleSource; the counters survived, so
// namingDue's AutoNameCount==0 first branch never re-triggered under a
// name-once policy and clear→re-arm was a silent dead-end.
func TestRegistry_SetTitle_ClearResetsAutoNamingCounters_ReArms(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	openForNaming(t, reg, id)

	// One completed turn, an auto-name lands (count=1).
	if _, err := reg.RecordCompletedTurn(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if err := reg.SetTitleAuto(ctxFor(id), id.SessionID, id, "First auto title"); err != nil {
		t.Fatalf("SetTitleAuto: %v", err)
	}
	st, err := reg.AutoNamingState(ctxFor(id), id.SessionID, id)
	if err != nil {
		t.Fatalf("AutoNamingState (post-auto): %v", err)
	}
	if st.AutoNameCount != 1 || st.LastAutoNamedTurn != 1 {
		t.Fatalf("post-auto counters = auto %d last %d, want 1/1", st.AutoNameCount, st.LastAutoNamedTurn)
	}

	// A manual clear must reset the naming counters — starting a new cycle.
	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, ""); err != nil {
		t.Fatalf("SetTitle (clear): %v", err)
	}
	st, err = reg.AutoNamingState(ctxFor(id), id.SessionID, id)
	if err != nil {
		t.Fatalf("AutoNamingState (post-clear): %v", err)
	}
	if st.TitleSource != sessions.TitleSourceUnset {
		t.Errorf("post-clear TitleSource = %q, want unset", st.TitleSource)
	}
	if st.AutoNameCount != 0 || st.LastAutoNamedTurn != 0 {
		t.Fatalf("post-clear counters = auto %d last %d, want 0/0 (re-arm)", st.AutoNameCount, st.LastAutoNamedTurn)
	}
	// TurnCount is untouched (it counts completed runs, not naming events).
	if st.TurnCount != 1 {
		t.Errorf("post-clear TurnCount = %d, want 1 (unchanged by a clear)", st.TurnCount)
	}

	// Re-arm proof: a new auto-name lands again on the next completed turn.
	if _, err := reg.RecordCompletedTurn(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if err := reg.SetTitleAuto(ctxFor(id), id.SessionID, id, "Second auto title"); err != nil {
		t.Fatalf("SetTitleAuto (re-arm) = %v, want nil (a clear re-armed naming)", err)
	}
	st, err = reg.AutoNamingState(ctxFor(id), id.SessionID, id)
	if err != nil {
		t.Fatalf("AutoNamingState (post-rearm): %v", err)
	}
	if st.CurrentTitle != "Second auto title" || st.AutoNameCount != 1 {
		t.Errorf("post-rearm = %q/auto %d, want \"Second auto title\"/1", st.CurrentTitle, st.AutoNameCount)
	}
}

func TestRegistry_SetTitleAuto_RefusesManualTitle(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	openForNaming(t, reg, id)

	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, "Human Named This"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	if err := reg.SetTitleAuto(ctxFor(id), id.SessionID, id, "auto override"); !errors.Is(err, sessions.ErrManualTitle) {
		t.Fatalf("SetTitleAuto over manual = %v, want ErrManualTitle", err)
	}
	// The manual title survives, untouched, and no auto counter moved.
	st, err := reg.AutoNamingState(ctxFor(id), id.SessionID, id)
	if err != nil {
		t.Fatalf("AutoNamingState: %v", err)
	}
	if st.TitleSource != sessions.TitleSourceManual || st.CurrentTitle != "Human Named This" {
		t.Errorf("title = %q/%q, want manual/Human Named This", st.CurrentTitle, st.TitleSource)
	}
	if st.AutoNameCount != 0 {
		t.Errorf("AutoNameCount = %d, want 0 (refused write must not bump)", st.AutoNameCount)
	}
}

func TestRegistry_SetTitleAuto_ClampsOversizeAndControlChars(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	openForNaming(t, reg, id)

	long := strings.Repeat("a", sessions.MaxSessionTitleLen+50)
	if err := reg.SetTitleAuto(ctxFor(id), id.SessionID, id, "First line\nsecond\n"+long); err != nil {
		t.Fatalf("SetTitleAuto: %v", err)
	}
	st, err := reg.AutoNamingState(ctxFor(id), id.SessionID, id)
	if err != nil {
		t.Fatalf("AutoNamingState: %v", err)
	}
	if st.CurrentTitle != "First line" {
		t.Errorf("clamped title = %q, want first line only", st.CurrentTitle)
	}
	if len([]rune(st.CurrentTitle)) > sessions.MaxSessionTitleLen {
		t.Errorf("clamped title exceeds MaxSessionTitleLen")
	}
}

func TestRegistry_SetTitleAuto_EmptyRejected(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	openForNaming(t, reg, id)
	if err := reg.SetTitleAuto(ctxFor(id), id.SessionID, id, "   \n  "); !errors.Is(err, sessions.ErrInvalidTitle) {
		t.Errorf("SetTitleAuto(empty) = %v, want ErrInvalidTitle", err)
	}
}

func TestRegistry_SetTitleAuto_ClearManualReArmsAuto(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	openForNaming(t, reg, id)

	// Manual title blocks auto.
	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, "Manual"); err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	if err := reg.SetTitleAuto(ctxFor(id), id.SessionID, id, "auto"); !errors.Is(err, sessions.ErrManualTitle) {
		t.Fatalf("expected ErrManualTitle, got %v", err)
	}
	// Clear the manual title (empty clears + resets to unset), re-arming auto.
	if err := reg.SetTitle(ctxFor(id), id.SessionID, id, ""); err != nil {
		t.Fatalf("SetTitle clear: %v", err)
	}
	if err := reg.SetTitleAuto(ctxFor(id), id.SessionID, id, "auto now allowed"); err != nil {
		t.Fatalf("SetTitleAuto after clear = %v, want nil (auto re-armed)", err)
	}
	st, _ := reg.AutoNamingState(ctxFor(id), id.SessionID, id)
	if st.TitleSource != sessions.TitleSourceAuto {
		t.Errorf("TitleSource = %q, want auto after re-arm", st.TitleSource)
	}
}

func TestRegistry_AutoNaming_ErrorBranches(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)
	id := ident("t1", "u1", "s1")
	openForNaming(t, reg, id)

	// Invalid (incomplete) identity fails closed on every entry point.
	bad := identity.Identity{TenantID: "t1"} // missing user + session
	if _, err := reg.RecordCompletedTurn(ctxFor(id), id.SessionID, bad); err == nil {
		t.Error("RecordCompletedTurn(incomplete ident) = nil, want error")
	}
	if err := reg.SetTitleAuto(ctxFor(id), id.SessionID, bad, "x"); err == nil {
		t.Error("SetTitleAuto(incomplete ident) = nil, want error")
	}
	if _, err := reg.AutoNamingState(ctxFor(id), id.SessionID, bad); err == nil {
		t.Error("AutoNamingState(incomplete ident) = nil, want error")
	}

	// A cross-(tenant,user) caller is simply not found at that StateStore key
	// (existence is never revealed across identities).
	foreign := ident("t2", "u2", "s1")
	if _, err := reg.AutoNamingState(ctxFor(foreign), id.SessionID, foreign); !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Errorf("AutoNamingState(foreign) = %v, want ErrSessionNotFound", err)
	}
	if _, err := reg.RecordCompletedTurn(ctxFor(foreign), id.SessionID, foreign); !errors.Is(err, sessions.ErrSessionNotFound) {
		t.Errorf("RecordCompletedTurn(foreign) = %v, want ErrSessionNotFound", err)
	}

	// After CloseRegistry, every entry point returns ErrRegistryClosed.
	if err := reg.CloseRegistry(ctxFor(id)); err != nil {
		t.Fatalf("CloseRegistry: %v", err)
	}
	if _, err := reg.RecordCompletedTurn(ctxFor(id), id.SessionID, id); !errors.Is(err, sessions.ErrRegistryClosed) {
		t.Errorf("RecordCompletedTurn(closed) = %v, want ErrRegistryClosed", err)
	}
	if err := reg.SetTitleAuto(ctxFor(id), id.SessionID, id, "x"); !errors.Is(err, sessions.ErrRegistryClosed) {
		t.Errorf("SetTitleAuto(closed) = %v, want ErrRegistryClosed", err)
	}
	if _, err := reg.AutoNamingState(ctxFor(id), id.SessionID, id); !errors.Is(err, sessions.ErrRegistryClosed) {
		t.Errorf("AutoNamingState(closed) = %v, want ErrRegistryClosed", err)
	}
}

// TestRegistry_ConcurrentReuse_AutoNaming_NoBleed extends the D-025 stress
// with a mix of RecordCompletedTurn + SetTitleAuto + SetTitle across N≥100
// distinct sessions against ONE shared Registry — asserting no cross-session
// title/counter bleed and -race clean.
func TestRegistry_ConcurrentReuse_AutoNaming_NoBleed(t *testing.T) {
	t.Parallel()
	reg, _, _ := titleTestWiring(t)

	const n = 120
	var wg sync.WaitGroup
	errCount := atomic.Int64{}
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := ident(fmt.Sprintf("t-%d", i%8), fmt.Sprintf("u-%d", i%8), fmt.Sprintf("s-%d", i))
			if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
				errCount.Add(1)
				t.Errorf("Open: %v", err)
				return
			}
			if _, err := reg.RecordCompletedTurn(ctxFor(id), id.SessionID, id); err != nil {
				errCount.Add(1)
				t.Errorf("RecordCompletedTurn: %v", err)
				return
			}
			autoTitle := fmt.Sprintf("auto-%d", i)
			if err := reg.SetTitleAuto(ctxFor(id), id.SessionID, id, autoTitle); err != nil {
				errCount.Add(1)
				t.Errorf("SetTitleAuto: %v", err)
				return
			}
			st, err := reg.AutoNamingState(ctxFor(id), id.SessionID, id)
			if err != nil {
				errCount.Add(1)
				t.Errorf("AutoNamingState: %v", err)
				return
			}
			if st.CurrentTitle != autoTitle {
				errCount.Add(1)
				t.Errorf("title bleed: session %q has %q, want %q", id.SessionID, st.CurrentTitle, autoTitle)
			}
			if st.TurnCount != 1 || st.AutoNameCount != 1 {
				errCount.Add(1)
				t.Errorf("counter bleed: session %q turn=%d auto=%d, want 1/1", id.SessionID, st.TurnCount, st.AutoNameCount)
			}
		}(i)
	}
	wg.Wait()
	if errCount.Load() != 0 {
		t.Fatalf("concurrent auto-naming stress observed %d errors", errCount.Load())
	}
}
