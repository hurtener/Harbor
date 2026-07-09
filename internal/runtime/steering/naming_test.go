package steering

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/sessions"
)

// ---------------------------------------------------------------------------
// Fakes.
// ---------------------------------------------------------------------------

// fakeTitler is an in-test SessionTitler that behaves like the real registry
// for the counters + manual-wins semantics, so an eligibility SEQUENCE across
// multiple completions is exercised faithfully. Thread-safe: the no-bleed
// concurrency test shares one instance across N sessions.
type fakeTitler struct {
	mu      sync.Mutex
	state   map[string]*sessions.AutoNamingState
	seedTS  sessions.TitleSource // initial TitleSource for a fresh session
	recErr  error                // forced RecordCompletedTurn error
	stErr   error                // forced AutoNamingState error
	setErr  error                // forced SetTitleAuto error (besides manual)
	applied map[string]string    // session -> last auto title applied
	panicOn string               // method name to panic on ("record"/"state"/"set")
}

func newFakeTitler() *fakeTitler {
	return &fakeTitler{state: map[string]*sessions.AutoNamingState{}, applied: map[string]string{}}
}

func (f *fakeTitler) get(id string) *sessions.AutoNamingState {
	st, ok := f.state[id]
	if !ok {
		st = &sessions.AutoNamingState{TitleSource: f.seedTS}
		f.state[id] = st
	}
	return st
}

