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

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
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
	// A fresh turn has no answer / usage / reasoning / app yet — honest
	// Unavailable, never a fabricated zero.
	if row.Answer.Complete != CompletenessUnavailable {
		t.Errorf("Answer.Complete=%q, want unavailable", row.Answer.Complete)
	}
	if row.Usage.Complete != CompletenessUnavailable {
		t.Errorf("Usage.Complete=%q, want unavailable", row.Usage.Complete)
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

	usage := Usage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150, CostUSD: 0.01, Model: "gpt-x", Complete: CompletenessComplete}
	answer := Answer{State: AnswerStateInline, Inline: "It is sunny."}
	act := []ActivityRow{{Tool: "search", Status: ActivitySucceeded, Summary: "0.4s"}}
	upd, err := p.Update(context.Background(), id, "run-1", row.Version, Update{
		Status:   StatusPaused,
		Answer:   &answer,
		Usage:    &usage,
		Activity: act,
	})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if upd.Status != StatusPaused {
		t.Errorf("Status=%q, want paused", upd.Status)
	}
	if upd.Version != row.Version+1 {
		t.Errorf("Version=%d, want %d", upd.Version, row.Version+1)
	}
	if upd.Usage.TotalTokens != 150 || upd.Usage.Model != "gpt-x" {
		t.Errorf("Usage not replaced: %+v", upd.Usage)
	}
	if upd.Answer.Inline != "It is sunny." {
		t.Errorf("Answer not replaced: %+v", upd.Answer)
	}
	if len(upd.Activity.Rows) != 1 || upd.Activity.Rows[0].Tool != "search" || upd.Activity.Complete != CompletenessComplete {
		t.Errorf("Activity not replaced: %+v", upd.Activity)
	}

	// nil components leave the stored ones unchanged.
	upd2, err := p.Update(context.Background(), id, "run-1", upd.Version, Update{Status: StatusRunning})
	if err != nil {
		t.Fatalf("Update 2: %v", err)
	}
	if upd2.Status != StatusRunning {
		t.Errorf("Status=%q, want running", upd2.Status)
	}
	if upd2.Answer.Inline != "It is sunny." || upd2.Usage.TotalTokens != 150 {
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
	if err := sealComplete(p, id, "run-1", row.Version); err != nil {
		t.Fatalf("seal: %v", err)
	}
	if _, err := p.Update(context.Background(), id, "run-1", row.Version+1, Update{}); !errors.Is(err, ErrTurnSealed) {
		t.Errorf("update of sealed row error=%v, want ErrTurnSealed", err)
	}
}

