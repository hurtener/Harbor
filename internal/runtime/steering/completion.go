package steering

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
)

type trustedCompletionHookContextKey struct{}

// WithTrustedCompletionHook marks the runtime-owned completion-hook path.
// This narrowly scoped bypass is not available to planner decisions.
func WithTrustedCompletionHook(ctx context.Context) context.Context {
	return context.WithValue(ctx, trustedCompletionHookContextKey{}, true)
}

// IsTrustedCompletionHook reports whether ctx was marked by the completion
// hook dispatcher.
func IsTrustedCompletionHook(ctx context.Context) bool {
	trusted, ok := ctx.Value(trustedCompletionHookContextKey{}).(bool)
	return ok && trusted
}

// DefaultCompletionHookTimeout bounds the detached run-completion hook
// dispatch when the configured timeout is non-positive. The dispatch runs
// under a context detached from the (possibly already-cancelled) run ctx
// but bounded by this timeout, so a wedged sink can never leak the hook
// goroutine.
const DefaultCompletionHookTimeout = 10 * time.Second

// RunCompletionPayloadFormatVersion is the pinned schema version of the
// [RunCompletionPayload] JSON. The payload leaves the process to operator
// sinks, so it is a public contract: an incompatible shape change bumps
// this value.
const RunCompletionPayloadFormatVersion = 1

// CompletionHookSpec is the per-run run-completion hook configuration on
// [RunSpec.CompletionHook]. When set, [RunLoop.Run] fires the hook exactly
// once at its terminal boundary — every terminal outcome, never mid-run or
// on pause — dispatching the run's [RunCompletionPayload] to the named
// catalog tool through the run's [ToolExecutor]. A hook failure never
// alters the run outcome.
//
// It is per-run immutable input, resolved once at run start (agent-config
// hooks section over static yaml over none) and pinned into the run's
// snapshot; an edit is invisible to an in-flight run by construction.
type CompletionHookSpec struct {
	// Tool is the catalog tool name the transcript is dispatched to. The
	// executor resolves it against the FULL catalog, so the hook target
	// need not be exposed to the planner. An empty Tool disables the hook
	// (treated as no hook configured).
	Tool string
	// Timeout bounds the detached dispatch. A non-positive value falls
	// back to DefaultCompletionHookTimeout.
	Timeout time.Duration
	// AgentID is the acting agent's registration id, carried as payload
	// metadata (never an isolation key — §6). Empty for a bare embed run
	// whose wiring layer knows no agent.
	AgentID string
}

// TranscriptEntry is one turn in the ordered run transcript. Role is
// "user" or "assistant"; Kind classifies the entry ("goal",
// "user_message", "redirect", "assistant", "tool", "final_answer"). Step
// is the trajectory index the entry precedes (steering entries) or occupies
// (assistant / tool entries); it is 0 for the initial goal and the final
// answer. At is the wall-clock instant when known (assistant / tool
// entries carry the step's start time; steering entries and the goal /
// final answer carry the zero time).
type TranscriptEntry struct {
	Role    string     `json:"role"`
	Kind    string     `json:"kind"`
	Content string     `json:"content"`
	Step    int        `json:"step"`
	At      *time.Time `json:"at,omitempty"`
}

// Transcript entry roles and kinds — stable, low-cardinality (part of the
// public payload contract).
const (
	transcriptRoleUser      = "user"
	transcriptRoleAssistant = "assistant"

	transcriptKindGoal        = "goal"
	transcriptKindUserMessage = "user_message"
	transcriptKindRedirect    = "redirect"
	transcriptKindAssistant   = "assistant"
	transcriptKindTool        = "tool"
	transcriptKindFinalAnswer = "final_answer"
)

