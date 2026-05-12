package trajectory

import "fmt"

// ErrUnserializable is the fail-loudly sentinel returned by
// Trajectory.Serialize when ANY leaf of the trajectory is not
// JSON-encodable. The Field path names the offending location
// (e.g. "Trajectory.Steps[3].Observation.callback") so the
// returned error is actionable.
//
// The error is a struct (not a sentinel var) so callers can extract
// the Field path via errors.As:
//
//	var unserr trajectory.ErrUnserializable
//	if errors.As(err, &unserr) {
//	    log.Printf("non-encodable leaf at %s", unserr.Field)
//	}
//
// This closes the predecessor's silent-context-loss bug: there is
// no `try { ... } catch { return nil }`-shaped path through
// Serialize. Either the trajectory encodes cleanly and bytes are
// returned, or this struct error is raised. See RFC §3.4 + §6.2
// and brief 02 §4.
type ErrUnserializable struct {
	// Field is the dotted path to the offending leaf, rooted at
	// "Trajectory". Example: "Trajectory.Steps[2].Action.fn".
	Field string
}

// Error returns the canonical message naming the offending field.
func (e ErrUnserializable) Error() string {
	return fmt.Sprintf("trajectory: serialize failed: non-JSON-encodable value at %s", e.Field)
}

// ErrToolContextLost is the fail-loudly sentinel returned by
// HandleRegistry.Get when the requested HandleID has no live mapping
// in the registry. Typical cause: a serialised trajectory referencing
// a HandleID whose owning runtime process died before resume.
//
// The error is a struct so callers can extract the Handle via
// errors.As:
//
//	var lost trajectory.ErrToolContextLost
//	if errors.As(err, &lost) {
//	    log.Printf("cannot resume: handle %s is lost", lost.Handle)
//	}
//
// Resume MUST surface this error to the operator — silent fallback to
// (nil, nil) is the bug this contract closes.
type ErrToolContextLost struct {
	// Handle is the missing HandleID.
	Handle HandleID
}

// Error returns the canonical message naming the missing handle.
func (e ErrToolContextLost) Error() string {
	return fmt.Sprintf("trajectory: tool context lost: no live registry mapping for handle %q", string(e.Handle))
}
