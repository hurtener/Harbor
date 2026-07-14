// Package projectioncheck is the registry-gated projection-completeness
// gate — the mechanical close of the silent-absence class the runtime's
// read surfaces kept re-introducing (a read surface declares a typed,
// wire-visible field, ships a facet / sort / aggregate over it, and never
// populates it, so the operation returns FALSE ABSENCE on a fleet full of
// matching data).
//
// # The two coverage halves
//
// The class has two variants, and a single reflection probe closes only
// one, so the gate ships two halves:
//
//   - Half A — never-assigned (reflection). For every projection surface,
//     cross-reference the set of wire fields the filter / sort / aggregate
//     layer OPERATES over against the set the production projector actually
//     ASSIGNS (via a probe that runs the projector over a fully-populated
//     source record). A filtered / sorted / aggregated field the projector
//     leaves at its zero value — and not on an explicit, reason-carrying
//     honest-omission allow-list — FAILS THE BUILD. CheckProbe implements
//     the reflection.
//   - Half B — never-wired (prod-wiring test). Half A cannot catch the
//     never-wired variant: for a read-time-enriched field the probe wires
//     its own populated fake and passes while the production assembly can
//     OMIT the `WithX` enricher and ship false absence. So each
//     ProjectionContract MUST name a prod-wiring test that exercises the
//     surface's projector as assembled through the real production wiring;
//     a contract with no prod-wiring test FAILS the build. The named test
//     runs the REAL assembly and asserts the enriched fields are populated,
//     so a forgotten `WithX` yields a zero field the same reflection
//     (CheckProbe over the real assembly) catches.
//
// # Coverage of surfaces
//
// Every projection surface must be registered; a surface that ships
// without registering cannot dodge either half. This mirrors the events
// `RegisteredDrivers()` conformance-parity cross-check (the events driver-registry conformance gate): a
// registered member without its mandatory run fails the build.
//
// # Self-registration (§4.4)
//
// Each surface self-registers a ProjectionContract from an init() in its
// own package, exactly like a driver self-registers into its factory. The
// gate test blank-imports the surfaces (and the prod aggregator) so the
// registered set reflects the REAL shipped surfaces. Registration lives in
// a production (non-test) file because a blank import never runs a
// package's _test.go init.
//
// # Scope (what the gate does NOT prove)
//
// The gate catches "declared-but-never-assigned filtered field," not
// "assigned with a wrong value." Deeper correctness stays the per-surface
// truthful-data tests (the projection-completeness gate). CheckProbe is pure and holds no shared
// state; the registry is write-once at init and read-many by the gate test.
package projectioncheck

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
)

// ProjectionContract is what one projection surface registers so the
// completeness gate can prove it never filters / sorts / aggregates over a
// field its projector leaves unassigned (Half A) AND that its production
// assembly actually wires the enricher the probe assumes (Half B).
type ProjectionContract struct {
	// Surface is the projection surface name (e.g. "sessions", "tasks").
	// It must be unique across registrations and non-empty.
	Surface string
	// Probe builds a fully-populated source record, runs the PRODUCTION
	// projector, and returns the projected wire row as an `any` the gate
	// reflects over (Half A — never-assigned). It MUST be pure (no shared
	// state) and MUST return a struct (or pointer to one).
	Probe func() any
	// OperatedFields is the set of wire-row json tags the surface's
	// filter / sort / aggregate layer reads. ONLY tags that are both
	// operated-over AND legitimately zero belong in HonestOmissions — a
	// populated or non-operated field needs no omission entry.
	OperatedFields []string
	// HonestOmissions maps a json tag the projector legitimately leaves
	// zero to the one-line reason it is honest (e.g. a capability-gated
	// facet pending a later phase). Every entry MUST carry a NON-EMPTY
	// reason — the gate FAILS on an empty-string reason (anti-theater): an
	// unexplained allow-list entry is how a real false-absence bug gets
	// silenced. A json tag that appears in HonestOmissions MUST also appear
	// in OperatedFields (an omission for a non-operated field is bloat).
	HonestOmissions map[string]string
	// ProdWiringTest names the test that exercises this surface's projector
	// as assembled through the real production wiring (Half B —
	// never-wired). A contract with an empty ProdWiringTest FAILS the gate:
	// a read-time-enriched surface whose probe uses a fake but whose
	// production assembly omits the `WithX` enricher would otherwise pass
	// Half A while shipping false absence in prod (the tools bug).
	ProdWiringTest string
}

// KnownSurfaces is the closed set of projection surfaces the runtime
// ships. The gate cross-checks the registered set against it (surface
// coverage): a new surface that ships without registering fails the build,
// and a registered surface not in this set is an unknown-surface failure.
// A new projection surface extends this set in the phase that ships it,
// exactly as a new capability extends canonicalCapabilities.
var KnownSurfaces = []string{"flows", "memory", "sessions", "tasks", "tools"}

var (
	mu       sync.RWMutex
	registry = map[string]ProjectionContract{}
)

// Register installs a surface's contract. Surfaces self-register from
// init(); a duplicate surface name or an empty name panics at init (a
// registration bug is a build-time programmer error, never a runtime
// condition — the same posture as a driver-registry double-register).
func Register(c ProjectionContract) {
	if strings.TrimSpace(c.Surface) == "" {
		panic("projectioncheck: Register called with an empty Surface name")
	}
	mu.Lock()
	defer mu.Unlock()
	if _, dup := registry[c.Surface]; dup {
		panic(fmt.Sprintf("projectioncheck: surface %q registered twice", c.Surface))
	}
	registry[c.Surface] = c
}

