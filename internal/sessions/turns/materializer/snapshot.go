package materializer

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions/turns"
)

// ErrTaskSnapshotNotFound reports that the task record the injected
// reader was asked for does not exist (or is not visible under the
// event identity). The materializer treats it as the HONEST LEGACY
// ABSENCE case: every component the snapshot would carry stays
// explicitly unavailable and the projection continues — a missing
// record is never a hard pass failure. Any OTHER error the reader
// returns is a transient/hard snapshot failure that aborts the
// projection WITHOUT advancing the checkpoint (the runtime's canonical
// task store is down, slow, or corrupt — never silently assumed away).
var ErrTaskSnapshotNotFound = errors.New("materializer: task record not found")

// TaskSnapshotReader is the injected, READ-ONLY seam over the runtime's
// already-redacted canonical task records. It lets the materializer
// converge the query / agent / input-attachment / answer / output-
// attachment / failure components from the authoritative task record
// while projecting a successfully persisted task event.
//
// The implementation contract is binding:
//
//   - READ-ONLY — the reader never mutates the task record.
//   - ALREADY-REDACTED — every string the reader returns has already
//     run through the runtime's audit redactor (the same redaction the
//     task record received before persistence). The materializer does
//     not re-redact and cannot make hostile text safe.
//   - BOUNDED — query, inline answer, terminal messages, and
//     attachment / reference metadata must respect the turns
//     projection's bounds (turns.MaxQueryRunes,
//     turns.MaxInlineAnswerBytes, turns.MaxTerminalMessageRunes, ...).
//     An over-bound value is REFUSED loudly by the projector — the
//     seam never truncates, clamps, or silently omits.
//   - STRUCTURALLY EXCLUDED — the snapshot DTO (TaskSnapshot) carries
//     no byte fields, no raw arguments, no secrets, no caller memory,
//     no provider reasoning, no tool results, no correlation/context
//     ids, and no pause / resume / approval authority; an
//     implementation cannot surface them through it.
//
// The materializer invokes the reader ONLY while projecting a
// successfully persisted task event (task.spawned / task.completed /
// task.failed), under the EVENT's isolation triple — never on a
// Protocol read (the projector's List / Get surface never touches it)
// and never with a widened identity.
//
// A nil reader (the runtime declared none) is an HONEST availability
// gap: the query / agent / input / answer / output / failure-message
// components stay unavailable and a complete seal defers, exactly as
// before the seam existed. Runtimes that persist the answer on the
// task record MUST wire a reader so the durable turn converges.
//
// ErrTaskSnapshotNotFound reports a missing record; the materializer
// treats it as the legacy-absence case (all components unavailable),
// never as a hard failure.
type TaskSnapshotReader interface {
	// Task returns the bounded, already-redacted snapshot of the task
	// record identified by taskID, read under the event identity id.
	Task(ctx context.Context, id identity.Identity, taskID string) (TaskSnapshot, error)
}

// TaskSnapshot is the bounded, already-redacted READ of one canonical
// task record the materializer may project. Every component group
// carries its own presence flag: a legacy record that lacks the field
// reports the group absent and the corresponding turn component stays
// explicitly unavailable — absence is never fabricated into a value,
// and a present-but-empty query / answer stays honest at the projector
// (a run with no user-visible query, or a definite empty answer).
//
// The DTO is structurally UNABLE to carry artifact bytes, raw
// arguments, secrets, caller memory, provider reasoning, tool results,
// correlation/context ids, or pause authority — there are no fields
// for them.
type TaskSnapshot struct {
	// TaskID is the authoritative task id of the record when the
	// record reports one separately from the requested id; empty means
	// the requested id stands.
	TaskID string
	// RunID is the actual runtime run id the record executed under;
	// empty = unavailable (never equated with TaskID).
	RunID string

	// QueryPresent reports whether the record carries a renderable
	// user query. When true, Query (bounded by turns.MaxQueryRunes)
	// and QueryAt feed the turn's Query component; when false the
	// component stays unavailable.
	QueryPresent bool
	Query        string
	QueryAt      time.Time

	// AgentPresent reports whether the record carries an effective
	// agent binding. When true, AgentID / AgentName /
	// AgentBindingSource feed the turn's Agent component; when false
	// it stays unavailable.
	AgentPresent       bool
	AgentID            string
	AgentName          string
	AgentBindingSource turns.AgentBindingSource

	// InputsPresent reports whether the record carries input
	// attachment metadata (never bytes). When true, Inputs seed the
	// turn's input list at spawn; when false the turn starts with the
	// disposition-derived inputs only (possibly none).
	InputsPresent bool
	Inputs        []turns.Attachment

	// AnswerPresent reports whether the record carries the terminal
	// answer envelope. When true, Answer.State is one of the closed
	// definite envelope states — inline / artifact_ref / empty — and
	// Answer.Inline / Answer.Ref carry the bounded shape; when false
	// the answer component stays unavailable and a complete seal is
	// deferred honestly (the row stays mutable).
	AnswerPresent bool
	Answer        turns.Answer

	// OutputsPresent reports whether the record carries output
	// attachment metadata (never bytes). When true, Outputs replace
	// the turn's output list at completion; when false the outputs
	// stay as accumulated (or absent).
	OutputsPresent bool
	Outputs        []turns.Attachment

	// FailurePresent reports whether the record carries the terminal
	// failure classification. When true, ErrorCode is the canonical
	// classified code (driving the closed error class) and
	// ErrorMessage the bounded safe message ("" = none available);
	// when false the FAILED seal derives the closed error class from
	// the task.failed event's code alone and the message stays
	// unavailable.
	FailurePresent bool
	ErrorCode      string
	ErrorMessage   string
}