// RunCompletionPayload is the typed, golden-pinned, versioned payload the
// run-completion hook dispatches to the named catalog tool. It leaves the
// process to operator sinks, so it is treated as a public contract from day
// one (RFC §6.17). It carries run metadata plus the ordered conversation
// assembled from live run state at completion; raw tool observations are
// deliberately excluded (the transcript is conversation-shaped, not a
// trajectory dump).
type RunCompletionPayload struct {
	// FormatVersion is the pinned schema version (RunCompletionPayloadFormatVersion).
	FormatVersion int `json:"format_version"`
	// TenantID / UserID / SessionID / RunID are the run's identity
	// quadruple.
	TenantID  string `json:"tenant_id"`
	UserID    string `json:"user_id"`
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id"`
	// AgentID is the acting agent's registration id (metadata, never an
	// isolation key — §6). Empty for a bare embed run.
	AgentID string `json:"agent_id,omitempty"`
	// Outcome is the terminal outcome: "goal" / "no_path" /
	// "constraints_conflict" / "cancelled" / "deadline_exceeded" / "error".
	Outcome string `json:"outcome"`
	// StartedAt / CompletedAt bound the run; DurationMS is their delta.
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	DurationMS  int64     `json:"duration_ms"`
	// StepCount is the number of trajectory steps.
	StepCount int `json:"step_count"`
	// ToolInvocations is the true tool-invocation count over the trajectory
	// (planner.CountToolInvocations — attempted invocations, counting a
	// CallParallel as its branch count). The hook dispatch itself is NOT
	// counted (it appends no trajectory step).
	ToolInvocations int `json:"tool_invocations"`
	// Conversation is the ordered transcript: initial goal, steering user
	// messages / redirects in arrival order, per-step assistant preambles
	// and compact tool lines, and the final answer.
	Conversation []TranscriptEntry `json:"conversation"`
}

// steeringEntry is one applied steering USER_MESSAGE / REDIRECT captured
// from live run state during a run, tagged with the trajectory index it
// precedes. Per-run loop state on the run goroutine's stack — never a
// RunLoop-struct field (the concurrent-reuse contract: per-run state lives
// on the run goroutine, never on the compiled artifact).
type steeringEntry struct {
	kind    ControlType
	content string
	step    int
}

// captureSteeringEntry extracts a run-completion transcript entry from a
// drained USER_MESSAGE / REDIRECT control event, tagged with the trajectory
// index it precedes. It returns (entry, false) for any other control type
// or an empty payload — those never enter the transcript.
func captureSteeringEntry(ev ControlEvent, step int) (steeringEntry, bool) {
	switch ev.Type {
	case ControlUserMessage:
		if msg, ok := stringFromPayload(ev.Payload, "message"); ok && msg != "" {
			return steeringEntry{kind: ControlUserMessage, content: msg, step: step}, true
		}
	case ControlRedirect:
		if g, ok := stringFromPayload(ev.Payload, "goal"); ok && g != "" {
			return steeringEntry{kind: ControlRedirect, content: g, step: step}, true
		}
	}
	return steeringEntry{}, false
}

// trajStepLen returns the number of appended steps on a trajectory, or 0 for
// a nil trajectory. It is the transcript's interleave key at capture time.
func trajStepLen(t *planner.Trajectory) int {
	if t == nil {
		return 0
	}
	return len(t.Steps)
}

// outcomeFor derives the payload's terminal outcome string from the run's
// settled (fin, err). A non-nil err classifies as "cancelled" when it
// wraps context.Canceled, "deadline_exceeded" when it wraps
// context.DeadlineExceeded, else "error"; a nil err projects the planner's
// FinishReason. The outcome rides IN the payload — the hook fires for every
// terminal outcome, so the outcome is never encoded in WHETHER the hook
// fires (RFC §6.17).
func outcomeFor(fin planner.Finish, err error) string {
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return "cancelled"
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return "deadline_exceeded"
		}
		return "error"
	}
	switch fin.Reason {
	case planner.FinishGoal:
		return "goal"
	case planner.FinishNoPath:
		return "no_path"
	case planner.FinishCancelled:
		return "cancelled"
	case planner.FinishDeadlineExceeded:
		return "deadline_exceeded"
	case planner.FinishConstraintsConflict:
		return "constraints_conflict"
	default:
		// An unknown / empty FinishReason on a nil-error return is a
		// runtime invariant slip; classify it as "error" rather than
		// emit an empty outcome.
		return "error"
	}
}

