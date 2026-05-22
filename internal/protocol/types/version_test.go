package types_test

import (
	"encoding/json"
	stderrors "errors"
	"testing"

	"github.com/hurtener/Harbor/internal/protocol/types"
)

// --- Version --------------------------------------------------------------

func TestParseVersion_RoundTrips(t *testing.T) {
	cases := []struct {
		in   string
		want types.Version
	}{
		{"0.1.0", types.Version{Major: 0, Minor: 1, Patch: 0}},
		{"1.0.0", types.Version{Major: 1, Minor: 0, Patch: 0}},
		{"2.13.7", types.Version{Major: 2, Minor: 13, Patch: 7}},
		{"0.0.0", types.Version{Major: 0, Minor: 0, Patch: 0}},
		{"10.20.30", types.Version{Major: 10, Minor: 20, Patch: 30}},
	}
	for _, tc := range cases {
		got, err := types.ParseVersion(tc.in)
		if err != nil {
			t.Errorf("ParseVersion(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseVersion(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
		if got.String() != tc.in {
			t.Errorf("ParseVersion(%q).String() = %q, want %q — String must round-trip", tc.in, got.String(), tc.in)
		}
	}
}

func TestParseVersion_RejectsMalformed(t *testing.T) {
	bad := []string{
		"",          // empty
		"1",         // wrong arity
		"1.2",       // wrong arity
		"1.2.3.4",   // wrong arity
		"1.2.x",     // non-numeric component
		"a.b.c",     // all non-numeric
		"1.-2.3",    // negative component
		"-1.2.3",    // negative major
		" 1.2.3",    // surrounding whitespace
		"1.2.3 ",    // trailing whitespace
		"1..3",      // empty component
		"v1.2.3",    // leading v
		"1.2.3-rc1", // pre-release suffix (not supported)
	}
	for _, s := range bad {
		got, err := types.ParseVersion(s)
		if err == nil {
			t.Errorf("ParseVersion(%q) = %+v, nil — want a wrapped ErrInvalidVersion", s, got)
			continue
		}
		if !stderrors.Is(err, types.ErrInvalidVersion) {
			t.Errorf("ParseVersion(%q) error = %v, want it to wrap ErrInvalidVersion", s, err)
		}
		// Fail-loudly (CLAUDE.md §5): a malformed input must NOT yield a
		// usable zero-Version — the error is the only signal.
		if got != (types.Version{}) {
			t.Errorf("ParseVersion(%q) returned a non-zero Version %+v alongside the error — must be the zero value", s, got)
		}
	}
}

func TestCurrentVersion_MatchesProtocolVersion(t *testing.T) {
	// CurrentVersion is derived from the ProtocolVersion constant at
	// package-init; the two can never drift. This is the pin.
	if types.CurrentVersion.String() != types.ProtocolVersion {
		t.Fatalf("CurrentVersion.String() = %q, ProtocolVersion = %q — the parsed form and the pinned string drifted",
			types.CurrentVersion.String(), types.ProtocolVersion)
	}
	reparsed, err := types.ParseVersion(types.ProtocolVersion)
	if err != nil {
		t.Fatalf("ParseVersion(ProtocolVersion): %v", err)
	}
	if types.CurrentVersion != reparsed {
		t.Fatalf("CurrentVersion = %+v, ParseVersion(ProtocolVersion) = %+v — must be identical",
			types.CurrentVersion, reparsed)
	}
}

func TestVersion_Compare_Ordering(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.1.0", "0.1.0", 0},
		{"0.1.0", "0.2.0", -1},
		{"0.2.0", "0.1.0", 1},
		{"1.0.0", "0.9.9", 1},
		{"0.9.9", "1.0.0", -1},
		{"1.2.3", "1.2.4", -1},
		{"1.2.4", "1.2.3", 1},
		{"1.3.0", "1.2.9", 1},
		{"2.0.0", "10.0.0", -1}, // numeric, not lexical, on the component
	}
	for _, tc := range cases {
		a, err := types.ParseVersion(tc.a)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", tc.a, err)
		}
		b, err := types.ParseVersion(tc.b)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", tc.b, err)
		}
		if got := a.Compare(b); got != tc.want {
			t.Errorf("(%s).Compare(%s) = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		// Compare is antisymmetric.
		if got := b.Compare(a); got != -tc.want {
			t.Errorf("(%s).Compare(%s) = %d, want %d (antisymmetry)", tc.b, tc.a, got, -tc.want)
		}
	}
}

func TestVersion_Compatible_SameMajor(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"0.1.0", "0.1.0", true},
		{"0.1.0", "0.2.0", true}, // same major, different minor
		{"0.1.0", "0.1.9", true}, // same major, different patch
		{"1.0.0", "1.99.99", true},
		{"0.1.0", "1.0.0", false}, // major bump = breaking
		{"1.0.0", "2.0.0", false},
		{"2.5.0", "3.5.0", false},
	}
	for _, tc := range cases {
		a, err := types.ParseVersion(tc.a)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", tc.a, err)
		}
		b, err := types.ParseVersion(tc.b)
		if err != nil {
			t.Fatalf("ParseVersion(%q): %v", tc.b, err)
		}
		if got := a.Compatible(b); got != tc.want {
			t.Errorf("(%s).Compatible(%s) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		// Compatible is symmetric.
		if got := b.Compatible(a); got != tc.want {
			t.Errorf("(%s).Compatible(%s) = %v, want %v (symmetry)", tc.b, tc.a, got, tc.want)
		}
	}
}