// RegisteredSurfaces returns the sorted registered surface names — the
// gate cross-checks this against KnownSurfaces (surface coverage).
func RegisteredSurfaces() []string {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]string, 0, len(registry))
	for name := range registry {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Contracts returns a snapshot of every registered contract, sorted by
// surface name. The gate test ranges it.
func Contracts() []ProjectionContract {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]ProjectionContract, 0, len(registry))
	for _, c := range registry {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Surface < out[j].Surface })
	return out
}

// Violation is a single gate failure — a surface whose operated wire field
// the projector left at its zero value without an honest-omission entry.
type Violation struct {
	// Surface is the offending projection surface.
	Surface string
	// Field is the json tag of the operated-but-unassigned field.
	Field string
	// Reason describes the failure for the gate's error output.
	Reason string
}

func (v Violation) String() string {
	return fmt.Sprintf("surface %q: operated field %q is left at its zero value by the projector and is not on the honest-omission allow-list (%s)",
		v.Surface, v.Field, v.Reason)
}

// ValidateContract checks a contract is well-formed BEFORE the reflection
// runs: a non-empty surface, a non-nil probe, a non-empty prod-wiring test
// (Half B coverage), every HonestOmissions reason non-empty (anti-theater),
// and every honest-omission key present in OperatedFields (no bloat). It
// returns the first structural problem, or nil. The gate runs this over
// every registered contract; the non-vacuity self-test runs it over
// synthetic contracts to prove each rule bites.
func ValidateContract(c ProjectionContract) error {
	if strings.TrimSpace(c.Surface) == "" {
		return fmt.Errorf("projectioncheck: contract has an empty Surface name")
	}
	if c.Probe == nil {
		return fmt.Errorf("projectioncheck: surface %q has a nil Probe", c.Surface)
	}
	if strings.TrimSpace(c.ProdWiringTest) == "" {
		return fmt.Errorf("projectioncheck: surface %q declares no prod-wiring test (Half B coverage) — a registered surface with no prod-wiring test fails the build", c.Surface)
	}
	operated := make(map[string]struct{}, len(c.OperatedFields))
	for _, f := range c.OperatedFields {
		operated[f] = struct{}{}
	}
	for field, reason := range c.HonestOmissions {
		if strings.TrimSpace(reason) == "" {
			return fmt.Errorf("projectioncheck: surface %q honest-omission for field %q carries an empty reason — the reason is load-bearing (an unexplained allow-list entry is how a real false-absence bug gets silenced)", c.Surface, field)
		}
		if _, ok := operated[field]; !ok {
			return fmt.Errorf("projectioncheck: surface %q honest-omission for field %q is not in OperatedFields — every allow-list line must be a real, operated-over absence, not bloat", c.Surface, field)
		}
	}
	return nil
}

// CheckProbe runs Half A over one contract: it executes the probe, reflects
// the projected wire row, and returns a Violation for every OperatedFields
// json tag the projector left at its zero value that is not on the
// honest-omission allow-list. A structural problem with the contract (an
// unresolvable json tag, a non-struct probe return) is returned as an
// error so a mis-authored contract fails loudly rather than passing
// vacuously.
func CheckProbe(c ProjectionContract) ([]Violation, error) {
	row := c.Probe()
	if row == nil {
		return nil, fmt.Errorf("projectioncheck: surface %q probe returned nil", c.Surface)
	}
	rv := reflect.ValueOf(row)
	for rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return nil, fmt.Errorf("projectioncheck: surface %q probe returned a nil pointer", c.Surface)
		}
		rv = rv.Elem()
	}
	if rv.Kind() != reflect.Struct {
		return nil, fmt.Errorf("projectioncheck: surface %q probe returned a %s, want a struct", c.Surface, rv.Kind())
	}
	byTag := jsonFieldIndex(rv.Type())
	var violations []Violation
	for _, tag := range c.OperatedFields {
		idx, ok := byTag[tag]
		if !ok {
			return nil, fmt.Errorf("projectioncheck: surface %q operated field %q has no json tag on %s — OperatedFields must name real wire tags", c.Surface, tag, rv.Type())
		}
		if !rv.Field(idx).IsZero() {
			continue
		}
		if reason, allowed := c.HonestOmissions[tag]; allowed {
			// Reason non-emptiness is enforced by ValidateContract; a
			// caller running CheckProbe without ValidateContract still gets
			// honest behaviour here.
			_ = reason
			continue
		}
		violations = append(violations, Violation{
			Surface: c.Surface,
			Field:   tag,
			Reason:  "assign it in the projector, or add a reason-carrying honest-omission entry if the absence is intentional",
		})
	}
	return violations, nil
}

// jsonFieldIndex maps a struct type's json tag names to field indices. The
// tag's first comma-separated segment is the name; a "-" tag or an
// unexported field is skipped. An untagged field maps under its Go field
// name so a probe can still reference a field that (incorrectly) lacks a
// tag.
func jsonFieldIndex(t reflect.Type) map[string]int {
	out := make(map[string]int, t.NumField())
	for i := range t.NumField() {
		f := t.Field(i)
		if f.PkgPath != "" { // unexported
			continue
		}
		tag := f.Tag.Get("json")
		name := f.Name
		if tag != "" {
			seg := strings.Split(tag, ",")[0]
			if seg == "-" {
				continue
			}
			if seg != "" {
				name = seg
			}
		}
		out[name] = i
	}
	return out
}
