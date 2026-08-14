package turns

import (
	"context"
	"errors"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// TestCoreDTO_RawThinkingSentinelsAbsent is the raw-thinking honesty
// pin: NO consumer DTO may carry a field that could hold raw provider
// thinking (traces, chain-of-thought, transcripts) or raw tool
// arguments / results. Every consumer component struct is scanned for
// the sentinel names, and ReasoningStep — the only reasoning payload —
// is pinned to EXACTLY {Index, Kind}, the structurally safe derived
// summary. A future field that could carry arbitrary provider text
// fails here before it can reach the wire.
func TestCoreDTO_RawThinkingSentinelsAbsent(t *testing.T) {
	consumerTypes := []reflect.Type{
		reflect.TypeOf(TurnRow{}),
		reflect.TypeOf(Agent{}),
		reflect.TypeOf(Query{}),
		reflect.TypeOf(Answer{}),
		reflect.TypeOf(AnswerRef{}),
		reflect.TypeOf(Pause{}),
		reflect.TypeOf(Attachment{}),
		reflect.TypeOf(Usage{}),
		reflect.TypeOf(UsageMeasure{}),
		reflect.TypeOf(Reasoning{}),
		reflect.TypeOf(ReasoningStep{}),
		reflect.TypeOf(Activity{}),
		reflect.TypeOf(ActivityRow{}),
		reflect.TypeOf(ActivityTotals{}),
		reflect.TypeOf(AppRef{}),
		reflect.TypeOf(AppRefKey{}),
	}
	exactForbidden := map[string]bool{
		"Trace": true, "ReasoningTrace": true, "Thinking": true,
		"ChainOfThought": true, "Transcript": true, "History": true,
		"Messages": true, "ToolArgs": true, "ToolArguments": true,
		"ToolResults": true, "RawText": true, "RawReasoning": true,
		"ReasoningText": true, "Prompt": true, "RawPrompt": true,
		"Token": true,
	}
	for _, typ := range consumerTypes {
		if typ.Kind() != reflect.Struct {
			continue
		}
		for i := range typ.NumField() {
			name := typ.Field(i).Name
			if exactForbidden[name] {
				t.Errorf("%s.%s must never exist — raw provider thinking / tool content cannot be represented", typ.Name(), name)
			}
			for _, sub := range []string{"Trace", "Thinking", "Transcript", "ChainOfThought", "ToolArg", "ToolResult", "RawReasoning", "ReasoningText"} {
				if hasSubstring(name, sub) {
					t.Errorf("%s.%s must never exist — raw provider thinking sentinel present", typ.Name(), name)
				}
			}
		}
	}

	// ReasoningStep is exactly the closed derived summary: an index and
	// a kind, and nothing else. There is structurally no place for a
	// trace / text / raw reasoning payload.
	got := fieldNames(reflect.TypeOf(ReasoningStep{}))
	want := []string{"Index", "Kind"}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ReasoningStep field set drift: got %v, want exactly %v — raw provider thinking must have no representable form", got, want)
	}
}