// WithTaskSnapshotReader wires the injected READ-ONLY seam over the
// runtime's already-redacted canonical task records. A nil reader (the
// runtime declared none) is an honest availability gap — the
// components the snapshot would carry stay unavailable and a complete
// seal defers, exactly as before the seam existed.
func WithTaskSnapshotReader(r TaskSnapshotReader) Option {
	return func(m *Materializer) { m.snap = r }
}

// snapshotOutputs returns the snapshot's output attachment metadata
// when the record reports it, or nil (leave the row's outputs
// unchanged) for a legacy record that carries none. Update slice
// semantics: nil leaves unchanged, a non-nil slice (even empty)
// replaces wholesale.
func snapshotOutputs(snap TaskSnapshot) []turns.Attachment {
	if !snap.OutputsPresent {
		return nil
	}
	return snap.Outputs
}

// readTaskSnapshot reads the canonical task record through the
// injected seam. It is invoked ONLY while projecting a successfully
// persisted task event (the spawn / completion / failure projections
// in observe.go) — never on a Protocol read. A nil reader or a missing
// record (ErrTaskSnapshotNotFound) is the honest legacy-absence case:
// the zero snapshot leaves every component explicitly unavailable. Any
// other error — or a snapshot that violates the DTO contract — is a
// transient/hard snapshot failure that aborts the projection without
// advancing the checkpoint.
func (m *Materializer) readTaskSnapshot(ctx context.Context, id identity.Identity, taskID string) (TaskSnapshot, error) {
	if m.snap == nil {
		return TaskSnapshot{}, nil
	}
	snap, err := m.snap.Task(ctx, id, taskID)
	if err != nil {
		if errors.Is(err, ErrTaskSnapshotNotFound) {
			return TaskSnapshot{}, nil
		}
		return TaskSnapshot{}, fmt.Errorf("materializer: task snapshot %s: %w", taskID, err)
	}
	if err := validateTaskSnapshot(snap); err != nil {
		return TaskSnapshot{}, err
	}
	return snap, nil
}

// validateTaskSnapshot enforces the snapshot DTO contract at the seam:
// an AnswerPresent snapshot must carry a DEFINITE closed answer
// envelope (inline / artifact_ref / empty — the only states the reader
// may report), an artifact_ref answer must carry its ref, an inline /
// empty answer must not, and an absent answer must carry no content. A
// reader that violates the contract is corrupt and fails the
// projection loudly — it is never silently mapped onto a weaker claim.
func validateTaskSnapshot(s TaskSnapshot) error {
	if !s.AnswerPresent {
		if s.Answer.State != "" || s.Answer.Inline != "" || s.Answer.Ref != nil {
			return fmt.Errorf("materializer: task snapshot answer is absent but carries content (state %q) — corrupt snapshot refused", s.Answer.State)
		}
		return nil
	}
	switch s.Answer.State {
	case turns.AnswerStateInline:
		if s.Answer.Ref != nil {
			return fmt.Errorf("materializer: task snapshot inline answer carries a ref — corrupt snapshot refused")
		}
	case turns.AnswerStateArtifactRef:
		if s.Answer.Ref == nil || s.Answer.Ref.ID == "" {
			return fmt.Errorf("materializer: task snapshot artifact_ref answer carries no ref — corrupt snapshot refused")
		}
		if s.Answer.Inline != "" {
			return fmt.Errorf("materializer: task snapshot artifact_ref answer carries inline text — corrupt snapshot refused")
		}
	case turns.AnswerStateEmpty:
		if s.Answer.Inline != "" || s.Answer.Ref != nil {
			return fmt.Errorf("materializer: task snapshot empty answer carries content — corrupt snapshot refused")
		}
	default:
		return fmt.Errorf("materializer: task snapshot answer state %q is not a definite envelope (inline / artifact_ref / empty) — corrupt snapshot refused", s.Answer.State)
	}
	return nil
}