// sealComplete attaches a complete answer and seals the turn complete.
func sealComplete(p *Projector, id identity.Identity, turnID TurnID, version int) error {
	row, err := p.Get(context.Background(), id, turnID)
	if err != nil {
		return err
	}
	if row.Answer.Complete != CompletenessComplete {
		ans := Answer{State: AnswerStateInline, Inline: "final answer"}
		if _, err := p.Update(context.Background(), id, turnID, row.Version, Update{Answer: &ans}); err != nil {
			return err
		}
		row, err = p.Get(context.Background(), id, turnID)
		if err != nil {
			return err
		}
	}
	_, err = p.Seal(context.Background(), id, turnID, row.Version, Seal{Status: StatusComplete, FinishReason: "goal"})
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
	negUsage := Usage{PromptTokens: -1, Complete: CompletenessComplete}
	if _, err := p.Update(context.Background(), id, "run-1", v, Update{Usage: &negUsage}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("negative usage error=%v, want ErrInvalidInput", err)
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
	sealed, err := p.Seal(context.Background(), id, "run-1", row.Version, Seal{Status: StatusCancelled, FinishReason: "cancelled"})
	if err != nil {
		t.Fatalf("cancelled seal: %v", err)
	}
	if !sealed.Sealed || sealed.Status != StatusCancelled || sealed.FinishReason != "cancelled" {
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
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := sealComplete(p, id, "run-1", row.Version); err != nil {
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
	if _, err := p.Seal(context.Background(), id, "run-1", sealed.Version, Seal{Status: StatusFailed, ErrorClass: "x"}); !errors.Is(err, ErrTurnSealed) {
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
		{Index: 0, Trace: "first thought"},
		{Index: 2, Trace: "gap-tolerant step"},
		{Index: 5, Trace: "third thought"},
	}
	got, err := p.AttachReasoning(context.Background(), id, "run-1", row.Version, ReasoningInput{Steps: steps})
	if err != nil {
		t.Fatalf("AttachReasoning: %v", err)
	}
	if got.Reasoning.Complete != CompletenessComplete || len(got.Reasoning.Steps) != 3 {
		t.Errorf("reasoning projection wrong: %+v", got.Reasoning)
	}
	if got.Reasoning.Steps[1].Index != 2 || got.Reasoning.Steps[1].Trace != "gap-tolerant step" {
		t.Errorf("ordered steps wrong: %+v", got.Reasoning.Steps)
	}

	// Overflow keeps the FIRST MaxReasoningSteps and reports the tail
	// drop as Partial + Dropped.
	overflow := make([]ReasoningStep, MaxReasoningSteps+5)
	for i := range overflow {
		overflow[i] = ReasoningStep{Index: i, Trace: "t"}
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
	if _, err := p.AttachReasoning(context.Background(), id, "run-1", got3.Version, ReasoningInput{Steps: []ReasoningStep{{Index: 3, Trace: "a"}, {Index: 1, Trace: "b"}}}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("non-increasing indices error=%v, want ErrInvalidInput", err)
	}
	// Over-bound trace fails loud.
	if _, err := p.AttachReasoning(context.Background(), id, "run-1", got3.Version, ReasoningInput{Steps: []ReasoningStep{{Index: 0, Trace: strings.Repeat("t", MaxStepTraceRunes+1)}}}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("over-bound trace error=%v, want ErrInvalidInput", err)
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
}

// TestActivityReader_Contract_CompletesTheLowerBound proves the
// separately named optional activity-read contract: when a row's
// window overflows (More == true), a caller pages the FULL activity
// through ActivityReader — including the rows the projection dropped —
// and a non-overflowed row (More == false) never needs the reader.
func TestActivityReader_Contract_CompletesTheLowerBound(t *testing.T) {
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

	// The runtime wires a reader over the durable event log; the
	// test-grade reader replays the fed rows.
	reader := &recordingActivityReader{rows: fed}
	if !got.Activity.More {
		t.Fatalf("precondition: the overflow marker must be set")
	}
	var full []ActivityRow
	cursor := 0
	for {
		page, hasMore, err := reader.Activity(context.Background(), id, "run-1", 10)
		if err != nil {
			t.Fatalf("Activity: %v", err)
		}
		full = append(full, page...)
		cursor += len(page)
		if !hasMore {
			break
		}
		// The recording reader is stateless per call in this test
		// harness; page from the offset to model a real cursor.
		reader.offset = cursor
	}
	if len(full) != len(fed) {
		t.Errorf("reader returned %d rows, want %d (the full activity)", len(full), len(fed))
	}

	// A non-overflowed window (More == false) never needs the reader.
	small, err := p.Update(context.Background(), id, "run-1", got.Version, Update{Activity: []ActivityRow{{Tool: "a", Status: ActivitySucceeded}}})
	if err != nil {
		t.Fatalf("update small activity: %v", err)
	}
	if small.Activity.More || small.Activity.Dropped != 0 {
		t.Errorf("small window must carry no lower-bound marker: %+v", small.Activity)
	}
}

// recordingActivityReader is a test-grade ActivityReader over a fixed
// row slice; offset models a paged read position.
type recordingActivityReader struct {
	rows   []ActivityRow
	offset int
}

func (r *recordingActivityReader) Activity(_ context.Context, _ identity.Identity, _ TurnID, limit int) ([]ActivityRow, bool, error) {
	if limit < 1 {
		return nil, false, ErrInvalidInput
	}
	start := r.offset
	if start >= len(r.rows) {
		return nil, false, nil
	}
	end := start + limit
	if end > len(r.rows) {
		end = len(r.rows)
	}
	return r.rows[start:end], end < len(r.rows), nil
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
		if err := sealComplete(p, id, "run-1", row.Version+1); err != nil {
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
		row, err := p.Get(ctx, id, "run-1")
		if err != nil {
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
		if err := sealComplete(p, id, "run-1", row.Version); err != nil {
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
	if _, err := p.AttachReasoning(context.Background(), id, "run-1", row.Version, ReasoningInput{Steps: []ReasoningStep{{Index: 0, Trace: "t"}}}); !errors.Is(err, ErrErasureFenced) {
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
			return 0, fmt.Errorf("replay append error=%v, want ErrErasureFenced (no resurrection)", err)
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
	for i := 0; i < testStoreRetentionTiny+3; i++ {
		if _, err := appendTurn(p, id, TurnID(strings.Repeat("r", 1)+string(rune('a'+i)))); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}
	page, err := p.List(context.Background(), id, ListOptions{Limit: MaxListLimit})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !page.Truncated {
		t.Errorf("Truncated=false after retention eviction, want the explicit marker")
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
	for i := 0; i < pt.NumField(); i++ {
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
		Steps:    []ReasoningStep{{Index: 0, Trace: "think"}},
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
	// Dropped + Partial) — the named ActivityReader pages the rest.
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
	row, err = p.Update(context.Background(), id, "run-1", row.Version, Update{Answer: &ans, Usage: &Usage{PromptTokens: 10, TotalTokens: 10, Model: "m", Complete: CompletenessComplete}, EventSeq: 4})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
	if _, err := p.AttachReasoning(context.Background(), id, "run-1", row.Version, ReasoningInput{
		Steps:    []ReasoningStep{{Index: 0, Trace: "secret reasoning"}},
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
	if ops.AgentID != "agent-1" || ops.AgentBindingSource != AgentBindingExplicit {
		t.Errorf("ops agent binding wrong: %+v", ops)
	}
	if ops.Usage.TotalTokens != 10 {
		t.Errorf("ops usage lost: %+v", ops.Usage)
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
	// reasoning traces, resource URI, tool_call_id, App context. The
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