// assembleTranscript interleaves the run's live state into the ordered
// conversation: the initial goal (user), then per trajectory index — the
// steering user messages / redirects captured before that step (user, in
// arrival order), the step's assistant preamble (assistant, when non-empty),
// and one compact `{tool: ok|err}` line per tool invocation on that step
// (assistant) — and finally the run's terminal answer (assistant). Raw tool
// observations are never included.
func assembleTranscript(initialGoal string, traj *planner.Trajectory, steering []steeringEntry, finalAnswer string, hasFinalAnswer bool) []TranscriptEntry {
	out := make([]TranscriptEntry, 0, len(steering)+8)
	if initialGoal != "" {
		out = append(out, TranscriptEntry{
			Role:    transcriptRoleUser,
			Kind:    transcriptKindGoal,
			Content: initialGoal,
			Step:    0,
		})
	}

	var steps []planner.Step
	if traj != nil {
		steps = traj.Steps
	}

	// emitSteeringAt appends every steering entry captured before trajectory
	// index i, preserving arrival order.
	emitSteeringAt := func(i int) {
		for _, se := range steering {
			if se.step != i {
				continue
			}
			out = append(out, TranscriptEntry{
				Role:    transcriptRoleUser,
				Kind:    steeringKind(se.kind),
				Content: se.content,
				Step:    se.step,
			})
		}
	}

	for i := range steps {
		emitSteeringAt(i)
		step := steps[i]
		if step.AssistantPreamble != "" {
			out = append(out, TranscriptEntry{
				Role:    transcriptRoleAssistant,
				Kind:    transcriptKindAssistant,
				Content: step.AssistantPreamble,
				Step:    i,
				At:      nonZeroTime(step.StartedAt),
			})
		}
		for _, line := range toolLines(step) {
			out = append(out, TranscriptEntry{
				Role:    transcriptRoleAssistant,
				Kind:    transcriptKindTool,
				Content: line,
				Step:    i,
				At:      nonZeroTime(step.StartedAt),
			})
		}
	}
	// Steering entries captured after the last trajectory step (a drain-only
	// final boundary, or a run that finished immediately) trail the steps.
	emitSteeringAt(len(steps))

	if hasFinalAnswer {
		out = append(out, TranscriptEntry{
			Role:    transcriptRoleAssistant,
			Kind:    transcriptKindFinalAnswer,
			Content: finalAnswer,
			Step:    len(steps),
		})
	}
	return out
}

// nonZeroTime returns a pointer to t, or nil when t is the zero time, so a
// step with no captured start time omits the transcript entry's `at` field.
func nonZeroTime(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}

// steeringKind maps the captured control type onto the transcript kind.
func steeringKind(t ControlType) string {
	if t == ControlRedirect {
		return transcriptKindRedirect
	}
	return transcriptKindUserMessage
}

// toolLines renders a step's tool invocation(s) as compact `name: ok|err`
// lines — one per invocation (a CallParallel yields one per branch). A step
// whose action is not a tool invocation (Finish / RequestPause / Spawn /
// Await) yields no lines. Raw observations are never rendered.
func toolLines(step planner.Step) []string {
	status := "ok"
	if stepFailed(step) {
		status = "err"
	}
	switch d := step.Action.(type) {
	case planner.CallTool:
		return []string{fmt.Sprintf("%s: %s", d.Tool, status)}
	case *planner.CallTool:
		if d == nil {
			return nil
		}
		return []string{fmt.Sprintf("%s: %s", d.Tool, status)}
	case planner.CallParallel:
		lines := make([]string, 0, len(d.Branches))
		for _, b := range d.Branches {
			lines = append(lines, fmt.Sprintf("%s: %s", b.Tool, status))
		}
		return lines
	case *planner.CallParallel:
		if d == nil {
			return nil
		}
		lines := make([]string, 0, len(d.Branches))
		for _, b := range d.Branches {
			lines = append(lines, fmt.Sprintf("%s: %s", b.Tool, status))
		}
		return lines
	default:
		return nil
	}
}

// stepFailed reports whether a trajectory step recorded a tool failure. The
// runloop stamps a failed dispatch's observation as a map carrying an
// "error" key (it does not set Step.Error on that path), so both channels
// are checked.
func stepFailed(step planner.Step) bool {
	if step.Error != "" {
		return true
	}
	if m, ok := step.Observation.(map[string]any); ok {
		if _, has := m["error"]; has {
			return true
		}
	}
	return false
}

