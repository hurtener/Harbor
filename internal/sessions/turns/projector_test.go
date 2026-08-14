package turns

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// fakeClock is the controllable clock injected via WithClock so the
// projector's timestamps are deterministic without time.Sleep.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

var testClockStart = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

func newTestProjector(t *testing.T, retain int, durable bool) (*Projector, *testStore) {
	t.Helper()
	st, err := newTestStore(retain, durable)
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}
	clock := &fakeClock{now: testClockStart}
	p, err := New(st, WithClock(clock))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p, st
}

func tripleA() identity.Identity {
	return identity.Identity{TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a"}
}

func tripleB() identity.Identity {
	return identity.Identity{TenantID: "tenant-b", UserID: "user-b", SessionID: "session-b"}
}

func appendTurn(p *Projector, id identity.Identity, turnID TurnID) (TurnRow, error) {
	return p.Append(context.Background(), id, Append{
		TurnID:  turnID,
		Query:   "what is the weather?",
		AgentID: "agent-1",
	})
}

func TestProjector_Append_CreatesMutableRunningRow(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	row, err := appendTurn(p, tripleA(), "run-1")
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if row.TurnID != "run-1" {
		t.Errorf("TurnID=%q, want run-1", row.TurnID)
	}
	if row.Sequence != 1 {
		t.Errorf("Sequence=%d, want 1 (first minted)", row.Sequence)
	}
	if row.Status != StatusRunning {
		t.Errorf("Status=%q, want running", row.Status)
	}
	if row.Sealed {
		t.Errorf("Sealed=true on a fresh append")
	}
	if row.Version != 1 {
		t.Errorf("Version=%d, want 1", row.Version)
	}
	if !row.StartedAt.Equal(testClockStart) {
		t.Errorf("StartedAt=%v, want the injected clock", row.StartedAt)
	}
	if row.Query.Text != "what is the weather?" || row.Query.Complete != CompletenessComplete {
		t.Errorf("Query projection wrong: %+v", row.Query)
	}
	if row.Agent.ID != "agent-1" || row.Agent.Complete != CompletenessComplete {
		t.Errorf("Agent projection wrong: %+v", row.Agent)
	}
	// The authoritative task id derives from the row key when the
	// runtime did not report one separately; the run id is honestly
	// empty (unavailable) — never equated with the task id.
	if row.TaskID != "run-1" {
		t.Errorf("TaskID=%q, want run-1 (derived from TurnID)", row.TaskID)
	}
	if row.RunID != "" {
		t.Errorf("RunID=%q, want empty (unavailable — never fabricated)", row.RunID)
	}
	// A fresh turn has no answer / usage / reasoning / app yet — honest
	// Unavailable, never a fabricated zero.
	if row.Answer.Complete != CompletenessUnavailable {
		t.Errorf("Answer.Complete=%q, want unavailable", row.Answer.Complete)
	}
	// Every usage measure is honestly unavailable on a fresh turn, with
	// NO value — never a fabricated zero.
	for name, m := range usageMeasures(row.Usage) {
		if m.State != UsageUnavailable {
			t.Errorf("fresh Usage.%s state=%q, want unavailable", name, m.State)
		}
		if m.Value != nil {
			t.Errorf("fresh Usage.%s carries a value %d — a missing measure is unavailable, never zero", name, *m.Value)
		}
	}
	if row.Usage.Model != "" {
		t.Errorf("fresh Usage.Model=%q, want empty (unavailable)", row.Usage.Model)
	}
	if row.Reasoning.Complete != CompletenessUnavailable {
		t.Errorf("Reasoning.Complete=%q, want unavailable", row.Reasoning.Complete)
	}
	if len(row.Apps) != 0 {
		t.Errorf("Apps=%+v, want an empty collection", row.Apps)
	}
}

func TestProjector_Append_SequencesAreMonotonicAndPerSession(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	if _, err := appendTurn(p, tripleA(), "run-1"); err != nil {
		t.Fatalf("append 1: %v", err)
	}
	r2, err := appendTurn(p, tripleA(), "run-2")
	if err != nil {
		t.Fatalf("append 2: %v", err)
	}
	if r2.Sequence != 2 {
		t.Errorf("second sequence=%d, want 2", r2.Sequence)
	}
	// A different session restarts the counter.
	r3, err := appendTurn(p, tripleB(), "run-1")
	if err != nil {
		t.Fatalf("append other session: %v", err)
	}
	if r3.Sequence != 1 {
		t.Errorf("other-session sequence=%d, want 1 (per-session counters)", r3.Sequence)
	}
}

func TestProjector_Append_IdempotentOnTurnID(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	first, err := appendTurn(p, tripleA(), "run-1")
	if err != nil {
		t.Fatalf("first append: %v", err)
	}
	second, err := p.Append(context.Background(), tripleA(), Append{
		TurnID: "run-1",
		Query:  "a DIFFERENT query must not overwrite",
	})
	if err != nil {
		t.Fatalf("re-append: %v", err)
	}
	if second.TurnID != first.TurnID || second.Sequence != first.Sequence || second.Version != first.Version {
		t.Errorf("re-append returned a changed row: %+v vs %+v", second, first)
	}
	if second.Query.Text != "what is the weather?" {
		t.Errorf("idempotent re-append overwrote the query: %q", second.Query.Text)
	}
}

func TestProjector_Append_ValidationFailsLoud(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	cases := []struct {
		name string
		a    Append
		want error
	}{
		{"empty turn id", Append{Query: "q"}, ErrInvalidInput},
		{"over-bound query", Append{TurnID: "r", Query: strings.Repeat("x", MaxQueryRunes+1)}, ErrInvalidInput},
		{"terminal status", Append{TurnID: "r", Query: "q", Status: StatusComplete}, ErrInvalidStatus},
		{"unknown status", Append{TurnID: "r", Query: "q", Status: Status("bogus")}, ErrInvalidStatus},
		{"empty activity tool", Append{TurnID: "r", Query: "q", Activity: []ActivityRow{{Tool: ""}}}, ErrInvalidInput},
		{"bad activity status", Append{TurnID: "r", Query: "q", Activity: []ActivityRow{{Tool: "t", Status: ActivityStatus("x")}}}, ErrInvalidInput},
		{"long activity summary", Append{TurnID: "r", Query: "q", Activity: []ActivityRow{{Tool: "t", Summary: strings.Repeat("s", MaxActivitySummaryRunes+1)}}}, ErrInvalidInput},
		{"empty attachment id", Append{TurnID: "r", Query: "q", Inputs: []Attachment{{Filename: "f"}}}, ErrInvalidInput},
		{"negative attachment size", Append{TurnID: "r", Query: "q", Outputs: []Attachment{{ID: "a", SizeBytes: -1}}}, ErrInvalidInput},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.Append(context.Background(), id, tc.a)
			if !errors.Is(err, tc.want) {
				t.Errorf("Append error=%v, want errors.Is(%v)", err, tc.want)
			}
		})
	}
	// Identity is mandatory.
	if _, err := p.Append(context.Background(), identity.Identity{}, Append{TurnID: "r"}); !errors.Is(err, ErrIdentityRequired) {
		t.Errorf("empty identity error=%v, want ErrIdentityRequired", err)
	}
}

