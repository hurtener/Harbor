package materializer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools"
	mcpdrv "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// ---------------------------------------------------------------------------
// Canonical event → projection observation mapping.
//
// The materializer consumes ONLY canonical persisted events (the
// successfully-persisted source with the existing local sequence) from
// the closed families below. Every piece of information the projection
// carries is derived from one of these events; anything the events do
// not carry is materialized as the honest Unavailable / unknown /
// omitted state — never a fabricated value, never a silent zero
// (CLAUDE.md §13).
//
// The persisted source rehydrates payloads as events.RedactedMap (the
// durable log's generic post-persistence shape — Go struct field names
// as keys), so decoding is field-map based, not type-assertion based.
// Typed payloads (an in-memory source) decode identically via a JSON
// round-trip, so the mapping is source-shape independent.
// ---------------------------------------------------------------------------

// recognizedType reports whether t is in the materializer's closed
// canonical consumption set. Everything else (session.*, agent.*,
// notification.*, governance.*, memory.*, topology.*, ...) is not
// turn-relevant and is skipped.
func recognizedType(t events.EventType) bool {
	switch t {
	case tasks.EventTypeTaskSpawned,
		tasks.EventTypeTaskStarted,
		tasks.EventTypeTaskResumed,
		tasks.EventTypeTaskCompleted,
		tasks.EventTypeTaskFailed,
		tasks.EventTypeTaskCancelled,
		tasks.EventTypeTaskPaused,
		runctx.EventTypeInputDispositionResolved,
		planner.EventTypePlannerDecision,
		tools.EventTypeToolInvoked,
		tools.EventTypeToolCompleted,
		tools.EventTypeToolFailed,
		tools.EventTypeToolPolicyExhausted,
		mcpdrv.EventTypeMCPAppAvailable,
		pauseresume.EventTypePauseRequested,
		pauseresume.EventTypePauseResumed,
		llm.EventTypeCostRecorded:
		return true
	}
	return false
}

// payloadMap normalises any event payload to the generic field map the
// materializer decodes from: a RedactedMap (the durable source's
// rehydrated shape) passes its Data through; any typed payload is
// JSON-round-tripped so the same field-name keys result. A nil payload
// (structurally impossible from a valid event, defended anyway) yields
// ok=false.
func payloadMap(ev events.Event) (map[string]any, bool) {
	switch p := ev.Payload.(type) {
	case nil:
		return nil, false
	case events.RedactedMap:
		if p.Data == nil {
			return nil, false
		}
		return p.Data, true
	default:
		raw, err := json.Marshal(p)
		if err != nil {
			return nil, false
		}
		var m map[string]any
		if err := json.Unmarshal(raw, &m); err != nil {
			return nil, false
		}
		return m, true
	}
}

// --- typed field decoders over the generic payload map --------------

func fieldString(m map[string]any, key string) string {
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return fmt.Sprint(v)
}

func fieldInt64(m map[string]any, key string) int64 {
	v, ok := m[key]
	if !ok || v == nil {
		return 0
	}
	switch n := v.(type) {
	case float64:
		return int64(math.Round(n))
	case int64:
		return n
	case int:
		return int64(n)
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return i
		}
		f, err := n.Float64()
		if err == nil {
			return int64(math.Round(f))
		}
	}
	return 0
}

func fieldBool(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok || v == nil {
		return false
	}
	if b, ok := v.(bool); ok {
		return b
	}
	return false
}

func fieldTime(m map[string]any, key string) (time.Time, bool) {
	v, ok := m[key]
	if !ok || v == nil {
		return time.Time{}, false
	}
	switch t := v.(type) {
	case string:
		parsed, err := time.Parse(time.RFC3339Nano, t)
		if err != nil {
			return time.Time{}, false
		}
		return parsed, true
	case time.Time:
		return t, true
	}
	return time.Time{}, false
}

func fieldNested(m map[string]any, key string) map[string]any {
	v, ok := m[key]
	if !ok {
		return nil
	}
	if nested, ok := v.(map[string]any); ok {
		return nested
	}
	return nil
}

// ---------------------------------------------------------------------------
// applyEvent
// ---------------------------------------------------------------------------

// applyEvent processes one canonical persisted event. It returns
// applied=true when the event was routed to a session state and
// processed, skipped=true when it was deliberately not processed
// (unrecognised type, incomplete identity, unknown session, fenced
// session, sealed turn, unroutable run), or a wrapped error for a hard
// failure that must abort the pass.
func (m *Materializer) applyEvent(ctx context.Context, ev events.Event) (applied, skipped bool, err error) {
	if !recognizedType(ev.Type) {
		return false, true, nil
	}
	id, ok := splitIdentity(ev.Identity)
	if !ok {
		return false, true, nil
	}
	payload, ok := payloadMap(ev)
	if !ok {
		return false, true, nil
	}

	sess := m.sessions[id]
	if sess == nil {
		// First contact with this session: materialize the root
		// foreground spawn (which creates the session) or create the
		// session for a routed child/tool event. The durable erasure
		// probe is consulted ONCE at creation: an erased session is
		// never re-materialized from sequence zero (the restart gate
		// mirroring the projector's Reconcile).
		if m.probe != nil {
			erased, perr := m.probe.Erased(ctx, id)
			if perr != nil {
				return false, false, fmt.Errorf("materializer: erasure probe for %s: %w", id.SessionID, perr)
			}
			if erased {
				sess = &sessionState{id: id, fenced: true, runs: map[string]string{}, tasks: map[string]string{}, turns: map[turns.TurnID]*turnState{}}
				m.sessions[id] = sess
				return false, true, nil
			}
		}
		created, err := m.newSessionState(ctx, id)
		if err != nil {
			return false, false, fmt.Errorf("materializer: open session %s: %w", id.SessionID, err)
		}
		sess = created
		m.sessions[id] = sess
		m.passTouched++
	}
	if sess.fenced {
		return false, true, nil
	}
	if ev.Sequence <= sess.memSeq {
		// Same-instance re-application guard: an event at or below the
		// session's in-memory incorporated sequence was already FULLY
		// applied to this instance's accumulators (and, when it was
		// above the durable checkpoint at the time, to the store) — a
		// page retry after a mid-page failure re-pages the same events
		// and re-deriving them would double-count the accumulators.
		// This instance has already incorporated them: no-op.
		return true, false, nil
	}

	applied, err = m.applyToSession(ctx, sess, ev, payload)
	if err != nil {
		if errors.Is(err, turns.ErrErasureFenced) {
			// Erasure converged: the store-local fence refused the
			// write. The session is permanently fenced — skip it from
			// here on (no resurrection), never a hard pass failure.
			sess.fenced = true
			return false, true, nil
		}
		return false, false, err
	}
	if applied {
		m.passTouched++
		// The event is now fully incorporated into this instance's
		// in-memory state: advance the in-memory mirror so a
		// same-instance page retry never re-derives it (restart
		// catch-up re-derives events into a FRESH instance, whose
		// memSeq starts at zero).
		sess.memSeq = ev.Sequence
		// The session's state is now ahead of its durable checkpoint
		// (state-only restart catch-up at or below the checkpoint never
		// reaches here): advance the mirror and the store checkpoint
		// MONOTONICALLY — the store refuses a regression and an erased
		// (fenced) session refuses any write.
		if ev.Sequence > sess.checkpoint {
			sess.checkpoint = ev.Sequence
			if err := m.proj.AdvanceCheckpoint(ctx, id, ev.Sequence); err != nil {
				if errors.Is(err, turns.ErrErasureFenced) {
					// Erasure converged between the row write and the
					// checkpoint write: the session is now permanently
					// fenced — skip it from here on (no resurrection).
					sess.fenced = true
					return false, true, nil
				}
				return false, false, fmt.Errorf("materializer: advance checkpoint %s @%d: %w", id.SessionID, ev.Sequence, err)
			}
		}
	}
	return applied, !applied, nil
}