// TestUsage_PerMeasureHonesty_UnavailableVsExactZero pins the
// per-measure usage honesty contract: each measure states its own
// availability / exactness; an unavailable measure carries NO value
// (a missing measure is never a fabricated zero), while a genuinely
// exact zero IS distinguishable from unavailable. Cost is exact
// integer micro-dollars — never float64.
func TestUsage_PerMeasureHonesty_UnavailableVsExactZero(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	// Only prompt tokens are source-backed; every other measure is
	// honestly unavailable with NO value — not an exact zero.
	usage := Usage{PromptTokens: usageExact(100)}
	got, err := p.Update(context.Background(), id, "run-1", row.Version, Update{Usage: &usage})
	if err != nil {
		t.Fatalf("update usage: %v", err)
	}
	if m := got.Usage.PromptTokens; m.State != UsageExact || m.Value == nil || *m.Value != 100 {
		t.Errorf("PromptTokens=%+v, want exact 100", m)
	}
	for name, m := range usageMeasures(got.Usage) {
		if name == "PromptTokens" {
			continue
		}
		if m.State != UsageUnavailable {
			t.Errorf("%s state=%q, want unavailable (no source reported it)", name, m.State)
		}
		if m.Value != nil {
			t.Errorf("%s carries value %d with no source — a missing measure is unavailable, never a fabricated amount", name, *m.Value)
		}
	}

	// A genuinely exact zero IS distinguishable from unavailable: the
	// provider reported zero total tokens.
	zero := Usage{TotalTokens: usageExact(0)}
	got2, err := p.Update(context.Background(), id, "run-1", got.Version, Update{Usage: &zero})
	if err != nil {
		t.Fatalf("update zero usage: %v", err)
	}
	if m := got2.Usage.TotalTokens; m.State != UsageExact || m.Value == nil || *m.Value != 0 {
		t.Errorf("TotalTokens=%+v, want exact zero (distinct from unavailable)", m)
	}
	// The unavailable measures survived the wholesale replacement.
	if m := got2.Usage.CompletionTokens; m.State != UsageUnavailable || m.Value != nil {
		t.Errorf("CompletionTokens=%+v, want unavailable-without-value after wholesale replacement", m)
	}

	// Exact integer micro-dollar cost — never float64 accumulation.
	cost := Usage{CostMicroUSD: usageExact(2_500_000)} // $2.50
	got3, err := p.Update(context.Background(), id, "run-1", got2.Version, Update{Usage: &cost})
	if err != nil {
		t.Fatalf("update cost: %v", err)
	}
	if m := got3.Usage.CostMicroUSD; m.State != UsageExact || m.Value == nil || *m.Value != 2_500_000 {
		t.Errorf("CostMicroUSD=%+v, want exact integer 2500000 micro-dollars", m)
	}

	// An estimated measure is honestly stated as approximate.
	est := Usage{LatencyNS: UsageMeasure{State: UsageEstimated, Value: usageInt64(1_234_567)}}
	got4, err := p.Update(context.Background(), id, "run-1", got3.Version, Update{Usage: &est})
	if err != nil {
		t.Fatalf("update estimated latency: %v", err)
	}
	if m := got4.Usage.LatencyNS; m.State != UsageEstimated || m.Value == nil || *m.Value != 1_234_567 {
		t.Errorf("LatencyNS=%+v, want estimated 1234567", m)
	}

	// Model is bounded: an over-bound model fails loud.
	longModel := Usage{Model: strings.Repeat("m", MaxModelRunes+1)}
	if _, err := p.Update(context.Background(), id, "run-1", got4.Version, Update{Usage: &longModel}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("over-bound model error=%v, want ErrInvalidInput", err)
	}
	// An invalid per-measure state fails loud.
	badState := Usage{PromptTokens: UsageMeasure{State: UsageState("bogus")}}
	if _, err := p.Update(context.Background(), id, "run-1", got4.Version, Update{Usage: &badState}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bogus measure state error=%v, want ErrInvalidInput", err)
	}
}

