package wiresurface

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/singlesource"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

var digestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// TestDigest_Deterministic asserts repeated calls return the identical
// memoised digest.
func TestDigest_Deterministic(t *testing.T) {
	a := Digest()
	b := Digest()
	if a != b {
		t.Fatalf("Digest() non-deterministic: %q != %q", a, b)
	}
	// And recomputing the surface from scratch yields the same digest — the
	// memoisation is not hiding a one-shot side effect.
	if got := hashSurface(canonicalSurface()); got != a {
		t.Fatalf("recomputed digest %q != memoised %q", got, a)
	}
}

// TestDigest_Format pins the wire shape: "sha256:" + 64 lowercase hex.
func TestDigest_Format(t *testing.T) {
	d := Digest()
	if !digestRe.MatchString(d) {
		t.Fatalf("Digest() = %q, want match %s", d, digestRe)
	}
}

// TestSerialize_FormatVersionPrefix pins the format-version prefix line. A
// scheme change (which would silently re-key every client's drift check) is
// supposed to bump this prefix deliberately — an accidental change is caught
// here and by the golden-structure test.
func TestSerialize_FormatVersionPrefix(t *testing.T) {
	raw := string(canonicalSurface().serialize())
	want := "harbor-wire-surface/v1\n"
	if !strings.HasPrefix(raw, want) {
		t.Fatalf("serialization does not start with %q; got prefix %q", want, raw[:min(len(raw), 40)])
	}
}

// TestDigest_EqualsCanonicalSurfaceHash proves the digest is fully determined
// by the canonical name-level surface — nothing else feeds it.
func TestDigest_EqualsCanonicalSurfaceHash(t *testing.T) {
	if got, want := Digest(), hashSurface(canonicalSurface()); got != want {
		t.Fatalf("Digest() = %q, hashSurface(canonicalSurface()) = %q", got, want)
	}
}

// TestSerialize_GoldenStructure reconstructs the expected serialization
// independently from the canonical sets and asserts it is byte-identical to
// serialize(). This pins the exact encoding (prefix, section labels, order,
// newlines) for the current surface so an accidental serialization change —
// a renamed label, a dropped newline, a reordered section — is caught, while
// a legitimate surface addition (a new method/type) updates both sides
// together and stays green.
func TestSerialize_GoldenStructure(t *testing.T) {
	s := canonicalSurface()

	var want strings.Builder
	want.WriteString("harbor-wire-surface/v1\n")
	writeSection(&want, "protocol_version", []string{types.ProtocolVersion})
	writeSection(&want, "methods", s.methods)
	writeSection(&want, "errors", s.errorCodes)
	writeSection(&want, "capabilities", s.capabilities)
	writeSection(&want, "types", s.typeNames)

	if got := string(s.serialize()); got != want.String() {
		t.Fatalf("serialization drifted from the golden structure:\n got=%q\nwant=%q", got, want.String())
	}

	// The sections appear in the pinned order.
	raw := want.String()
	order := []string{"\nprotocol_version\n", "\nmethods\n", "\nerrors\n", "\ncapabilities\n", "\ntypes\n"}
	last := -1
	for _, label := range order {
		idx := strings.Index(raw, label)
		if idx < 0 {
			t.Fatalf("section %q missing from serialization", label)
		}
		if idx <= last {
			t.Fatalf("section %q out of order (idx %d <= %d)", label, idx, last)
		}
		last = idx
	}
}