// --- Deprecation ----------------------------------------------------------

// validDeprecation is a well-formed Deprecation fixture the malformed
// cases each mutate one field of.
func validDeprecation() types.Deprecation {
	return types.Deprecation{
		Subject:      "legacy_method",
		Kind:         types.DeprecationMethod,
		DeprecatedIn: "0.2.0",
		RemovedIn:    "0.4.0",
		Replacement:  "new_method",
		Note:         "migrate before the 0.4.0 cut",
	}
}

func TestDeprecation_Validate_RejectsMalformed(t *testing.T) {
	if err := validDeprecation().Validate(); err != nil {
		t.Fatalf("the valid fixture failed Validate: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*types.Deprecation)
	}{
		{"empty subject", func(d *types.Deprecation) { d.Subject = "" }},
		{"whitespace subject", func(d *types.Deprecation) { d.Subject = "   " }},
		{"unknown kind", func(d *types.Deprecation) { d.Kind = "not_a_kind" }},
		{"empty kind", func(d *types.Deprecation) { d.Kind = "" }},
		{"malformed deprecated_in", func(d *types.Deprecation) { d.DeprecatedIn = "0.2" }},
		{"malformed removed_in", func(d *types.Deprecation) { d.RemovedIn = "soon" }},
		{"removed_in equals deprecated_in", func(d *types.Deprecation) { d.RemovedIn = d.DeprecatedIn }},
		{"removed_in before deprecated_in", func(d *types.Deprecation) {
			d.DeprecatedIn = "0.4.0"
			d.RemovedIn = "0.3.0"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := validDeprecation()
			tc.mutate(&d)
			err := d.Validate()
			if err == nil {
				t.Fatalf("Validate() = nil, want a wrapped ErrInvalidDeprecation")
			}
			if !stderrors.Is(err, types.ErrInvalidDeprecation) {
				t.Errorf("Validate() error = %v, want it to wrap ErrInvalidDeprecation", err)
			}
		})
	}
}

func TestDeprecation_String_NoteFormat(t *testing.T) {
	full := validDeprecation()
	want := `method "legacy_method" is deprecated in 0.2.0, removed in 0.4.0; use new_method — migrate before the 0.4.0 cut`
	if got := full.String(); got != want {
		t.Errorf("String() with replacement+note =\n  %q\nwant\n  %q", got, want)
	}

	// No replacement, no note — a pure removal.
	bare := types.Deprecation{
		Subject:      "old_capability",
		Kind:         types.DeprecationCapability,
		DeprecatedIn: "1.0.0",
		RemovedIn:    "2.0.0",
	}
	wantBare := `capability "old_capability" is deprecated in 1.0.0, removed in 2.0.0`
	if got := bare.String(); got != wantBare {
		t.Errorf("String() bare =\n  %q\nwant\n  %q", got, wantBare)
	}

	// Replacement but no note.
	replOnly := bare
	replOnly.Replacement = "new_capability"
	wantRepl := `capability "old_capability" is deprecated in 1.0.0, removed in 2.0.0; use new_capability`
	if got := replOnly.String(); got != wantRepl {
		t.Errorf("String() replacement-only =\n  %q\nwant\n  %q", got, wantRepl)
	}
}

func TestDeprecation_JSONRoundTrip(t *testing.T) {
	in := validDeprecation()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.Deprecation
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}

	// omitempty: a bare Deprecation drops Replacement + Note from the
	// wire form.
	bare := types.Deprecation{
		Subject:      "x",
		Kind:         types.DeprecationWireField,
		DeprecatedIn: "0.1.0",
		RemovedIn:    "0.2.0",
	}
	bb, err := json.Marshal(bare)
	if err != nil {
		t.Fatalf("Marshal bare: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(bb, &generic); err != nil {
		t.Fatalf("Unmarshal bare: %v", err)
	}
	for _, k := range []string{"replacement", "note"} {
		if _, present := generic[k]; present {
			t.Errorf("empty %q should be omitted from the wire form", k)
		}
	}
}

func TestDeprecations_RegistryIsValidAndEmpty(t *testing.T) {
	// At Protocol 0.1.0 the registry is empty — nothing has been
	// superseded yet.
	got := types.Deprecations()
	if len(got) != 0 {
		t.Errorf("Deprecations() has %d entries at Protocol %s, want 0 — nothing should be deprecated yet",
			len(got), types.ProtocolVersion)
	}
	// Defensive: whatever ever lands in the registry must Validate, and
	// the slice must be sorted by RemovedIn then Subject.
	for i, d := range got {
		if err := d.Validate(); err != nil {
			t.Errorf("Deprecations()[%d] (%q) does not Validate: %v", i, d.Subject, err)
		}
		if i > 0 {
			prev := got[i-1]
			pv, _ := types.ParseVersion(prev.RemovedIn)
			cv, _ := types.ParseVersion(d.RemovedIn)
			if c := pv.Compare(cv); c > 0 || (c == 0 && prev.Subject > d.Subject) {
				t.Errorf("Deprecations() not sorted: [%d]=%q@%s before [%d]=%q@%s",
					i-1, prev.Subject, prev.RemovedIn, i, d.Subject, d.RemovedIn)
			}
		}
	}

	// The returned slice is a copy — mutating it must not affect the
	// next call. We exercise both mutation shapes: appending past the
	// slice's length (which, if the registry's backing array were
	// shared and had spare capacity, would overwrite registry storage)
	// and overwriting an element in place.
	got = append(got, validDeprecation())
	if len(got) != 1 {
		t.Fatalf("append to Deprecations() result produced len %d, want 1", len(got))
	}
	if len(types.Deprecations()) != 0 {
		t.Error("Deprecations() is non-empty after the caller appended to a prior result — the registry is not copy-protected against append")
	}
	if base := types.Deprecations(); len(base) > 0 {
		base[0] = types.Deprecation{}
		if again := types.Deprecations(); len(again) > 0 && again[0] == (types.Deprecation{}) {
			t.Error("Deprecations() reflects an element overwrite from a prior result — the registry is not copy-protected against in-place mutation")
		}
	}
}

// --- Capability + VersionHandshake ---------------------------------------

func TestCapabilities_DeterministicAndValid(t *testing.T) {
	caps := types.Capabilities()
	if len(caps) == 0 {
		t.Fatal("Capabilities() is empty — at least task_control (Phase 54) must be advertised")
	}

	// Sorted + every entry valid.
	for i, c := range caps {
		if !types.IsValidCapability(c) {
			t.Errorf("Capabilities()[%d] = %q is not IsValidCapability", i, c)
		}
		if i > 0 && caps[i-1] >= c {
			t.Errorf("Capabilities() not strictly sorted: [%d]=%q >= [%d]=%q", i-1, caps[i-1], i, c)
		}
	}

	// task_control is present (the Phase 54 surface).
	var hasTaskControl bool
	for _, c := range caps {
		if c == types.CapTaskControl {
			hasTaskControl = true
		}
	}
	if !hasTaskControl {
		t.Errorf("Capabilities() = %v, missing CapTaskControl — the Phase 54 surface must be advertised", caps)
	}

	// A non-canonical capability is not valid.
	if types.IsValidCapability("not_a_capability") {
		t.Error("IsValidCapability(\"not_a_capability\") = true, want false")
	}

	// The returned slice is a fresh copy — mutating it must not affect
	// the next call.
	caps[0] = "mutated"
	if again := types.Capabilities(); again[0] == "mutated" {
		t.Error("Capabilities() returned a slice sharing backing storage across calls — must be a fresh copy")
	}
}

// TestCapRuntimePosture_Registered pins the Phase 72f (D-111) posture
// capability: CapRuntimePosture is registered, IsValidCapability
// returns true, Capabilities() includes it, and the current handshake
// advertises it so a Protocol client can negotiate the surface.
func TestCapRuntimePosture_Registered(t *testing.T) {
	if string(types.CapRuntimePosture) != "runtime_posture" {
		t.Fatalf("CapRuntimePosture wire string = %q, want %q",
			string(types.CapRuntimePosture), "runtime_posture")
	}
	if !types.IsValidCapability(types.CapRuntimePosture) {
		t.Error("IsValidCapability(CapRuntimePosture) = false, want true")
	}
	var found bool
	for _, c := range types.Capabilities() {
		if c == types.CapRuntimePosture {
			found = true
		}
	}
	if !found {
		t.Errorf("Capabilities() = %v, missing CapRuntimePosture — the Phase 72f surface must be advertised", types.Capabilities())
	}
	if !types.CurrentHandshake().Accepts(types.CapRuntimePosture) {
		t.Error("CurrentHandshake().Accepts(CapRuntimePosture) = false, want true")
	}
}

func TestVersionHandshake_CurrentAndAccepts(t *testing.T) {
	h := types.CurrentHandshake()
	if h.ProtocolVersion != types.ProtocolVersion {
		t.Errorf("CurrentHandshake().ProtocolVersion = %q, want %q", h.ProtocolVersion, types.ProtocolVersion)
	}
	wantCaps := types.Capabilities()
	if len(h.Capabilities) != len(wantCaps) {
		t.Fatalf("CurrentHandshake().Capabilities has %d entries, Capabilities() has %d", len(h.Capabilities), len(wantCaps))
	}
	for i := range wantCaps {
		if h.Capabilities[i] != wantCaps[i] {
			t.Errorf("CurrentHandshake().Capabilities[%d] = %q, want %q", i, h.Capabilities[i], wantCaps[i])
		}
	}

	// Accepts: true for an advertised capability, false otherwise.
	if !h.Accepts(types.CapTaskControl) {
		t.Error("CurrentHandshake().Accepts(CapTaskControl) = false, want true")
	}
	if h.Accepts("not_advertised") {
		t.Error("CurrentHandshake().Accepts(\"not_advertised\") = true, want false")
	}

	// An empty handshake accepts nothing.
	var empty types.VersionHandshake
	if empty.Accepts(types.CapTaskControl) {
		t.Error("(empty VersionHandshake).Accepts(CapTaskControl) = true, want false")
	}
}

func TestVersionHandshake_JSONRoundTrip(t *testing.T) {
	in := types.CurrentHandshake()
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out types.VersionHandshake
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out.ProtocolVersion != in.ProtocolVersion {
		t.Errorf("ProtocolVersion round-trip: got %q want %q", out.ProtocolVersion, in.ProtocolVersion)
	}
	if len(out.Capabilities) != len(in.Capabilities) {
		t.Fatalf("Capabilities round-trip: got %d entries want %d", len(out.Capabilities), len(in.Capabilities))
	}
	for i := range in.Capabilities {
		if out.Capabilities[i] != in.Capabilities[i] {
			t.Errorf("Capabilities[%d] round-trip: got %q want %q", i, out.Capabilities[i], in.Capabilities[i])
		}
	}

	// The version a client reads off the wire parses and is Compatible
	// with CurrentVersion (a client talking to its own Runtime).
	v, err := types.ParseVersion(out.ProtocolVersion)
	if err != nil {
		t.Fatalf("ParseVersion(handshake version): %v", err)
	}
	if !v.Compatible(types.CurrentVersion) {
		t.Errorf("handshake version %s not Compatible with CurrentVersion %s", v, types.CurrentVersion)
	}
}