// applyToSession routes one recognized event to the owning turn and
// derives the projector observation. Returns applied=false for a
// deliberate skip (routing miss, sealed turn, unclassifiable step).
func (m *Materializer) applyToSession(ctx context.Context, sess *sessionState, ev events.Event, payload map[string]any) (bool, error) {
	switch ev.Type {
	case tasks.EventTypeTaskSpawned:
		return m.applyTaskSpawned(ctx, sess, ev, payload)
	case tasks.EventTypeTaskStarted, tasks.EventTypeTaskResumed:
		return m.applyTaskRunning(ctx, sess, ev, payload)
	case tasks.EventTypeTaskCompleted:
		return m.applyTaskCompleted(ctx, sess, ev, payload)
	case tasks.EventTypeTaskFailed:
		return m.applyTaskFailed(ctx, sess, ev, payload)
	case tasks.EventTypeTaskCancelled:
		return m.applyTaskCancelled(ctx, sess, ev, payload)
	case tasks.EventTypeTaskPaused:
		// task.paused is not the live pause path (the unified pause
		// primitive's events are pause.requested / pause.resumed); a
		// bare task.paused carries no pause class and cannot form a
		// valid pause episode — omitted by the explicit relationship
		// rule, never surfaced as a fabricated paused row.
		return false, nil
	case runctx.EventTypeInputDispositionResolved:
		return m.applyInputDisposition(ctx, sess, ev, payload)
	case planner.EventTypePlannerDecision:
		return m.applyPlannerDecision(ctx, sess, ev, payload)
	case tools.EventTypeToolInvoked:
		return m.applyToolInvoked(ctx, sess, ev, payload)
	case tools.EventTypeToolCompleted:
		return m.applyToolCompleted(ctx, sess, ev, payload)
	case tools.EventTypeToolFailed:
		return m.applyToolFailed(ctx, sess, ev, payload)
	case tools.EventTypeToolPolicyExhausted:
		return m.applyToolPolicyExhausted(ctx, sess, ev, payload)
	case mcpdrv.EventTypeMCPAppAvailable:
		return m.applyAppAvailable(ctx, sess, ev, payload)
	case pauseresume.EventTypePauseRequested:
		return m.applyPauseRequested(ctx, sess, ev, payload)
	case pauseresume.EventTypePauseResumed:
		return m.applyPauseResumed(ctx, sess, ev)
	case llm.EventTypeCostRecorded:
		return m.applyCostRecorded(ctx, sess, ev, payload)
	}
	return false, nil
}

// ---------------------------------------------------------------------------
// task lifecycle
// ---------------------------------------------------------------------------

// applyTaskSpawned selects root foreground runs (kind foreground, no
// parent task) as TURNS and folds every other spawn into the session's
// routing index only — child/background tasks never become user
// messages ("background/child tasks fold into the root turn's Activity
// or are omitted by an explicit relationship rule"; their trajectory
// presence reaches the parent turn through the parent's own planner
// decisions and tool dispatches).
//
// When the runtime wires the TaskSnapshotReader seam, the root spawn
// projection ALSO reads the already-redacted canonical task record
// under the event identity and captures the authoritative TaskID +
// RunID, the bounded renderable query + instant, the effective agent
// binding, and the input attachment metadata onto the append. The
// read is BOUND: a nonempty returned TaskID must equal the event's
// canonical task id (it can never replace it), and the run id must
// agree between the event envelope and the record — the record may
// FILL the run only when the event genuinely lacks one. The reader is
// invoked only while projecting this successfully persisted spawn
// event — never on a Protocol read. A nil reader or a missing record
// is the honest legacy gap (the components stay unavailable); a
// transient snapshot error or a binding mismatch aborts the projection
// without advancing the checkpoint.
func (m *Materializer) applyTaskSpawned(ctx context.Context, sess *sessionState, ev events.Event, payload map[string]any) (bool, error) {
	taskID := fieldString(payload, "TaskID")
	if taskID == "" {
		return false, nil
	}
	kind := fieldString(payload, "Kind")
	parent := fieldString(payload, "ParentTaskID")
	runID := runIDFromIdentity(ev.Identity)

	if runID != "" {
		sess.runs[runID] = taskID
	}
	sess.tasks[taskID] = parent

	isRootForeground := kind == string(tasks.KindForeground) && parent == ""
	if !isRootForeground {
		return false, nil
	}
	if hasPipe(taskID) {
		return false, nil
	}
	if _, exists := sess.turns[turns.TurnID(taskID)]; exists {
		// Idempotent re-append (a response-loss replay): the row
		// already exists; leave it untouched.
		return false, nil
	}

	// The task-record snapshot enriches the append with the
	// authoritative task/run identity, the bounded renderable query +
	// instant, the effective agent binding, and the input attachment
	// metadata. It is read on BOTH the fresh-apply path and restart
	// catch-up: on catch-up the append is a no-op, but the in-memory
	// input accumulator still needs the record's input metadata so a
	// later disposition update never replaces the row's inputs without
	// them (restart convergence). A nil reader / missing record leaves
	// the components honestly unavailable.
	snap, err := m.readTaskSnapshot(ctx, sess.id, taskID)
	if err != nil {
		return false, err
	}
	// Run binding at the turn's birth: the event envelope's run id and
	// the record's run id must agree when both are present; the record
	// may FILL the run only when the event genuinely lacks one. The
	// task-id binding was already enforced inside readTaskSnapshot (the
	// record can never replace the event's canonical task id).
	runID, err = bindRunID(runID, "", snap.RunID)
	if err != nil {
		return false, err
	}

	// Transactional creation: the durable append lands BEFORE the
	// turn's in-memory state is registered, so a failed append (a
	// mid-page store error, a transient snapshot error, or an
	// over-bound snapshot field refused by the projector) leaves no
	// phantom routing entry — the retry re-attempts the append cleanly
	// instead of seeing an "already exists" skip.
	if ev.Sequence > sess.checkpoint {
		var appendInputs []turns.Attachment
		var appendQuery string
		var appendQueryAt time.Time
		var appendAgentID, appendAgentName string
		var appendBinding turns.AgentBindingSource
		if snap.InputsPresent {
			appendInputs = snap.Inputs
		}
		if snap.QueryPresent {
			appendQuery = snap.Query
			appendQueryAt = snap.QueryAt
		}
		if snap.AgentPresent {
			appendAgentID = snap.AgentID
			appendAgentName = snap.AgentName
			appendBinding = snap.AgentBindingSource
		}
		if _, err := m.proj.Append(ctx, sess.id, turns.Append{
			TurnID:             turns.TurnID(taskID),
			TaskID:             taskID, // the event's canonical task id — a snapshot can never replace it
			RunID:              runID,
			Query:              appendQuery,
			QueryAt:            appendQueryAt,
			AgentID:            appendAgentID,
			AgentName:          appendAgentName,
			AgentBindingSource: appendBinding,
			Inputs:             appendInputs,
			Status:             turns.StatusPending,
			StartedAt:          ev.OccurredAt,
			EventSeq:           ev.Sequence,
		}); err != nil {
			return false, fmt.Errorf("materializer: append turn %s: %w", taskID, err)
		}
	}

	ts := &turnState{taskID: taskID, runID: runID}
	if snap.InputsPresent {
		// Seed the in-memory input accumulator with the record's input
		// metadata (deep-copied) so a later disposition event for the
		// SAME artifact is deduplicated and the cumulative list fed to
		// the projector always includes the append-time inputs.
		ts.inputs = append([]turns.Attachment(nil), snap.Inputs...)
	}
	sess.turns[turns.TurnID(taskID)] = ts
	return true, nil
}