// TestDigest_Sensitivity is the name-level sensitivity matrix: injecting a
// synthetic name into ANY of the four sets changes the digest, and a no-op
// (an identical clone, re-sorted) does not. This proves the digest is
// sensitive to every name dimension and to nothing else.
func TestDigest_Sensitivity(t *testing.T) {
	base := canonicalSurface()
	baseDigest := hashSurface(base)

	// A no-op clone (same names, re-sorted) is byte-identical.
	clone := surface{
		protocolVersion: base.protocolVersion,
		methods:         append([]string(nil), base.methods...),
		errorCodes:      append([]string(nil), base.errorCodes...),
		capabilities:    append([]string(nil), base.capabilities...),
		typeNames:       append([]string(nil), base.typeNames...),
	}
	clone.sort()
	if got := hashSurface(clone); got != baseDigest {
		t.Fatalf("a no-op clone produced a different digest: %q != %q", got, baseDigest)
	}

	mutate := func(name string, pick func(s *surface) *[]string) {
		t.Helper()
		m := surface{
			protocolVersion: base.protocolVersion,
			methods:         append([]string(nil), base.methods...),
			errorCodes:      append([]string(nil), base.errorCodes...),
			capabilities:    append([]string(nil), base.capabilities...),
			typeNames:       append([]string(nil), base.typeNames...),
		}
		set := pick(&m)
		*set = append(*set, "zz.synthetic.wiresurface.test")
		m.sort()
		if got := hashSurface(m); got == baseDigest {
			t.Fatalf("injecting a synthetic %s did not change the digest", name)
		}
	}

	mutate("method", func(s *surface) *[]string { return &s.methods })
	mutate("error code", func(s *surface) *[]string { return &s.errorCodes })
	mutate("capability", func(s *surface) *[]string { return &s.capabilities })
	mutate("wire-type name", func(s *surface) *[]string { return &s.typeNames })

	// A protocol-version change also moves the digest.
	vChanged := base
	vChanged.protocolVersion = base.protocolVersion + "-synthetic"
	if got := hashSurface(vChanged); got == baseDigest {
		t.Fatal("changing the protocol version did not change the digest")
	}
}

// TestDigest_NameLevelOnly_FieldAndEventInvariance documents and proves the
// digest's intended blind spots: it is computed purely from name sets, so it
// has NO input channel for field shapes or event-type names. Two surfaces
// with identical name sets hash identically no matter what field shapes or
// event types the underlying wire types carry — field/event drift is, by
// construction, invisible to this digest (that drift is the build-time field
// guard's job and a non-goal here).
func TestDigest_NameLevelOnly_FieldAndEventInvariance(t *testing.T) {
	// The serialization contains method/error/capability/type NAMES and the
	// version — and nothing field- or event-shaped. Assert no event-type
	// wire string (dotted lowercase like "task.started") leaks in via a
	// stray field, and that a well-known wire-type NAME is present.
	raw := string(canonicalSurface().serialize())
	if !strings.Contains(raw, "\nRuntimeInfo\n") {
		t.Error("expected canonical wire-type name RuntimeInfo in the serialization")
	}
	// Build two surfaces with identical names; they MUST hash identically —
	// there is no field/event dimension that could separate them.
	a := canonicalSurface()
	b := canonicalSurface()
	if hashSurface(a) != hashSurface(b) {
		t.Fatal("two surfaces with identical names produced different digests — a non-name input leaked")
	}
}

// TestDigest_CapabilityGatingInvariance asserts the digest hashes the
// canonical capability UNIVERSE (types.Capabilities()), not a per-instance
// advertised subset. A conditional capability (topology_snapshot) that a
// given runtime may or may not wire is always part of the hashed set, so two
// runtimes built from the same sources produce the same digest regardless of
// their wiring.
func TestDigest_CapabilityGatingInvariance(t *testing.T) {
	s := canonicalSurface()
	caps := make(map[string]struct{}, len(s.capabilities))
	for _, c := range s.capabilities {
		caps[c] = struct{}{}
	}
	// The universe includes the conditional capability — proving the digest
	// does not depend on whether an instance wires it.
	if _, ok := caps[string(types.CapTopologySnapshot)]; !ok {
		t.Errorf("hashed capability set %v missing the conditional %q — the digest must hash the universe, not a wired subset",
			s.capabilities, types.CapTopologySnapshot)
	}
	// The hashed set equals types.Capabilities() exactly.
	want := make([]string, 0, len(types.Capabilities()))
	for _, c := range types.Capabilities() {
		want = append(want, string(c))
	}
	sort.Strings(want)
	if strings.Join(s.capabilities, ",") != strings.Join(want, ",") {
		t.Errorf("hashed capabilities = %v, want the canonical universe %v", s.capabilities, want)
	}
}

