// Package types is the single source of truth for Harbor Protocol wire
// types (CLAUDE.md §8). Every Protocol message struct lives here; other
// packages import these types, and nothing else defines a Protocol
// message struct. The Phase 58 lint formalises this — Phase 54 lays the
// foundation so that lint is a no-op formalisation, not a cleanup.
//
// # What Phase 54 ships
//
// Phase 54 ships the Protocol task control surface's wire types: the
// flat IdentityScope every request carries, the StartRequest /
// StartResponse pair, and the ControlRequest / ControlResponse pair the
// nine steering-control methods share. The wire shapes are deliberately
// flat (string identity fields + a payload map) — a Protocol type that
// re-exported an internal runtime Go struct would be the RFC §5.1
// reject-on-sight smell ("a Protocol method that maps 1:1 to an internal
// Go function signature"). The runtime-facing translation lives in the
// protocol package's ControlSurface, not in these types.
//
// # Versioning
//
// ProtocolVersion is pinned here. Bumping it is an RFC change (RFC §5.3,
// CLAUDE.md §8) — the Protocol surface is versioned independently of the
// Runtime implementation so third-party Consoles are not whipsawed by a
// Runtime refactor.
//
// # Versioning discipline (Phase 59)
//
// Phase 59 turns the version *pin* into a versioning *discipline*:
//
//   - Version + ParseVersion + Compatible — the pinned string parsed
//     into a comparable Major/Minor/Patch value, so a client detects
//     version skew mechanically (same-major ⇒ Compatible) instead of
//     string-comparing. CurrentVersion is the parsed form of
//     ProtocolVersion; the two cannot drift.
//   - Deprecation + Deprecations() — the settled structured note format
//     for a breaking Protocol change's removal window (RFC §5.3:
//     "Breaking changes require a deprecation window"), plus the
//     single-home registry of active deprecations. The registry is
//     empty at 0.1.0 — a 0.1.0 Protocol that just shipped has nothing
//     to deprecate — but the format and its home exist so the first
//     real deprecation lands structured, not as a free-text comment.
//   - Capability + Capabilities() + VersionHandshake — the
//     capability-negotiation shape. A Protocol client asks the Runtime
//     which surfaces are live and gets a structured VersionHandshake
//     (the version + the advertised capability set) rather than
//     discovering a missing surface by a 404. V1 advertises exactly
//     the surfaces that have shipped — CapTaskControl, the Phase 54
//     surface; later Protocol-surface phases add their constant here.
//
// None of this bumps the version: Phase 59 ships the *mechanism* for
// living with versions, not a new version. It is transport-agnostic —
// a Phase 60 SSE+REST adapter or a Phase 63 `harbor version` subcommand
// consumes these values; Phase 59 binds to no transport.
package types

