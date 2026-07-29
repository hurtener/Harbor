package planner_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
)

// classified is the shape the runtime's dispatch layer attaches: an
// error that names its class without changing its message or breaking
// its wrap chain.
type classified struct {
	err   error
	class planner.ObservationClass
}

func (c classified) Error() string                              { return c.err.Error() }
func (c classified) Unwrap() error                              { return c.err }
func (c classified) ObservationClass() planner.ObservationClass { return c.class }

// TestObservationClassOf_ReadsTheClassThroughTheWrapChain is the
// property the whole vocabulary rests on: the classification is
// attached at the dispatch boundary and read at the run loop, five %w
// hops apart. A lookup that only worked on an unwrapped error would be
// a guard that never fires in production.
func TestObservationClassOf_ReadsTheClassThroughTheWrapChain(t *testing.T) {
	t.Parallel()

	sentinel := errors.New("dispatch: artifact reference does not resolve under this run's scope")
	base := classified{
		err:   fmt.Errorf("%w: %q", sentinel, "abc123"),
		class: planner.ObservationClassArtifactRefNotFound,
	}

	for _, tc := range []struct {
		name string
		err  error
		want planner.ObservationClass
	}{
		{"nil carries no class", nil, ""},
		{"an unrelated error carries no class", errors.New("tool blew up"), ""},
		{"an unwrapped classified error", base, planner.ObservationClassArtifactRefNotFound},
		{
			"one wrap deep",
			fmt.Errorf("resolve artifact reference: %w", error(base)),
			planner.ObservationClassArtifactRefNotFound,
		},
		{
			"three wraps deep",
			fmt.Errorf("tool %q invoke: %w", "t",
				fmt.Errorf("invalid args: %w",
					fmt.Errorf("resolve artifact reference: %w", error(base)))),
			planner.ObservationClassArtifactRefNotFound,
		},
		{
			"the resolver-unavailable class",
			fmt.Errorf("a: %w", error(classified{
				err:   errors.New("no artifact store wired"),
				class: planner.ObservationClassArtifactResolverUnavailable,
			})),
			planner.ObservationClassArtifactResolverUnavailable,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := planner.ObservationClassOf(tc.err); got != tc.want {
				t.Errorf("ObservationClassOf(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestObservationClassOf_PreservesTheUnderlyingMessage pins that
// classifying does not rewrite the error text. No shipped transcript or
// log grep is invalidated by this phase — that is why the classification
// is computed over the sentinels that already exist rather than declared
// as a new one.
func TestObservationClassOf_PreservesTheUnderlyingMessage(t *testing.T) {
	t.Parallel()
	inner := errors.New("dispatch: artifact reference does not resolve under this run's scope: \"abc\"")
	wrapped := error(classified{err: inner, class: planner.ObservationClassArtifactRefNotFound})
	if wrapped.Error() != inner.Error() {
		t.Fatalf("classifying rewrote the message:\n got %q\nwant %q", wrapped.Error(), inner.Error())
	}
	if !errors.Is(wrapped, inner) {
		t.Fatal("classifying broke the wrap chain — errors.Is no longer reaches the sentinel")
	}
}

// TestObservationClass_KeyAndValuesAreStable pins the wire-visible
// strings. They travel into the trajectory, into replay and into the
// rendered prompt, so a rename is a contract change, not a refactor.
func TestObservationClass_KeyAndValuesAreStable(t *testing.T) {
	t.Parallel()
	if planner.ObservationClassKey != "error_class" {
		t.Errorf("ObservationClassKey = %q, want error_class", planner.ObservationClassKey)
	}
	if planner.ObservationClassArtifactRefNotFound != "artifact_ref_not_found" {
		t.Errorf("ref-not-found class = %q", planner.ObservationClassArtifactRefNotFound)
	}
	if planner.ObservationClassArtifactResolverUnavailable != "artifact_resolver_unavailable" {
		t.Errorf("resolver-unavailable class = %q", planner.ObservationClassArtifactResolverUnavailable)
	}
}

// TestParallelBranchObservation_ClassJSONRoundTrip covers both legs of
// the persisted shape: a classified branch carries `error_class`, and an
// unclassified one is byte-identical to what it was before the field
// existed (the `omitempty` leg). A widened payload on every tool error
// would be an unannounced prompt change on the hottest path.
func TestParallelBranchObservation_ClassJSONRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("classified", func(t *testing.T) {
		in := planner.ParallelBranchObservation{
			CallID: "c1", Tool: "reader", Index: 0,
			Error:      "tool \"reader\" invoke: artifact reference does not resolve",
			ErrorClass: planner.ObservationClassArtifactRefNotFound,
		}
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if !strings.Contains(string(raw), `"error_class":"artifact_ref_not_found"`) {
			t.Fatalf("encoded branch does not carry the class: %s", raw)
		}
		var out planner.ParallelBranchObservation
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}
		if out.ErrorClass != in.ErrorClass {
			t.Errorf("ErrorClass = %q after round trip, want %q", out.ErrorClass, in.ErrorClass)
		}
	})

	t.Run("unclassified emits no key", func(t *testing.T) {
		in := planner.ParallelBranchObservation{
			CallID: "c1", Tool: "reader", Index: 0, Error: "boom",
		}
		raw, err := json.Marshal(in)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		if strings.Contains(string(raw), "error_class") {
			t.Fatalf("an unclassified branch emitted the class key: %s", raw)
		}
	})
}
