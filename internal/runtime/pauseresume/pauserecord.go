package pauseresume

import (
	"encoding/json"
	"fmt"

	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

// FormatVersion is the pause-record wire-format version. RFC §6.3
// settles the pause-state serialisation format as "JSON with
// format_version: 1" — aligned with the event bus (also JSON) and
// operational simplicity (resolves brief 02 Q-2). Phase 50 shipped the
// checkpointRecord envelope with this field as the forward-compat
// hinge; Phase 51 closes the fail-loudly serialise contract ON it.
//
// FormatVersion is an int so a future format bump is a single-line
// change with a deterministic, comparable guard on load (see
// DeserializeRecord). Bumping it is an RFC change.
const FormatVersion = 1

// SerializeRecord returns the canonical JSON bytes of a pause-record
// envelope, failing LOUD on ANY non-JSON-encodable leaf — never
// silently dropping a field.
//
// # The contract (RFC §6.3 + §3.4, D-069)
//
// This is the Phase 51 closure of the silent-context-loss bug for the
// pause record's OWN envelope. Phase 43 already closed it for the
// trajectory (trajectory.Trajectory.Serialize); Phase 50 propagated
// trajectory.ErrUnserializable verbatim out of Request. But the pause
// record carries one more caller-controlled, JSON-tree-shaped field —
// Payload map[string]any — and Phase 50's checkpoint save reached it
// via a bare json.Marshal. A bare json.Marshal on a non-encodable
// Payload leaf returns *json.UnsupportedTypeError: technically loud,
// but WITHOUT the actionable dotted field path the fail-loudly
// contract requires (RFC §3.4: "MUST return ErrUnserializable naming
// the offending field path"). SerializeRecord closes that gap.
//
//   - Success: returns canonical JSON bytes with format_version set to
//     the current FormatVersion. The round-trip
//     SerializeRecord → DeserializeRecord → SerializeRecord is
//     byte-identical for any record whose Payload holds JSON-tree
//     shapes (map[string]any / []any / primitives).
//   - Failure: returns (nil, trajectory.ErrUnserializable{Field:
//     "PauseRecord.payload.<key>"}) — the offending leaf is named in
//     the caller's own envelope vocabulary. No silent-drop path; no
//     half-persisted checkpoint (coordinator.go rejects the Request
//     before touching the in-memory registry or the store).
//
// The pre-flight reflective walk is trajectory.ValidateEncodable — the
// SAME walker Phase 43 uses for the trajectory. Phase 51 does NOT
// re-implement a second fail-loudly serialiser (that would be the
// CLAUDE.md §13 two-parallel-implementations anti-pattern, exactly the
// shape the Wave 8 audit's capfilter extraction killed); it shares the
// Phase 43 primitive. See D-069's "reuse vs share" call.
//
// FormatVersion is stamped here, not trusted from the caller: the
// caller hands SerializeRecord a checkpointRecord and SerializeRecord
// owns the version field. This keeps "what version did we write" a
// single source of truth.
func SerializeRecord(rec checkpointRecord) ([]byte, error) {
	// Stamp the current format version — SerializeRecord owns it.
	rec.FormatVersion = FormatVersion

	// Pre-flight: walk the envelope reflectively and fail loud on the
	// first non-JSON-encodable leaf with a precise field path. The
	// load-bearing field is Payload (caller-controlled map[string]any);
	// TrajectoryBytes is already-canonical json.RawMessage (validated
	// at the trajectory.Serialize boundary in coordinator.go) and the
	// rest of the envelope is typed runtime bookkeeping. Walking the
	// whole struct is correct and cheap — the walker mirrors
	// encoding/json's rules, so a leaf the walker passes is one
	// json.Marshal below cannot choke on.
	if err := trajectory.ValidateEncodable(rec, "PauseRecord"); err != nil {
		// trajectory.ErrUnserializable propagates verbatim — the caller
		// reaches it via errors.As against the trajectory package's
		// struct sentinel. No silent nil.
		return nil, err
	}

	// Happy path: the walker passed, so stdlib json.Marshal is the
	// canonical encoder and cannot fail on the envelope. Map keys are
	// alphabetised; struct fields encode per JSON tag in declaration
	// order — the same canonical-ordering discipline D-049 pins for the
	// trajectory, so the pause-record round-trip is byte-stable too.
	b, err := json.Marshal(rec)
	if err != nil {
		// Defence in depth: the walker mirrors encoding/json's rules,
		// so this branch is unreachable on a record the walker passed.
		// If a future-tightened json.Marshal edge case slips past the
		// walker, surface it loud as ErrUnserializable rather than a
		// bare json error — the fail-loudly contract is one shape.
		return nil, trajectory.ErrUnserializable{
			Field: fmt.Sprintf("PauseRecord (json.Marshal: %v)", err),
		}
	}
	return b, nil
}

// DeserializeRecord parses canonical pause-record JSON bytes back into
// a checkpointRecord, failing LOUD on a corrupt envelope or an
// unsupported format_version — never returning a half-decoded record.
//
//   - Malformed JSON or a type mismatch on a typed field surfaces
//     ErrCheckpointCorrupt (wrapping the underlying json error).
//   - A format_version the runtime does not recognise surfaces
//     ErrUnsupportedFormatVersion naming the version read — a record
//     written by a newer Runtime is rejected loud, not silently
//     mis-decoded against the current schema.
//
// The format_version guard is the load-side half of the RFC §6.3
// "JSON with format_version: 1" contract: SerializeRecord stamps it,
// DeserializeRecord enforces it. A pause record with a missing /
// zero / unknown version is a corruption or a forward-incompatible
// write — both fail loud here (D-069).
func DeserializeRecord(b []byte) (checkpointRecord, error) {
	if len(b) == 0 {
		return checkpointRecord{}, fmt.Errorf("%w: empty pause-record bytes", ErrCheckpointCorrupt)
	}

	var rec checkpointRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return checkpointRecord{}, fmt.Errorf("%w: %w", ErrCheckpointCorrupt, err)
	}

	// format_version guard: a record the current Runtime cannot read is
	// rejected loud. This catches both a corrupt/absent version (zero)
	// and a forward-incompatible write (a higher version from a newer
	// Runtime).
	if rec.FormatVersion != FormatVersion {
		return checkpointRecord{}, fmt.Errorf(
			"%w: read format_version %d, runtime supports %d",
			ErrUnsupportedFormatVersion, rec.FormatVersion, FormatVersion,
		)
	}

	return rec, nil
}