import (
	stderrors "errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ProtocolVersion is the pinned Harbor Protocol version. Bumping this
// constant is an RFC change (RFC §5.3): the Protocol surface is versioned
// independently of the Runtime, and a breaking change requires a
// deprecation window so third-party Consoles are not whipsawed.
//
// V1 ships 0.1.0 — the task control surface (Phase 54) is the first
// Protocol surface to land; the streaming-events / state-snapshot /
// topology / artifacts / traces / metrics surfaces (RFC §5.2) extend it
// in later phases without bumping the major while V1 is in flight.
const ProtocolVersion = "0.1.0"

// ErrInvalidVersion is returned (wrapped) by ParseVersion when the input
// is not a well-formed `MAJOR.MINOR.PATCH` triple of non-negative
// integers. Parsing fails loudly — there is no silent zero-Version
// degradation path (CLAUDE.md §5 "fail loudly").
var ErrInvalidVersion = stderrors.New("types: invalid Protocol version")

// Version is the parsed, comparable form of a Harbor Protocol version: a
// `MAJOR.MINOR.PATCH` triple of non-negative integers. The string
// ProtocolVersion is the pinned source of truth and the RFC-change
// trip-wire; Version is what a client uses to *reason* about a version —
// to detect skew (Compatible) or order two versions (Compare).
//
// Version is an immutable value type — safe to copy, compare, and share
// across goroutines without synchronisation.
type Version struct {
	// Major is bumped on a breaking Protocol change. Two versions are
	// Compatible iff their Major components are equal.
	Major int
	// Minor is bumped on a backward-compatible surface addition (a new
	// method, a new capability, a new optional wire field).
	Minor int
	// Patch is bumped on a backward-compatible fix with no surface
	// change.
	Patch int
}

// ParseVersion parses a `MAJOR.MINOR.PATCH` string into a Version. Each
// component must be a non-negative base-10 integer; anything else
// (empty input, wrong arity, a non-numeric or negative component,
// surrounding whitespace) fails loudly with a wrapped ErrInvalidVersion.
func ParseVersion(s string) (Version, error) {
	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return Version{}, fmt.Errorf("%w: %q is not MAJOR.MINOR.PATCH", ErrInvalidVersion, s)
	}
	out := Version{}
	for i, part := range parts {
		n, err := strconv.Atoi(part)
		if err != nil || n < 0 {
			return Version{}, fmt.Errorf("%w: component %d of %q is not a non-negative integer", ErrInvalidVersion, i+1, s)
		}
		switch i {
		case 0:
			out.Major = n
		case 1:
			out.Minor = n
		case 2:
			out.Patch = n
		}
	}
	return out, nil
}

// mustParseVersion parses s or panics. It is used exactly once, at
// package scope, to derive CurrentVersion from the ProtocolVersion
// constant — a malformed ProtocolVersion is a build-time bug ("this is
// impossible by construction" per CLAUDE.md §5), and panicking at
// package-init surfaces it the instant the constant is edited wrong.
func mustParseVersion(s string) Version {
	v, err := ParseVersion(s)
	if err != nil {
		panic(fmt.Sprintf("types: ProtocolVersion constant %q is malformed: %v", s, err))
	}
	return v
}

// CurrentVersion is the parsed form of the ProtocolVersion constant. It
// is derived from ProtocolVersion at package-init, so the two can never
// drift — TestCurrentVersion_MatchesProtocolVersion pins
// CurrentVersion.String() == ProtocolVersion. Callers that need to
// *reason* about the version (skew detection, ordering) use
// CurrentVersion; the ProtocolVersion string stays the RFC-change
// trip-wire.
var CurrentVersion = mustParseVersion(ProtocolVersion)

// String renders the Version back to its canonical
// `MAJOR.MINOR.PATCH` form. String(ParseVersion(s)) == s for every
// well-formed s.
func (v Version) String() string {
	return fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
}

// Compare orders two versions component-wise (Major, then Minor, then
// Patch). It returns -1 if v sorts before o, +1 if after, 0 if equal.
func (v Version) Compare(o Version) int {
	switch {
	case v.Major != o.Major:
		return cmpInt(v.Major, o.Major)
	case v.Minor != o.Minor:
		return cmpInt(v.Minor, o.Minor)
	case v.Patch != o.Patch:
		return cmpInt(v.Patch, o.Patch)
	default:
		return 0
	}
}

// cmpInt returns -1 / 0 / +1 for a < b / a == b / a > b.
func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}

// Compatible reports whether a client speaking version o can talk to a
// Runtime speaking version v. The rule is same-Major: a Major bump is a
// breaking change (which is why bumping it is an RFC change, RFC §5.3),
// so a client and a Runtime are compatible iff their Major components
// match — Minor / Patch differences are backward-compatible by
// construction. Compatible is symmetric: v.Compatible(o) == o.Compatible(v).
func (v Version) Compatible(o Version) bool {
	return v.Major == o.Major
}

// DeprecationKind classifies which Protocol element a Deprecation
// applies to. It is a fixed string enum — the four kinds of element the
// versioned Protocol surface exposes (RFC §5.2, §5.3) — not a
// registration seam.
type DeprecationKind string

