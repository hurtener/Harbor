package approval

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/state"
)

// The durable approval decision record.
//
// Resuming a pause deletes its checkpoint, and the decision itself is
// otherwise only on the event stream. The decision record keeps the
// most recent resolved approvals of a session in the StateStore, keyed
// by the gated session's own (tenant, user, session) triple, so a host
// can read who decided what after the run moved on or the process
// restarted. sessions.inspect projects it as the session's recent
// interventions. Retention is bounded: each write prunes the session
// down to MaxRecordedDecisions entries, and session erasure removes the
// rows with every other session-scoped record.

// DecisionOutcome is how a gated approval resolved.
type DecisionOutcome string

const (
	// OutcomeApproved — an approver approved; the tool ran.
	OutcomeApproved DecisionOutcome = "approved"
	// OutcomeRejected — an approver rejected; the tool did not run.
	OutcomeRejected DecisionOutcome = "rejected"
	// OutcomeExpired — the max-park deadline elapsed with no decision;
	// the tool did not run.
	OutcomeExpired DecisionOutcome = "expired"
	// OutcomeInterrupted — the approval waiter could no longer continue.
	OutcomeInterrupted DecisionOutcome = "interrupted"
)

// MaxRecordedDecisions bounds the decision records retained per
// session (newest kept).
const MaxRecordedDecisions = 20

// maxArgsSummaryBytes bounds the redacted args summary stored on a
// record; a larger summary is replaced by a size marker.
const maxArgsSummaryBytes = 2048