// applyTaskRunning folds task.started / task.resumed into the owning
// turn's lifecycle: the run is executing. A resume (the task FSM's
// Paused → Running transition) clears the pause episode explicitly —
// the row must not carry an active episode while running.
func (m *Materializer) applyTaskRunning(ctx context.Context, sess *sessionState, ev events.Event, payload map[string]any) (bool, error) {
	taskID := fieldString(payload, "TaskID")
	ts, ok := sess.taskTurn(taskID)
	if !ok || ts.terminal() {
		return false, nil
	}
	if ev.Sequence <= sess.checkpoint {
		return true, nil
	}
	err := m.updateTurn(ctx, sess, ts, turns.Update{
		Status:   turns.StatusRunning,
		Pause:    &turns.Pause{Availability: turns.CompletenessUnavailable},
		EventSeq: ev.Sequence,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// applyTaskCompleted folds a successful terminal into a COMPLETE seal.
// Only the ROOT foreground task's own completion may seal (or
// defer-seal) the turn: a child/background task's completion never
// touches the root lifecycle.
//
// When the runtime wires the TaskSnapshotReader seam, the projection
// reads the already-redacted canonical task record under the EVENT
// identity and captures the bounded Harbor answer envelope (inline,
// empty, or artifact-reference shaped) plus the output attachment
// metadata; the definite answer is attached to the row (with the
// outputs) and the seal lands immediately, so the task record and the
// durable turn agree. The read is BOUND: a nonempty returned TaskID
// must equal the event's canonical task id, and the run id must agree
// with the turn's established binding — a later snapshot can never move
// it. The reader is invoked only while projecting this successfully
// persisted completion event — never on a Protocol read.
//
// Without a wired reader (or for a legacy record with no answer
// envelope) the answer stays honestly unavailable: the projector
// refuses the complete seal (ErrSealIncomplete), the turn is enqueued
// in the bounded pending-work queue, and the CONVERGENCE pass rereads
// the exact task snapshot under the original event identity / task id
// and seals only after the answer source converges — the row honestly
// stays mutable (running) while its sources have not converged; it is
// never fabricated as complete, and a sequence no-op is never equated
// with a durable seal. A transient snapshot error or a binding
// mismatch aborts the projection without advancing the checkpoint.
//
// On RESTART CATCH-UP (the completion event is at or below the durable
// checkpoint) the deferred state is reconstructed — but ONLY after
// reading the existing exact turn and proving it is the same unsealed
// incomplete row (same authoritative task id, not sealed, not evicted):
// a sealed row is never touched (sealed content is never overwritten),
// an evicted row is retired (never resurrected), ordinary history is
// not re-applied, and the checkpoint / row sequence never regresses.
func (m *Materializer) applyTaskCompleted(ctx context.Context, sess *sessionState, ev events.Event, payload map[string]any) (bool, error) {
	taskID := fieldString(payload, "TaskID")
	ts, ok := sess.rootTaskTurn(taskID)
	if !ok || ts.terminal() {
		return false, nil
	}
	if ev.Sequence <= sess.checkpoint {
		// Restart catch-up: the completion observation already landed in
		// a previous lifetime (its row mutation and the checkpoint write
		// both completed). If the durable row is the SAME unsealed
		// incomplete row — its answer source had not converged when the
		// checkpoint advanced — reconstruct the deferred-complete state
		// so the end-of-pass convergence rereads the snapshot and seals
		// it. The proof is the exact row: the same authoritative task
		// id, not sealed, not evicted. Ordinary history is NOT
		// re-applied (only the in-memory deferred marker is
		// reconstructed — no row write, no sequence or checkpoint
		// regression); a sealed row is never touched; an evicted row is
		// retired, never resurrected.
		current, err := m.proj.Get(ctx, sess.id, turns.TurnID(taskID))
		if err != nil {
			if errors.Is(err, turns.ErrTurnNotFound) {
				ts.retired = true
				return true, nil
			}
			return false, err
		}
		if current.Sealed {
			return true, nil
		}
		if current.TaskID != "" && current.TaskID != taskID {
			// The durable row under this turn id belongs to a DIFFERENT
			// task — a corrupt store. Never reconstruct deferred state
			// against it.
			return true, nil
		}
		m.enqueuePending(sess, ts)
		return true, nil
	}
	sealed, stillPending, err := m.completeSealProjection(ctx, sess, ts, runIDFromIdentity(ev.Identity), ev.Sequence)
	if err != nil {
		return false, err
	}
	if stillPending {
		m.enqueuePending(sess, ts)
		return true, nil
	}
	if sealed {
		ts.sealed = true
		m.clearPending(sess, ts)
	}
	return true, nil
}

// applyTaskFailed folds a terminal failure into a FAILED seal with the
// closed content-free error class derived from the task's error code.
// Only the ROOT foreground task's own failure seals the turn — a
// child/background task's failure never seals (or otherwise mutates)
// the root lifecycle.
//
// When the runtime wires the TaskSnapshotReader seam, the projection
// reads the already-redacted canonical task record under the EVENT
// identity and captures compatible optional failure metadata. The
// persisted lifecycle event remains canonical: its nonempty code drives
// the closed error class. A task-record code fills only a legacy event
// whose code is empty; when both are nonempty and disagree, all snapshot
// failure metadata is omitted rather than letting one legacy record stall
// the global projector. A bounded redacted message rides the seal only
// with compatible classification; an over-bound optional message is
// honestly unavailable. The read is BOUND: a nonempty
// returned TaskID must equal the event's canonical task id, the run id
// must agree with the turn's established binding (a later snapshot can
// never move it). A nil reader / legacy record leaves the message
// unavailable ("") and derives the class from the event's code alone. The reader is
// invoked only while projecting this successfully persisted failure
// event — never on a Protocol read. A transient snapshot error, a
// binding mismatch aborts the projection without advancing the checkpoint.
func (m *Materializer) applyTaskFailed(ctx context.Context, sess *sessionState, ev events.Event, payload map[string]any) (bool, error) {
	taskID := fieldString(payload, "TaskID")
	ts, ok := sess.rootTaskTurn(taskID)
	if !ok || ts.terminal() {
		return false, nil
	}
	if ev.Sequence <= sess.checkpoint {
		return true, nil
	}
	snap, err := m.readTaskSnapshot(ctx, sess.id, taskID)
	if err != nil {
		return false, err
	}
	// Run binding: the terminal read's run observations (the event
	// envelope, the spawn-established turn binding, the snapshot) must
	// all agree; a snapshot may fill the run only when no binding
	// exists yet — never move an established one. Enforced BEFORE any
	// mutation so a mismatch leaves the row untouched and the
	// checkpoint unadvanced.
	bound, err := bindRunID(runIDFromIdentity(ev.Identity), ts.runID, snap.RunID)
	if err != nil {
		return false, err
	}
	if bound != ts.runID {
		ts.runID = bound
	}
	// The persisted lifecycle event is canonical. Snapshot failure fields
	// are optional render metadata: they may fill an absent legacy event code,
	// but a disagreement makes the whole snapshot failure group unavailable.
	// That preserves one internally consistent classification without letting
	// an incompatible historical task record wedge the global projector.
	code := fieldString(payload, "ErrorCode")
	message := ""
	if snap.FailurePresent {
		compatible := code == "" || snap.ErrorCode == "" || code == snap.ErrorCode
		if code == "" && snap.ErrorCode != "" {
			code = snap.ErrorCode
		}
		if compatible {
			message = snapshotFailureMessage(snap.ErrorMessage)
		}
	}
	row, err := m.sealTurn(ctx, sess, ts, turns.Seal{
		Status:       turns.StatusFailed,
		ErrorClass:   mapTaskErrorClass(code),
		ErrorMessage: message,
		EventSeq:     ev.Sequence,
	})
	if err != nil {
		return false, err
	}
	if ts.retired {
		// The row was evicted past retention: the seal is an honest
		// terminal projection gap — retire the routing state, never
		// resurrect.
		m.clearPending(sess, ts)
		return true, nil
	}
	if !row.Sealed {
		// A sequence no-op: the durable row is not sealed. Leave the
		// local state unsealed (the row stays mutable); never fabricate
		// a seal.
		return true, nil
	}
	ts.sealed = true
	m.clearPending(sess, ts)
	return true, nil
}

// applyTaskCancelled folds a cancellation into a CANCELLED seal. Only
// the ROOT foreground task's own cancellation seals the turn — a
// child/background task's cancellation never seals the root. The
// operator/cascade reason string is not surfaced (the payload's Reason
// is caller-controlled short text with the same SafePayload contract as
// session-closed reasons; the projection carries the closed finish
// reason only).
func (m *Materializer) applyTaskCancelled(ctx context.Context, sess *sessionState, ev events.Event, payload map[string]any) (bool, error) {
	taskID := fieldString(payload, "TaskID")
	ts, ok := sess.rootTaskTurn(taskID)
	if !ok || ts.terminal() {
		return false, nil
	}
	if ev.Sequence <= sess.checkpoint {
		return true, nil
	}
	row, err := m.sealTurn(ctx, sess, ts, turns.Seal{
		Status:       turns.StatusCancelled,
		FinishReason: turns.FinishCancelled,
		EventSeq:     ev.Sequence,
	})
	if err != nil {
		return false, err
	}
	if ts.retired {
		// The row was evicted past retention: the seal is an honest
		// terminal projection gap — retire the routing state, never
		// resurrect.
		m.clearPending(sess, ts)
		return true, nil
	}
	if !row.Sealed {
		// A sequence no-op: the durable row is not sealed. Leave the
		// local state unsealed; never fabricate a seal.
		return true, nil
	}
	ts.sealed = true
	m.clearPending(sess, ts)
	return true, nil
}

// mapTaskErrorClass maps the task error code onto the closed
// content-free terminal error class set. Known codes map precisely;
// everything else is unclassified (never a fabricated stronger claim).
func mapTaskErrorClass(code string) turns.ErrorClass {
	switch strings.ToLower(code) {
	case "timeout", "deadline_exceeded", "deadline", "context_deadline_exceeded":
		return turns.ErrorClassTimeout
	case "transient", "retryable", "retry_exhausted":
		return turns.ErrorClassTransient
	case "5xx", "upstream_5xx", "service_unavailable":
		return turns.ErrorClass5xx
	case "permanent", "invalid", "invalid_input", "output_invalid", "validation_failed", "schema_invalid":
		return turns.ErrorClassPermanent
	default:
		return turns.ErrorClassUnclassified
	}
}

// ---------------------------------------------------------------------------
// attachments
// ---------------------------------------------------------------------------

// applyInputDisposition folds a resolved input-artifact disposition
// into the owning turn's input attachment METADATA (never bytes). The
// disposition event is the canonical input-attachment signal: artifact
// id, MIME, and the effective disposition (ref / inline /
// provider_native / tool:<name>). Filename / size / digest are not
// carried by the event and stay unavailable. Deduplicated by artifact
// id so a replay can never double-list an attachment.
//
// The payload's JSON keys are snake_case (the payload carries explicit
// json tags), unlike the untagged payloads of the other canonical
// families — the decoder honours the persisted key shape.
func (m *Materializer) applyInputDisposition(ctx context.Context, sess *sessionState, ev events.Event, payload map[string]any) (bool, error) {
	taskID := fieldString(payload, "task_id")
	ts, ok := sess.taskTurn(taskID)
	if !ok || ts.terminal() {
		return false, nil
	}
	artifactID := fieldString(payload, "artifact_id")
	if artifactID == "" {
		return false, nil
	}
	for _, a := range ts.inputs {
		if a.ID == artifactID {
			return true, nil // already listed; nothing to do
		}
	}
	// Transactional update: the candidate feed commits to memory only
	// after the durable write succeeds, so a mid-page failure never
	// double-lists (or silently drops) an attachment on retry.
	newInputs := append(append([]turns.Attachment(nil), ts.inputs...), turns.Attachment{
		ID:           artifactID,
		MimeType:     fieldString(payload, "mime"),
		Disposition:  fieldString(payload, "disposition"),
		Availability: turns.CompletenessComplete,
	})
	if ev.Sequence <= sess.checkpoint {
		ts.inputs = newInputs
		return true, nil
	}
	err := m.updateTurn(ctx, sess, ts, turns.Update{
		Inputs:   newInputs,
		EventSeq: ev.Sequence,
	})
	if err != nil {
		return false, err
	}
	ts.inputs = newInputs
	return true, nil
}

// ---------------------------------------------------------------------------
// derived reasoning
// ---------------------------------------------------------------------------

// maxReasoningFeed bounds the materializer's in-memory reasoning feed
// at the projector's per-observation feed-acceptance bound
// (turns.MaxReasoningSteps*4 — see turns validateReasoningSteps), so
// the cumulative feed never grows unbounded and never fails the
// projector's validation. The feed keeps the chronological HEAD: the
// projector retains the FIRST turns.MaxReasoningSteps of what it is
// fed and reports the tail drop honestly as Partial + Dropped, so an
// over-bound trajectory stays Partial and converging instead of
// hard-failing the pass.
const maxReasoningFeed = turns.MaxReasoningSteps * 4

// reasoningKindFor maps the planner decision shape name onto the
// CLOSED derived reasoning kind set. Shapes without a safe derivative
// (CallParallel, Finish, RequestPause) are omitted honestly — the
// component reports its availability/dropped counts, never arbitrary
// text.
func reasoningKindFor(decisionKind string) (turns.ReasoningKind, bool) {
	switch decisionKind {
	case "CallTool":
		return turns.ReasoningKindToolCall, true
	case "SpawnTask":
		return turns.ReasoningKindSpawn, true
	case "AwaitTask":
		return turns.ReasoningKindAwait, true
	}
	return "", false
}

// applyPlannerDecision derives ONE consumer-safe reasoning step from a
// planner decision event. The raw provider thinking the payload may
// carry (ReasoningTrace) is structurally absent from the row — only the
// closed kind and the chronological index are derived. The bounded
// ordered reasoning feed (clamped at maxReasoningFeed, keeping the
// chronological head the projector retains) is retained in state and
// attached wholesale so the component's ordering and snapshot stay
// stable across restarts; beyond the bound the new step is dropped
// honestly (the row already reports Partial + Dropped) and the event
// still applies — the feed never grows unbounded and never fails.
func (m *Materializer) applyPlannerDecision(ctx context.Context, sess *sessionState, ev events.Event, payload map[string]any) (bool, error) {
	ts, ok := sess.rootTurn(runIDFromIdentity(ev.Identity))
	if !ok || ts.terminal() {
		return false, nil
	}
	kind, ok := reasoningKindFor(fieldString(payload, "DecisionKind"))
	if !ok {
		return false, nil
	}
	if len(ts.reasoning) >= maxReasoningFeed {
		// The feed is at the projector's acceptance bound: the new step
		// is beyond the retained chronological head and is dropped
		// honestly (the component is already Partial + Dropped at the
		// projector). The event is still applied so the checkpoint and
		// cursor keep advancing — never a feed failure, never unbounded
		// growth.
		return true, nil
	}
	// Transactional update: the candidate feed commits to memory only
	// after the durable attach succeeds, so a mid-page failure never
	// double-attaches a step on retry.
	newReasoning := append(append([]turns.ReasoningStep(nil), ts.reasoning...), turns.ReasoningStep{Index: ts.nextReasoningIndex, Kind: kind})
	if ev.Sequence <= sess.checkpoint {
		ts.reasoning = newReasoning
		ts.nextReasoningIndex++
		return true, nil
	}
	_, err := m.attachReasoning(ctx, sess, ts, turns.ReasoningInput{
		Steps:    newReasoning,
		EventSeq: ev.Sequence,
	})
	if err != nil {
		return false, err
	}
	ts.reasoning = newReasoning
	ts.nextReasoningIndex++
	return true, nil
}

// ---------------------------------------------------------------------------
// tool activity
// ---------------------------------------------------------------------------

// applyToolInvoked folds a tool dispatch start into the owning turn's
// content-free activity window. The invocation id is not carried by the
// canonical tool events, so it stays empty (honest unavailable — never
// derived into a fabricated correlation). Position, terminal class, and
// the exact turn-level totals are derived by the projector from the
// full cumulative feed.
func (m *Materializer) applyToolInvoked(ctx context.Context, sess *sessionState, ev events.Event, payload map[string]any) (bool, error) {
	ts, ok := sess.rootTurn(runIDFromIdentity(ev.Identity))
	if !ok || ts.terminal() {
		return false, nil
	}
	toolName := fieldString(payload, "ToolName")
	if toolName == "" {
		return false, nil
	}
	started := ev.OccurredAt
	if t, ok := fieldTime(payload, "StartedAt"); ok {
		started = t
	}
	// Transactional update: the candidate feed commits to memory only
	// after the durable write succeeds, so a mid-page failure never
	// double-inserts a dispatch row on retry.
	newActivity := append(append([]turns.ActivityRow(nil), ts.activity...), turns.ActivityRow{
		Tool:         toolName,
		StepSequence: ev.Sequence,
		Status:       turns.ActivityInvoked,
		StartedAt:    started,
	})
	if ev.Sequence <= sess.checkpoint {
		ts.activity = newActivity
		return true, nil
	}
	err := m.updateTurn(ctx, sess, ts, turns.Update{
		Activity: newActivity,
		EventSeq: ev.Sequence,
	})
	if err != nil {
		return false, err
	}
	ts.activity = newActivity
	return true, nil
}

// applyToolCompleted folds a successful dispatch into the owning
// turn's activity: the most recent in-flight row for the same tool
// (LIFO — parallel dispatches of one tool resolve newest-first, since
// the canonical events carry no invocation id to match exactly) is
// marked succeeded with the reported attempt count and duration. A
// completion with no tracked invocation is omitted honestly (a ring
// eviction that dropped the invoked event is surfaced by the source's
// RetentionGap signal, never silently hidden).
func (m *Materializer) applyToolCompleted(ctx context.Context, sess *sessionState, ev events.Event, payload map[string]any) (bool, error) {
	ts, ok := sess.rootTurn(runIDFromIdentity(ev.Identity))
	if !ok || ts.terminal() {
		return false, nil
	}
	toolName := fieldString(payload, "ToolName")
	idx := findInFlight(ts.activity, toolName)
	if idx < 0 {
		return false, nil
	}
	// Transactional update: mutate a COPY of the feed and commit it to
	// memory only after the durable write succeeds, so a mid-page
	// failure never half-applies a terminal transition on retry.
	newActivity := append([]turns.ActivityRow(nil), ts.activity...)
	newActivity[idx].Status = turns.ActivitySucceeded
	newActivity[idx].FinishedAt = ev.OccurredAt
	newActivity[idx].Duration = time.Duration(fieldInt64(payload, "DurationMS")) * time.Millisecond
	newActivity[idx].AttemptCount = int(fieldInt64(payload, "Attempts"))
	if ev.Sequence <= sess.checkpoint {
		ts.activity = newActivity
		return true, nil
	}
	err := m.updateTurn(ctx, sess, ts, turns.Update{
		Activity: newActivity,
		EventSeq: ev.Sequence,
	})
	if err != nil {
		return false, err
	}
	ts.activity = newActivity
	return true, nil
}

// applyToolFailed folds a terminal dispatch failure into the owning
// turn's activity. The summary is a DERIVED content-free string (the
// closed error class) — the payload's error message is caller content
// and never reaches the row.
func (m *Materializer) applyToolFailed(ctx context.Context, sess *sessionState, ev events.Event, payload map[string]any) (bool, error) {
	ts, ok := sess.rootTurn(runIDFromIdentity(ev.Identity))
	if !ok || ts.terminal() {
		return false, nil
	}
	toolName := fieldString(payload, "ToolName")
	idx := findInFlight(ts.activity, toolName)
	if idx < 0 {
		return false, nil
	}
	class := fieldString(payload, "ErrorClass")
	if class == "" {
		class = "unclassified"
	}
	// Transactional update: mutate a COPY of the feed and commit it to
	// memory only after the durable write succeeds, so a mid-page
	// failure never half-applies a terminal transition on retry.
	newActivity := append([]turns.ActivityRow(nil), ts.activity...)
	newActivity[idx].Status = turns.ActivityFailed
	newActivity[idx].FinishedAt = ev.OccurredAt
	newActivity[idx].AttemptCount = int(fieldInt64(payload, "Attempts"))
	newActivity[idx].Summary = "failed: " + class
	if ev.Sequence <= sess.checkpoint {
		ts.activity = newActivity
		return true, nil
	}
	err := m.updateTurn(ctx, sess, ts, turns.Update{
		Activity: newActivity,
		EventSeq: ev.Sequence,
	})
	if err != nil {
		return false, err
	}
	ts.activity = newActivity
	return true, nil
}

// applyToolPolicyExhausted folds a retry-budget-exhausted dispatch into
// the owning turn's activity: the matching in-flight row is marked
// policy_exhausted (the status and its derived terminal class), with
// the closed last error class in the summary.
func (m *Materializer) applyToolPolicyExhausted(ctx context.Context, sess *sessionState, ev events.Event, payload map[string]any) (bool, error) {
	ts, ok := sess.rootTurn(runIDFromIdentity(ev.Identity))
	if !ok || ts.terminal() {
		return false, nil
	}
	toolName := fieldString(payload, "ToolName")
	idx := findInFlight(ts.activity, toolName)
	if idx < 0 {
		return false, nil
	}
	class := fieldString(payload, "LastClass")
	if class == "" {
		class = "unclassified"
	}
	// Transactional update: mutate a COPY of the feed and commit it to
	// memory only after the durable write succeeds, so a mid-page
	// failure never half-applies a terminal transition on retry.
	newActivity := append([]turns.ActivityRow(nil), ts.activity...)
	newActivity[idx].Status = turns.ActivityPolicyExhausted
	newActivity[idx].PolicyExhausted = true
	newActivity[idx].FinishedAt = ev.OccurredAt
	newActivity[idx].AttemptCount = int(fieldInt64(payload, "Attempts"))
	newActivity[idx].Summary = "policy_exhausted: " + class
	if ev.Sequence <= sess.checkpoint {
		ts.activity = newActivity
		return true, nil
	}
	err := m.updateTurn(ctx, sess, ts, turns.Update{
		Activity: newActivity,
		EventSeq: ev.Sequence,
	})
	if err != nil {
		return false, err
	}
	ts.activity = newActivity
	return true, nil
}

// findInFlight returns the index of the most recent in-flight activity
// row (invoked / retried) for the given tool, or -1. LIFO matching is
// the deterministic resolution for parallel dispatches of one tool.
func findInFlight(rows []turns.ActivityRow, tool string) int {
	for i := len(rows) - 1; i >= 0; i-- {
		if rows[i].Tool != tool {
			continue
		}
		if rows[i].Status == turns.ActivityInvoked || rows[i].Status == turns.ActivityRetried {
			return i
		}
	}
	return -1
}

// ---------------------------------------------------------------------------
// MCP App references
// ---------------------------------------------------------------------------

// applyAppAvailable folds the canonical mcp.app_available discovery
// into the turn's ORDERED App reference collection. The replacement
// identity is exactly (effective agent id, server id, resource uri);
// the projector's upsert fixes first-declaration position and replaces
// repeats in place. The payload's Binding (an opaque runtime-issued
// callback capability) is structurally absent from the AppRef — a
// replayed projection never rehydrates live callback authority — and
// the tool_call_id rides as correlation metadata only.
func (m *Materializer) applyAppAvailable(ctx context.Context, sess *sessionState, ev events.Event, payload map[string]any) (bool, error) {
	ts, ok := sess.rootTurn(runIDFromIdentity(ev.Identity))
	if !ok || ts.terminal() {
		return false, nil
	}
	serverID := fieldString(payload, "ServerID")
	resourceURI := fieldString(payload, "ResourceURI")
	if serverID == "" || resourceURI == "" {
		return false, nil
	}
	if ev.Sequence <= sess.checkpoint {
		return true, nil
	}
	_, err := m.attachAppRefs(ctx, sess, ts, turns.AppRefInput{
		Refs: []turns.AppRef{{
			EffectiveAgentID: fieldString(payload, "AgentID"),
			ServerID:         serverID,
			ResourceURI:      resourceURI,
			DisplayMode:      fieldString(payload, "DisplayMode"),
			RawHTMLTrusted:   fieldBool(payload, "RawHTMLTrusted"),
			ToolCallID:       fieldString(payload, "ToolCallID"),
			ToolName:         fieldString(payload, "ToolName"),
			Availability:     turns.AppAvailable,
			Complete:         turns.CompletenessComplete,
		}},
		EventSeq: ev.Sequence,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// durable pause
// ---------------------------------------------------------------------------

// pauseClassFor derives the CLOSED pause producer class from the
// canonical planner pause reason. The mapping is the materializer's
// documented interpretation — the event carries the planner-side
// reason, not the producer family — and an unclassifiable reason
// omits the episode honestly (never a fabricated class).
func pauseClassFor(reason string) (turns.PauseClass, bool) {
	switch reason {
	case string(planner.PauseApprovalRequired):
		return turns.PauseClassHitlApproval, true
	case string(planner.PauseAwaitInput):
		return turns.PauseClassA2AInputRequired, true
	case string(planner.PauseExternalEvent), string(planner.PauseConstraintsConflict):
		return turns.PauseClassSteering, true
	}
	return "", false
}

// applyPauseRequested folds the unified pause primitive's request into
// the owning turn: the row transitions to paused with a durable,
// token-free pause episode (class / reason / lifecycle / availability —
// the opaque pause Token is structurally absent from the Pause
// component and actionability is never stored).
func (m *Materializer) applyPauseRequested(ctx context.Context, sess *sessionState, ev events.Event, payload map[string]any) (bool, error) {
	ts, ok := sess.rootTurn(runIDFromIdentity(ev.Identity))
	if !ok || ts.terminal() {
		return false, nil
	}
	class, ok := pauseClassFor(fieldString(payload, "Reason"))
	if !ok {
		return false, nil
	}
	if ev.Sequence <= sess.checkpoint {
		return true, nil
	}
	err := m.updateTurn(ctx, sess, ts, turns.Update{
		Status: turns.StatusPaused,
		Pause: &turns.Pause{
			Class:        class,
			Reason:       fieldString(payload, "Reason"),
			Lifecycle:    turns.PauseLifecycleActive,
			Availability: turns.CompletenessComplete,
		},
		EventSeq: ev.Sequence,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// applyPauseResumed folds a pause termination into the owning turn:
// the row returns to running and the pause episode is cleared
// explicitly (a resume never leaves an active episode behind).
func (m *Materializer) applyPauseResumed(ctx context.Context, sess *sessionState, ev events.Event) (bool, error) {
	ts, ok := sess.rootTurn(runIDFromIdentity(ev.Identity))
	if !ok || ts.terminal() {
		return false, nil
	}
	if ev.Sequence <= sess.checkpoint {
		return true, nil
	}
	err := m.updateTurn(ctx, sess, ts, turns.Update{
		Status:   turns.StatusRunning,
		Pause:    &turns.Pause{Availability: turns.CompletenessUnavailable},
		EventSeq: ev.Sequence,
	})
	if err != nil {
		return false, err
	}
	return true, nil
}

// ---------------------------------------------------------------------------
// usage
// ---------------------------------------------------------------------------

// usageDelta decodes ONE per-call usage measure from the payload field
// map. It returns (delta, present, nil) where present is true when the
// field carries a numeric value, and a wrapped error for a CORRUPT
// source: a NaN / ±Inf float64, a float64 outside the int64 range (a
// silent conversion would yield a garbage int64 — the wrap the
// fail-loud contract forbids), or an unparseable json.Number. An
// absent field returns (0, false, nil). The semantic sign checks
// (negative deltas) live in accumulateUsage, so the sign authority is
// in one place.
func usageDelta(m map[string]any, key string) (int64, bool, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return 0, false, nil
	}
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, false, fmt.Errorf("materializer: usage %s is non-finite (%v) — corrupt arithmetic never wraps or is silently omitted", key, n)
		}
		if n >= float64(math.MaxInt64) || n < float64(math.MinInt64) {
			return 0, false, fmt.Errorf("materializer: usage %s %v overflows int64 — corrupt arithmetic never wraps or is silently omitted", key, n)
		}
		return int64(math.Round(n)), true, nil
	case int64:
		return n, true, nil
	case int:
		return int64(n), true, nil
	case json.Number:
		i, err := n.Int64()
		if err == nil {
			return i, true, nil
		}
		f, err := n.Float64()
		if err != nil {
			return 0, false, fmt.Errorf("materializer: usage %s is not a number: %w", key, err)
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return 0, false, fmt.Errorf("materializer: usage %s is non-finite (%v) — corrupt arithmetic never wraps or is silently omitted", key, f)
		}
		if f >= float64(math.MaxInt64) || f < float64(math.MinInt64) {
			return 0, false, fmt.Errorf("materializer: usage %s %v overflows int64 — corrupt arithmetic never wraps or is silently omitted", key, f)
		}
		return int64(math.Round(f)), true, nil
	}
	return 0, false, nil
}

// costUSD decodes the per-call TotalCost field into a finite,
// non-negative float64 USD amount, failing loud on a corrupt source: a
// non-numeric type, a NaN / ±Inf, or a negative total. Zero (absent)
// is NOT an error — the canonical payload contract treats a zero /
// absent total as "no cost reported".
func costUSD(v any) (float64, error) {
	switch n := v.(type) {
	case float64:
		if math.IsNaN(n) || math.IsInf(n, 0) {
			return 0, fmt.Errorf("materializer: cost total is non-finite (%v) — corrupt arithmetic never wraps or is silently omitted", n)
		}
		if n < 0 {
			return 0, fmt.Errorf("materializer: cost total is negative (%v) — corrupt arithmetic never wraps or is silently omitted", n)
		}
		return n, nil
	case json.Number:
		f, err := n.Float64()
		if err != nil {
			return 0, fmt.Errorf("materializer: cost total is not a number: %w", err)
		}
		return costUSD(f)
	case int64:
		if n < 0 {
			return 0, fmt.Errorf("materializer: cost total is negative (%d) — corrupt arithmetic never wraps or is silently omitted", n)
		}
		return float64(n), nil
	case int:
		if n < 0 {
			return 0, fmt.Errorf("materializer: cost total is negative (%d) — corrupt arithmetic never wraps or is silently omitted", n)
		}
		return float64(n), nil
	}
	return 0, fmt.Errorf("materializer: cost total has unsupported type %T — corrupt cost arithmetic is never silently omitted", v)
}

// accumulateUsage derives the cumulative usage CANDIDATE from the
// current accumulator and one per-call llm.cost.recorded payload
// (the payload's usage / cost field maps and the reported model).
// Updated measures receive FRESH value pointers; the caller commits
// the candidate to the in-memory accumulator only after the durable
// write succeeds, so a mid-page failure can never double-accumulate a
// per-call snapshot on retry.
//
// The arithmetic FAILS LOUD — it never wraps, clamps, or silently
// omits a corrupt value: a negative token / latency / cost delta, a
// non-finite or out-of-range numeric source, or an int64 accumulation
// / latency-ms→ns / USD→micro-dollar conversion overflow aborts the
// projection with a wrapped error (and therefore without advancing the
// checkpoint). A zero delta leaves the measure unchanged (the
// canonical payload contract makes zero indistinguishable from
// not-reported, so zero never becomes a fabricated exact claim).
func accumulateUsage(cur turns.Usage, usage, cost map[string]any, model string) (turns.Usage, error) {
	next := cur
	addMeasure := func(prev turns.UsageMeasure, delta int64) (turns.UsageMeasure, error) {
		if delta < 0 {
			return turns.UsageMeasure{}, fmt.Errorf("materializer: cumulative usage delta %d is negative — corrupt arithmetic never wraps or is silently omitted", delta)
		}
		if delta == 0 {
			return prev, nil
		}
		acc := int64(0)
		if prev.Value != nil {
			acc = *prev.Value
		}
		if acc < 0 {
			return turns.UsageMeasure{}, fmt.Errorf("materializer: cumulative usage accumulator is negative (%d) — corrupt arithmetic never wraps or is silently omitted", acc)
		}
		if delta > math.MaxInt64-acc {
			return turns.UsageMeasure{}, fmt.Errorf("materializer: cumulative usage overflows int64 (%d + %d) — corrupt arithmetic never wraps or is silently omitted", acc, delta)
		}
		acc += delta
		return turns.UsageMeasure{State: turns.UsageExact, Value: &acc}, nil
	}
	addField := func(key string, prev turns.UsageMeasure) (turns.UsageMeasure, error) {
		delta, present, err := usageDelta(usage, key)
		if err != nil {
			return turns.UsageMeasure{}, err
		}
		if !present {
			return prev, nil
		}
		return addMeasure(prev, delta)
	}

	var err error
	if next.PromptTokens, err = addField("PromptTokens", next.PromptTokens); err != nil {
		return turns.Usage{}, err
	}
	if next.CompletionTokens, err = addField("CompletionTokens", next.CompletionTokens); err != nil {
		return turns.Usage{}, err
	}
	if next.ReasoningTokens, err = addField("ReasoningTokens", next.ReasoningTokens); err != nil {
		return turns.Usage{}, err
	}
	if next.CacheReadTokens, err = addField("CacheReadTokens", next.CacheReadTokens); err != nil {
		return turns.Usage{}, err
	}
	if next.CacheWriteTokens, err = addField("CacheWriteTokens", next.CacheWriteTokens); err != nil {
		return turns.Usage{}, err
	}
	if next.TotalTokens, err = addField("TotalTokens", next.TotalTokens); err != nil {
		return turns.Usage{}, err
	}
	// Latency: the payload reports integer milliseconds; the projection
	// accumulates exact integer nanoseconds. The ms→ns conversion must
	// not overflow — a corruptly huge latency aborts loudly.
	latencyMS, present, err := usageDelta(usage, "LatencyMS")
	if err != nil {
		return turns.Usage{}, err
	}
	if present {
		if latencyMS > math.MaxInt64/int64(time.Millisecond) {
			return turns.Usage{}, fmt.Errorf("materializer: latency %d ms overflows integer nanoseconds — corrupt arithmetic never wraps or is silently omitted", latencyMS)
		}
		if next.LatencyNS, err = addMeasure(next.LatencyNS, latencyMS*int64(time.Millisecond)); err != nil {
			return turns.Usage{}, err
		}
	}

	// Cost: the float64 USD per-call total is converted to integer
	// micro-dollars by rounding and accumulated as integers; the measure
	// is honestly ESTIMATED (the source is approximate) and money is
	// never accumulated in float64. The conversion and accumulation are
	// overflow-checked — corrupt cost arithmetic never wraps.
	if v, ok := cost["TotalCost"]; ok && v != nil {
		totalUSD, err := costUSD(v)
		if err != nil {
			return turns.Usage{}, err
		}
		if totalUSD > 0 {
			if totalUSD > math.MaxInt64/1e6 {
				return turns.Usage{}, fmt.Errorf("materializer: cost total %v overflows integer micro-dollars — corrupt arithmetic never wraps or is silently omitted", totalUSD)
			}
			micro := int64(math.Round(totalUSD * 1e6))
			if micro < 0 {
				// A float64 conversion wrap is impossible after the
				// bound above, but the guard keeps the arithmetic
				// fail-loud against a future bound regression.
				return turns.Usage{}, fmt.Errorf("materializer: cost total %v overflows integer micro-dollars — corrupt arithmetic never wraps or is silently omitted", totalUSD)
			}
			acc := int64(0)
			if next.CostMicroUSD.Value != nil {
				acc = *next.CostMicroUSD.Value
			}
			if acc < 0 {
				return turns.Usage{}, fmt.Errorf("materializer: cost accumulator is negative (%d) — corrupt arithmetic never wraps or is silently omitted", acc)
			}
			if micro > math.MaxInt64-acc {
				return turns.Usage{}, fmt.Errorf("materializer: cumulative cost overflows integer micro-dollars (%d + %d) — corrupt arithmetic never wraps or is silently omitted", acc, micro)
			}
			acc += micro
			next.CostMicroUSD = turns.UsageMeasure{State: turns.UsageEstimated, Value: &acc}
		}
	}
	if model != "" {
		next.Model = model
	}
	return next, nil
}

// applyCostRecorded folds the per-call llm.cost.recorded event into the
// owning turn's CUMULATIVE per-measure usage accumulator. The payload's
// Usage is a per-call snapshot (provider cache accounting included);
// the projection's Usage is a cumulative rollup, so the materializer
// accumulates. Honesty is PER MEASURE: a measure is exact only when a
// strictly positive cumulative amount exists (the canonical payload's
// own contract — "fields are zero when the provider doesn't report a
// category" — makes zero indistinguishable from not-reported, so a zero
// never becomes a fabricated exact claim); cost is derived from the
// float64 USD source per call, converted to exact integer micro-dollars
// by rounding and accumulated as integers (money is never accumulated
// in float64); latency is accumulated as exact integer nanoseconds.
func (m *Materializer) applyCostRecorded(ctx context.Context, sess *sessionState, ev events.Event, payload map[string]any) (bool, error) {
	ts, ok := sess.rootTurn(runIDFromIdentity(ev.Identity))
	if !ok || ts.terminal() {
		return false, nil
	}
	usage := fieldNested(payload, "Usage")
	cost := fieldNested(payload, "Cost")
	// Transactional update: the candidate rollup commits to memory only
	// after the durable write succeeds, so a mid-page failure never
	// double-counts a per-call snapshot on retry. Corrupt arithmetic
	// (negative / non-finite / overflowing deltas) fails loud here —
	// the pass aborts without advancing the checkpoint and the corrupt
	// event is never wrapped, clamped, or silently omitted.
	candidate, err := accumulateUsage(ts.usage, usage, cost, fieldString(payload, "Model"))
	if err != nil {
		return false, err
	}
	if ev.Sequence <= sess.checkpoint {
		ts.usage = candidate
		return true, nil
	}
	err = m.updateTurn(ctx, sess, ts, turns.Update{
		Usage:    &candidate,
		EventSeq: ev.Sequence,
	})
	if err != nil {
		return false, err
	}
	ts.usage = candidate
	return true, nil
}

// ---------------------------------------------------------------------------
// projector write helpers (versioned, replay-safe)
// ---------------------------------------------------------------------------

// updateTurn applies one Update observation to a mutable turn. The
// expected version is read fresh from the store each attempt so a
// concurrent write (another materializer replica) is reconciled by
// retry; the projector's monotonic EventSeq guard makes an
// already-applied observation a no-op with no version expectation. A
// row the store no longer retains (evicted past retention) is an
// HONEST TERMINAL PROJECTION GAP: the turn's routing state is retired
// and the event is skipped (nil error, no write) — never a hard pass
// failure, never a resurrected row, never a wedged cursor.
func (m *Materializer) updateTurn(ctx context.Context, sess *sessionState, ts *turnState, u turns.Update) error {
	var lastErr error
	for range 5 {
		current, err := m.proj.Get(ctx, sess.id, turns.TurnID(ts.taskID))
		if err != nil {
			if errors.Is(err, turns.ErrTurnNotFound) {
				ts.retired = true
				return nil
			}
			return err
		}
		if current.Sealed {
			ts.sealed = true
			return nil
		}
		_, err = m.proj.Update(ctx, sess.id, turns.TurnID(ts.taskID), current.Version, u)
		if err == nil {
			return nil
		}
		if errors.Is(err, turns.ErrStaleVersion) {
			lastErr = err
			continue
		}
		if errors.Is(err, turns.ErrTurnNotFound) {
			// Evicted between the read and the write (or the read raced
			// a retention pass): same honest terminal-gap handling.
			ts.retired = true
			return nil
		}
		if errors.Is(err, turns.ErrTurnSealed) {
			ts.sealed = true
			return err
		}
		return err
	}
	return fmt.Errorf("materializer: update %s: %w", ts.taskID, lastErr)
}

// sealTurn applies one Seal observation. Same retry/guard semantics as
// updateTurn, including the honest terminal-gap handling: a row the
// store no longer retains retires the turn's routing state and skips
// (never a hard pass failure, never a resurrected row).
func (m *Materializer) sealTurn(ctx context.Context, sess *sessionState, ts *turnState, s turns.Seal) (turns.TurnRow, error) {
	var lastErr error
	for range 5 {
		current, err := m.proj.Get(ctx, sess.id, turns.TurnID(ts.taskID))
		if err != nil {
			if errors.Is(err, turns.ErrTurnNotFound) {
				ts.retired = true
				return turns.TurnRow{}, nil
			}
			return turns.TurnRow{}, err
		}
		if current.Sealed {
			ts.sealed = true
			return current, nil
		}
		row, err := m.proj.Seal(ctx, sess.id, turns.TurnID(ts.taskID), current.Version, s)
		if err == nil {
			return row, nil
		}
		if errors.Is(err, turns.ErrStaleVersion) {
			lastErr = err
			continue
		}
		if errors.Is(err, turns.ErrTurnNotFound) {
			ts.retired = true
			return turns.TurnRow{}, nil
		}
		return turns.TurnRow{}, err
	}
	return turns.TurnRow{}, fmt.Errorf("materializer: seal %s: %w", ts.taskID, lastErr)
}

// attachReasoning applies one AttachReasoning observation (replay-safe
// like updateTurn, with the same honest terminal-gap handling for an
// evicted row).
func (m *Materializer) attachReasoning(ctx context.Context, sess *sessionState, ts *turnState, r turns.ReasoningInput) (turns.TurnRow, error) {
	var lastErr error
	for range 5 {
		current, err := m.proj.Get(ctx, sess.id, turns.TurnID(ts.taskID))
		if err != nil {
			if errors.Is(err, turns.ErrTurnNotFound) {
				ts.retired = true
				return turns.TurnRow{}, nil
			}
			return turns.TurnRow{}, err
		}
		if current.Sealed {
			ts.sealed = true
			return current, nil
		}
		row, err := m.proj.AttachReasoning(ctx, sess.id, turns.TurnID(ts.taskID), current.Version, r)
		if err == nil {
			return row, nil
		}
		if errors.Is(err, turns.ErrStaleVersion) {
			lastErr = err
			continue
		}
		if errors.Is(err, turns.ErrTurnNotFound) {
			ts.retired = true
			return turns.TurnRow{}, nil
		}
		return turns.TurnRow{}, err
	}
	return turns.TurnRow{}, fmt.Errorf("materializer: attach reasoning %s: %w", ts.taskID, lastErr)
}

// attachAppRefs applies one AttachAppRefs observation (replay-safe like
// updateTurn, with the same honest terminal-gap handling for an
// evicted row).
func (m *Materializer) attachAppRefs(ctx context.Context, sess *sessionState, ts *turnState, a turns.AppRefInput) (turns.TurnRow, error) {
	var lastErr error
	for range 5 {
		current, err := m.proj.Get(ctx, sess.id, turns.TurnID(ts.taskID))
		if err != nil {
			if errors.Is(err, turns.ErrTurnNotFound) {
				ts.retired = true
				return turns.TurnRow{}, nil
			}
			return turns.TurnRow{}, err
		}
		if current.Sealed {
			ts.sealed = true
			return current, nil
		}
		row, err := m.proj.AttachAppRefs(ctx, sess.id, turns.TurnID(ts.taskID), current.Version, a)
		if err == nil {
			return row, nil
		}
		if errors.Is(err, turns.ErrStaleVersion) {
			lastErr = err
			continue
		}
		if errors.Is(err, turns.ErrTurnNotFound) {
			ts.retired = true
			return turns.TurnRow{}, nil
		}
		return turns.TurnRow{}, err
	}
	return turns.TurnRow{}, fmt.Errorf("materializer: attach app refs %s: %w", ts.taskID, lastErr)
}