// TestLifecycle_PendingAndPauseInvariantMatrix pins the honest pending
// lifecycle state and the pause invariants as a matrix:
//
//   - pending / running / terminal rows cannot carry an ACTIVE pause;
//   - a paused row REQUIRES an active, available, valid pause;
//   - a resume (paused → running) must clear the episode explicitly;
//   - terminal sealing resolves / clears the active pause without
//     retaining any callback / approval authority.
func TestLifecycle_PendingAndPauseInvariantMatrix(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()

	// pending is an honest mutable lifecycle state: the run has been
	// created but has not started executing.
	pend, err := p.Append(context.Background(), id, Append{TurnID: "pend-1", Query: "q", Status: StatusPending})
	if err != nil {
		t.Fatalf("pending append: %v", err)
	}
	if pend.Status != StatusPending || !pend.Status.Mutable() || pend.Sealed {
		t.Errorf("pending row wrong: %+v", pend)
	}
	if pend.Pause.Availability != CompletenessUnavailable {
		t.Errorf("pending row must carry no pause episode: %+v", pend.Pause)
	}

	active := Pause{Class: PauseClassHitlApproval, Lifecycle: PauseLifecycleActive}
	requested := Pause{Class: PauseClassSteering, Lifecycle: PauseLifecycleRequested}

	// Append: paused requires an active available pause.
	if _, err := p.Append(context.Background(), id, Append{TurnID: "p-no", Query: "q", Status: StatusPaused}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("paused append without pause error=%v, want ErrInvalidInput", err)
	}
	if _, err := p.Append(context.Background(), id, Append{TurnID: "p-ok", Query: "q", Status: StatusPaused, Pause: &active}); err != nil {
		t.Errorf("paused append with active pause rejected: %v", err)
	}
	// Append: pending / running cannot carry an active pause.
	for _, st := range []Status{StatusPending, StatusRunning} {
		if _, err := p.Append(context.Background(), id, Append{TurnID: TurnID("active-on-" + string(st)), Query: "q", Status: st, Pause: &active}); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("%s append with active pause error=%v, want ErrInvalidInput", st, err)
		}
	}
	// A REQUESTED episode is the honest pre-quiesce state on a
	// non-paused row.
	if _, err := p.Append(context.Background(), id, Append{TurnID: "req-ok", Query: "q", Status: StatusRunning, Pause: &requested}); err != nil {
		t.Errorf("running append with requested pause rejected: %v", err)
	}

	// pending → running via Update.
	run, err := p.Update(context.Background(), id, "pend-1", pend.Version, Update{Status: StatusRunning})
	if err != nil {
		t.Fatalf("pending→running update: %v", err)
	}
	if run.Status != StatusRunning {
		t.Errorf("status after update=%q, want running", run.Status)
	}
	// running → paused without an active pause is refused.
	if _, err := p.Update(context.Background(), id, "pend-1", run.Version, Update{Status: StatusPaused}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("running→paused without pause error=%v, want ErrInvalidInput", err)
	}
	// running → paused WITH an active pause succeeds.
	pz, err := p.Update(context.Background(), id, "pend-1", run.Version, Update{Status: StatusPaused, Pause: &active})
	if err != nil {
		t.Fatalf("running→paused with pause: %v", err)
	}
	if pz.Status != StatusPaused || pz.Pause.Availability != CompletenessComplete || pz.Pause.Lifecycle != PauseLifecycleActive {
		t.Errorf("paused row must carry an active available pause: %+v", pz)
	}
	// A paused row cannot carry a non-active (requested) episode.
	if _, err := p.Update(context.Background(), id, "pend-1", pz.Version, Update{Status: StatusPaused, Pause: &requested}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("paused with requested pause error=%v, want ErrInvalidInput", err)
	}
	// Resume (paused → running) WITHOUT clearing the active pause is
	// refused — the invariants are enforced, never silently dropped.
	if _, err := p.Update(context.Background(), id, "pend-1", pz.Version, Update{Status: StatusRunning}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("resume without clearing pause error=%v, want ErrInvalidInput", err)
	}
	// Resume WITH an explicit clear succeeds and leaves no active
	// pause.
	cleared := Pause{Availability: CompletenessUnavailable}
	resumed, err := p.Update(context.Background(), id, "pend-1", pz.Version, Update{Status: StatusRunning, Pause: &cleared})
	if err != nil {
		t.Fatalf("resume with clear: %v", err)
	}
	if resumed.Status != StatusRunning || resumed.Pause.Availability != CompletenessUnavailable {
		t.Errorf("resumed row wrong: %+v", resumed)
	}

	// Terminal sealing resolves / clears the active pause without
	// retaining callback / approval authority: seal a paused row.
	sealed, err := p.Seal(context.Background(), id, "pend-1", resumed.Version, Seal{Status: StatusCancelled, FinishReason: FinishCancelled})
	if err != nil {
		t.Fatalf("seal paused row: %v", err)
	}
	if sealed.Status != StatusCancelled || sealed.Pause.Availability != CompletenessUnavailable || sealed.Pause.Class != "" {
		t.Errorf("sealed row must clear the pause episode: %+v", sealed.Pause)
	}
}