// The four kinds of Protocol element a Deprecation can apply to.
const (
	// DeprecationMethod — a Protocol method name (internal/protocol/
	// methods) is being retired.
	DeprecationMethod DeprecationKind = "method"
	// DeprecationErrorCode — a Protocol error code (internal/protocol/
	// errors) is being retired.
	DeprecationErrorCode DeprecationKind = "error_code"
	// DeprecationWireField — a field on a Protocol wire type
	// (internal/protocol/types) is being retired.
	DeprecationWireField DeprecationKind = "wire_field"
	// DeprecationCapability — a Protocol Capability is being retired.
	DeprecationCapability DeprecationKind = "capability"
)

// validDeprecationKinds is the closed set DeprecationKind.Validate
// checks against.
var validDeprecationKinds = map[DeprecationKind]struct{}{
	DeprecationMethod:     {},
	DeprecationErrorCode:  {},
	DeprecationWireField:  {},
	DeprecationCapability: {},
}

// Deprecation is the settled structured note format for a breaking
// Protocol change's removal window. RFC §5.3: "Breaking changes require
// a deprecation window so third-party Consoles aren't whipsawed." A
// deprecated Protocol element carries its window in this shape — a
// structured, machine-readable record — rather than a free-text code
// comment, so a Protocol client (or a `harbor version` subcommand,
// Phase 63) can surface "this method is going away in 0.3.0, use X
// instead" mechanically.
//
// Deprecation is a wire type — it round-trips through JSON so the
// negotiation surface (Phase 60) can hand the active set to a client.
type Deprecation struct {
	// Subject names the Protocol element being deprecated — the method
	// name, error code, `Type.Field` wire-field path, or capability
	// string, depending on Kind.
	Subject string `json:"subject"`
	// Kind classifies what Subject is — one of the DeprecationKind
	// constants.
	Kind DeprecationKind `json:"kind"`
	// DeprecatedIn is the Protocol version (a MAJOR.MINOR.PATCH string)
	// in which Subject was first marked deprecated.
	DeprecatedIn string `json:"deprecated_in"`
	// RemovedIn is the Protocol version in which Subject is removed. It
	// MUST sort strictly after DeprecatedIn — the span between the two
	// is the deprecation window.
	RemovedIn string `json:"removed_in"`
	// Replacement names the Protocol element callers should migrate to.
	// Optional — a deprecation with no replacement is a pure removal.
	Replacement string `json:"replacement,omitempty"`
	// Note is an optional human-readable migration hint.
	Note string `json:"note,omitempty"`
}

// Validate reports whether the Deprecation is well-formed: a non-empty
// Subject, a recognised Kind, parseable DeprecatedIn / RemovedIn
// versions, and a RemovedIn that sorts strictly after DeprecatedIn (an
// empty or inverted window is a malformed deprecation). It fails loudly
// — a malformed Deprecation in the registry is a bug, not a
// silently-tolerated entry.
func (d Deprecation) Validate() error {
	if strings.TrimSpace(d.Subject) == "" {
		return fmt.Errorf("%w: deprecation has an empty Subject", ErrInvalidDeprecation)
	}
	if _, ok := validDeprecationKinds[d.Kind]; !ok {
		return fmt.Errorf("%w: deprecation %q has an unknown Kind %q", ErrInvalidDeprecation, d.Subject, d.Kind)
	}
	from, err := ParseVersion(d.DeprecatedIn)
	if err != nil {
		return fmt.Errorf("%w: deprecation %q has a malformed DeprecatedIn: %w", ErrInvalidDeprecation, d.Subject, err)
	}
	to, err := ParseVersion(d.RemovedIn)
	if err != nil {
		return fmt.Errorf("%w: deprecation %q has a malformed RemovedIn: %w", ErrInvalidDeprecation, d.Subject, err)
	}
	if to.Compare(from) <= 0 {
		return fmt.Errorf("%w: deprecation %q has RemovedIn %s not strictly after DeprecatedIn %s",
			ErrInvalidDeprecation, d.Subject, d.RemovedIn, d.DeprecatedIn)
	}
	return nil
}