// DecidedBy is the stored attribution of a decision: the verified
// principal whose control decided it. Empty fields mean no verified
// principal decided (expiry, restart).
type DecidedBy struct {
	TenantID  string `json:"tenant_id,omitempty"`
	UserID    string `json:"user_id,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	Speaker   string `json:"speaker,omitempty"`
}

func deciderRecord(d pauseresume.Decider) DecidedBy {
	return DecidedBy{TenantID: d.TenantID, UserID: d.UserID, SessionID: d.SessionID, Speaker: d.Speaker}
}

// DecisionRecord is one resolved approval.
type DecisionRecord struct {
	// Tool is the gated tool's catalog name.
	Tool string `json:"tool"`
	// PauseToken correlates with the approval's events.
	PauseToken string `json:"pause_token"`
	// Outcome is how the approval resolved.
	Outcome DecisionOutcome `json:"outcome"`
	// PolicyReason is why the policy required approval.
	PolicyReason string `json:"policy_reason,omitempty"`
	// DecisionReason is the approver's note, or the system reason for an
	// expired / interrupted approval.
	DecisionReason string `json:"decision_reason,omitempty"`
	// DecidedBy names the deciding principal.
	DecidedBy DecidedBy `json:"decided_by"`
	// RequestedAt is when the approval was requested.
	RequestedAt time.Time `json:"requested_at"`
	// DecidedAt is when it resolved.
	DecidedAt time.Time `json:"decided_at"`
	// ArgsSummary is the audit-redacted args summary the approval
	// request carried (bounded; never the original args). Absent for an
	// interrupted approval, whose summary was never persisted.
	ArgsSummary any `json:"args_summary,omitempty"`
}

// DecisionLog is the durable decision record seam.
type DecisionLog interface {
	// Record stores rec under the gated session's identity and prunes
	// the session to MaxRecordedDecisions entries.
	Record(ctx context.Context, id identity.Identity, rec DecisionRecord) error
	// Recent returns the session's recorded decisions, newest first.
	Recent(ctx context.Context, id identity.Identity) ([]DecisionRecord, error)
}

// decisionKindPrefix namespaces one decision record per pause token.
const decisionKindPrefix = "tools.approval.decision."

// ErrDecisionLogMisconfigured — NewStateDecisionLog was called with a
// nil StateStore.
var ErrDecisionLogMisconfigured = errors.New("approval: NewStateDecisionLog requires a StateStore")

type stateDecisionLog struct {
	store state.StateStore
}

// NewStateDecisionLog builds the decision record over a StateStore.
// The returned log is immutable and safe for concurrent use.
func NewStateDecisionLog(store state.StateStore) (DecisionLog, error) {
	if store == nil {
		return nil, ErrDecisionLogMisconfigured
	}
	return &stateDecisionLog{store: store}, nil
}

// Record implements DecisionLog.
func (l *stateDecisionLog) Record(ctx context.Context, id identity.Identity, rec DecisionRecord) error {
	if err := identity.Validate(id); err != nil {
		return fmt.Errorf("approval: decision record: %w", err)
	}
	if rec.PauseToken == "" {
		return errors.New("approval: decision record: pause token is empty")
	}
	rec.ArgsSummary = boundSummary(rec.ArgsSummary)
	b, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("approval: decision record encode: %w", err)
	}
	q := identity.Quadruple{Identity: id}
	kind := decisionKindPrefix + rec.PauseToken
	// First writer wins: a pause resolves once, so an existing record
	// for the token is the decision that committed.
	err = l.store.SaveIf(ctx, []state.SlotExpectation{{Identity: q, Kind: kind}}, state.StateRecord{
		ID:       state.NewEventID(),
		Identity: q,
		Kind:     kind,
		Bytes:    b,
	})
	if errors.Is(err, state.ErrConditionFailed) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("approval: decision record save: %w", err)
	}
	// Retention is best-effort and separate from the write: a prune
	// failure is logged and the next write retries it.
	if perr := l.prune(ctx, q); perr != nil {
		slog.WarnContext(ctx, "approval: decision record prune failed", slog.String("error", perr.Error()))
	}
	return nil
}

// prune deletes the session's oldest records beyond MaxRecordedDecisions.
// Concurrent prunes are safe: Delete is idempotent.
func (l *stateDecisionLog) prune(ctx context.Context, q identity.Quadruple) error {
	recs, err := l.list(ctx, q)
	if err != nil {
		return err
	}
	for _, r := range recs[min(len(recs), MaxRecordedDecisions):] {
		if err := l.store.Delete(ctx, q, decisionKindPrefix+r.PauseToken); err != nil {
			return fmt.Errorf("approval: decision record prune: %w", err)
		}
	}
	return nil
}

// Recent implements DecisionLog.
func (l *stateDecisionLog) Recent(ctx context.Context, id identity.Identity) ([]DecisionRecord, error) {
	if err := identity.Validate(id); err != nil {
		return nil, fmt.Errorf("approval: decision read: %w", err)
	}
	recs, err := l.list(ctx, identity.Quadruple{Identity: id})
	if err != nil {
		return nil, err
	}
	return recs[:min(len(recs), MaxRecordedDecisions)], nil
}

// list reads every decision record of the session, newest first. A
// corrupt row is skipped with a warning (kind only, never bytes).
func (l *stateDecisionLog) list(ctx context.Context, q identity.Quadruple) ([]DecisionRecord, error) {
	rows, err := l.store.ListKindForIdentity(ctx, q, decisionKindPrefix)
	if err != nil {
		return nil, fmt.Errorf("approval: decision record list: %w", err)
	}
	out := make([]DecisionRecord, 0, len(rows))
	for _, row := range rows {
		var rec DecisionRecord
		if err := json.Unmarshal(row.Bytes, &rec); err != nil || rec.PauseToken == "" {
			slog.WarnContext(ctx, "approval: skipping corrupt decision record", slog.String("kind", row.Kind))
			continue
		}
		out = append(out, rec)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DecidedAt.Equal(out[j].DecidedAt) {
			return out[i].PauseToken > out[j].PauseToken
		}
		return out[i].DecidedAt.After(out[j].DecidedAt)
	})
	return out, nil
}

// boundSummary keeps a redacted args summary only while its JSON form
// fits maxArgsSummaryBytes; a larger one becomes a size marker.
func boundSummary(v any) any {
	if v == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil {
		return map[string]any{"omitted": "unencodable"}
	}
	if len(b) > maxArgsSummaryBytes {
		return map[string]any{"omitted": "too_large", "bytes": len(b)}
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		return map[string]any{"omitted": "unencodable"}
	}
	return out
}

// NewDecisionRecorder returns the Coordinator resolution recorder that
// writes every committed approval-gate resolution to log
// (pauseresume.WithResolutionRecorder), with the Coordinator's decision
// time. The record is a convenience projection: a write failure is
// logged by the Coordinator and never blocks the decision; the events
// are the source of truth. Non-gate pauses are ignored.
func NewDecisionRecorder(log DecisionLog) pauseresume.ResolutionRecorder {
	return func(ctx context.Context, r pauseresume.Resolution) error {
		if !isGatePause(r.Pause) {
			return nil
		}
		tool := payloadString(r.Pause.Payload, "tool")
		policyReason := payloadString(r.Pause.Payload, "reason")
		rec := DecisionRecord{
			Tool:         tool,
			PauseToken:   string(r.Pause.Token),
			PolicyReason: policyReason,
			DecidedBy:    deciderRecord(r.Decider),
			RequestedAt:  r.Pause.PausedAt.UTC(),
			DecidedAt:    r.DecidedAt.UTC(),
			ArgsSummary:  r.Pause.Payload["args_summary"],
		}
		note := payloadString(r.ResumePayload, "reason")
		switch r.Decision {
		case pauseresume.DecisionApprove:
			rec.Outcome, rec.DecisionReason = OutcomeApproved, note
		case pauseresume.DecisionReject:
			rec.Outcome, rec.DecisionReason = OutcomeRejected, note
		case pauseresume.DecisionTimeout:
			rec.Outcome, rec.DecisionReason = OutcomeExpired, ExpiredReason
		case pauseresume.DecisionInterrupted:
			if note == "" {
				note = InterruptedReason
			}
			rec.Outcome, rec.DecisionReason = OutcomeInterrupted, note
		default:
			return nil
		}
		return log.Record(ctx, r.Pause.Identity, rec)
	}
}

// ResolveInterruptedApprovals is the boot-time pass for approvals a
// runtime restart left without a waiter. An approval gate keeps the
// gated call's original arguments in memory and waits on an in-process
// channel, so a new process cannot continue it: every such checkpoint
// is resolved with the typed `interrupted` Decision (pause.resumed; the
// Coordinator's resolution recorder records the outcome, best-effort) and
// `tool.rejected` is emitted with Decision "interrupted" and
// InterruptedReason. The gated run's task is failed by the
// task-recovery sweep (`runtime_restarted`); this pass makes the pause
// side match it instead of leaving an approval that looks actionable but
// cannot be approved. Run it before the pause sweeper starts. It assumes
// no other live runtime process shares the checkpoint store. Returns how
// many approvals were resolved.
func ResolveInterruptedApprovals(ctx context.Context, coord pauseresume.Coordinator, bus events.EventBus, logger *slog.Logger) (int, error) {
	if logger == nil {
		logger = slog.Default()
	}
	resolved, err := pauseresume.ResolveInterrupted(ctx, coord, isGatePause, logger)
	for _, p := range resolved {
		tool := payloadString(p.Payload, "tool")
		decidedAt := time.Now().UTC()
		if st, serr := coord.Status(ctx, p.Token); serr == nil && !st.ResumedAt.IsZero() {
			decidedAt = st.ResumedAt.UTC()
		}
		res := resolution{reason: InterruptedReason, decidedAt: decidedAt}
		if bus != nil {
			q := identity.Quadruple{Identity: p.Identity, RunID: p.RunID}
			if perr := publishRejectedEvent(ctx, bus, q, tool, p.Token, pauseresume.DecisionInterrupted, res); perr != nil {
				logger.WarnContext(ctx, "approval: interrupted-approval event publish failed",
					slog.String("pause_token", string(p.Token)), slog.String("error", perr.Error()))
			}
		}
	}
	return len(resolved), err
}

// isGatePause names the pauses an approval gate parks: approval-required
// with the gated tool's name on the payload (ResolveInterrupted already
// requires no trajectory and no continuation).
func isGatePause(p pauseresume.Pause) bool {
	if p.Reason != pauseresume.ReasonApprovalRequired {
		return false
	}
	tool, ok := p.Payload["tool"].(string)
	return ok && tool != ""
}

// InterventionReader adapts a DecisionLog to the sessions.inspect
// recent-interventions projection.
type InterventionReader struct {
	Log DecisionLog
}

// RecentInterventions projects the session's recorded decisions onto
// the wire summary, most recent first, capped at
// prototypes.MaxSessionInterventionSummaries.
func (r InterventionReader) RecentInterventions(ctx context.Context, id identity.Identity) ([]prototypes.InterventionSummary, error) {
	recs, err := r.Log.Recent(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]prototypes.InterventionSummary, 0, min(len(recs), prototypes.MaxSessionInterventionSummaries))
	for _, rec := range recs[:min(len(recs), prototypes.MaxSessionInterventionSummaries)] {
		s := prototypes.InterventionSummary{
			Type:           InterventionTypeToolApproval,
			Reason:         rec.PolicyReason,
			Outcome:        string(rec.Outcome),
			OccurredAt:     rec.RequestedAt,
			Tool:           rec.Tool,
			PauseToken:     rec.PauseToken,
			DecisionReason: rec.DecisionReason,
			DecidedAt:      rec.DecidedAt,
			ArgsSummary:    rec.ArgsSummary,
		}
		if rec.DecidedBy != (DecidedBy{}) {
			s.DecidedBy = &prototypes.InterventionDecider{
				TenantID:  rec.DecidedBy.TenantID,
				UserID:    rec.DecidedBy.UserID,
				SessionID: rec.DecidedBy.SessionID,
				Speaker:   rec.DecidedBy.Speaker,
			}
		}
		out = append(out, s)
	}
	return out, nil
}

// InterventionTypeToolApproval is the InterventionSummary.Type of a
// tool-approval gate decision.
const InterventionTypeToolApproval = "tool_approval"

// payloadString returns the string stored under key, or "" when the key
// is absent or holds another type.
func payloadString(m map[string]any, key string) string {
	if s, ok := m[key].(string); ok {
		return s
	}
	return ""
}