func (f *fakeTitler) RecordCompletedTurn(_ context.Context, id string, _ identity.Identity) (int, error) {
	if f.panicOn == "record" {
		panic("boom-record")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.recErr != nil {
		return 0, f.recErr
	}
	st := f.get(id)
	st.TurnCount++
	return st.TurnCount, nil
}

func (f *fakeTitler) AutoNamingState(_ context.Context, id string, _ identity.Identity) (sessions.AutoNamingState, error) {
	if f.panicOn == "state" {
		panic("boom-state")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.stErr != nil {
		return sessions.AutoNamingState{}, f.stErr
	}
	return *f.get(id), nil
}

func (f *fakeTitler) SetTitleAuto(_ context.Context, id string, _ identity.Identity, title string) error {
	if f.panicOn == "set" {
		panic("boom-set")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	st := f.get(id)
	if st.TitleSource == sessions.TitleSourceManual {
		return sessions.ErrManualTitle
	}
	if f.setErr != nil {
		return f.setErr
	}
	st.CurrentTitle = title
	st.TitleSource = sessions.TitleSourceAuto
	st.AutoNameCount++
	st.LastAutoNamedTurn = st.TurnCount
	f.applied[id] = title
	return nil
}

func (f *fakeTitler) appliedTitle(id string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.applied[id]
}

// fakeCompleter is an in-test NamingCompleter: returns a configured content /
// error and records call count + the last requested model.
type fakeCompleter struct {
	mu      sync.Mutex
	content string
	err     error
	calls   int
	lastReq llm.CompleteRequest
	perCall func(req llm.CompleteRequest) (string, error)
}

func (c *fakeCompleter) Complete(_ context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	c.lastReq = req
	if c.perCall != nil {
		content, err := c.perCall(req)
		if err != nil {
			return llm.CompleteResponse{}, err
		}
		return llm.CompleteResponse{Content: content}, nil
	}
	if c.err != nil {
		return llm.CompleteResponse{}, c.err
	}
	return llm.CompleteResponse{Content: c.content}, nil
}

func (c *fakeCompleter) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

func namingRunSpec(q identity.Quadruple, p planner.Planner, ns *NamingSpec) RunSpec {
	return RunSpec{
		Planner:  p,
		Base:     planner.RunContext{Quadruple: q, Goal: "reach the goal", Trajectory: &planner.Trajectory{Query: "reach the goal"}},
		MaxSteps: 16,
		Naming:   ns,
	}
}

func activePolicy(after, repeat, maxReps int) NamingPolicy {
	return NamingPolicy{AfterTurns: after, RepeatEvery: repeat, MaxRepetitions: maxReps, MaxTitleLen: 80}.WithDefaults()
}

func finishPlanner() planner.Planner {
	return &scriptedPlanner{defaultDec: planner.Finish{Reason: planner.FinishGoal, Payload: "the answer"}}
}

// ---------------------------------------------------------------------------
// Pure helpers: defaults, eligibility, clamp, classify, digest bound.
// ---------------------------------------------------------------------------

func TestNamingPolicy_WithDefaults(t *testing.T) {
	got := NamingPolicy{}.WithDefaults()
	if got.AfterTurns != 1 {
		t.Errorf("AfterTurns default = %d, want 1", got.AfterTurns)
	}
	if got.MaxTitleLen != 80 {
		t.Errorf("MaxTitleLen default = %d, want 80", got.MaxTitleLen)
	}
	if clamped := (NamingPolicy{MaxTitleLen: 5}).WithDefaults(); clamped.MaxTitleLen != 8 {
		t.Errorf("MaxTitleLen 5 clamps to %d, want 8", clamped.MaxTitleLen)
	}
	if clamped := (NamingPolicy{MaxTitleLen: 999}).WithDefaults(); clamped.MaxTitleLen != 200 {
		t.Errorf("MaxTitleLen 999 clamps to %d, want 200", clamped.MaxTitleLen)
	}
	// A repeating policy with an unset cap gets the documented default (5) —
	// the no-unlimited invariant holds for programmatically-built policies too.
	if def := (NamingPolicy{RepeatEvery: 2}).WithDefaults(); def.MaxRepetitions != 5 {
		t.Errorf("repeating policy with unset cap: MaxRepetitions = %d, want defaulted 5", def.MaxRepetitions)
	}
	// A non-repeating policy leaves the cap alone (it is never consulted).
	if def := (NamingPolicy{}).WithDefaults(); def.MaxRepetitions != 0 {
		t.Errorf("once-only policy: MaxRepetitions = %d, want 0 (untouched)", def.MaxRepetitions)
	}
	// An explicit cap is respected.
	if def := (NamingPolicy{RepeatEvery: 2, MaxRepetitions: 3}).WithDefaults(); def.MaxRepetitions != 3 {
		t.Errorf("explicit cap: MaxRepetitions = %d, want 3", def.MaxRepetitions)
	}
}

func TestNamingDue_TruthTable(t *testing.T) {
	cases := []struct {
		name string
		pol  NamingPolicy
		st   sessions.AutoNamingState
		want bool
	}{
		{"first_not_reached", activePolicy(3, 0, 0), sessions.AutoNamingState{TurnCount: 2}, false},
		{"first_reached", activePolicy(3, 0, 0), sessions.AutoNamingState{TurnCount: 3}, true},
		{"first_default_after1", activePolicy(0, 0, 0), sessions.AutoNamingState{TurnCount: 1}, true},
		{"once_only_after_first", activePolicy(1, 0, 0), sessions.AutoNamingState{TurnCount: 5, AutoNameCount: 1, LastAutoNamedTurn: 1}, false},
		{"repeat_not_due", activePolicy(1, 2, 3), sessions.AutoNamingState{TurnCount: 2, AutoNameCount: 1, LastAutoNamedTurn: 1}, false},
		{"repeat_due", activePolicy(1, 2, 3), sessions.AutoNamingState{TurnCount: 3, AutoNameCount: 1, LastAutoNamedTurn: 1}, true},
		{"repeat_cap_reached", activePolicy(1, 2, 3), sessions.AutoNamingState{TurnCount: 99, AutoNameCount: 3, LastAutoNamedTurn: 5}, false},
		// Re-arm after a manual clear (FIX-3): SetTitle's clear zeroes
		// AutoNameCount + LastAutoNamedTurn, so the AutoNameCount==0 first
		// branch fires again even for a name-once (repeat_every==0) policy that
		// had already named — the cap is per-cycle.
		{"rearm_after_clear_name_once", activePolicy(1, 0, 0), sessions.AutoNamingState{TurnCount: 5, AutoNameCount: 0, LastAutoNamedTurn: 0}, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := namingDue(c.pol, c.st); got != c.want {
				t.Errorf("namingDue = %v, want %v", got, c.want)
			}
		})
	}
}

func TestClampNamingTitle_Determinism(t *testing.T) {
	cases := []struct {
		name, in, want string
		maxLen         int
	}{
		{"trim", "  Hello World  ", "Hello World", 80},
		{"first_line", "\n\nActual Title\nsecond line", "Actual Title", 80},
		{"strip_quotes", "\"Quoted Title\"", "Quoted Title", 80},
		{"oversize_cut", "abcdefghij", "abcde", 5},
		{"empty", "   \n  ", "", 80},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := clampNamingTitle(c.in, c.maxLen); got != c.want {
				t.Errorf("clampNamingTitle(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

func TestClassifyNamingErr(t *testing.T) {
	cases := []struct {
		err  error
		want string
	}{
		{governance.ErrBudgetExceeded, namingClassGovernanceBlocked},
		{fmt.Errorf("wrap: %w", governance.ErrRateLimited), namingClassGovernanceBlocked},
		{governance.ErrMaxTokensExceeded, namingClassGovernanceBlocked},
		{context.DeadlineExceeded, namingClassTimeout},
		{errors.New("provider 500"), namingClassLLMError},
	}
	for _, c := range cases {
		if got := classifyNamingErr(c.err); got != c.want {
			t.Errorf("classifyNamingErr(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestBuildNamingDigest_ByteBound(t *testing.T) {
	// A huge goal + many huge steps must still produce a digest <= the cap.
	big := strings.Repeat("x", 100_000)
	traj := &planner.Trajectory{}
	for range 50 {
		traj.Steps = append(traj.Steps, planner.Step{
			Action:            planner.CallTool{Tool: "search"},
			AssistantPreamble: big,
		})
	}
	steer := []steeringEntry{{kind: ControlUserMessage, content: big, step: 0}}
	fin := planner.Finish{Reason: planner.FinishGoal, Payload: big}
	digest := buildNamingDigest(big, traj, steer, fin, big)
	if len(digest) > namingDigestMaxBytes {
		t.Fatalf("digest = %d bytes, want <= %d", len(digest), namingDigestMaxBytes)
	}
	// Deterministic: identical inputs → identical digest.
	if again := buildNamingDigest(big, traj, steer, fin, big); again != digest {
		t.Fatal("buildNamingDigest is not deterministic")
	}
}

// ---------------------------------------------------------------------------
// Opt-in proof: nil Naming is byte-identical (no titler calls, no LLM, no events).
// ---------------------------------------------------------------------------

func TestFireNaming_NilNaming_ByteIdentical(t *testing.T) {
	bus := &fakeBus{}
	rl, _, _ := newTestRunLoop(t, WithRunLoopBus(bus))
	titler := newFakeTitler()
	comp := &fakeCompleter{content: "Title"}
	// Naming nil — even though the deps exist, no policy is wired on the spec.
	if _, err := rl.Run(context.Background(), namingRunSpec(runA, finishPlanner(), nil)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if comp.callCount() != 0 {
		t.Errorf("nil Naming made %d LLM calls, want 0", comp.callCount())
	}
	if n := bus.countType(EventTypeSessionNamingFailed); n != 0 {
		t.Errorf("nil Naming emitted %d naming events, want 0", n)
	}
	if len(titler.state) != 0 {
		t.Errorf("nil Naming touched %d session records, want 0", len(titler.state))
	}
}

// ---------------------------------------------------------------------------
// First-name + counter write.
// ---------------------------------------------------------------------------

func TestFireNaming_FirstNameAtAfterTurns(t *testing.T) {
	bus := &fakeBus{}
	rl, _, _ := newTestRunLoop(t, WithRunLoopBus(bus))
	titler := newFakeTitler()
	comp := &fakeCompleter{content: "  Weather in Paris  \n"}
	ns := &NamingSpec{Policy: activePolicy(1, 0, 0), Titler: titler, LLM: comp, Model: "profile-a"}

	if _, err := rl.Run(context.Background(), namingRunSpec(runA, finishPlanner(), ns)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if comp.callCount() != 1 {
		t.Fatalf("LLM calls = %d, want 1", comp.callCount())
	}
	if got := titler.appliedTitle("session-a"); got != "Weather in Paris" {
		t.Errorf("applied title = %q, want clamped %q", got, "Weather in Paris")
	}
	if comp.lastReq.Model != "profile-a" {
		t.Errorf("naming call model = %q, want policy model profile-a", comp.lastReq.Model)
	}
	st := titler.get("session-a")
	if st.TurnCount != 1 || st.AutoNameCount != 1 || st.LastAutoNamedTurn != 1 {
		t.Errorf("counters = turn %d auto %d last %d, want 1/1/1", st.TurnCount, st.AutoNameCount, st.LastAutoNamedTurn)
	}
	if n := bus.countType(EventTypeSessionNamingFailed); n != 0 {
		t.Errorf("naming_failed count = %d, want 0 on success", n)
	}
}

func TestFireNaming_NotYetDue_CountsButNoCall(t *testing.T) {
	bus := &fakeBus{}
	rl, _, _ := newTestRunLoop(t, WithRunLoopBus(bus))
	titler := newFakeTitler()
	comp := &fakeCompleter{content: "Title"}
	ns := &NamingSpec{Policy: activePolicy(3, 0, 0), Titler: titler, LLM: comp}

	if _, err := rl.Run(context.Background(), namingRunSpec(runA, finishPlanner(), ns)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if comp.callCount() != 0 {
		t.Errorf("LLM calls = %d, want 0 (not due at turn 1, after_turns=3)", comp.callCount())
	}
	if st := titler.get("session-a"); st.TurnCount != 1 {
		t.Errorf("TurnCount = %d, want 1 (counter always written when policy active)", st.TurnCount)
	}
	if n := bus.countType(EventTypeSessionNamingFailed); n != 0 {
		t.Errorf("naming_failed count = %d, want 0 (not-due is silent)", n)
	}
}

// ---------------------------------------------------------------------------
// Manual-wins.
// ---------------------------------------------------------------------------

func TestFireNaming_ManualTitleSkips(t *testing.T) {
	bus := &fakeBus{}
	rl, _, _ := newTestRunLoop(t, WithRunLoopBus(bus))
	titler := newFakeTitler()
	titler.seedTS = sessions.TitleSourceManual
	comp := &fakeCompleter{content: "Auto Title"}
	ns := &NamingSpec{Policy: activePolicy(1, 0, 0), Titler: titler, LLM: comp}

	if _, err := rl.Run(context.Background(), namingRunSpec(runA, finishPlanner(), ns)); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if comp.callCount() != 0 {
		t.Errorf("LLM calls = %d, want 0 (manual title wins, no naming call)", comp.callCount())
	}
	if n := bus.countType(EventTypeSessionNamingFailed); n != 1 {
		t.Errorf("naming_failed count = %d, want 1 (manual skip is loud)", n)
	}
	if cls := lastNamingClass(bus); cls != namingClassManualTitle {
		t.Errorf("naming class = %q, want %q", cls, namingClassManualTitle)
	}
}

// ---------------------------------------------------------------------------
// Failure classes — the run outcome is untouched in every case.
// ---------------------------------------------------------------------------

func TestFireNaming_FailureClasses(t *testing.T) {
	cases := []struct {
		name  string
		comp  *fakeCompleter
		class string
	}{
		{"governance_blocked", &fakeCompleter{err: governance.ErrBudgetExceeded}, namingClassGovernanceBlocked},
		{"llm_error", &fakeCompleter{err: errors.New("provider down")}, namingClassLLMError},
		{"empty_title", &fakeCompleter{content: "   \n  "}, namingClassEmptyTitle},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			bus := &fakeBus{}
			rl, _, _ := newTestRunLoop(t, WithRunLoopBus(bus))
			titler := newFakeTitler()
			ns := &NamingSpec{Policy: activePolicy(1, 0, 0), Titler: titler, LLM: c.comp}

			fin, err := rl.Run(context.Background(), namingRunSpec(runA, finishPlanner(), ns))
			if err != nil {
				t.Fatalf("Run err = %v, want nil (naming failure must not fail the run)", err)
			}
			if fin.Reason != planner.FinishGoal {
				t.Errorf("Finish.Reason = %q, want goal (unchanged)", fin.Reason)
			}
			if n := bus.countType(EventTypeSessionNamingFailed); n != 1 {
				t.Fatalf("naming_failed count = %d, want 1", n)
			}
			if cls := lastNamingClass(bus); cls != c.class {
				t.Errorf("naming class = %q, want %q", cls, c.class)
			}
			if got := titler.appliedTitle("session-a"); got != "" {
				t.Errorf("a title was applied (%q) on a failure — want none", got)
			}
		})
	}
}

func TestFireNaming_PanicContained_OutcomeUnchanged(t *testing.T) {
	bus := &fakeBus{}
	rl, _, _ := newTestRunLoop(t, WithRunLoopBus(bus))
	titler := newFakeTitler()
	titler.panicOn = "state"
	comp := &fakeCompleter{content: "Title"}
	ns := &NamingSpec{Policy: activePolicy(1, 0, 0), Titler: titler, LLM: comp}

	fin, err := rl.Run(context.Background(), namingRunSpec(runA, finishPlanner(), ns))
	if err != nil {
		t.Fatalf("Run err = %v, want nil (a contained panic must not fail the run)", err)
	}
	if fin.Reason != planner.FinishGoal {
		t.Errorf("Finish.Reason = %q, want goal", fin.Reason)
	}
	if n := bus.countType(EventTypeSessionNamingFailed); n != 1 {
		t.Errorf("naming_failed count = %d, want 1 (internal class)", n)
	}
	if cls := lastNamingClass(bus); cls != namingClassInternal {
		t.Errorf("naming class = %q, want internal", cls)
	}
}

// ---------------------------------------------------------------------------
// Repeat cadence + cap.
// ---------------------------------------------------------------------------

func TestFireNaming_RepeatCadence_StopsAtCap(t *testing.T) {
	rl, _, _ := newTestRunLoop(t)
	titler := newFakeTitler()
	seq := 0
	comp := &fakeCompleter{perCall: func(llm.CompleteRequest) (string, error) {
		seq++
		return fmt.Sprintf("Title v%d", seq), nil
	}}
	// after_turns=1, repeat_every=2, max_repetitions=3 (3 total auto-names).
	ns := &NamingSpec{Policy: activePolicy(1, 2, 3), Titler: titler, LLM: comp}

	// Complete 10 runs of the same session; naming should fire at turns
	// 1, 3, 5 (first, then every 2) and STOP at the cap of 3.
	for i := range 10 {
		if _, err := rl.Run(context.Background(), namingRunSpec(runA, finishPlanner(), ns)); err != nil {
			t.Fatalf("Run %d: %v", i, err)
		}
	}
	if comp.callCount() != 3 {
		t.Fatalf("naming calls = %d, want exactly 3 (capped at max_repetitions)", comp.callCount())
	}
	st := titler.get("session-a")
	if st.TurnCount != 10 {
		t.Errorf("TurnCount = %d, want 10", st.TurnCount)
	}
	if st.AutoNameCount != 3 {
		t.Errorf("AutoNameCount = %d, want 3", st.AutoNameCount)
	}
	if st.LastAutoNamedTurn != 5 {
		t.Errorf("LastAutoNamedTurn = %d, want 5 (turns 1,3,5)", st.LastAutoNamedTurn)
	}
}

// ---------------------------------------------------------------------------
// Concurrency: N sessions, distinct titles, no bleed, -race.
// ---------------------------------------------------------------------------

func TestFireNaming_ConcurrentSessions_NoBleed(t *testing.T) {
	const n = 24
	rl, _, _ := newTestRunLoop(t)
	titler := newFakeTitler()
	// Distinct title per session, derived from the digest's goal line.
	comp := &fakeCompleter{perCall: func(req llm.CompleteRequest) (string, error) {
		// The user message is the digest; echo a title referencing it so a
		// bled digest would surface as a wrong title.
		digest := ""
		if len(req.Messages) == 2 && req.Messages[1].Content.Text != nil {
			digest = *req.Messages[1].Content.Text
		}
		// Extract the session marker embedded in the goal.
		marker := "unknown"
		if idx := strings.Index(digest, "SESS-"); idx >= 0 {
			end := idx + 5
			for end < len(digest) && digest[end] != '\n' && digest[end] != ' ' {
				end++
			}
			marker = digest[idx:end]
		}
		return "Title-" + marker, nil
	}}
	pol := activePolicy(1, 0, 0)

	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			q := identity.Quadruple{
				Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: fmt.Sprintf("SESS-%d", i)},
				RunID:    fmt.Sprintf("run-%d", i),
			}
			p := &scriptedPlanner{defaultDec: planner.Finish{Reason: planner.FinishGoal, Payload: "done"}}
			spec := RunSpec{
				Planner:  p,
				Base:     planner.RunContext{Quadruple: q, Goal: fmt.Sprintf("goal for SESS-%d here", i), Trajectory: &planner.Trajectory{}},
				MaxSteps: 8,
				Naming:   &NamingSpec{Policy: pol, Titler: titler, LLM: comp},
			}
			if _, err := rl.Run(context.Background(), spec); err != nil {
				t.Errorf("Run session %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	for i := range n {
		sid := fmt.Sprintf("SESS-%d", i)
		want := "Title-" + sid
		if got := titler.appliedTitle(sid); got != want {
			t.Errorf("session %s title = %q, want %q (cross-session bleed)", sid, got, want)
		}
	}
}

// lastNamingClass returns the ErrorClass of the most recent
// session.naming_failed payload published to bus.
func lastNamingClass(bus *fakeBus) string {
	for i := len(bus.published) - 1; i >= 0; i-- {
		if bus.published[i].Type != EventTypeSessionNamingFailed {
			continue
		}
		if p, ok := bus.published[i].Payload.(SessionNamingFailedPayload); ok {
			return p.ErrorClass
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Terminal-boundary ordering: the hook fires BEFORE naming (LIFO — the
// naming defer registers first, the hook defer second).
// ---------------------------------------------------------------------------

// orderRecordingCompleter is a NamingCompleter that records its invocation on
// a shared sequence AND advances the fake clock — simulating a SLOW naming
// LLM call. If the hook ran after naming, the clock advance would inflate the
// hook payload's CompletedAt/DurationMS.
type orderRecordingCompleter struct {
	clk   *fakeClock
	slow  time.Duration
	seq   *[]string
	seqMu *sync.Mutex
}

func (c *orderRecordingCompleter) Complete(_ context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.seqMu.Lock()
	*c.seq = append(*c.seq, "naming")
	c.seqMu.Unlock()
	c.clk.advance(c.slow) // the "slow LLM": time passes during the naming call
	return llm.CompleteResponse{Content: "A Title"}, nil
}

// orderRecordingExecutor wraps recordingHookExecutor and records the hook
// dispatch on the shared sequence.
type orderRecordingExecutor struct {
	*recordingHookExecutor
	seq   *[]string
	seqMu *sync.Mutex
}

func (e *orderRecordingExecutor) ExecuteDecision(ctx context.Context, rc planner.RunContext, d planner.Decision) (any, any, error) {
	e.seqMu.Lock()
	*e.seq = append(*e.seq, "hook")
	e.seqMu.Unlock()
	return e.recordingHookExecutor.ExecuteDecision(ctx, rc, d)
}

// TestRun_TerminalOrdering_HookFiresBeforeNaming pins the S1 defer ordering:
// at the terminal boundary the run-completion hook dispatches FIRST, so a
// slow naming LLM call can never inflate the hook's CompletedAt/DurationMS or
// delay transcript egress. The naming completer advances the fake clock by
// 7s; the hook payload's DurationMS must not include it.
func TestRun_TerminalOrdering_HookFiresBeforeNaming(t *testing.T) {
	clk := newFakeClock()
	reg := NewRegistry(WithClock(clk))
	coord := &stubCoordinator{}
	rl, err := NewRunLoop(reg, coord, WithRunLoopClock(clk))
	if err != nil {
		t.Fatalf("NewRunLoop: %v", err)
	}

	var seq []string
	var seqMu sync.Mutex
	exec := &orderRecordingExecutor{recordingHookExecutor: &recordingHookExecutor{}, seq: &seq, seqMu: &seqMu}
	titler := newFakeTitler()
	comp := &orderRecordingCompleter{clk: clk, slow: 7 * time.Second, seq: &seq, seqMu: &seqMu}

	spec := RunSpec{
		Planner:        finishPlanner(),
		Base:           planner.RunContext{Quadruple: runA, Goal: "order test", Trajectory: &planner.Trajectory{}},
		MaxSteps:       8,
		ToolExecutor:   exec,
		CompletionHook: &CompletionHookSpec{Tool: hookTool},
		Naming:         &NamingSpec{Policy: activePolicy(1, 0, 0), Titler: titler, LLM: comp},
	}
	if _, err := rl.Run(context.Background(), spec); err != nil {
		t.Fatalf("Run: %v", err)
	}

	seqMu.Lock()
	gotSeq := append([]string(nil), seq...)
	seqMu.Unlock()
	if len(gotSeq) != 2 || gotSeq[0] != "hook" || gotSeq[1] != "naming" {
		t.Fatalf("terminal-boundary order = %v, want [hook naming] (the hook fires first)", gotSeq)
	}

	calls := exec.hookCalls()
	if len(calls) != 1 {
		t.Fatalf("hook dispatched %d times, want 1", len(calls))
	}
	// The naming call advanced the clock 7s AFTER the hook stamped its
	// payload — DurationMS must not include the naming latency.
	if d := calls[0].payload.DurationMS; d >= 7000 {
		t.Errorf("hook payload DurationMS = %d, includes the slow naming call (want < 7000)", d)
	}
	// Naming still succeeded (running second costs it nothing).
	if got := titler.appliedTitle("session-a"); got != "A Title" {
		t.Errorf("naming title = %q, want %q", got, "A Title")
	}
}