func TestProjector_Update_MutatesMutableRowInPlace(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	usage := Usage{
		PromptTokens:     usageExact(100),
		CompletionTokens: usageExact(50),
		TotalTokens:      usageExact(150),
		CostMicroUSD:     usageExact(2500), // $0.0025 in exact integer micro-dollars
		Model:            "gpt-x",
	}
	answer := Answer{State: AnswerStateInline, Inline: "It is sunny."}
	act := []ActivityRow{{Tool: "search", Status: ActivitySucceeded, Summary: "0.4s"}}
	pause := Pause{Class: PauseClassHitlApproval, Reason: "approval", Lifecycle: PauseLifecycleActive}
	upd, err := p.Update(context.Background(), id, "run-1", row.Version, Update{
		Status:   StatusPaused,
		Answer:   &answer,
		Usage:    &usage,
		Activity: act,
		Pause:    &pause,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if upd.Status != StatusPaused {
		t.Errorf("Status=%q, want paused", upd.Status)
	}
	if upd.Pause.Availability != CompletenessComplete || upd.Pause.Lifecycle != PauseLifecycleActive {
		t.Errorf("paused row must carry an active available pause: %+v", upd.Pause)
	}
	if upd.Version != row.Version+1 {
		t.Errorf("Version=%d, want %d", upd.Version, row.Version+1)
	}
	if got := upd.Usage.TotalTokens; got.State != UsageExact || got.Value == nil || *got.Value != 150 {
		t.Errorf("Usage.TotalTokens not replaced: %+v", got)
	}
	if upd.Usage.Model != "gpt-x" {
		t.Errorf("Usage.Model not replaced: %+v", upd.Usage)
	}
	if got := upd.Usage.CostMicroUSD; got.State != UsageExact || got.Value == nil || *got.Value != 2500 {
		t.Errorf("Usage.CostMicroUSD not replaced (must be exact integer micro-dollars): %+v", got)
	}
	if upd.Answer.Inline != "It is sunny." {
		t.Errorf("Answer not replaced: %+v", upd.Answer)
	}
	if len(upd.Activity.Rows) != 1 || upd.Activity.Rows[0].Tool != "search" || upd.Activity.Complete != CompletenessComplete {
		t.Errorf("Activity not replaced: %+v", upd.Activity)
	}
	if upd.Activity.Rows[0].TerminalClass != ActivityTerminalSucceeded || upd.Activity.Rows[0].Position != 0 {
		t.Errorf("Activity row derived fields wrong: %+v", upd.Activity.Rows[0])
	}

	// nil components leave the stored ones unchanged.
	upd2, err := p.Update(context.Background(), id, "run-1", upd.Version, Update{Status: StatusRunning, Pause: &Pause{Availability: CompletenessUnavailable}})
	if err != nil {
		t.Fatalf("Update 2: %v", err)
	}
	if upd2.Status != StatusRunning {
		t.Errorf("Status=%q, want running", upd2.Status)
	}
	if upd2.Answer.Inline != "It is sunny." || *upd2.Usage.TotalTokens.Value != 150 {
		t.Errorf("nil components must leave the row unchanged: %+v", upd2)
	}
}

func TestProjector_Update_StaleVersionAndSealedRejected(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := p.Update(context.Background(), id, "run-1", row.Version+99, Update{}); !errors.Is(err, ErrStaleVersion) {
		t.Errorf("stale update error=%v, want ErrStaleVersion", err)
	}
	if _, err := p.Update(context.Background(), id, "no-such-turn", 1, Update{}); !errors.Is(err, ErrTurnNotFound) {
		t.Errorf("missing turn error=%v, want ErrTurnNotFound", err)
	}

	// A terminal status cannot be set through Update.
	if _, err := p.Update(context.Background(), id, "run-1", row.Version, Update{Status: StatusComplete}); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("terminal status via Update error=%v, want ErrInvalidStatus", err)
	}

	// After sealing, updates are refused.
	if err := sealComplete(p, id); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := p.Update(context.Background(), id, "run-1", row.Version+1, Update{}); !errors.Is(err, ErrTurnSealed) {
		t.Errorf("update of sealed row error=%v, want ErrTurnSealed", err)
	}
}

// sealComplete attaches a complete answer and seals the turn complete.
func sealComplete(p *Projector, id identity.Identity) error {
	row, err := p.Get(context.Background(), id, "run-1")
	if err != nil {
		return err
	}
	if row.Answer.Complete != CompletenessComplete {
		ans := Answer{State: AnswerStateInline, Inline: "final answer"}
		if _, err := p.Update(context.Background(), id, "run-1", row.Version, Update{Answer: &ans}); err != nil {
			return err
		}
		row, err = p.Get(context.Background(), id, "run-1")
		if err != nil {
			return err
		}
	}
	_, err = p.Seal(context.Background(), id, "run-1", row.Version, Seal{Status: StatusComplete, FinishReason: FinishGoal})
	return err
}

func TestProjector_Update_ValidationFailsLoud(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	v := row.Version

	heavy := Answer{State: AnswerStateInline, Inline: strings.Repeat("x", MaxInlineAnswerBytes)}
	if _, err := p.Update(context.Background(), id, "run-1", v, Update{Answer: &heavy}); !errors.Is(err, ErrContextLeak) {
		t.Errorf("heavy inline answer error=%v, want ErrContextLeak", err)
	}
	both := Answer{State: AnswerStateInline, Inline: "x", Ref: &AnswerRef{ID: "a"}}
	if _, err := p.Update(context.Background(), id, "run-1", v, Update{Answer: &both}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("inline+ref answer error=%v, want ErrInvalidInput", err)
	}
	contentInUnavailable := Answer{State: AnswerStateUnavailable, Inline: "x"}
	if _, err := p.Update(context.Background(), id, "run-1", v, Update{Answer: &contentInUnavailable}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("content in unavailable answer error=%v, want ErrInvalidInput", err)
	}
	negUsage := Usage{PromptTokens: usageExact(-1)}
	if _, err := p.Update(context.Background(), id, "run-1", v, Update{Usage: &negUsage}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("negative usage error=%v, want ErrInvalidInput", err)
	}
	// An unavailable measure must carry NO value — a missing measure is
	// unavailable, never a fabricated zero.
	lyingUsage := Usage{PromptTokens: UsageMeasure{State: UsageUnavailable, Value: usageInt64(0)}}
	if _, err := p.Update(context.Background(), id, "run-1", v, Update{Usage: &lyingUsage}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("unavailable-with-value usage error=%v, want ErrInvalidInput", err)
	}
	// A present measure must carry a value.
	valueless := Usage{PromptTokens: UsageMeasure{State: UsageExact}}
	if _, err := p.Update(context.Background(), id, "run-1", v, Update{Usage: &valueless}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("exact-without-value usage error=%v, want ErrInvalidInput", err)
	}
	// The referenced answer is the D-026-safe route for heavy content.
	refAns := Answer{State: AnswerStateArtifactRef, Ref: &AnswerRef{ID: "art_1", MimeType: "text/plain", SizeBytes: 1 << 20, SHA256: "ab"}}
	if _, err := p.Update(context.Background(), id, "run-1", v, Update{Answer: &refAns}); err != nil {
		t.Errorf("referenced answer rejected: %v", err)
	}
}

func TestProjector_Seal_RequiresTerminalSources(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	// Non-terminal seal statuses fail loud.
	if _, err := p.Seal(context.Background(), id, "run-1", row.Version, Seal{Status: StatusRunning}); !errors.Is(err, ErrNotTerminal) {
		t.Errorf("non-terminal seal error=%v, want ErrNotTerminal", err)
	}
	if _, err := p.Seal(context.Background(), id, "run-1", row.Version, Seal{Status: Status("bogus")}); !errors.Is(err, ErrInvalidStatus) {
		t.Errorf("unknown seal status error=%v, want ErrInvalidStatus", err)
	}

	// Complete seal without an answer: refused, naming the source.
	_, err = p.Seal(context.Background(), id, "run-1", row.Version, Seal{Status: StatusComplete})
	if !errors.Is(err, ErrSealIncomplete) || !strings.Contains(err.Error(), "answer") {
		t.Errorf("complete seal without answer error=%v, want ErrSealIncomplete naming answer", err)
	}

	// Failed seal without an error class: refused, naming the source.
	_, err = p.Seal(context.Background(), id, "run-1", row.Version, Seal{Status: StatusFailed})
	if !errors.Is(err, ErrSealIncomplete) || !strings.Contains(err.Error(), "error_class") {
		t.Errorf("failed seal without error class error=%v, want ErrSealIncomplete naming error_class", err)
	}

	// Cancelled seal needs nothing beyond the lifecycle.
	sealed, err := p.Seal(context.Background(), id, "run-1", row.Version, Seal{Status: StatusCancelled, FinishReason: FinishCancelled})
	if err != nil {
		t.Fatalf("cancelled seal: %v", err)
	}
	if !sealed.Sealed || sealed.Status != StatusCancelled || sealed.FinishReason != FinishCancelled {
		t.Errorf("sealed row wrong: %+v", sealed)
	}
	if !sealed.FinishedAt.Equal(testClockStart) {
		t.Errorf("FinishedAt=%v, want the injected clock", sealed.FinishedAt)
	}
	if sealed.Version != row.Version+1 {
		t.Errorf("sealed Version=%d, want %d", sealed.Version, row.Version+1)
	}
}

func TestProjector_Seal_CompleteWithReferencedAnswer(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	refAns := Answer{State: AnswerStateArtifactRef, Ref: &AnswerRef{ID: "art_1", SizeBytes: 4096}}
	if _, err := p.Update(context.Background(), id, "run-1", row.Version, Update{Answer: &refAns}); err != nil {
		t.Fatalf("update answer: %v", err)
	}
	sealed, err := p.Seal(context.Background(), id, "run-1", row.Version+1, Seal{Status: StatusComplete})
	if err != nil {
		t.Fatalf("seal complete with referenced answer: %v", err)
	}
	if !sealed.Sealed || sealed.Answer.Ref == nil || sealed.Answer.Ref.ID != "art_1" {
		t.Errorf("sealed row lost the referenced answer: %+v", sealed.Answer)
	}
}

func TestProjector_Seal_ReSealSemantics(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	if _, err := appendTurn(p, id, "run-1"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := sealComplete(p, id); err != nil {
		t.Fatalf("seal: %v", err)
	}
	sealed, err := p.Get(context.Background(), id, "run-1")
	if err != nil {
		t.Fatalf("get sealed: %v", err)
	}
	// Same-status re-seal is an idempotent replay no-op.
	again, err := p.Seal(context.Background(), id, "run-1", sealed.Version, Seal{Status: StatusComplete})
	if err != nil {
		t.Fatalf("same-status re-seal: %v", err)
	}
	if again.Version != sealed.Version || again.Sealed != true {
		t.Errorf("re-seal changed the sealed row: %+v", again)
	}
	// Conflicting re-seal fails loud.
	if _, err := p.Seal(context.Background(), id, "run-1", sealed.Version, Seal{Status: StatusFailed, ErrorClass: ErrorClassTransient}); !errors.Is(err, ErrTurnSealed) {
		t.Errorf("conflicting re-seal error=%v, want ErrTurnSealed", err)
	}
}

func TestProjector_Seal_StaleVersion(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := p.Seal(context.Background(), id, "run-1", row.Version+7, Seal{Status: StatusCancelled}); !errors.Is(err, ErrStaleVersion) {
		t.Errorf("stale seal error=%v, want ErrStaleVersion", err)
	}
}

func TestProjector_AttachReasoning_OrderedAndBounded(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	steps := []ReasoningStep{
		{Index: 0, Kind: ReasoningKindToolCall},
		{Index: 2, Kind: ReasoningKindSpawn},
		{Index: 5, Kind: ReasoningKindAwait},
	}
	got, err := p.AttachReasoning(context.Background(), id, "run-1", row.Version, ReasoningInput{Steps: steps})
	if err != nil {
		t.Fatalf("AttachReasoning: %v", err)
	}
	if got.Reasoning.Complete != CompletenessComplete || len(got.Reasoning.Steps) != 3 {
		t.Errorf("reasoning projection wrong: %+v", got.Reasoning)
	}
	if got.Reasoning.Steps[1].Index != 2 || got.Reasoning.Steps[1].Kind != ReasoningKindSpawn {
		t.Errorf("ordered steps wrong: %+v", got.Reasoning.Steps)
	}

	// Overflow keeps the FIRST MaxReasoningSteps and reports the tail
	// drop as Partial + Dropped.
	overflow := make([]ReasoningStep, MaxReasoningSteps+5)
	for i := range overflow {
		overflow[i] = ReasoningStep{Index: i, Kind: ReasoningKindToolCall}
	}
	got2, err := p.AttachReasoning(context.Background(), id, "run-1", got.Version, ReasoningInput{Steps: overflow})
	if err != nil {
		t.Fatalf("AttachReasoning overflow: %v", err)
	}
	if got2.Reasoning.Complete != CompletenessPartial || got2.Reasoning.Dropped != 5 || len(got2.Reasoning.Steps) != MaxReasoningSteps {
		t.Errorf("overflow reasoning wrong: %+v", got2.Reasoning)
	}
	if got2.Reasoning.Steps[0].Index != 0 || got2.Reasoning.Steps[len(got2.Reasoning.Steps)-1].Index != MaxReasoningSteps-1 {
		t.Errorf("overflow kept the wrong window (must keep the FIRST steps): %+v", got2.Reasoning.Steps)
	}

	// An empty feed marks the component Unavailable.
	got3, err := p.AttachReasoning(context.Background(), id, "run-1", got2.Version, ReasoningInput{})
	if err != nil {
		t.Fatalf("AttachReasoning empty: %v", err)
	}
	if got3.Reasoning.Complete != CompletenessUnavailable || len(got3.Reasoning.Steps) != 0 {
		t.Errorf("empty reasoning feed wrong: %+v", got3.Reasoning)
	}

	// Non-monotonic indices fail loud.
	if _, err := p.AttachReasoning(context.Background(), id, "run-1", got3.Version, ReasoningInput{Steps: []ReasoningStep{{Index: 3, Kind: ReasoningKindToolCall}, {Index: 1, Kind: ReasoningKindToolCall}}}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("non-increasing indices error=%v, want ErrInvalidInput", err)
	}
	// A kind outside the CLOSED set fails loud — raw thinking has no
	// representable form.
	if _, err := p.AttachReasoning(context.Background(), id, "run-1", got3.Version, ReasoningInput{Steps: []ReasoningStep{{Index: 0, Kind: ReasoningKind("raw_thinking")}}}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("unknown reasoning kind error=%v, want ErrInvalidInput", err)
	}
}

func TestProjector_AttachAppRefs_OrderedCollection_ReplaceInPlace(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	// A ref that could only mount broken fails loud.
	if _, err := p.AttachAppRefs(context.Background(), id, "run-1", row.Version, AppRefInput{Refs: []AppRef{{ServerID: "srv"}}}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("missing resource uri error=%v, want ErrInvalidInput", err)
	}

	// First declaration [A, B]: fixes their positions.
	refA := AppRef{EffectiveAgentID: "agent-1", ServerID: "srv-1", ResourceURI: "ui://srv/app", DisplayMode: "inline", ToolName: "tool-a"}
	refB := AppRef{EffectiveAgentID: "agent-1", ServerID: "srv-2", ResourceURI: "ui://srv/app2", ToolName: "tool-b"}
	got, err := p.AttachAppRefs(context.Background(), id, "run-1", row.Version, AppRefInput{Refs: []AppRef{refA, refB}})
	if err != nil {
		t.Fatalf("AttachAppRefs: %v", err)
	}
	if len(got.Apps) != 2 || got.Apps[0].ServerID != "srv-1" || got.Apps[1].ServerID != "srv-2" {
		t.Fatalf("first-declaration collection wrong: %+v", got.Apps)
	}
	if got.Apps[0].Availability != AppAvailable || got.Apps[0].Complete != CompletenessComplete {
		t.Errorf("defaults not applied: %+v", got.Apps[0])
	}

	// A repeat of B's identity (same effective agent, server, URI)
	// replaces IN PLACE with the latest correlation metadata — position
	// stays fixed by the first declaration.
	refB2 := AppRef{EffectiveAgentID: "agent-1", ServerID: "srv-2", ResourceURI: "ui://srv/app2", ToolName: "tool-b-v2", ToolCallID: "tc-99", Availability: AppUnavailable}
	got2, err := p.AttachAppRefs(context.Background(), id, "run-1", got.Version, AppRefInput{Refs: []AppRef{refB2}})
	if err != nil {
		t.Fatalf("AttachAppRefs replace: %v", err)
	}
	if len(got2.Apps) != 2 {
		t.Fatalf("repeat must replace in place, got %d entries: %+v", len(got2.Apps), got2.Apps)
	}
	if got2.Apps[1].ServerID != "srv-2" || got2.Apps[1].ToolName != "tool-b-v2" || got2.Apps[1].ToolCallID != "tc-99" || got2.Apps[1].Availability != AppUnavailable {
		t.Errorf("in-place replacement wrong: %+v", got2.Apps[1])
	}
	if got2.Apps[0].ServerID != "srv-1" {
		t.Errorf("unrelated ref moved: %+v", got2.Apps[0])
	}

	// A new identity appends at the END; a re-declared A stays at its
	// first-declaration position.
	refC := AppRef{EffectiveAgentID: "agent-1", ServerID: "srv-3", ResourceURI: "ui://srv/app3", ToolName: "tool-c"}
	refA2 := AppRef{EffectiveAgentID: "agent-1", ServerID: "srv-1", ResourceURI: "ui://srv/app", ToolName: "tool-a-v2"}
	got3, err := p.AttachAppRefs(context.Background(), id, "run-1", got2.Version, AppRefInput{Refs: []AppRef{refC, refA2}})
	if err != nil {
		t.Fatalf("AttachAppRefs append: %v", err)
	}
	if len(got3.Apps) != 3 {
		t.Fatalf("collection size wrong after append: %d, want 3", len(got3.Apps))
	}
	if got3.Apps[0].ServerID != "srv-1" || got3.Apps[0].ToolName != "tool-a-v2" {
		t.Errorf("first-declaration position not preserved: %+v", got3.Apps[0])
	}
	if got3.Apps[1].ServerID != "srv-2" || got3.Apps[2].ServerID != "srv-3" {
		t.Errorf("collection order wrong: %+v", got3.Apps)
	}

	// The SAME identity declared twice in ONE feed: the second
	// declaration replaces the first in place (no duplicate entries).
	dup1 := AppRef{EffectiveAgentID: "agent-1", ServerID: "srv-9", ResourceURI: "ui://srv/app9", ToolName: "t1"}
	dup2 := AppRef{EffectiveAgentID: "agent-1", ServerID: "srv-9", ResourceURI: "ui://srv/app9", ToolName: "t2", ToolCallID: "tc-1"}
	got4, err := p.AttachAppRefs(context.Background(), id, "run-1", got3.Version, AppRefInput{Refs: []AppRef{dup1, dup2}})
	if err != nil {
		t.Fatalf("AttachAppRefs dup-in-feed: %v", err)
	}
	if len(got4.Apps) != 4 {
		t.Fatalf("duplicate identity in one feed created %d entries, want 4: %+v", len(got4.Apps), got4.Apps)
	}
	last := got4.Apps[len(got4.Apps)-1]
	if last.ServerID != "srv-9" || last.ToolName != "t2" {
		t.Errorf("same-feed duplicate did not collapse: %+v", last)
	}
}

// TestProjector_AppRefs_IdentityKeySeparatesAgents proves the
// replacement identity is exactly (effective_agent_id, server_id,
// resource_uri): the same server + resource under DIFFERENT effective
// agents are distinct entries.
func TestProjector_AppRefs_IdentityKeySeparatesAgents(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := p.AttachAppRefs(context.Background(), id, "run-1", row.Version, AppRefInput{Refs: []AppRef{
		{EffectiveAgentID: "agent-1", ServerID: "srv", ResourceURI: "ui://srv/app", ToolName: "t1"},
		{EffectiveAgentID: "agent-2", ServerID: "srv", ResourceURI: "ui://srv/app", ToolName: "t2"},
	}})
	if err != nil {
		t.Fatalf("AttachAppRefs: %v", err)
	}
	if len(got.Apps) != 2 {
		t.Fatalf("distinct effective agents must be distinct entries: %+v", got.Apps)
	}
}

func TestProjector_ActivityOverflow_ExplicitLowerBound(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	fed := make([]ActivityRow, DefaultActivityLimit+7)
	for i := range fed {
		fed[i] = ActivityRow{Tool: "t", Status: ActivitySucceeded, Summary: "0.1s"}
	}
	got, err := p.Update(context.Background(), id, "run-1", row.Version, Update{Activity: fed})
	if err != nil {
		t.Fatalf("update activity: %v", err)
	}
	if got.Activity.Complete != CompletenessPartial {
		t.Errorf("Activity.Complete=%q, want partial", got.Activity.Complete)
	}
	if !got.Activity.More {
		t.Errorf("Activity.More=false, want the explicit lower-bound marker")
	}
	if got.Activity.Dropped != 7 {
		t.Errorf("Activity.Dropped=%d, want 7", got.Activity.Dropped)
	}
	if len(got.Activity.Rows) != DefaultActivityLimit {
		t.Errorf("retained %d rows, want %d", len(got.Activity.Rows), DefaultActivityLimit)
	}
	// The EXACT totals cover the FULL fed activity — truncating the
	// inline window never erases the turn summary.
	if got.Activity.Totals.Succeeded != int64(len(fed)) {
		t.Errorf("Activity.Totals.Succeeded=%d, want %d (the full fed count, not the window)", got.Activity.Totals.Succeeded, len(fed))
	}
	if got.Activity.Totals.Invoked != 0 || got.Activity.Totals.Failed != 0 {
		t.Errorf("Activity.Totals must not invent categories: %+v", got.Activity.Totals)
	}
}

// TestActivity_TotalsSurviveInlineOverflow pins the exact turn-level
// totals contract: even when the inline window truncates an
// over-budget feed, Activity.Totals carries the exact per-status
// counts across the FULL fed activity, so list/get still render the
// turn's activity summary.
func TestActivity_TotalsSurviveInlineOverflow(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	// A deliberately mixed feed that exceeds the default window by a
	// wide margin.
	fed := []ActivityRow{
		{Tool: "a", Status: ActivitySucceeded},
		{Tool: "b", Status: ActivityFailed},
		{Tool: "c", Status: ActivityCancelled},
		{Tool: "d", Status: ActivityPolicyExhausted, PolicyExhausted: true},
		{Tool: "e", Status: ActivityRetried},
		{Tool: "f", Status: ActivityInvoked},
	}
	// Grow to exceed the window while preserving the mix.
	for len(fed) < DefaultActivityLimit+5 {
		fed = append(fed, ActivityRow{Tool: "x", Status: ActivitySucceeded})
	}
	got, err := p.Update(context.Background(), id, "run-1", row.Version, Update{Activity: fed})
	if err != nil {
		t.Fatalf("update activity: %v", err)
	}
	if !got.Activity.More || got.Activity.Dropped == 0 {
		t.Fatalf("precondition: inline overflow required, got %+v", got.Activity)
	}
	want := ActivityTotals{
		Invoked:         1,
		Succeeded:       int64(len(fed) - 5),
		Failed:          1,
		Cancelled:       1,
		Retried:         1,
		PolicyExhausted: 1,
	}
	if got.Activity.Totals != want {
		t.Errorf("Activity.Totals=%+v, want %+v (exact across the FULL fed activity)", got.Activity.Totals, want)
	}
	if int64(got.Activity.Dropped)+int64(len(got.Activity.Rows)) != int64(len(fed)) {
		t.Errorf("window accounting wrong: dropped %d + retained %d != fed %d",
			got.Activity.Dropped, len(got.Activity.Rows), len(fed))
	}
	// The retained window is the NEWEST rows with their derived
	// terminal classes and immutable positions.
	last := got.Activity.Rows[len(got.Activity.Rows)-1]
	if last.Position != len(fed)-1 || last.TerminalClass != ActivityTerminalSucceeded {
		t.Errorf("retained newest row wrong: %+v", last)
	}

	// Re-feeding the same cumulative list replaces the totals
	// wholesale (cumulative snapshot semantics).
	got2, err := p.Update(context.Background(), id, "run-1", got.Version, Update{Activity: fed})
	if err != nil {
		t.Fatalf("re-feed activity: %v", err)
	}
	if got2.Activity.Totals != want {
		t.Errorf("re-feed Activity.Totals=%+v, want the same exact totals %+v", got2.Activity.Totals, want)
	}
}

// TestProjector_Update_NonNilEmptyActivity_Refused pins the fail-loud
// semantic for an accidental non-nil EMPTY Update.Activity: the runtime
// feeds CUMULATIVE activity snapshots, and an empty cumulative feed
// applied to a row would erase the turn's accumulated exact activity
// totals. The projector refuses it with ErrInvalidInput — durable
// truth is never silently erased — and a caller with nothing to report
// passes nil (leave unchanged). No new "clear activity" API exists.
func TestProjector_Update_NonNilEmptyActivity_Refused(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	// Accumulate exact activity totals first.
	fed := []ActivityRow{
		{Tool: "a", Status: ActivitySucceeded},
		{Tool: "b", Status: ActivityFailed},
	}
	got, err := p.Update(context.Background(), id, "run-1", row.Version, Update{Activity: fed})
	if err != nil {
		t.Fatalf("update activity: %v", err)
	}
	if got.Activity.Totals.Succeeded != 1 || got.Activity.Totals.Failed != 1 {
		t.Fatalf("precondition: exact totals must be accumulated, got %+v", got.Activity.Totals)
	}

	// A non-nil EMPTY feed is refused — it would erase the totals.
	if _, err := p.Update(context.Background(), id, "run-1", got.Version, Update{Activity: []ActivityRow{}}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("non-nil empty activity feed error=%v, want ErrInvalidInput (never silently erases exact totals)", err)
	}
	// The refusal is unconditional: even a turn with NO accumulated
	// activity refuses a non-nil empty feed (pass nil for "no
	// activity"; a fresh turn is already an empty window at append).
	row2, err := appendTurn(p, id, "run-2")
	if err != nil {
		t.Fatalf("append 2: %v", err)
	}
	if _, err := p.Update(context.Background(), id, "run-2", row2.Version, Update{Activity: []ActivityRow{}}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("non-nil empty feed on an empty turn error=%v, want ErrInvalidInput", err)
	}

	// The refused write never mutated the row: the exact totals survive
	// and the version is untouched.
	stored, err := p.Get(context.Background(), id, "run-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Activity.Totals != got.Activity.Totals {
		t.Errorf("refused empty feed erased the totals: %+v, want %+v", stored.Activity.Totals, got.Activity.Totals)
	}
	if stored.Version != got.Version {
		t.Errorf("refused empty feed bumped the version: %d, want %d", stored.Version, got.Version)
	}

	// nil leaves activity unchanged (the documented no-op channel).
	unchanged, err := p.Update(context.Background(), id, "run-1", stored.Version, Update{Status: StatusRunning})
	if err != nil {
		t.Fatalf("update status only: %v", err)
	}
	if unchanged.Activity.Totals != got.Activity.Totals {
		t.Errorf("nil activity feed changed the totals: %+v, want %+v", unchanged.Activity.Totals, got.Activity.Totals)
	}
}

// TestActivity_NoThirdRead_APIAbsent pins the P1 surface decision: the
// v1.28 Protocol surface is exactly `sessions.turns.list/get` — there
// is no third activity read. The ActivityReader / PageActivity /
// activity-cursor API is gone: the projector exposes no activity
// paging method and the Activity component carries no reader / cursor
// / page fields.
func TestActivity_NoThirdRead_APIAbsent(t *testing.T) {
	projType := reflect.TypeOf((*Projector)(nil))
	if _, ok := projType.MethodByName("PageActivity"); ok {
		t.Errorf("Projector.PageActivity must not exist — the v1.28 surface is exactly list/get, no third activity read")
	}
	if _, ok := projType.MethodByName("ActivityReader"); ok {
		t.Errorf("Projector.ActivityReader must not exist — no activity-read contract")
	}
	actType := reflect.TypeOf(Activity{})
	for _, forbidden := range []string{"Reader", "Cursor", "Page", "NextCursor"} {
		if _, ok := actType.FieldByName(forbidden); ok {
			t.Errorf("Activity.%s must never exist — no activity subresource surface", forbidden)
		}
	}
	// The mutation DTOs are structurally free of any activity-read
	// plumbing too.
	for _, tn := range []reflect.Type{
		reflect.TypeOf(Append{}), reflect.TypeOf(Update{}),
	} {
		for i := range tn.NumField() {
			if strings.Contains(tn.Field(i).Name, "Reader") || strings.Contains(tn.Field(i).Name, "Cursor") {
				t.Errorf("%s.%s must never exist — no third activity read", tn.Name(), tn.Field(i).Name)
			}
		}
	}
}

// newTestStoreOrFail builds a fresh in-memory test Store, failing the
// test on error.
func newTestStoreOrFail(t *testing.T) *testStore {
	t.Helper()
	st, err := newTestStore(0, false)
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}
	return st
}

func TestProjector_Checkpoint_MonotonicIdempotent(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	if seq, err := p.Checkpoint(context.Background(), id); err != nil || seq != 0 {
		t.Fatalf("fresh checkpoint=%d err=%v, want 0 nil", seq, err)
	}
	if err := p.AdvanceCheckpoint(context.Background(), id, 42); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if seq, _ := p.Checkpoint(context.Background(), id); seq != 42 {
		t.Errorf("checkpoint=%d, want 42", seq)
	}
	// Monotonic idempotent: a regress is a no-op, never an error.
	if err := p.AdvanceCheckpoint(context.Background(), id, 41); err != nil {
		t.Errorf("regress advance error=%v, want nil no-op", err)
	}
	if seq, _ := p.Checkpoint(context.Background(), id); seq != 42 {
		t.Errorf("checkpoint after regress=%d, want 42 (never regresses)", seq)
	}
	if err := p.AdvanceCheckpoint(context.Background(), id, 42); err != nil {
		t.Errorf("same-value advance error=%v, want nil no-op", err)
	}
	if err := p.AdvanceCheckpoint(context.Background(), id, 99); err != nil {
		t.Errorf("higher advance: %v", err)
	}
	if seq, _ := p.Checkpoint(context.Background(), id); seq != 99 {
		t.Errorf("checkpoint=%d, want 99", seq)
	}
}

func TestProjector_Reconcile_ResumesFromCheckpoint(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()

	// First pass: the runtime's apply closure replays the durable
	// observations from the checkpoint (0) to the high-water mark,
	// applying idempotent ops per event.
	apply := func(ctx context.Context, id identity.Identity, from uint64) (uint64, error) {
		row, err := appendTurn(p, id, "run-1")
		if err != nil {
			return 0, err
		}
		ans := Answer{State: AnswerStateInline, Inline: "reconciled answer"}
		if _, err := p.Update(ctx, id, "run-1", row.Version, Update{Answer: &ans}); err != nil {
			return 0, err
		}
		if err := sealComplete(p, id); err != nil {
			return 0, err
		}
		return 7, nil // the durable event log's high-water mark
	}
	high, err := p.Reconcile(context.Background(), id, apply)
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if high != 7 {
		t.Errorf("high=%d, want 7", high)
	}
	if seq, _ := p.Checkpoint(context.Background(), id); seq != 7 {
		t.Errorf("checkpoint after reconcile=%d, want 7", seq)
	}

	// A FAILED pass leaves the checkpoint untouched: a retry resumes
	// from the same point and re-applies idempotently.
	applyFail := func(ctx context.Context, id identity.Identity, from uint64) (uint64, error) {
		if from != 7 {
			t.Errorf("apply from=%d, want 7 (the stored checkpoint)", from)
		}
		return 0, errors.New("simulated replay failure")
	}
	if _, err := p.Reconcile(context.Background(), id, applyFail); err == nil {
		t.Fatalf("failed apply must surface loudly")
	}
	if seq, _ := p.Checkpoint(context.Background(), id); seq != 7 {
		t.Errorf("checkpoint after failed pass=%d, want 7 (unchanged)", seq)
	}

	// A replay that re-applies already-applied appends is a no-op:
	// appends are idempotent on the turn id; a mutation of an
	// already-sealed row (or a stale-version update) is "already
	// applied".
	replay := func(ctx context.Context, id identity.Identity, from uint64) (uint64, error) {
		if _, err := appendTurn(p, id, "run-1"); err != nil { // already exists: no-op
			return 0, err
		}
		if _, err := p.Get(ctx, id, "run-1"); err != nil {
			return 0, err
		}
		// Re-apply an update with a STALE version against the sealed
		// row — the replay treats it as already applied and skips
		// (the projector refuses with ErrTurnSealed: immutable wins
		// over the version check).
		if _, err := p.Update(ctx, id, "run-1", 1, Update{Answer: &Answer{State: AnswerStateInline, Inline: "stale"}}); !errors.Is(err, ErrTurnSealed) {
			return 0, err
		}
		// Same-status re-seal is an idempotent replay no-op.
		if err := sealComplete(p, id); err != nil {
			return 0, err
		}
		return 9, nil
	}
	high2, err := p.Reconcile(context.Background(), id, replay)
	if err != nil {
		t.Fatalf("Reconcile replay: %v", err)
	}
	if high2 != 9 {
		t.Errorf("high2=%d, want 9", high2)
	}
	row, err := p.Get(context.Background(), id, "run-1")
	if err != nil {
		t.Fatalf("get after replay: %v", err)
	}
	if row.Answer.Inline != "reconciled answer" {
		t.Errorf("stale re-apply overwrote the answer: %q", row.Answer.Inline)
	}
}

func TestProjector_InMemoryRestartLoss_Explicit(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	if p.Durable() {
		t.Errorf("in-memory test store must report Durable()==false")
	}
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := p.AdvanceCheckpoint(context.Background(), id, 5); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	// "Restart": the process loses the in-memory store wholesale.
	st2, err := newTestStore(0, false)
	if err != nil {
		t.Fatalf("restart store: %v", err)
	}
	p2, err := New(st2)
	if err != nil {
		t.Fatalf("restart projector: %v", err)
	}

	// Explicit loss: rows AND checkpoint are gone — never a silent
	// claim that anything survived.
	if _, err := p2.Get(context.Background(), id, row.TurnID); !errors.Is(err, ErrTurnNotFound) {
		t.Errorf("post-restart Get error=%v, want ErrTurnNotFound (explicit loss)", err)
	}
	if seq, _ := p2.Checkpoint(context.Background(), id); seq != 0 {
		t.Errorf("post-restart checkpoint=%d, want 0 (explicit loss)", seq)
	}
	// The runtime rebuilds from sequence zero via Reconcile — honest
	// rebuild against the durable event log, not silent retention.
	high, err := p2.Reconcile(context.Background(), id, func(ctx context.Context, id identity.Identity, from uint64) (uint64, error) {
		if from != 0 {
			t.Errorf("rebuild from=%d, want 0", from)
		}
		if _, err := appendTurn(p2, id, "run-1"); err != nil {
			return 0, err
		}
		return 3, nil
	})
	if err != nil {
		t.Fatalf("rebuild reconcile: %v", err)
	}
	if high != 3 {
		t.Errorf("rebuild high=%d, want 3", high)
	}
}

func TestProjector_Erase_RemovesRowsAndFencesLaterWrites(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	if _, err := appendTurn(p, id, "run-1"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := appendTurn(p, id, "run-2"); err != nil {
		t.Fatalf("append 2: %v", err)
	}
	if err := p.AdvanceCheckpoint(context.Background(), id, 3); err != nil {
		t.Fatalf("checkpoint: %v", err)
	}

	n, err := p.Erase(context.Background(), id)
	if err != nil {
		t.Fatalf("Erase: %v", err)
	}
	if n < 2 {
		t.Errorf("Erase deleted %d records, want >= 2 (rows + checkpoint + seq + retention)", n)
	}
	if _, err := p.Get(context.Background(), id, "run-1"); !errors.Is(err, ErrTurnNotFound) {
		t.Errorf("post-erase Get error=%v, want ErrTurnNotFound", err)
	}
	if seq, _ := p.Checkpoint(context.Background(), id); seq != 0 {
		t.Errorf("post-erase checkpoint=%d, want 0", seq)
	}
	// The STORE-LOCAL fence survives the erase: an erased session
	// admits no turn write and no checkpoint advance (no resurrection).
	if _, err := appendTurn(p, id, "run-3"); !errors.Is(err, ErrErasureFenced) {
		t.Errorf("post-erase append error=%v, want ErrErasureFenced", err)
	}
	if err := p.AdvanceCheckpoint(context.Background(), id, 4); !errors.Is(err, ErrErasureFenced) {
		t.Errorf("post-erase checkpoint advance error=%v, want ErrErasureFenced", err)
	}
	// Idempotent: re-erase on the fenced session stays a no-op.
	if _, err := p.Erase(context.Background(), id); err != nil {
		t.Errorf("re-erase: %v", err)
	}
}

func TestProjector_ErasureFence_RefusesWritesAfterErasureBegins(t *testing.T) {
	p, st := newTestProjector(t, 0, false)
	id := tripleA()
	if _, err := appendTurn(p, id, "run-1"); err != nil {
		t.Fatalf("append: %v", err)
	}

	// The erasure cascade sets the STORE-LOCAL fence FIRST (the
	// store-local durable session fence — never an external slot).
	if err := st.FenceSession(context.Background(), id); err != nil {
		t.Fatalf("FenceSession: %v", err)
	}

	if _, err := appendTurn(p, id, "run-2"); !errors.Is(err, ErrErasureFenced) {
		t.Errorf("append during erasure error=%v, want ErrErasureFenced", err)
	}
	row, err := p.Get(context.Background(), id, "run-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if _, err := p.Update(context.Background(), id, "run-1", row.Version, Update{}); !errors.Is(err, ErrErasureFenced) {
		t.Errorf("update during erasure error=%v, want ErrErasureFenced", err)
	}
	if _, err := p.Seal(context.Background(), id, "run-1", row.Version, Seal{Status: StatusCancelled}); !errors.Is(err, ErrErasureFenced) {
		t.Errorf("seal during erasure error=%v, want ErrErasureFenced", err)
	}
	if _, err := p.AttachReasoning(context.Background(), id, "run-1", row.Version, ReasoningInput{Steps: []ReasoningStep{{Index: 0, Kind: ReasoningKindToolCall}}}); !errors.Is(err, ErrErasureFenced) {
		t.Errorf("attach reasoning during erasure error=%v, want ErrErasureFenced", err)
	}
	if _, err := p.AttachAppRefs(context.Background(), id, "run-1", row.Version, AppRefInput{Refs: []AppRef{{ServerID: "s", ResourceURI: "ui://s/a"}}}); !errors.Is(err, ErrErasureFenced) {
		t.Errorf("attach app refs during erasure error=%v, want ErrErasureFenced", err)
	}
	if err := p.AdvanceCheckpoint(context.Background(), id, 5); !errors.Is(err, ErrErasureFenced) {
		t.Errorf("checkpoint advance during erasure error=%v, want ErrErasureFenced", err)
	}
}

// TestProjector_Erase_NoResurrectionAfterReplay proves the erased
// session stays fenced across a REPLAY: Reconcile's apply closure
// cannot re-create rows on an erased session (its writes fail with
// ErrErasureFenced), so the projection cannot be resurrected.
func TestProjector_Erase_NoResurrectionAfterReplay(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	if _, err := appendTurn(p, id, "run-1"); err != nil {
		t.Fatalf("append: %v", err)
	}
	if _, err := p.Erase(context.Background(), id); err != nil {
		t.Fatalf("Erase: %v", err)
	}
	// A replay of the erased session's observations: the append fails
	// with ErrErasureFenced — the fence is never removed by the erase —
	// and even a replay that reports a high-water mark cannot advance
	// the erased session's checkpoint (SaveCheckpoint refuses fenced
	// sessions). The replay surfaces loudly; nothing is resurrected.
	_, err := p.Reconcile(context.Background(), id, func(ctx context.Context, id identity.Identity, from uint64) (uint64, error) {
		if _, err := appendTurn(p, id, "run-1"); !errors.Is(err, ErrErasureFenced) {
			return 0, fmt.Errorf("replay append error=%w, want ErrErasureFenced (no resurrection)", err)
		}
		return 5, nil
	})
	if !errors.Is(err, ErrErasureFenced) {
		t.Fatalf("Reconcile replay error=%v, want ErrErasureFenced (checkpoint advance refused on erased session)", err)
	}
	if _, err := p.Get(context.Background(), id, "run-1"); !errors.Is(err, ErrTurnNotFound) {
		t.Errorf("post-replay get error=%v, want ErrTurnNotFound (no resurrection)", err)
	}
	// And the checkpoint stayed put: the replay's high water never
	// landed (SaveCheckpoint refuses fenced sessions).
	if seq, _ := p.Checkpoint(context.Background(), id); seq != 0 {
		t.Errorf("post-replay checkpoint=%d, want 0", seq)
	}
}

func TestProjector_CrossSession_Isolation(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	if _, err := appendTurn(p, tripleA(), "run-1"); err != nil {
		t.Fatalf("append A: %v", err)
	}
	if _, err := appendTurn(p, tripleB(), "run-1"); err != nil {
		t.Fatalf("append B (same turn id, different session): %v", err)
	}
	// Same turn id under a different session is a DIFFERENT turn.
	page, err := p.List(context.Background(), tripleA(), ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("list A: %v", err)
	}
	if len(page.Rows) != 1 {
		t.Errorf("session A lists %d rows, want 1 (no cross-session bleed)", len(page.Rows))
	}
	pageB, err := p.List(context.Background(), tripleB(), ListOptions{Limit: 10})
	if err != nil {
		t.Fatalf("list B: %v", err)
	}
	if len(pageB.Rows) != 1 {
		t.Errorf("session B lists %d rows, want 1", len(pageB.Rows))
	}
}

func TestProjector_ContextCancellation_Honored(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := p.Append(ctx, id, Append{TurnID: "r", Query: "q"}); err == nil {
		t.Errorf("cancelled append must fail")
	}
}

func TestProjector_Get_UnknownTurnNotFound(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	if _, err := p.Get(context.Background(), tripleA(), "no-such"); !errors.Is(err, ErrTurnNotFound) {
		t.Errorf("unknown get error=%v, want ErrTurnNotFound", err)
	}
}

func TestProjector_List_TruncatedFlag_AfterRetentionEviction(t *testing.T) {
	p, _ := newTestProjector(t, testStoreRetentionTiny, false)
	id := tripleA()
	for i := range testStoreRetentionTiny + 3 {
		if _, err := appendTurn(p, id, TurnID(strings.Repeat("r", 1)+string(rune('a'+i)))); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	page, err := p.List(context.Background(), id, ListOptions{Limit: MaxListLimit})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if page.Complete {
		t.Errorf("Complete=true after retention eviction, want the explicit partial marker")
	}
	if page.PartialReason != "retention_eviction" {
		t.Errorf("PartialReason=%q, want %q (the Protocol-mappable stable token)", page.PartialReason, "retention_eviction")
	}
	if len(page.Rows) != testStoreRetentionTiny {
		t.Errorf("retained %d rows, want %d (the retention bound)", len(page.Rows), testStoreRetentionTiny)
	}
	// The evicted (oldest) turn is honestly not-found — the projection
	// does not retain it (the durable event log does).
	if _, err := p.Get(context.Background(), id, TurnID("r"+string(rune('a')))); !errors.Is(err, ErrTurnNotFound) {
		t.Errorf("evicted turn error=%v, want ErrTurnNotFound", err)
	}
}

func TestProjector_Pause_DataNoTokens(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	// A fresh row honestly reports no pause episode.
	if row.Pause.Availability != CompletenessUnavailable {
		t.Errorf("fresh pause=%+v, want Unavailable", row.Pause)
	}

	// Pausing the run carries the durable pause component: class /
	// reason / lifecycle / availability — never a token.
	pause := Pause{Class: PauseClassHitlApproval, Reason: "approval required", Lifecycle: PauseLifecycleActive}
	got, err := p.Update(context.Background(), id, "run-1", row.Version, Update{Status: StatusPaused, Pause: &pause})
	if err != nil {
		t.Fatalf("update paused: %v", err)
	}
	if got.Status != StatusPaused {
		t.Errorf("Status=%q, want paused", got.Status)
	}
	if got.Pause.Class != PauseClassHitlApproval || got.Pause.Reason != "approval required" || got.Pause.Lifecycle != PauseLifecycleActive {
		t.Errorf("pause component wrong: %+v", got.Pause)
	}
	if got.Pause.Availability != CompletenessComplete {
		t.Errorf("pause availability=%q, want complete", got.Pause.Availability)
	}
	// The Pause type structurally cannot carry a token — the
	// ops_safety allowlist pin enforces the field set; here we also
	// assert the honest absence of any token-ish field via reflection.
	pt := reflect.TypeOf(Pause{})
	for i := range pt.NumField() {
		if strings.Contains(strings.ToLower(pt.Field(i).Name), "token") {
			t.Errorf("Pause.%s must never exist — pause/resume/approval tokens are not stored", pt.Field(i).Name)
		}
	}

	// An unavailable pause cannot carry class/reason/lifecycle.
	bad := Pause{Class: PauseClassOAuth, Availability: CompletenessUnavailable}
	if _, err := p.Update(context.Background(), id, "run-1", got.Version, Update{Pause: &bad}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("unavailable pause with class error=%v, want ErrInvalidInput", err)
	}
	// An unknown pause class fails loud.
	badClass := Pause{Class: PauseClass("bogus")}
	if _, err := p.Update(context.Background(), id, "run-1", got.Version, Update{Pause: &badClass}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bogus pause class error=%v, want ErrInvalidInput", err)
	}
	// An over-bound reason fails loud.
	longReason := Pause{Class: PauseClassSteering, Reason: strings.Repeat("r", MaxPauseReasonRunes+1)}
	if _, err := p.Update(context.Background(), id, "run-1", got.Version, Update{Pause: &longReason}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("over-bound pause reason error=%v, want ErrInvalidInput", err)
	}
	// Resuming clears the episode honestly (Unavailable).
	resumed, err := p.Update(context.Background(), id, "run-1", got.Version, Update{Status: StatusRunning, Pause: &Pause{Availability: CompletenessUnavailable}})
	if err != nil {
		t.Fatalf("update resumed: %v", err)
	}
	if resumed.Pause.Availability != CompletenessUnavailable || resumed.Pause.Class != "" {
		t.Errorf("resumed pause not cleared honestly: %+v", resumed.Pause)
	}
}

func TestProjector_AnswerStates_ClosedUnion(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	v := row.Version

	// The closed union accepts exactly the five states, each with its
	// declared content shape, and the projector derives the uniform
	// Completeness.
	cases := []struct {
		name  string
		state AnswerState
		build func() Answer
		want  Completeness
	}{
		{"inline", AnswerStateInline, func() Answer { return Answer{State: AnswerStateInline, Inline: "hi"} }, CompletenessComplete},
		{"empty inline is legitimate", AnswerStateInline, func() Answer { return Answer{State: AnswerStateInline} }, CompletenessComplete},
		{"artifact_ref", AnswerStateArtifactRef, func() Answer {
			return Answer{State: AnswerStateArtifactRef, Ref: &AnswerRef{ID: "art_1", SizeBytes: 8}}
		}, CompletenessComplete},
		{"empty", AnswerStateEmpty, func() Answer { return Answer{State: AnswerStateEmpty} }, CompletenessComplete},
		{"evicted", AnswerStateEvicted, func() Answer { return Answer{State: AnswerStateEvicted} }, CompletenessUnavailable},
		{"unavailable", AnswerStateUnavailable, func() Answer { return Answer{State: AnswerStateUnavailable} }, CompletenessUnavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := p.Update(context.Background(), id, "run-1", v, Update{Answer: ansPtr(tc.build())})
			if err != nil {
				t.Fatalf("update answer: %v", err)
			}
			if got.Answer.State != tc.state {
				t.Errorf("State=%q, want %q", got.Answer.State, tc.state)
			}
			if got.Answer.Complete != tc.want {
				t.Errorf("Complete=%q, want %q (derived from state)", got.Answer.Complete, tc.want)
			}
			v = got.Version
		})
	}

	// Invalid state fails loud.
	if _, err := p.Update(context.Background(), id, "run-1", v, Update{Answer: ansPtr(Answer{State: AnswerState("bogus")})}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bogus answer state error=%v, want ErrInvalidInput", err)
	}
	// Inconsistent content fails loud: inline with a ref.
	both := Answer{State: AnswerStateInline, Inline: "x", Ref: &AnswerRef{ID: "a"}}
	if _, err := p.Update(context.Background(), id, "run-1", v, Update{Answer: &both}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("inline+ref error=%v, want ErrInvalidInput", err)
	}
	// artifact_ref without a ref fails loud.
	noRef := Answer{State: AnswerStateArtifactRef}
	if _, err := p.Update(context.Background(), id, "run-1", v, Update{Answer: &noRef}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("artifact_ref without ref error=%v, want ErrInvalidInput", err)
	}
	// A failed read NEVER becomes empty: an evicted answer with content
	// is refused (it must carry none), and a caller cannot mask a read
	// failure as a definite empty.
	evictedWithContent := Answer{State: AnswerStateEvicted, Inline: "x"}
	if _, err := p.Update(context.Background(), id, "run-1", v, Update{Answer: &evictedWithContent}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("evicted with content error=%v, want ErrInvalidInput (failed reads never become empty)", err)
	}
	// A caller-supplied Complete inconsistent with State fails loud.
	lying := Answer{State: AnswerStateUnavailable, Complete: CompletenessComplete}
	if _, err := p.Update(context.Background(), id, "run-1", v, Update{Answer: &lying}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("inconsistent completeness error=%v, want ErrInvalidInput", err)
	}

	// An evicted / unavailable answer does NOT satisfy a complete seal
	// (only inline / artifact_ref / empty do).
	got, err := p.Update(context.Background(), id, "run-1", v, Update{Answer: ansPtr(Answer{State: AnswerStateInline, Inline: "final"})})
	if err != nil {
		t.Fatalf("update final answer: %v", err)
	}
	evicted := Answer{State: AnswerStateEvicted}
	if _, err := p.Update(context.Background(), id, "run-1", got.Version, Update{Answer: &evicted}); err != nil {
		t.Fatalf("update evicted: %v", err)
	}
	if _, err := p.Seal(context.Background(), id, "run-1", got.Version+1, Seal{Status: StatusComplete}); !errors.Is(err, ErrSealIncomplete) {
		t.Errorf("complete seal with evicted answer error=%v, want ErrSealIncomplete", err)
	}
}

func ansPtr(a Answer) *Answer { return &a }

// usageExact builds an exact usage measure with the given value.
func usageExact(v int64) UsageMeasure { return UsageMeasure{State: UsageExact, Value: &v} }

// usageInt64 returns a pointer to v for building UsageMeasure values.
func usageInt64(v int64) *int64 { return &v }

// usageMeasures returns the numeric measures of a Usage keyed by name
// for uniform per-measure assertions.
func usageMeasures(u Usage) map[string]UsageMeasure {
	return map[string]UsageMeasure{
		"PromptTokens":     u.PromptTokens,
		"CompletionTokens": u.CompletionTokens,
		"ReasoningTokens":  u.ReasoningTokens,
		"CacheReadTokens":  u.CacheReadTokens,
		"CacheWriteTokens": u.CacheWriteTokens,
		"TotalTokens":      u.TotalTokens,
		"CostMicroUSD":     u.CostMicroUSD,
		"LatencyNS":        u.LatencyNS,
	}
}

func TestProjector_HonestFields_TimestampBindingAttachmentsEventSeq(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	queryAt := testClockStart.Add(-5 * time.Minute)

	// Append carries the query/input timestamp, the agent binding
	// source, attachment reference availability, and the event
	// sequence.
	row, err := p.Append(context.Background(), id, Append{
		TurnID:             "run-1",
		Query:              "weather?",
		QueryAt:            queryAt,
		AgentID:            "agent-1",
		AgentName:          "Agent One",
		AgentBindingSource: AgentBindingDefaulted,
		Inputs:             []Attachment{{ID: "in_1", Filename: "f.txt"}},
		Outputs:            []Attachment{{ID: "out_1", Availability: CompletenessComplete}},
		EventSeq:           11,
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if !row.Query.At.Equal(queryAt) {
		t.Errorf("Query.At=%v, want %v", row.Query.At, queryAt)
	}
	if row.Agent.BindingSource != AgentBindingDefaulted {
		t.Errorf("BindingSource=%q, want defaulted", row.Agent.BindingSource)
	}
	if len(row.Inputs) != 1 || row.Inputs[0].Availability != CompletenessUnavailable {
		t.Errorf("unreported attachment availability must normalize to Unavailable: %+v", row.Inputs)
	}
	if len(row.Outputs) != 1 || row.Outputs[0].Availability != CompletenessComplete {
		t.Errorf("attachment availability lost: %+v", row.Outputs)
	}
	if row.LastAppliedEventSeq != 11 {
		t.Errorf("LastAppliedEventSeq=%d, want 11", row.LastAppliedEventSeq)
	}

	// An update with an event sequence stamps the row AND the replaced
	// accumulated answer snapshot (component/version consistency).
	ans := Answer{State: AnswerStateInline, Inline: "sunny"}
	got, err := p.Update(context.Background(), id, "run-1", row.Version, Update{Answer: &ans, EventSeq: 12})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.LastAppliedEventSeq != 12 {
		t.Errorf("LastAppliedEventSeq=%d, want 12", got.LastAppliedEventSeq)
	}
	if got.Answer.Seq != 12 {
		t.Errorf("Answer.Seq=%d, want 12 (accumulated snapshot consistency)", got.Answer.Seq)
	}
	if got.Answer.Complete != CompletenessComplete {
		t.Errorf("Answer.Complete=%q, want complete (derived)", got.Answer.Complete)
	}

	// AttachReasoning stamps the reasoning snapshot's event sequence.
	got2, err := p.AttachReasoning(context.Background(), id, "run-1", got.Version, ReasoningInput{
		Steps:    []ReasoningStep{{Index: 0, Kind: ReasoningKindToolCall}},
		EventSeq: 13,
	})
	if err != nil {
		t.Fatalf("AttachReasoning: %v", err)
	}
	if got2.Reasoning.Seq != 13 {
		t.Errorf("Reasoning.Seq=%d, want 13", got2.Reasoning.Seq)
	}
	if got2.LastAppliedEventSeq != 13 {
		t.Errorf("LastAppliedEventSeq=%d, want 13", got2.LastAppliedEventSeq)
	}

	// A seal without an event sequence leaves the row's seq untouched.
	got3, err := p.Seal(context.Background(), id, "run-1", got2.Version, Seal{Status: StatusComplete})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if got3.LastAppliedEventSeq != 13 {
		t.Errorf("seal without EventSeq rewrote LastAppliedEventSeq: %d, want 13", got3.LastAppliedEventSeq)
	}
}

func TestProjector_AgentBindingSource_Derivation(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	// A non-empty agent id with no reported source derives explicit.
	row, err := p.Append(context.Background(), id, Append{TurnID: "run-1", Query: "q", AgentID: "agent-1"})
	if err != nil {
		t.Fatalf("append explicit: %v", err)
	}
	if row.Agent.BindingSource != AgentBindingExplicit {
		t.Errorf("BindingSource=%q, want explicit (derived from agent id)", row.Agent.BindingSource)
	}
	// An empty agent id with no reported source derives unknown (never
	// a fabricated defaulted claim).
	row2, err := p.Append(context.Background(), id, Append{TurnID: "run-2", Query: "q"})
	if err != nil {
		t.Fatalf("append unknown: %v", err)
	}
	if row2.Agent.BindingSource != AgentBindingUnknown {
		t.Errorf("BindingSource=%q, want unknown", row2.Agent.BindingSource)
	}
	// A bogus source fails loud.
	if _, err := p.Append(context.Background(), id, Append{TurnID: "run-3", Query: "q", AgentBindingSource: AgentBindingSource("bogus")}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bogus binding source error=%v, want ErrInvalidInput", err)
	}
}

func TestProjector_ActivityBudget_ConfigRequiresCoverage(t *testing.T) {
	st, err := newTestStore(0, false)
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}
	// The inline activity limit MUST cover the configured per-turn
	// tool-call budget: a budget above the limit fails loud at
	// construction.
	if _, err := New(st, WithToolBudget(DefaultActivityLimit+8)); err == nil {
		t.Errorf("budget %d above default limit %d must fail loud", DefaultActivityLimit+8, DefaultActivityLimit)
	}
	// The limit is capped at the absolute Protocol ceiling.
	if _, err := New(st, WithActivityLimit(MaxActivityRows+1)); err == nil {
		t.Errorf("activity limit above the Protocol ceiling %d must fail loud", MaxActivityRows)
	}
	if _, err := New(st, WithActivityLimit(0)); err == nil {
		t.Errorf("zero activity limit must fail loud")
	}
	// An explicit matching configuration is accepted.
	p, err := New(st, WithToolBudget(24), WithActivityLimit(24))
	if err != nil {
		t.Fatalf("New with budget 24 / limit 24: %v", err)
	}
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	// A feed within the configured budget is fully covered (no
	// lower-bound marker).
	covered := make([]ActivityRow, 24)
	for i := range covered {
		covered[i] = ActivityRow{Tool: "t", Status: ActivitySucceeded}
	}
	got, err := p.Update(context.Background(), id, "run-1", row.Version, Update{Activity: covered})
	if err != nil {
		t.Fatalf("update activity: %v", err)
	}
	if got.Activity.Complete != CompletenessComplete || got.Activity.More || got.Activity.Dropped != 0 {
		t.Errorf("in-budget activity must be fully covered: %+v", got.Activity)
	}
	// A feed beyond the configured budget overflows honestly (More +
	// Dropped + Partial) and the exact totals cover the full feed.
	over := make([]ActivityRow, 24+5)
	for i := range over {
		over[i] = ActivityRow{Tool: "t", Status: ActivitySucceeded}
	}
	got2, err := p.Update(context.Background(), id, "run-1", got.Version, Update{Activity: over})
	if err != nil {
		t.Fatalf("update over-budget activity: %v", err)
	}
	if !got2.Activity.More || got2.Activity.Dropped != 5 || got2.Activity.Complete != CompletenessPartial {
		t.Errorf("over-budget activity must overflow honestly: %+v", got2.Activity)
	}
	if len(got2.Activity.Rows) != 24 {
		t.Errorf("retained %d rows, want 24 (the configured window)", len(got2.Activity.Rows))
	}
	if got2.Activity.Totals.Succeeded != int64(len(over)) {
		t.Errorf("over-budget totals must cover the full feed: %+v", got2.Activity.Totals)
	}
}

func TestProjector_OpsTurn_StructuralOmissions(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := p.Append(context.Background(), id, Append{
		TurnID:   "run-1",
		Query:    "secret query",
		AgentID:  "agent-1",
		Inputs:   []Attachment{{ID: "in_1"}},
		Outputs:  []Attachment{{ID: "out_1"}},
		EventSeq: 3,
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	ans := Answer{State: AnswerStateInline, Inline: "secret answer"}
	row, err = p.Update(context.Background(), id, "run-1", row.Version, Update{
		Answer:   &ans,
		Usage:    &Usage{PromptTokens: usageExact(10), TotalTokens: usageExact(10), Model: "m"},
		EventSeq: 4,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := p.AttachReasoning(context.Background(), id, "run-1", row.Version, ReasoningInput{
		Steps:    []ReasoningStep{{Index: 0, Kind: ReasoningKindToolCall}},
		EventSeq: 5,
	}); err != nil {
		t.Fatalf("AttachReasoning: %v", err)
	}
	appRow, err := p.AttachAppRefs(context.Background(), id, "run-1", row.Version+1, AppRefInput{
		Refs:     []AppRef{{EffectiveAgentID: "agent-1", ServerID: "srv", ResourceURI: "ui://srv/app", ToolName: "tool", ToolCallID: "tc-1", Availability: AppAvailable}},
		EventSeq: 6,
	})
	if err != nil {
		t.Fatalf("AttachAppRefs: %v", err)
	}

	ops, err := p.OpsTurn(context.Background(), id, "run-1")
	if err != nil {
		t.Fatalf("OpsTurn: %v", err)
	}
	// The operations READ projection retains lifecycle / agent /
	// timing / usage / cost / tool-name / status / counts.
	if ops.TurnID != "run-1" || ops.Status != StatusRunning || ops.Version != appRow.Version {
		t.Errorf("ops lifecycle wrong: %+v", ops)
	}
	// The operations READ carries the distinct task/run identity too
	// (TaskID derived from the row key here; RunID honestly empty).
	if ops.TaskID != "run-1" {
		t.Errorf("ops TaskID=%q, want run-1 (derived)", ops.TaskID)
	}
	if ops.RunID != "" {
		t.Errorf("ops RunID=%q, want empty (unavailable — never equated with TaskID)", ops.RunID)
	}
	if ops.AgentID != "agent-1" || ops.AgentBindingSource != AgentBindingExplicit {
		t.Errorf("ops agent binding wrong: %+v", ops)
	}
	if ops.Usage.TotalTokens.State != UsageExact || ops.Usage.TotalTokens.Value == nil || *ops.Usage.TotalTokens.Value != 10 {
		t.Errorf("ops usage lost: %+v", ops.Usage)
	}
	if ops.Usage.PromptTokens.State != UsageExact || ops.Usage.PromptTokens.Value == nil || *ops.Usage.PromptTokens.Value != 10 {
		t.Errorf("ops usage prompt lost: %+v", ops.Usage)
	}
	// Per-measure honesty flows into the operations read: measures the
	// runtime never reported stay unavailable with NO value.
	if ops.Usage.CostMicroUSD.State != UsageUnavailable || ops.Usage.CostMicroUSD.Value != nil {
		t.Errorf("ops usage cost must be unavailable-without-value: %+v", ops.Usage.CostMicroUSD)
	}
	if ops.ReasoningSteps != 1 {
		t.Errorf("ops reasoning count=%d, want 1", ops.ReasoningSteps)
	}
	if ops.Inputs != 1 || ops.Outputs != 1 {
		t.Errorf("ops attachment counts wrong: %d/%d", ops.Inputs, ops.Outputs)
	}
	if len(ops.Apps) != 1 || ops.Apps[0].ServerID != "srv" || ops.Apps[0].ToolName != "tool" || ops.Apps[0].Availability != AppAvailable {
		t.Errorf("ops app summaries wrong: %+v", ops.Apps)
	}
	if ops.LastAppliedEventSeq != 6 {
		t.Errorf("ops LastAppliedEventSeq=%d, want 6", ops.LastAppliedEventSeq)
	}

	// Structurally absent from the operations READ row: query, answer,
	// raw provider thinking (reasoning summaries), resource URI,
	// tool_call_id, App context. The
	// types have no such fields (asserted reflectively — a compile-time
	// guarantee enforced by the ops_safety allowlist pin + scan).
	opsType := reflect.TypeOf(OpsTurnRow{})
	for _, forbidden := range []string{"Query", "Answer", "Reasoning", "ResourceURI", "ToolCallID", "Inline", "Ref"} {
		if _, ok := opsType.FieldByName(forbidden); ok {
			t.Errorf("OpsTurnRow.%s must never exist — consumer-only content is omitted from the operations read projection", forbidden)
		}
	}
	appOpsType := reflect.TypeOf(AppOpsRef{})
	for _, forbidden := range []string{"ResourceURI", "ToolCallID", "DisplayMode", "RawHTMLTrusted"} {
		if _, ok := appOpsType.FieldByName(forbidden); ok {
			t.Errorf("AppOpsRef.%s must never exist — the operations App summary omits it", forbidden)
		}
	}
}

func TestProjector_EventSeq_ZeroNeverErasesRecorded(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := p.Append(context.Background(), id, Append{TurnID: "run-1", Query: "q", EventSeq: 5})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if row.LastAppliedEventSeq != 5 {
		t.Fatalf("LastAppliedEventSeq=%d, want 5", row.LastAppliedEventSeq)
	}
	// An update without an event sequence leaves it untouched.
	got, err := p.Update(context.Background(), id, "run-1", row.Version, Update{})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if got.LastAppliedEventSeq != 5 {
		t.Errorf("zero EventSeq erased the recorded sequence: %d, want 5", got.LastAppliedEventSeq)
	}
}

// TestProjector_TaskRunID_DistinctAndHonest pins the P1 contract: the
// consumer row and the operations READ carry DISTINCT authoritative
// TaskID and RunID fields; the row key (TurnID) may be TaskID-derived,
// but the ACTUAL runtime run id is never erased or equated with the
// task id, and a legacy missing run id is EXPLICIT unavailable.
func TestProjector_TaskRunID_DistinctAndHonest(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()

	// Runtime reports the authoritative task id AND the run id.
	row, err := p.Append(context.Background(), id, Append{
		TurnID: "task-001", TaskID: "task-001", RunID: "run-abc-123", Query: "q",
	})
	if err != nil {
		t.Fatalf("Append with ids: %v", err)
	}
	if row.TurnID != "task-001" || row.TaskID != "task-001" || row.RunID != "run-abc-123" {
		t.Errorf("row ids wrong: turn=%q task=%q run=%q", row.TurnID, row.TaskID, row.RunID)
	}
	// The operations READ projection carries both too (the operations
	// surface needs them for correlation).
	ops, err := p.OpsTurn(context.Background(), id, "task-001")
	if err != nil {
		t.Fatalf("OpsTurn: %v", err)
	}
	if ops.TaskID != "task-001" || ops.RunID != "run-abc-123" {
		t.Errorf("ops ids wrong: task=%q run=%q", ops.TaskID, ops.RunID)
	}

	// A runtime that reports ONLY the row key: TaskID derives from
	// TurnID, and the MISSING run id stays explicitly unavailable —
	// never silently equated with the task id.
	legacy, err := p.Append(context.Background(), id, Append{TurnID: "task-002", Query: "q"})
	if err != nil {
		t.Fatalf("Append legacy: %v", err)
	}
	if legacy.TaskID != "task-002" {
		t.Errorf("TaskID=%q, want task-002 (derived from the row key)", legacy.TaskID)
	}
	if legacy.RunID != "" {
		t.Errorf("RunID=%q, want empty — a legacy run id is EXPLICIT unavailable, never equated with the task id", legacy.RunID)
	}
	if legacy.RunID == legacy.TaskID {
		t.Errorf("RunID must never be silently equated with TaskID")
	}

	// A runtime that reports a DIFFERENT task id from the row key:
	// both are preserved — the row key may be TaskID-derived, and the
	// authoritative task id is never erased.
	split, err := p.Append(context.Background(), id, Append{TurnID: "row-key-3", TaskID: "authoritative-task-3", RunID: "run-xyz", Query: "q"})
	if err != nil {
		t.Fatalf("Append split: %v", err)
	}
	if split.TurnID != "row-key-3" || split.TaskID != "authoritative-task-3" || split.RunID != "run-xyz" {
		t.Errorf("split ids wrong: turn=%q task=%q run=%q", split.TurnID, split.TaskID, split.RunID)
	}

	// A reserved cursor separator in the turn id is rejected at
	// creation — it would break the row's own opaque cursor encoding.
	if _, err := p.Append(context.Background(), id, Append{TurnID: "bad|pipe", Query: "q"}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("pipe turn id error=%v, want ErrInvalidInput", err)
	}
}

// TestProjector_EventSequence_MonotonicIdempotent_NoOpAtOrBelow pins
// the P2 contract: applying an observation at or below the row's
// last-applied event sequence is a NO-OP — no version bump, no content
// mutation, and NO lucky expected version required (a response-loss
// replay just returns the current row). Lower / equal / out-of-order
// cases are pinned.
func TestProjector_EventSequence_MonotonicIdempotent_NoOpAtOrBelow(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := p.Append(context.Background(), id, Append{TurnID: "run-1", Query: "q", EventSeq: 10})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if row.LastAppliedEventSeq != 10 {
		t.Fatalf("LastAppliedEventSeq=%d, want 10", row.LastAppliedEventSeq)
	}

	// Apply an update at seq 12.
	ans := Answer{State: AnswerStateInline, Inline: "v1"}
	row, err = p.Update(context.Background(), id, "run-1", row.Version, Update{Answer: &ans, EventSeq: 12})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if row.LastAppliedEventSeq != 12 || row.Answer.Seq != 12 {
		t.Fatalf("post-update seqs wrong: row=%d answer=%d, want 12/12", row.LastAppliedEventSeq, row.Answer.Seq)
	}

	// REPLAY of the SAME observation (seq 12, equal): no-op — the row
	// is returned unchanged with a DELIBERATELY WRONG expected version,
	// proving a response-loss replay never needs a lucky version.
	replayed, err := p.Update(context.Background(), id, "run-1", 999, Update{Answer: &ans, EventSeq: 12})
	if err != nil {
		t.Fatalf("replay update: %v", err)
	}
	if replayed.Version != row.Version || replayed.Answer.Inline != "v1" || replayed.LastAppliedEventSeq != 12 {
		t.Errorf("equal-seq replay changed the row: %+v", replayed)
	}

	// OUT-OF-ORDER observation (seq 11, below the applied 12): no-op.
	lower := Answer{State: AnswerStateInline, Inline: "MUST NOT APPLY"}
	oops, err := p.Update(context.Background(), id, "run-1", row.Version, Update{Answer: &lower, EventSeq: 11})
	if err != nil {
		t.Fatalf("lower-seq update: %v", err)
	}
	if oops.Version != row.Version || oops.Answer.Inline != "v1" || oops.LastAppliedEventSeq != 12 {
		t.Errorf("lower-seq observation mutated the row: %+v", oops)
	}

	// A REAL new observation (seq 13) applies normally.
	row, err = p.Update(context.Background(), id, "run-1", oops.Version, Update{Answer: &ans, EventSeq: 13})
	if err != nil {
		t.Fatalf("new-seq update: %v", err)
	}
	if row.LastAppliedEventSeq != 13 {
		t.Errorf("LastAppliedEventSeq=%d, want 13", row.LastAppliedEventSeq)
	}

	// The same monotonic guard applies to the SEPARATELY NAMED
	// channels and Seal.
	row, err = p.AttachReasoning(context.Background(), id, "run-1", row.Version, ReasoningInput{Steps: []ReasoningStep{{Index: 0, Kind: ReasoningKindToolCall}}, EventSeq: 14})
	if err != nil {
		t.Fatalf("AttachReasoning: %v", err)
	}
	if row.LastAppliedEventSeq != 14 || row.Reasoning.Seq != 14 {
		t.Fatalf("post-reasoning seqs wrong: row=%d reasoning=%d, want 14/14", row.LastAppliedEventSeq, row.Reasoning.Seq)
	}
	replayedReasoning, err := p.AttachReasoning(context.Background(), id, "run-1", 12345, ReasoningInput{Steps: []ReasoningStep{{Index: 9, Kind: ReasoningKindSpawn}}, EventSeq: 14})
	if err != nil {
		t.Fatalf("replay reasoning: %v", err)
	}
	if replayedReasoning.Version != row.Version || len(replayedReasoning.Reasoning.Steps) != 1 || replayedReasoning.Reasoning.Steps[0].Kind != ReasoningKindToolCall {
		t.Errorf("equal-seq reasoning replay changed the row: %+v", replayedReasoning.Reasoning)
	}

	row, err = p.AttachAppRefs(context.Background(), id, "run-1", row.Version, AppRefInput{Refs: []AppRef{{ServerID: "srv", ResourceURI: "ui://srv/app"}}, EventSeq: 15})
	if err != nil {
		t.Fatalf("AttachAppRefs: %v", err)
	}
	if row.LastAppliedEventSeq != 15 {
		t.Fatalf("post-apprefs seq=%d, want 15", row.LastAppliedEventSeq)
	}
	replayedApps, err := p.AttachAppRefs(context.Background(), id, "run-1", 9999, AppRefInput{Refs: []AppRef{{ServerID: "other", ResourceURI: "ui://other/app"}}, EventSeq: 15})
	if err != nil {
		t.Fatalf("replay apprefs: %v", err)
	}
	if replayedApps.Version != row.Version || len(replayedApps.Apps) != 1 || replayedApps.Apps[0].ServerID != "srv" {
		t.Errorf("equal-seq apprefs replay changed the row: %+v", replayedApps.Apps)
	}

	// Seal: apply at seq 16, then replay at 16 (equal) — no-op.
	sealed, err := p.Seal(context.Background(), id, "run-1", row.Version, Seal{Status: StatusComplete, EventSeq: 16})
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if sealed.LastAppliedEventSeq != 16 {
		t.Fatalf("post-seal seq=%d, want 16", sealed.LastAppliedEventSeq)
	}
	resealed, err := p.Seal(context.Background(), id, "run-1", 777, Seal{Status: StatusComplete, EventSeq: 16})
	if err != nil {
		t.Fatalf("re-seal: %v", err)
	}
	if resealed.Version != sealed.Version || resealed.Status != StatusComplete {
		t.Errorf("equal-seq re-seal changed the sealed row: %+v", resealed)
	}
}

// TestProjector_EventSequence_ComponentSeqNeverRegresses pins the P2
// component-level monotonicity: Answer.Seq and Reasoning.Seq never
// regress, even when an observation carries a lower or absent
// sequence, and LastAppliedEventSeq is never rewound by a later
// lower-seq observation.
func TestProjector_EventSequence_ComponentSeqNeverRegresses(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := p.Append(context.Background(), id, Append{TurnID: "run-1", Query: "q"})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}

	// A caller-supplied component Seq (no row-level EventSeq) anchors
	// the answer snapshot at 50.
	ans := Answer{State: AnswerStateInline, Inline: "v1", Seq: 50}
	row, err = p.Update(context.Background(), id, "run-1", row.Version, Update{Answer: &ans})
	if err != nil {
		t.Fatalf("Update with component seq: %v", err)
	}
	if row.Answer.Seq != 50 {
		t.Fatalf("Answer.Seq=%d, want 50", row.Answer.Seq)
	}

	// A later update with EventSeq 12 (> row's 0) but BELOW the
	// component anchor 50: the whole-op applies (12 > 0) but the
	// component Seq must NOT regress to 12.
	ans2 := Answer{State: AnswerStateInline, Inline: "v2"}
	row, err = p.Update(context.Background(), id, "run-1", row.Version, Update{Answer: &ans2, EventSeq: 12})
	if err != nil {
		t.Fatalf("Update lower event seq: %v", err)
	}
	if row.LastAppliedEventSeq != 12 {
		t.Errorf("LastAppliedEventSeq=%d, want 12", row.LastAppliedEventSeq)
	}
	if row.Answer.Seq != 50 {
		t.Errorf("Answer.Seq regressed to %d, want 50 (never regress)", row.Answer.Seq)
	}

	// An update with NO sequence info at all preserves the anchor.
	row, err = p.Update(context.Background(), id, "run-1", row.Version, Update{Answer: &ans2})
	if err != nil {
		t.Fatalf("Update no seq: %v", err)
	}
	if row.Answer.Seq != 50 {
		t.Errorf("Answer.Seq=%d after unsequenced update, want 50 (preserved)", row.Answer.Seq)
	}

	// Reasoning.Seq monotonic: attach at 20, then a lower-seq attach
	// (10) must not regress it.
	row, err = p.AttachReasoning(context.Background(), id, "run-1", row.Version, ReasoningInput{Steps: []ReasoningStep{{Index: 0, Kind: ReasoningKindToolCall}}, EventSeq: 20})
	if err != nil {
		t.Fatalf("AttachReasoning 20: %v", err)
	}
	if row.Reasoning.Seq != 20 {
		t.Fatalf("Reasoning.Seq=%d, want 20", row.Reasoning.Seq)
	}
	row, err = p.AttachReasoning(context.Background(), id, "run-1", row.Version, ReasoningInput{Steps: []ReasoningStep{{Index: 1, Kind: ReasoningKindAwait}}, EventSeq: 10})
	if err != nil {
		t.Fatalf("AttachReasoning 10: %v", err)
	}
	if row.Reasoning.Seq != 20 {
		t.Errorf("Reasoning.Seq regressed to %d, want 20 (never regress)", row.Reasoning.Seq)
	}
	if row.LastAppliedEventSeq != 20 {
		t.Errorf("LastAppliedEventSeq=%d, want 20 (never regress)", row.LastAppliedEventSeq)
	}
}

// TestProjector_EventSequence_ConcurrentAdvances_ConvergeMonotonic
// pins the P2 concurrent case: racing observations with different
// sequences converge to the HIGHEST applied sequence, the row version
// reflects exactly the accepted writes, and no interleaving leaves a
// regressed sequence. A writer that loses the version slot reloads and
// retries; a writer whose sequence is at or below the applied one
// no-ops.
func TestProjector_EventSequence_ConcurrentAdvances_ConvergeMonotonic(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := p.Append(context.Background(), id, Append{TurnID: "run-1", Query: "q"})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	_ = row // the writers race from a fresh read each attempt

	start := make(chan struct{})
	done := make(chan error, 2)
	race := func(seq uint64, inline string) {
		<-start
		ans := Answer{State: AnswerStateInline, Inline: inline}
		for range 5 {
			cur, err := p.Get(context.Background(), id, "run-1")
			if err != nil {
				done <- err
				return
			}
			if seq > 0 && seq <= cur.LastAppliedEventSeq {
				done <- nil // already applied (or lower): monotonic no-op
				return
			}
			_, err = p.Update(context.Background(), id, "run-1", cur.Version, Update{Answer: &ans, EventSeq: seq})
			if err == nil {
				done <- nil
				return
			}
			if errors.Is(err, ErrStaleVersion) {
				continue // reload and retry the version slot
			}
			done <- err
			return
		}
		done <- fmt.Errorf("update did not converge")
	}
	go race(30, "from-30")
	go race(20, "from-20")
	close(start)
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("concurrent update: %v", err)
		}
	}

	got, err := p.Get(context.Background(), id, "run-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.LastAppliedEventSeq != 30 {
		t.Errorf("LastAppliedEventSeq=%d, want 30 (converges to the max)", got.LastAppliedEventSeq)
	}
	if got.Answer.Seq != 30 {
		t.Errorf("Answer.Seq=%d, want 30", got.Answer.Seq)
	}
	if got.Answer.Inline != "from-30" {
		t.Errorf("answer=%q, want from-30 (the highest-seq observation wins)", got.Answer.Inline)
	}
}

// TestProjector_ToolBudget_AboveCeiling_ConstructionSucceeds pins the
// P3 contract: a configured per-turn tool budget ABOVE the Protocol
// inline ceiling (MaxActivityRows) NEVER fails construction — the
// inline window is capped at the ceiling, the row overflows honestly
// (More + Dropped + Partial), and the exact turn-level totals survive.
// The v1.28 surface is exactly `sessions.turns.list/get` — there is no
// third activity read to fall back to.
func TestProjector_ToolBudget_AboveCeiling_ConstructionSucceeds(t *testing.T) {
	// Budget > ceiling with the DEFAULT inline limit: construction
	// succeeds (the old behavior failed loud here).
	p, err := New(newTestStoreOrFail(t), WithToolBudget(MaxActivityRows+50))
	if err != nil {
		t.Fatalf("New with over-ceiling budget must succeed, got: %v", err)
	}
	if p.activityLimit != DefaultActivityLimit {
		t.Errorf("inline limit=%d, want the default %d (capped at the ceiling)", p.activityLimit, DefaultActivityLimit)
	}
	// An explicit limit at the ceiling also constructs.
	if _, err := New(newTestStoreOrFail(t), WithToolBudget(MaxActivityRows+200), WithActivityLimit(MaxActivityRows)); err != nil {
		t.Errorf("New with ceiling limit + over-ceiling budget must succeed: %v", err)
	}
	// A budget at or below the ceiling still requires inline coverage.
	if _, err := New(newTestStoreOrFail(t), WithToolBudget(MaxActivityRows), WithActivityLimit(MaxActivityRows-1)); err == nil {
		t.Errorf("budget == ceiling with limit below must fail loud (inline capacity must cover the budget)")
	}

	// An over-ceiling-budget turn feeds > 128 rows and overflows
	// HONESTLY (More + Dropped + Partial); the capped window retains
	// the newest rows and the EXACT totals cover the full fed activity
	// (the turn summary survives even though the older rows are not
	// retained and no third activity read exists).
	all := make([]ActivityRow, MaxActivityRows+40)
	for i := range all {
		all[i] = ActivityRow{Position: i, Tool: "t", Status: ActivitySucceeded}
	}
	p2, err := New(newTestStoreOrFail(t), WithToolBudget(MaxActivityRows+50), WithActivityLimit(MaxActivityRows))
	if err != nil {
		t.Fatalf("New over-ceiling: %v", err)
	}
	id := tripleA()
	row, err := appendTurn(p2, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	got, err := p2.Update(context.Background(), id, "run-1", row.Version, Update{Activity: all})
	if err != nil {
		t.Fatalf("update over-ceiling activity: %v", err)
	}
	if !got.Activity.More || got.Activity.Dropped != 40 || got.Activity.Complete != CompletenessPartial {
		t.Errorf("over-ceiling activity must overflow honestly: %+v", got.Activity)
	}
	if len(got.Activity.Rows) != MaxActivityRows {
		t.Errorf("retained %d rows, want %d (the ceiling window)", len(got.Activity.Rows), MaxActivityRows)
	}
	// The retained window is the NEWEST rows (highest positions).
	if got.Activity.Rows[len(got.Activity.Rows)-1].Position != len(all)-1 {
		t.Errorf("retained window must be the newest rows: last position=%d, want %d",
			got.Activity.Rows[len(got.Activity.Rows)-1].Position, len(all)-1)
	}
	// The exact totals cover the FULL fed activity — the turn summary
	// is renderable through list/get even without a third activity
	// read.
	if got.Activity.Totals.Succeeded != int64(len(all)) {
		t.Errorf("Activity.Totals.Succeeded=%d, want %d (exact across the full fed activity)", got.Activity.Totals.Succeeded, len(all))
	}
	// A fresh Get (as list/get would serve) carries the same honest
	// lower-bound and totals after a restart-free round trip.
	reread, err := p2.Get(context.Background(), id, "run-1")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !reread.Activity.More || reread.Activity.Totals != got.Activity.Totals {
		t.Errorf("persisted row lost the honest overflow/totals: %+v", reread.Activity)
	}
}

// TestProjector_AppRefs_BoundsAndControlRejection pins the P4
// contract: the Apps collection count and every App string / URI /
// tool field are bounded, every field is valid UTF-8, and NUL /
// control characters are rejected loudly (no ambiguity in the typed
// identity — the NUL-concatenation is gone).
func TestProjector_AppRefs_BoundsAndControlRejection(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	v := row.Version

	// NUL / control bytes in identity and URI fields are rejected.
	for name, ref := range map[string]AppRef{
		"nul agent id":     {EffectiveAgentID: "agent\x00x", ServerID: "srv", ResourceURI: "ui://srv/app"},
		"nul server id":    {ServerID: "srv\x00x", ResourceURI: "ui://srv/app"},
		"nul resource uri": {ServerID: "srv", ResourceURI: "ui://srv\x00/app"},
		"ctrl display":     {ServerID: "srv", ResourceURI: "ui://srv/app", DisplayMode: "inline\x1b"},
		"del tool name":    {ServerID: "srv", ResourceURI: "ui://srv/app", ToolName: "tool\x7f"},
	} {
		if _, err := p.AttachAppRefs(context.Background(), id, "run-1", v, AppRefInput{Refs: []AppRef{ref}}); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s error=%v, want ErrInvalidInput (control chars rejected)", name, err)
		}
	}
	// Invalid UTF-8 is rejected.
	badUTF8 := AppRef{ServerID: "srv", ResourceURI: "ui://srv/app", ToolName: "tool\xff\xfe"}
	if _, err := p.AttachAppRefs(context.Background(), id, "run-1", v, AppRefInput{Refs: []AppRef{badUTF8}}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("invalid UTF-8 error=%v, want ErrInvalidInput", err)
	}
	// Over-bound fields are rejected.
	over := AppRef{ServerID: "srv", ResourceURI: "ui://srv/" + strings.Repeat("a", MaxAppResourceURIRunes+1)}
	if _, err := p.AttachAppRefs(context.Background(), id, "run-1", v, AppRefInput{Refs: []AppRef{over}}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("over-bound URI error=%v, want ErrInvalidInput", err)
	}

	// The Apps collection count is bounded: MaxAppsPerTurn distinct
	// identities fit; one more NEW identity fails loud (never silently
	// dropped).
	refs := make([]AppRef, 0, MaxAppsPerTurn)
	for i := range MaxAppsPerTurn {
		refs = append(refs, AppRef{ServerID: "srv", ResourceURI: fmt.Sprintf("ui://srv/app-%d", i), ToolName: "t"})
	}
	got, err := p.AttachAppRefs(context.Background(), id, "run-1", v, AppRefInput{Refs: refs})
	if err != nil {
		t.Fatalf("AttachAppRefs at the bound: %v", err)
	}
	if len(got.Apps) != MaxAppsPerTurn {
		t.Fatalf("apps=%d, want %d at the bound", len(got.Apps), MaxAppsPerTurn)
	}
	// Replacing in place at the bound is fine (no growth).
	again := AppRef{ServerID: "srv", ResourceURI: "ui://srv/app-0", ToolName: "t-v2"}
	if _, err := p.AttachAppRefs(context.Background(), id, "run-1", got.Version, AppRefInput{Refs: []AppRef{again}}); err != nil {
		t.Fatalf("in-place replacement at the bound must succeed: %v", err)
	}
	// One NEW identity past the bound fails loud.
	oneMore := AppRef{ServerID: "srv", ResourceURI: "ui://srv/app-overflow", ToolName: "t"}
	if _, err := p.AttachAppRefs(context.Background(), id, "run-1", got.Version+1, AppRefInput{Refs: []AppRef{oneMore}}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("over-bound app count error=%v, want ErrInvalidInput", err)
	}
}

// TestProjector_AttachmentAndAnswerRef_StringBounds pins the closed
// UTF-8 / control / NUL and size bounds on attachment and artifact
// reference string fields: control-laden, invalid-UTF-8, and
// over-bound values are rejected (ErrInvalidInput) before they can
// reach a row, using the package limits MaxArtifactIDRunes /
// MaxFilenameRunes / MaxMimeTypeRunes / MaxSHA256Runes /
// MaxAttachmentDispositionRunes. Valid values still pass.
func TestProjector_AttachmentAndAnswerRef_StringBounds(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	v := row.Version

	// Attachment fields: control / NUL / invalid UTF-8 / over-bound are
	// rejected on both the append and the update surfaces.
	attachCases := []struct {
		name string
		att  Attachment
	}{
		{"nul id", Attachment{ID: "a\x00b"}},
		{"control filename", Attachment{ID: "a", Filename: "f\x1b.txt"}},
		{"del mime type", Attachment{ID: "a", MimeType: "text/\x7fplain"}},
		{"invalid utf-8 sha256", Attachment{ID: "a", SHA256: "\xff\xfe"}},
		{"nul disposition", Attachment{ID: "a", Disposition: "tool:\x00x"}},
		{"over-bound id", Attachment{ID: strings.Repeat("a", MaxArtifactIDRunes+1)}},
		{"over-bound filename", Attachment{ID: "a", Filename: strings.Repeat("f", MaxFilenameRunes+1)}},
		{"over-bound mime type", Attachment{ID: "a", MimeType: strings.Repeat("m", MaxMimeTypeRunes+1)}},
		{"over-bound sha256", Attachment{ID: "a", SHA256: strings.Repeat("a", MaxSHA256Runes+1)}},
		{"over-bound disposition", Attachment{ID: "a", Disposition: strings.Repeat("d", MaxAttachmentDispositionRunes+1)}},
	}
	for _, tc := range attachCases {
		t.Run("append "+tc.name, func(t *testing.T) {
			if _, err := p.Append(context.Background(), id, Append{TurnID: TurnID("bad-att"), Query: "q", Inputs: []Attachment{tc.att}}); !errors.Is(err, ErrInvalidInput) {
				t.Errorf("append error=%v, want ErrInvalidInput", err)
			}
		})
		t.Run("update "+tc.name, func(t *testing.T) {
			if _, err := p.Update(context.Background(), id, "run-1", v, Update{Outputs: []Attachment{tc.att}}); !errors.Is(err, ErrInvalidInput) {
				t.Errorf("update error=%v, want ErrInvalidInput", err)
			}
		})
	}

	// AnswerRef fields: control / invalid UTF-8 / over-bound are
	// rejected on the update surface (the only answer write surface).
	refCases := []struct {
		name string
		ref  AnswerRef
	}{
		{"nul id", AnswerRef{ID: "art\x00x"}},
		{"control mime type", AnswerRef{ID: "a", MimeType: "text/\x01plain"}},
		{"invalid utf-8 filename", AnswerRef{ID: "a", Filename: "\xff"}},
		{"del sha256", AnswerRef{ID: "a", SHA256: "ab\x7f"}},
		{"over-bound id", AnswerRef{ID: strings.Repeat("a", MaxArtifactIDRunes+1)}},
		{"over-bound mime type", AnswerRef{ID: "a", MimeType: strings.Repeat("m", MaxMimeTypeRunes+1)}},
		{"over-bound filename", AnswerRef{ID: "a", Filename: strings.Repeat("f", MaxFilenameRunes+1)}},
		{"over-bound sha256", AnswerRef{ID: "a", SHA256: strings.Repeat("a", MaxSHA256Runes+1)}},
	}
	for _, tc := range refCases {
		t.Run(tc.name, func(t *testing.T) {
			ans := Answer{State: AnswerStateArtifactRef, Ref: &tc.ref}
			if _, err := p.Update(context.Background(), id, "run-1", v, Update{Answer: &ans}); !errors.Is(err, ErrInvalidInput) {
				t.Errorf("error=%v, want ErrInvalidInput", err)
			}
		})
	}

	// Valid values still pass (regression guard).
	okAtt := []Attachment{{ID: "in_1", Filename: "f.txt", MimeType: "text/plain", SHA256: "ab", Disposition: "ref", Availability: CompletenessComplete}}
	if _, err := p.Update(context.Background(), id, "run-1", v, Update{Inputs: okAtt}); err != nil {
		t.Errorf("valid attachment rejected: %v", err)
	}
	// The update above bumped the version; re-fetch for the next write.
	row, err = p.Get(context.Background(), id, "run-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	okRef := Answer{State: AnswerStateArtifactRef, Ref: &AnswerRef{ID: "art_1", MimeType: "text/plain", Filename: "a.txt", SHA256: "ab", SizeBytes: 8}}
	if _, err := p.Update(context.Background(), id, "run-1", row.Version, Update{Answer: &okRef}); err != nil {
		t.Errorf("valid answer ref rejected: %v", err)
	}
}

// TestProjector_DeepCopy_InputSlicesNeverAliasTheStoredRow pins the P4
// deep-copy contract: mutating a caller's input slice AFTER an append
// / update / attach never corrupts the stored projection (D-025
// concurrent reuse — the projection owns its slices).
func TestProjector_DeepCopy_InputSlicesNeverAliasTheStoredRow(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()

	activity := []ActivityRow{{Tool: "t1", Status: ActivitySucceeded}}
	inputs := []Attachment{{ID: "in_1"}}
	if _, err := p.Append(context.Background(), id, Append{TurnID: "run-1", Query: "q", Activity: activity, Inputs: inputs}); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Corrupt the caller's slices after the call.
	activity[0].Tool = "CORRUPTED"
	inputs[0].ID = "CORRUPTED"
	got, err := p.Get(context.Background(), id, "run-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Activity.Rows[0].Tool != "t1" {
		t.Errorf("append activity aliased the caller's slice: %q", got.Activity.Rows[0].Tool)
	}
	if got.Inputs[0].ID != "in_1" {
		t.Errorf("append inputs aliased the caller's slice: %q", got.Inputs[0].ID)
	}

	// Update: the fed activity is deep-copied.
	fed := []ActivityRow{{Tool: "t2", Status: ActivitySucceeded}}
	if _, err := p.Update(context.Background(), id, "run-1", got.Version, Update{Activity: fed}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	fed[0].Tool = "CORRUPTED"
	got, err = p.Get(context.Background(), id, "run-1")
	if err != nil {
		t.Fatalf("Get 2: %v", err)
	}
	if got.Activity.Rows[0].Tool != "t2" {
		t.Errorf("update activity aliased the caller's slice: %q", got.Activity.Rows[0].Tool)
	}

	// AttachReasoning: the fed steps are deep-copied.
	steps := []ReasoningStep{{Index: 0, Kind: ReasoningKindToolCall}}
	got, err = p.AttachReasoning(context.Background(), id, "run-1", got.Version, ReasoningInput{Steps: steps})
	if err != nil {
		t.Fatalf("AttachReasoning: %v", err)
	}
	steps[0].Kind = ReasoningKindSpawn
	got, err = p.Get(context.Background(), id, "run-1")
	if err != nil {
		t.Fatalf("Get 3: %v", err)
	}
	if got.Reasoning.Steps[0].Kind != ReasoningKindToolCall {
		t.Errorf("reasoning aliased the caller's slice: %+v", got.Reasoning.Steps[0])
	}

	// AttachAppRefs: the fed refs are deep-copied.
	refs := []AppRef{{ServerID: "srv", ResourceURI: "ui://srv/app", ToolName: "tool"}}
	got, err = p.AttachAppRefs(context.Background(), id, "run-1", got.Version, AppRefInput{Refs: refs})
	if err != nil {
		t.Fatalf("AttachAppRefs: %v", err)
	}
	refs[0].ToolName = "CORRUPTED"
	got, err = p.Get(context.Background(), id, "run-1")
	if err != nil {
		t.Fatalf("Get 4: %v", err)
	}
	if got.Apps[0].ToolName != "tool" {
		t.Errorf("app refs aliased the caller's slice: %q", got.Apps[0].ToolName)
	}
}

// TestProjector_DeepCopy_PointerBackedFieldsNeverAliasTheStoredRow
// pins the deep-copy contract for the OPTIONAL POINTER-BACKED mutable
// fields — Answer.Ref and UsageMeasure.Value — on the projection
// mutation edge: a caller mutating its AnswerRef or its usage *int64
// after an update must never corrupt the returned row or the stored
// row, and mutating the returned row must never corrupt the stored
// row (D-025 concurrent reuse).
func TestProjector_DeepCopy_PointerBackedFieldsNeverAliasTheStoredRow(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	value := int64(42)
	ref := AnswerRef{ID: "art_1", MimeType: "text/plain", SizeBytes: 4096, SHA256: "ab"}
	ans := Answer{State: AnswerStateArtifactRef, Ref: &ref}
	usage := Usage{PromptTokens: UsageMeasure{State: UsageExact, Value: &value}}
	got, err := p.Update(context.Background(), id, "run-1", row.Version, Update{Answer: &ans, Usage: &usage})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Corrupt the caller's pointer-backed inputs after the write.
	ref.ID = "CORRUPTED"
	ref.Filename = "CORRUPTED"
	value = -1

	// The RETURNED row must hold its own copies (it must not alias the
	// caller's pointers).
	if got.Answer.Ref == nil || got.Answer.Ref.ID != "art_1" {
		t.Errorf("returned Answer.Ref aliased the caller's ref: %+v", got.Answer.Ref)
	}
	if got.Usage.PromptTokens.Value == nil || *got.Usage.PromptTokens.Value != 42 {
		t.Errorf("returned usage value aliased the caller's *int64: %+v", got.Usage.PromptTokens)
	}

	// The STORED row must hold its own copies.
	stored, err := p.Get(context.Background(), id, "run-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Answer.Ref == nil || stored.Answer.Ref.ID != "art_1" {
		t.Errorf("stored Answer.Ref aliased the caller's ref: %+v", stored.Answer.Ref)
	}
	if stored.Usage.PromptTokens.Value == nil || *stored.Usage.PromptTokens.Value != 42 {
		t.Errorf("stored usage value aliased the caller's *int64: %+v", stored.Usage.PromptTokens)
	}

	// Mutating the RETURNED row must not corrupt the stored row.
	got.Answer.Ref.ID = "MUTATED-RETURN"
	stored, err = p.Get(context.Background(), id, "run-1")
	if err != nil {
		t.Fatalf("Get 2: %v", err)
	}
	if stored.Answer.Ref.ID != "art_1" {
		t.Errorf("mutating the returned row corrupted the stored row: %+v", stored.Answer.Ref)
	}
}

// TestProjector_ConcurrentReuse_PointerBackedInputsNoAliasingRace is
// the race-friendly mutation test for the pointer-backed fields: a
// writer mutates its caller-owned AnswerRef / usage *int64 AFTER the
// write while a reader goroutine inspects the row the update returned.
// If the projection aliased caller memory, the two goroutines would
// share the same objects and the -race detector flags the access; the
// deep-copy contract keeps them separate, and the reader asserts the
// values never drift from the originals.
func TestProjector_ConcurrentReuse_PointerBackedInputsNoAliasingRace(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	value := int64(42)
	ref := AnswerRef{ID: "art-1", MimeType: "text/plain", SizeBytes: 8}
	ans := Answer{State: AnswerStateArtifactRef, Ref: &ref}
	usage := Usage{PromptTokens: UsageMeasure{State: UsageExact, Value: &value}}
	got, err := p.Update(context.Background(), id, "run-1", row.Version, Update{Answer: &ans, Usage: &usage})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}

	res := make(chan TurnRow, 1)
	res <- got
	readerErrs := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		r := <-res
		// Busy-read the returned row's pointer-backed fields while the
		// writer mutates the caller's inputs (20000 iterations give the
		// scheduler ample overlap for the -race detector).
		for range 20000 {
			if r.Answer.Ref == nil || r.Answer.Ref.ID != "art-1" {
				readerErrs <- fmt.Errorf("reader saw a corrupted Answer.Ref: %+v", r.Answer.Ref)
				return
			}
			if r.Usage.PromptTokens.Value == nil || *r.Usage.PromptTokens.Value != 42 {
				readerErrs <- fmt.Errorf("reader saw a corrupted usage value: %+v", r.Usage.PromptTokens)
				return
			}
		}
		readerErrs <- nil
	}()
	for range 20000 {
		ref.ID = "CORRUPTED"
		value = -1
	}
	wg.Wait()
	if err := <-readerErrs; err != nil {
		t.Fatalf("reader: %v", err)
	}

	// The stored row is intact after the concurrent mutation.
	stored, err := p.Get(context.Background(), id, "run-1")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if stored.Answer.Ref == nil || stored.Answer.Ref.ID != "art-1" {
		t.Errorf("stored Answer.Ref corrupted: %+v", stored.Answer.Ref)
	}
	if stored.Usage.PromptTokens.Value == nil || *stored.Usage.PromptTokens.Value != 42 {
		t.Errorf("stored usage value corrupted: %+v", stored.Usage.PromptTokens)
	}
}

// TestProjector_Reconcile_ErasureProbeGate pins the P6 contract: an
// in-memory-backed store loses its STORE-LOCAL fence on restart, so
// Reconcile consults the runtime's DURABLE erasure probe BEFORE
// rebuilding — an erased session is never rebuilt from sequence zero
// merely because the in-memory store restarted, and the store-local
// fence is restored for the process lifetime.
func TestProjector_Reconcile_ErasureProbeGate(t *testing.T) {
	probe := &fakeErasureProbe{erased: true}
	p2, err := New(newTestStoreOrFail(t), WithErasureProbe(probe))
	if err != nil {
		t.Fatalf("New with probe: %v", err)
	}
	id := tripleA()

	// The probe reports the session erased: Reconcile refuses loudly,
	// never calls apply, and never advances a checkpoint.
	applied := false
	_, err = p2.Reconcile(context.Background(), id, func(ctx context.Context, id identity.Identity, from uint64) (uint64, error) {
		applied = true
		return 42, nil
	})
	if !errors.Is(err, ErrErasureFenced) {
		t.Fatalf("Reconcile on erased session error=%v, want ErrErasureFenced", err)
	}
	if applied {
		t.Errorf("apply was called on an erased session — the probe gate must run first")
	}
	if seq, _ := p2.Checkpoint(context.Background(), id); seq != 0 {
		t.Errorf("checkpoint advanced on an erased session: %d, want 0 (no resurrection)", seq)
	}
	// The store-local fence was RESTORED for this process lifetime:
	// subsequent writes refuse.
	if _, err := appendTurn(p2, id, "run-1"); !errors.Is(err, ErrErasureFenced) {
		t.Errorf("post-refusal append error=%v, want ErrErasureFenced (fence restored)", err)
	}

	// A probe reporting NOT erased rebuilds normally from the
	// checkpoint.
	probe.erased = false
	p3, err := New(newTestStoreOrFail(t), WithErasureProbe(probe))
	if err != nil {
		t.Fatalf("New with probe 2: %v", err)
	}
	high, err := p3.Reconcile(context.Background(), id, func(ctx context.Context, id identity.Identity, from uint64) (uint64, error) {
		if from != 0 {
			t.Errorf("rebuild from=%d, want 0", from)
		}
		if _, err := appendTurn(p3, id, "run-1"); err != nil {
			return 0, err
		}
		return 3, nil
	})
	if err != nil {
		t.Fatalf("Reconcile not-erased: %v", err)
	}
	if high != 3 {
		t.Errorf("high=%d, want 3", high)
	}

	// A probe FAILURE surfaces loudly (never a silent rebuild).
	p4, err := New(newTestStoreOrFail(t), WithErasureProbe(&fakeErasureProbe{err: errors.New("probe broken")}))
	if err != nil {
		t.Fatalf("New with failing probe: %v", err)
	}
	if _, err := p4.Reconcile(context.Background(), id, func(ctx context.Context, id identity.Identity, from uint64) (uint64, error) { return 1, nil }); err == nil {
		t.Errorf("failing probe must surface loudly")
	}

	// A nil probe (the runtime declared no durable erasure authority)
	// rebuilds on the store-local fence alone — the HONEST availability
	// gap (the availability contract is documented, never claimed
	// otherwise).
	p5, _ := newTestProjector(t, 0, false)
	if high, err := p5.Reconcile(context.Background(), id, func(ctx context.Context, id identity.Identity, from uint64) (uint64, error) {
		return 1, nil
	}); err != nil || high != 1 {
		t.Errorf("nil-probe reconcile = (%d, %v), want (1, nil) — honest gap, not an error", high, err)
	}

	// The durable driver's STORE-LOCAL fence stays permanent: an
	// erased durable session is refused by the store itself even with a
	// probe that lies (the local fence is authoritative for durable
	// drivers).
	probe.erased = false
	p6, st6 := newTestProjector(t, 0, true) // durable backing store
	if _, err := p6.Erase(context.Background(), id); err != nil {
		t.Fatalf("Erase durable: %v", err)
	}
	_ = st6
	probe2 := &fakeErasureProbe{erased: false}
	p7, err := New(st6, WithErasureProbe(probe2))
	if err != nil {
		t.Fatalf("New durable with probe: %v", err)
	}
	_, err = p7.Reconcile(context.Background(), id, func(ctx context.Context, id identity.Identity, from uint64) (uint64, error) {
		return 1, nil
	})
	if !errors.Is(err, ErrErasureFenced) {
		t.Errorf("durable erased session reconcile error=%v, want ErrErasureFenced (store-local fence is permanent)", err)
	}
}

// fakeErasureProbe is a test-grade ErasureProbe with a settable
// answer / failure.
type fakeErasureProbe struct {
	erased bool
	err    error
}

func (f *fakeErasureProbe) Erased(_ context.Context, _ identity.Identity) (bool, error) {
	return f.erased, f.err
}