// TestTerminalFields_BoundedEnumsAndMessages pins the P5 terminal
// contract: finish reason and error class are CLOSED enums (no
// unbounded free-form strings), and the terminal messages are bounded
// redacted consumer-safe text (empty = none available; over-bound or
// control-laden input fails loud). The sealed row and the operations
// projection carry the same bounded shape.
func TestTerminalFields_BoundedEnumsAndMessages(t *testing.T) {
	p, _ := newTestProjector(t, 0, false)
	id := tripleA()
	row, err := appendTurn(p, id, "run-1")
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	// Unknown enums fail loud — a free-form reason/class cannot reach
	// the row.
	if _, err := p.Seal(context.Background(), id, "run-1", row.Version, Seal{Status: StatusCancelled, FinishReason: FinishReason("bogus_reason")}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bogus finish reason error=%v, want ErrInvalidInput", err)
	}
	if _, err := p.Seal(context.Background(), id, "run-1", row.Version, Seal{Status: StatusFailed, ErrorClass: ErrorClass("bogus_class")}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("bogus error class error=%v, want ErrInvalidInput", err)
	}
	// Over-bound / control-laden terminal messages fail loud.
	if _, err := p.Seal(context.Background(), id, "run-1", row.Version, Seal{Status: StatusCancelled, FinishMessage: strings.Repeat("m", MaxTerminalMessageRunes+1)}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("over-bound finish message error=%v, want ErrInvalidInput", err)
	}
	if _, err := p.Seal(context.Background(), id, "run-1", row.Version, Seal{Status: StatusFailed, ErrorClass: ErrorClassTransient, ErrorMessage: "bad\x00msg"}); !errors.Is(err, ErrInvalidInput) {
		t.Errorf("control-laden error message error=%v, want ErrInvalidInput", err)
	}

	// Valid closed enums + bounded redacted messages land on the row.
	sealed, err := p.Seal(context.Background(), id, "run-1", row.Version, Seal{
		Status:        StatusFailed,
		FinishReason:  FinishNoPath,
		ErrorClass:    ErrorClassTransient,
		FinishMessage: "no path to goal",
		ErrorMessage:  "transient tool failure",
	})
	if err != nil {
		t.Fatalf("failed seal: %v", err)
	}
	if sealed.FinishReason != FinishNoPath || sealed.ErrorClass != ErrorClassTransient {
		t.Errorf("sealed terminal enums wrong: reason=%q class=%q", sealed.FinishReason, sealed.ErrorClass)
	}
	if sealed.FinishMessage != "no path to goal" || sealed.ErrorMessage != "transient tool failure" {
		t.Errorf("sealed terminal messages wrong: %+v", sealed)
	}

	// The operations projection carries the same bounded shape.
	ops, err := p.OpsTurn(context.Background(), id, "run-1")
	if err != nil {
		t.Fatalf("OpsTurn: %v", err)
	}
	if ops.FinishReason != FinishNoPath || ops.ErrorClass != ErrorClassTransient ||
		ops.FinishMessage != "no path to goal" || ops.ErrorMessage != "transient tool failure" {
		t.Errorf("ops terminal fields wrong: %+v", ops)
	}

	// An empty finish reason is the honest "not reported" — a complete
	// seal with a definite answer and no reason is accepted.
	row2, err := appendTurn(p, id, "run-2")
	if err != nil {
		t.Fatalf("append 2: %v", err)
	}
	ans := Answer{State: AnswerStateInline, Inline: "done"}
	row2, err = p.Update(context.Background(), id, "run-2", row2.Version, Update{Answer: &ans})
	if err != nil {
		t.Fatalf("update answer: %v", err)
	}
	complete, err := p.Seal(context.Background(), id, "run-2", row2.Version, Seal{Status: StatusComplete})
	if err != nil {
		t.Fatalf("complete seal without finish reason: %v", err)
	}
	if complete.FinishReason != "" || complete.FinishMessage != "" {
		t.Errorf("unreported finish reason/message must stay empty: %+v", complete)
	}
}