// renderFinalAnswer projects a terminal Finish.Payload onto the transcript's
// final-answer string. A string / []byte / json.RawMessage payload is
// rendered verbatim; any other value is JSON-encoded. A nil payload yields
// ("", false) — no final-answer entry.
func renderFinalAnswer(fin planner.Finish) (string, bool) {
	switch v := fin.Payload.(type) {
	case nil:
		return "", false
	case string:
		return v, v != ""
	case []byte:
		return string(v), len(v) > 0
	case json.RawMessage:
		return string(v), len(v) > 0
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "", false
		}
		return string(b), true
	}
}

// buildRunCompletionPayload assembles the typed payload from the run's
// settled outcome and live state. ToolInvocations reads the trajectory via
// planner.CountToolInvocations (the true tool-invocation count) — the hook dispatch
// itself is not counted (it appends no step).
func buildRunCompletionPayload(q identity.Quadruple, agentID, outcome string, startedAt, completedAt time.Time, initialGoal string, traj *planner.Trajectory, steering []steeringEntry, fin planner.Finish) RunCompletionPayload {
	stepCount := 0
	if traj != nil {
		stepCount = len(traj.Steps)
	}
	finalAnswer, hasFinal := renderFinalAnswer(fin)
	return RunCompletionPayload{
		FormatVersion:   RunCompletionPayloadFormatVersion,
		TenantID:        q.TenantID,
		UserID:          q.UserID,
		SessionID:       q.SessionID,
		RunID:           q.RunID,
		AgentID:         agentID,
		Outcome:         outcome,
		StartedAt:       startedAt.UTC(),
		CompletedAt:     completedAt.UTC(),
		DurationMS:      completedAt.Sub(startedAt).Milliseconds(),
		StepCount:       stepCount,
		ToolInvocations: planner.CountToolInvocations(traj),
		Conversation:    assembleTranscript(initialGoal, traj, steering, finalAnswer, hasFinal),
	}
}

// fireCompletionHook dispatches the run's transcript to the configured
// catalog tool through spec.ToolExecutor at the run's terminal boundary. It
// runs from a deferred closure over Run's named returns AFTER (fin, err) are
// settled, and can NEVER alter them: a dispatch failure emits run.hook_failed
// (metadata only) + a Warn log; a success emits run.hook_dispatched. The
// dispatch runs under a context detached from the (possibly cancelled) run
// ctx but bounded by the hook timeout, values preserved (the identity
// quadruple keeps flowing) — the same bridge the tool-auth subsystem uses
// for post-cancellation work. The dispatch does NOT invoke OnToolDispatched,
// append a trajectory step, or advance the tool-invocation counters — it is not a
// planner tool invocation.
//
// Because this runs inside Run's deferred fire, a panic escaping it would
// replace the run's settled result with the panic. The recover below closes
// that hole: an executor-internal panic is contained, classified, and
// surfaced through the same run.hook_failed posture as any other dispatch
// failure — the ONE justified use of recover on this path (protecting a
// settled result from third-party sink code, not control flow).
func (rl *RunLoop) fireCompletionHook(runCtx context.Context, spec RunSpec, q identity.Quadruple, fin planner.Finish, runErr error, steering []steeringEntry, initialGoal string, startedAt time.Time) {
	hook := spec.CompletionHook
	if hook == nil || hook.Tool == "" {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			rl.completionHookFailed(runCtx, q, hook.Tool, outcomeFor(fin, runErr), "panic")
		}
	}()
	outcome := outcomeFor(fin, runErr)
	completedAt := rl.clock.Now()
	payload := buildRunCompletionPayload(q, hook.AgentID, outcome, startedAt, completedAt, initialGoal, spec.Base.Trajectory, steering, fin)

	args, err := json.Marshal(payload)
	if err != nil {
		rl.completionHookFailed(runCtx, q, hook.Tool, outcome, "encode_failed")
		return
	}
	if spec.ToolExecutor == nil {
		rl.completionHookFailed(runCtx, q, hook.Tool, outcome, "no_executor")
		return
	}

	timeout := hook.Timeout
	if timeout <= 0 {
		timeout = DefaultCompletionHookTimeout
	}
	// Detach cancellation (the run ctx may already be dead on the cancel
	// path) while preserving the ctx VALUES (the identity quadruple keeps
	// flowing — never context.Background(), which would drop identity), and
	// bound the detachment with the hook timeout.
	hookCtx, cancel := context.WithTimeout(context.WithoutCancel(runCtx), timeout)
	defer cancel()

	dispatchStart := rl.clock.Now()
	rc := planner.RunContext{Quadruple: q, Goal: initialGoal, Trajectory: spec.Base.Trajectory}
	hookCtx = WithTrustedCompletionHook(hookCtx)
	if _, _, execErr := spec.ToolExecutor.ExecuteDecision(hookCtx, rc, planner.CallTool{Tool: hook.Tool, Args: args}); execErr != nil {
		rl.completionHookFailed(runCtx, q, hook.Tool, outcome, classifyHookErr(execErr))
		return
	}
	rl.completionHookDispatched(runCtx, q, hook.Tool, outcome, len(args), len(payload.Conversation), rl.clock.Now().Sub(dispatchStart))
}

