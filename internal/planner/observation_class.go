package planner

import "errors"

// ObservationClass is the machine-readable kind of a failed step's
// observation.
//
// It exists so a planner can tell "the artifact id you named does not
// resolve" from "the tool itself failed" from "the runtime is
// misconfigured" by reading a FIELD, rather than by string-matching an
// error message that was never a contract. The empty value means "the
// tool's own error" — the shape every tool failure has always had, and
// the one this vocabulary deliberately does not disturb: an unclassified
// failure's observation is byte-identical to what it was before the
// class existed.
//
// The set is closed at two by decision, not by omission. The artifact
// carrier's remaining failures are not classed: an empty reference id is
// an argument-shape failure the schema is supposed to prevent and the
// in-process driver already carries as an invalid-arguments error, and
// "resolved before dispatch bound it" / "substitution target is not
// addressable" are programming errors for which neither a model nor an
// operator has a repair. Extending the set means adding a constant here
// and a case to the runtime dispatch layer's classifier — the two sites
// this vocabulary has.
type ObservationClass string

const (
	// ObservationClassArtifactRefNotFound says the model named an
	// artifact id that does not resolve under the run's isolation
	// triple. RECOVERABLE BY THE MODEL on its next turn: the session's
	// artifact manifest already lists what is reachable, so the repair is
	// to name a different id (or to stop asking).
	ObservationClassArtifactRefNotFound ObservationClass = "artifact_ref_not_found"

	// ObservationClassArtifactResolverUnavailable says no artifact store
	// was wired, or no resolver was seated on the dispatch context. That
	// is operator misconfiguration and is explicitly NOT recoverable by
	// the model — the class exists so a planner does not spend its step
	// budget discovering that by retrying.
	ObservationClassArtifactResolverUnavailable ObservationClass = "artifact_resolver_unavailable"
)

// ClassifiedError is an error that names the observation class it should
// be rendered under. The runtime's dispatch layer attaches it; the run
// loop reads it back with [ObservationClassOf].
//
// Deliberately NOT a new sentinel. The implementations classify with
// errors.Is over the sentinels that already exist, so no shipped error
// message changes and no transcript or log grep is invalidated.
type ClassifiedError interface {
	error

	// ObservationClass names the kind of failure this error describes.
	ObservationClass() ObservationClass
}

// ObservationClassKey is the map key a step's error observation carries
// the class under, alongside the existing "error" key. It is a JSON
// field name: the observation is persisted in the trajectory and
// rendered back into the prompt, so the key travels with it.
const ObservationClassKey = "error_class"

// ObservationClassOf reports the class carried anywhere in err's wrap
// chain, or the empty class when err is nil or carries none.
//
// The chain matters: a resolution failure is wrapped at five hops
// between the resolver that produced it and the run loop that renders
// it, and every hop uses %w precisely so this lookup keeps working.
func ObservationClassOf(err error) ObservationClass {
	if err == nil {
		return ""
	}
	var classified ClassifiedError
	if errors.As(err, &classified) {
		return classified.ObservationClass()
	}
	return ""
}