// String renders the settled human-readable deprecation note format:
//
//	<kind> "<subject>" is deprecated in <deprecated_in>, removed in <removed_in>[; use <replacement>][ — <note>]
//
// This is the canonical phrasing a Protocol client / CLI surfaces to a
// human operator.
func (d Deprecation) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %q is deprecated in %s, removed in %s", d.Kind, d.Subject, d.DeprecatedIn, d.RemovedIn)
	if d.Replacement != "" {
		fmt.Fprintf(&b, "; use %s", d.Replacement)
	}
	if d.Note != "" {
		fmt.Fprintf(&b, " — %s", d.Note)
	}
	return b.String()
}

// ErrInvalidDeprecation is returned (wrapped) by Deprecation.Validate
// when a Deprecation is malformed.
var ErrInvalidDeprecation = stderrors.New("types: invalid Protocol deprecation")

// activeDeprecations is the registry of active Protocol deprecations. It
// is EMPTY at Protocol 0.1.0 — the task control surface (Phase 54) just
// shipped and nothing has been superseded yet. The first real
// deprecation lands here, in the phase that supersedes a Protocol
// element, populating the format Phase 59 settled. Keeping the registry
// here — even empty — gives the Deprecation format a single home and a
// consumer (Deprecations()) from day one, per the §13
// primitive-with-consumer rule.
var activeDeprecations = []Deprecation{}

// Deprecations returns a copy of the active Protocol deprecation
// registry, sorted deterministically by RemovedIn version then Subject.
// The slice is a copy — a caller cannot mutate the registry through the
// return value. At Protocol 0.1.0 the registry is empty.
func Deprecations() []Deprecation {
	out := make([]Deprecation, len(activeDeprecations))
	copy(out, activeDeprecations)
	sort.Slice(out, func(i, j int) bool {
		// Both RemovedIn values are well-formed (Validate gates that);
		// fall back to string order if a future malformed entry slips
		// past, so the sort is still total.
		vi, ei := ParseVersion(out[i].RemovedIn)
		vj, ej := ParseVersion(out[j].RemovedIn)
		if ei == nil && ej == nil && vi.Compare(vj) != 0 {
			return vi.Compare(vj) < 0
		}
		if out[i].RemovedIn != out[j].RemovedIn {
			return out[i].RemovedIn < out[j].RemovedIn
		}
		return out[i].Subject < out[j].Subject
	})
	return out
}

// Capability is a Protocol surface a Runtime can advertise. The Protocol
// exposes several surfaces (RFC §5.2 — task control, streaming events,
// state snapshots, topology, artifacts, traces/metrics); a Capability is
// how a Runtime tells a client *which* of them are live, so a client
// negotiates rather than discovering a missing surface by a 404.
//
// Capability is a fixed string enum, not a registration seam: a new
// Protocol surface adds its Capability constant in the phase that ships
// the surface (and extends canonicalCapabilities), exactly as a new
// Protocol method extends methods.canonicalMethods.
type Capability string

// The V1 Protocol capability set. At Protocol 0.1.0 exactly one surface
// has shipped — the Phase 54 task control surface — so CapTaskControl
// is the only capability. RFC §5.2's other surfaces add their
// Capability constant here as their phase lands.
const (
	// CapTaskControl — the task control surface (RFC §5.2 "Task
	// control" row): the `start` method plus the nine steering-control
	// methods. Shipped in Phase 54.
	CapTaskControl Capability = "task_control"
	// CapEventsSubscribe — the streaming-events surface (RFC §5.2
	// "Streaming events" row): the `events.subscribe` method and the
	// `events.aggregate` time-bucket method. Shipped in Wave 13
	// (Phase 72 / 72a).
	CapEventsSubscribe Capability = "events_subscribe"
	// CapRuntimePosture — the runtime-posture surface (RFC §5.3, §6.15,
	// §7): the five read-only `runtime.*` / `metrics.*` methods
	// (`runtime.info`, `runtime.health`, `runtime.counters`,
	// `runtime.drivers`, `metrics.snapshot`). Shipped in Wave 13
	// (Phase 72f / D-111). A Protocol client negotiates "does this
	// Runtime advertise the posture surface?" via
	// `VersionHandshake.Accepts(CapRuntimePosture)`. The addition is
	// backward-compatible (RFC §5.3 minor-class change) — no version
	// bump.
	CapRuntimePosture Capability = "runtime_posture"
	// CapTopologySnapshot — the engine-graph topology projection
	// (`topology.snapshot`, Phase 74 / D-114). Conditional: a runtime
	// only advertises this capability when it hosts an engine (the
	// ControlSurface's topology accessor is non-nil). Planner /
	// RunLoop-shaped runtimes — including `harbor dev` against an
	// agent yaml — do NOT advertise it; the Console reads
	// `runtime.info.capabilities` at attach and gates its
	// `topology.snapshot` calls behind `caps.has('topology_snapshot')`
	// so the browser console stays clean on runtimes without the
	// surface (round-8 F1 / phase 84a). Backward-compatible (RFC §5.3
	// minor-class addition) — no version bump.
	CapTopologySnapshot Capability = "topology_snapshot"
)