// classifyHookErr maps a hook-dispatch error to a stable, low-cardinality,
// redaction-safe class for the run.hook_failed event. The raw error message
// is never used — it may quote caller data (§7).
func classifyHookErr(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, ErrDecisionShapeUnsupported):
		return "unsupported_shape"
	default:
		return "dispatch_failed"
	}
}

// completionHookDispatched emits the run.hook_dispatched observability event
// (metadata only — never transcript content). A nil bus is a no-op; a
// publish failure is logged, never escalated.
func (rl *RunLoop) completionHookDispatched(ctx context.Context, q identity.Quadruple, tool, outcome string, transcriptBytes, entryCount int, dur time.Duration) {
	if rl.bus == nil {
		return
	}
	ev := events.Event{
		Type:     EventTypeRunHookDispatched,
		Identity: q,
		Payload: RunHookDispatchedPayload{
			Tool:            tool,
			Outcome:         outcome,
			DurationMS:      dur.Milliseconds(),
			TranscriptBytes: transcriptBytes,
			EntryCount:      entryCount,
		},
	}
	if err := rl.bus.Publish(ctx, ev); err != nil {
		rl.logger.WarnContext(ctx, "steering: run.hook_dispatched publish failed",
			slog.String("run_id", q.RunID),
			slog.String("tool", tool),
			slog.Any("error", err))
	}
}

// completionHookFailed emits the run.hook_failed observability event
// (metadata only — identity, tool, outcome, error class; never transcript
// content or raw caller-quoting error text) AND Warn-logs. The Warn carries
// the CLASSIFIED error class, never the raw error text — a sink's error
// message may quote transcript/caller data, and the hook's whole hygiene
// posture is that such bytes never reach a log line or the bus (§7). A hook
// failure never alters the run outcome (§13 — fail loud, never silent,
// never escalated into the run result).
func (rl *RunLoop) completionHookFailed(ctx context.Context, q identity.Quadruple, tool, outcome, errClass string) {
	rl.logger.WarnContext(ctx, "steering: run-completion hook dispatch failed — run outcome unchanged",
		slog.String("tenant_id", q.TenantID),
		slog.String("user_id", q.UserID),
		slog.String("session_id", q.SessionID),
		slog.String("run_id", q.RunID),
		slog.String("tool", tool),
		slog.String("outcome", outcome),
		slog.String("error_class", errClass))
	if rl.bus == nil {
		return
	}
	ev := events.Event{
		Type:     EventTypeRunHookFailed,
		Identity: q,
		Payload: RunHookFailedPayload{
			Tool:       tool,
			Outcome:    outcome,
			ErrorClass: errClass,
		},
	}
	if err := rl.bus.Publish(ctx, ev); err != nil {
		rl.logger.WarnContext(ctx, "steering: run.hook_failed publish failed",
			slog.String("run_id", q.RunID),
			slog.String("tool", tool),
			slog.Any("error", err))
	}
}