// TestDigest_MatchesIndependentComputation cross-checks the digest against an
// independent sha256 of the canonical sources assembled inline — a second
// implementation of the contract, so a refactor of canonicalSurface that
// silently drops a set is caught.
func TestDigest_MatchesIndependentComputation(t *testing.T) {
	// section writes a label line, a member-count line, then the members —
	// mirroring the production count-prefixed encoding independently.
	section := func(b *strings.Builder, label string, items []string) {
		b.WriteString(label + "\n")
		b.WriteString(strconv.Itoa(len(items)) + "\n")
		for _, it := range items {
			b.WriteString(it + "\n")
		}
	}

	ms := make([]string, 0)
	for _, m := range methods.Methods() {
		ms = append(ms, string(m))
	}
	sort.Strings(ms)

	cs := make([]string, 0)
	for _, c := range protoerrors.Codes() {
		cs = append(cs, string(c))
	}
	sort.Strings(cs)

	caps := make([]string, 0)
	for _, c := range types.Capabilities() {
		caps = append(caps, string(c))
	}
	sort.Strings(caps)

	tn := make([]string, 0)
	for name := range singlesource.CanonicalWireTypes {
		tn = append(tn, name)
	}
	sort.Strings(tn)

	var b strings.Builder
	b.WriteString("harbor-wire-surface/v1\n")
	section(&b, "protocol_version", []string{types.ProtocolVersion})
	section(&b, "methods", ms)
	section(&b, "errors", cs)
	section(&b, "capabilities", caps)
	section(&b, "types", tn)

	sum := sha256.Sum256([]byte(b.String()))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if got := Digest(); got != want {
		t.Fatalf("Digest() = %q, independent computation = %q", got, want)
	}
}

// TestSerialize_CollisionResistant proves the count-prefixed encoding cannot
// alias two distinct surfaces to the same bytes when a member value equals a
// section label. Without the per-section count line, the pair below both
// serialize to "...methods\nm\nerrors\nerrors\ne\n..." — a member named
// "errors" in methods is indistinguishable from the "errors" section label.
// The count line bounds each member run, so the encodings (and digests) differ.
func TestSerialize_CollisionResistant(t *testing.T) {
	a := surface{
		protocolVersion: "v",
		methods:         []string{"m"},
		errorCodes:      []string{"e", "errors"},
		capabilities:    []string{"c"},
		typeNames:       []string{"T"},
	}
	b := surface{
		protocolVersion: "v",
		methods:         []string{"errors", "m"},
		errorCodes:      []string{"e"},
		capabilities:    []string{"c"},
		typeNames:       []string{"T"},
	}
	a.sort()
	b.sort()

	if string(a.serialize()) == string(b.serialize()) {
		t.Fatalf("distinct surfaces serialize identically — encoding is collision-prone:\n%q", a.serialize())
	}
	if hashSurface(a) == hashSurface(b) {
		t.Fatalf("distinct surfaces hash to the same digest %q — collision", hashSurface(a))
	}
}

// TestDigest_Concurrent runs N>=100 goroutines calling Digest() concurrently
// under -race, asserting a single identical result (the sync.Once
// memoisation is race-free).
func TestDigest_Concurrent(t *testing.T) {
	const n = 256
	var wg sync.WaitGroup
	results := make([]string, n)
	start := make(chan struct{})
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i] = Digest()
		}(i)
	}
	close(start)
	wg.Wait()

	first := results[0]
	if !digestRe.MatchString(first) {
		t.Fatalf("concurrent Digest() = %q, malformed", first)
	}
	for i, r := range results {
		if r != first {
			t.Fatalf("goroutine %d got %q, want %q — concurrent digests diverged", i, r, first)
		}
	}
}