// canonicalCapabilities is the registered set — the universe of
// capability values a runtime MAY advertise. A new Protocol surface
// extends this map in its own phase; there is no registration escape
// hatch.
//
// Phase 84a clarification: this is distinct from what a SPECIFIC
// runtime instance advertises in `runtime.info.capabilities`. The
// canonical set is the Protocol-version surface a handshake negotiates
// against; the per-instance list is the *wired subset* (e.g.
// `topology_snapshot` is in the canonical set, but only runtimes
// hosting an engine surface it on `runtime.info`).
var canonicalCapabilities = map[Capability]struct{}{
	CapTaskControl:      {},
	CapEventsSubscribe:  {},
	CapRuntimePosture:   {},
	CapTopologySnapshot: {},
}

// IsValidCapability reports whether c is one of the canonical Protocol
// capabilities. O(1).
func IsValidCapability(c Capability) bool {
	_, ok := canonicalCapabilities[c]
	return ok
}

// Capabilities returns the deterministic, lexicographically-sorted set
// of Protocol capabilities the Runtime advertises. At Protocol 0.1.0
// this is exactly {CapTaskControl}.
func Capabilities() []Capability {
	out := make([]Capability, 0, len(canonicalCapabilities))
	for c := range canonicalCapabilities {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// VersionHandshake is the capability-negotiation wire shape: the
// Protocol version the Runtime speaks plus the set of surfaces it
// advertises. A Protocol client compares the handshake's version
// against its own (Version.Compatible) to detect skew, and checks
// Accepts(cap) before exercising a surface — so a third-party Console
// built against an older Protocol negotiates explicitly instead of
// discovering a missing surface by a 404.
//
// VersionHandshake is a wire type — it round-trips through JSON; a
// Phase 60 transport adapter serves CurrentHandshake() at the
// negotiation entry point.
type VersionHandshake struct {
	// ProtocolVersion is the version string the Runtime speaks — the
	// ProtocolVersion constant. A client parses it (ParseVersion) and
	// checks Compatible against its own.
	ProtocolVersion string `json:"protocol_version"`
	// Capabilities is the set of Protocol surfaces the Runtime
	// advertises as live — deterministically sorted (Capabilities()).
	Capabilities []Capability `json:"capabilities"`
}

// CurrentHandshake builds the Runtime's VersionHandshake from the pinned
// ProtocolVersion and the advertised Capabilities() set. It is the value
// a negotiation entry point (Phase 60) returns to a connecting client.
func CurrentHandshake() VersionHandshake {
	return VersionHandshake{
		ProtocolVersion: ProtocolVersion,
		Capabilities:    Capabilities(),
	}
}

// Accepts reports whether the handshake advertises capability c — i.e.
// whether the Runtime that produced this handshake has the
// corresponding Protocol surface live.
func (h VersionHandshake) Accepts(c Capability) bool {
	for _, have := range h.Capabilities {
		if have == c {
			return true
		}
	}
	return false
}
