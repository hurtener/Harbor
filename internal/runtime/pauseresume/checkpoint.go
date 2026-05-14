package pauseresume

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// checkpointKindPrefix namespaces every pause checkpoint in the
// StateStore. The full Kind is checkpointKindPrefix + string(token):
// per-token so two pauses under the same (tenant, user, session, run)
// quadruple occupy distinct StateStore slots (StateStore.Save
// overwrites a slot keyed on (Quadruple, Kind), so a shared Kind would
// make the second pause evict the first).
const checkpointKindPrefix = "pauseresume.checkpoint:"

// checkpointRecord is the JSON envelope persisted to the StateStore
// for a durable pause — the canonical RFC §6.3 "Pause-state
// serialization format" (JSON with format_version: 1). Phase 50
// shipped the envelope shape as the Coordinator's checkpoint surface;
// Phase 51 closes the fail-loudly serialise contract ON it
// (SerializeRecord / DeserializeRecord in pauserecord.go) — see D-069.
//
// The FormatVersion field is the forward-compatibility hinge:
// SerializeRecord stamps it to the current FormatVersion constant,
// DeserializeRecord enforces it (ErrUnsupportedFormatVersion on a
// version this Runtime does not recognise).
//
// TrajectoryBytes holds the output of trajectory.Trajectory.Serialize
// verbatim — opaque, canonical JSON bytes. Storing it as a nested
// json.RawMessage (rather than re-marshalling) preserves the
// byte-stable round-trip the trajectory package guarantees.
type checkpointRecord struct {
	// FormatVersion is the envelope schema version (RFC §6.3:
	// format_version: 1). Owned by SerializeRecord — it stamps the
	// current FormatVersion constant on every write; DeserializeRecord
	// rejects any other value loud.
	FormatVersion int `json:"format_version"`
	// Token is the opaque pause Token (also the StateRecord EventID).
	Token Token `json:"token"`
	// Reason is one of the four canonical pause reasons.
	Reason Reason `json:"reason"`
	// State is the pause lifecycle state (paused / resumed).
	State State `json:"state"`
	// Identity is the (tenant, user, session) triple the pause was
	// recorded under. Persisted IN the envelope so Status / Resume can
	// recover the scope from a Token alone (the restart-survival path).
	Identity identity.Identity `json:"identity"`
	// RunID is the per-execution run id, when the pause is run-scoped.
	// Empty for session-scoped pauses (e.g. a pre-run approval gate).
	RunID string `json:"run_id,omitempty"`
	// Payload is the sanitised, bounded pause payload.
	Payload map[string]any `json:"payload,omitempty"`
	// PausedAt is the wall-clock time the pause was recorded.
	PausedAt time.Time `json:"paused_at"`
	// ResumedAt is the wall-clock time Resume was called; zero unless
	// State == StatusResumed.
	ResumedAt time.Time `json:"resumed_at,omitempty"`
	// TrajectoryBytes is trajectory.Trajectory.Serialize output,
	// stored verbatim. Nil when the PauseRequest carried no trajectory.
	TrajectoryBytes json.RawMessage `json:"trajectory_bytes,omitempty"`
}

// quadruple reconstructs the StateStore identity key from the
// envelope's Identity + RunID.
func (c checkpointRecord) quadruple() identity.Quadruple {
	return identity.Quadruple{Identity: c.Identity, RunID: c.RunID}
}

// kind is the per-token StateStore Kind for this checkpoint.
func checkpointKind(token Token) string {
	return checkpointKindPrefix + string(token)
}

// saveCheckpoint persists rec to the StateStore. The Token doubles as
// the StateRecord EventID (the StateStore's EventID is a free-form
// string; the SQLite/Postgres schemas index it without a ULID-format
// constraint), so LoadByEventID(token) resolves the record from a
// Token alone.
//
// Save is idempotent on EventID with byte-equal Bytes; the Coordinator
// re-saves an updated envelope (e.g. state flipped to resumed) under
// the SAME Token, which would trip the StateStore's
// ErrIdempotencyConflict guard. To allow the in-place state flip, the
// Coordinator deletes-then-saves on a status change rather than
// re-saving the same EventID with different Bytes — see
// coordinator.go's resume path.
func saveCheckpoint(ctx context.Context, store state.StateStore, rec checkpointRecord) error {
	// SerializeRecord is the fail-loudly pause-record serialise contract
	// (Phase 51 / D-069): it walks the envelope reflectively and
	// surfaces trajectory.ErrUnserializable naming the offending leaf
	// (the load-bearing case: a non-JSON-encodable Payload value) —
	// never a silent drop, never a half-persisted checkpoint. It also
	// stamps the current format_version.
	bytes, err := SerializeRecord(rec)
	if err != nil {
		// trajectory.ErrUnserializable propagates verbatim — the caller
		// reaches it via errors.As. No half-persist: this returns before
		// store.Save is ever called.
		return err
	}
	sr := state.StateRecord{
		ID:       state.EventID(rec.Token),
		Identity: rec.quadruple(),
		Kind:     checkpointKind(rec.Token),
		Bytes:    bytes,
	}
	if err := store.Save(ctx, sr); err != nil {
		return fmt.Errorf("pauseresume: save checkpoint for token %q: %w", rec.Token, err)
	}
	return nil
}

// loadCheckpoint resolves a checkpoint record from a Token alone via
// the StateStore's EventID secondary index. Returns ErrPauseNotFound
// (wrapping state.ErrNotFound) when no checkpoint exists for the
// token, ErrCheckpointCorrupt when the persisted bytes fail to decode,
// and ErrUnsupportedFormatVersion when the record carries a
// format_version this Runtime does not recognise — all loud, never a
// half-decoded record (Phase 51 / D-069).
func loadCheckpoint(ctx context.Context, store state.StateStore, token Token) (checkpointRecord, error) {
	sr, err := store.LoadByEventID(ctx, state.EventID(token))
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return checkpointRecord{}, fmt.Errorf("%w: token %q", ErrPauseNotFound, token)
		}
		return checkpointRecord{}, fmt.Errorf("pauseresume: load checkpoint for token %q: %w", token, err)
	}
	// DeserializeRecord is the load-side half of the fail-loudly
	// pause-record serialise contract: it surfaces ErrCheckpointCorrupt
	// on malformed bytes and ErrUnsupportedFormatVersion on a version
	// this Runtime cannot read.
	rec, err := DeserializeRecord(sr.Bytes)
	if err != nil {
		return checkpointRecord{}, fmt.Errorf("%w (token %q)", err, token)
	}
	return rec, nil
}

// deleteCheckpoint removes a checkpoint from the StateStore. Idempotent
// — deleting an absent checkpoint is a no-op (state.StateStore.Delete
// returns nil on a missing record).
func deleteCheckpoint(ctx context.Context, store state.StateStore, rec checkpointRecord) error {
	if err := store.Delete(ctx, rec.quadruple(), checkpointKind(rec.Token)); err != nil {
		return fmt.Errorf("pauseresume: delete checkpoint for token %q: %w", rec.Token, err)
	}
	return nil
}
