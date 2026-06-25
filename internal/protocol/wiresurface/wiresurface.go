// Package wiresurface computes the canonical Harbor Protocol wire-surface
// digest: a coarse, opaque, stable name-level fingerprint of the surface a
// Protocol client binds to.
//
// # What the digest covers
//
// The digest is a sha256 over a deterministic, format-versioned encoding of
// the Protocol's NAME-LEVEL surface:
//
//   - the pinned Protocol version,
//   - every canonical method name,
//   - every canonical error code,
//   - every canonical capability string (the canonical capability UNIVERSE,
//     not a per-instance advertised subset),
//   - every canonical wire-type name.
//
// It deliberately covers the SHAPE OF NAMES, not field shapes and not
// event-type names. A field-type swap on a same-named wire struct does NOT
// move the digest — field-level drift stays a build-time concern (the
// Console's manifest field guard catches it for any client that vendors the
// manifest), and exposing field shapes over the wire is a non-goal. Event
// types are excluded because enumerating them at runtime would require
// seating driver registries; the digest is scoped to the
// request/response/capability contract.
//
// # Why a name-level digest
//
// The runtime returns the digest on its attach-time posture call, and the
// committed wire manifest is stamped with the same digest, so a connected
// Protocol client compares the digest it vendored at build time against the
// live runtime's reported digest and surfaces a loud drift signal at
// connect-time instead of discovering wire drift field-by-field at runtime.
//
// # Purity and concurrency
//
// Digest is a pure function of the build's canonical Go sources, memoised
// once per process and safe for concurrent use. The package imports only the
// light canonical Protocol packages (methods, errors, types, singlesource) —
// no driver registries, no orchestration packages — so it can be shared by
// the runtime and the manifest build tool without an import cycle or a
// dependency-set balloon.
package wiresurface

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/singlesource"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// formatPrefix versions the serialization scheme. Bumping it (a scheme
// change) intentionally changes every digest; the value is pinned by a
// golden test so an accidental serialization change is caught.
const formatPrefix = "harbor-wire-surface/v1"

// digestOnce memoises the digest once per process. sync.OnceValue is used
// (rather than a bare sync.Once + package var) deliberately: if the canonical
// surface ever fails its build invariant and panics, OnceValue re-panics with
// the same value on every subsequent call — so a violation stays fail-loud
// instead of latching a silent empty digest.
var digestOnce = sync.OnceValue(func() string {
	return hashSurface(canonicalSurface())
})

// Digest returns the canonical Harbor Protocol wire-surface digest:
//
//	"sha256:" + lowercase-hex(sha256(canonical-serialization))
//
// over a deterministic, format-versioned encoding of the Protocol version,
// method names, error codes, capability strings (the canonical capability
// universe), and canonical wire-type names. It is a coarse name-level
// fingerprint — it covers the shape of names, not field shapes and not
// event-type names. It is a pure function of the build's canonical Go
// sources, memoised once per process; safe for concurrent use.
func Digest() string {
	return digestOnce()
}

// surface is the name-level input set the digest hashes. Every field is a
// sorted, deduplicated set of names; serialization is a pure function of it.
type surface struct {
	protocolVersion string
	methods         []string
	errorCodes      []string
	capabilities    []string
	typeNames       []string
}

// canonicalSurface reads the name-level surface from the canonical Go
// sources. It fails loudly (a build-by-construction impossibility) if any
// canonical set is empty — an empty surface would silently produce a digest
// of nothing, which is never a tolerable degradation.
func canonicalSurface() surface {
	ms := methods.Methods()
	methodNames := make([]string, len(ms))
	for i, m := range ms {
		methodNames[i] = string(m)
	}

	cs := protoerrors.Codes()
	codeNames := make([]string, len(cs))
	for i, c := range cs {
		codeNames[i] = string(c)
	}

	caps := types.Capabilities()
	capNames := make([]string, len(caps))
	for i, c := range caps {
		capNames[i] = string(c)
	}

	typeNames := make([]string, 0, len(singlesource.CanonicalWireTypes))
	for name := range singlesource.CanonicalWireTypes {
		typeNames = append(typeNames, name)
	}

	s := surface{
		protocolVersion: types.ProtocolVersion,
		methods:         methodNames,
		errorCodes:      codeNames,
		capabilities:    capNames,
		typeNames:       typeNames,
	}
	s.sort()
	s.mustBeComplete()
	return s
}

// sort lexicographically orders every set so the serialization is invariant
// to source iteration order (map ranges, registry insertion order).
func (s *surface) sort() {
	sort.Strings(s.methods)
	sort.Strings(s.errorCodes)
	sort.Strings(s.capabilities)
	sort.Strings(s.typeNames)
}

// mustBeComplete asserts the canonical surface is non-empty. The canonical
// method / error / capability / wire-type sets are compile-time constants;
// an empty one is impossible by construction, so a violation is a build
// invariant breach, not a runtime condition — fail loud rather than emit a
// digest over a hollow surface.
func (s surface) mustBeComplete() {
	switch {
	case s.protocolVersion == "":
		panic("wiresurface: empty ProtocolVersion — canonical surface invariant violated")
	case len(s.methods) == 0:
		panic("wiresurface: no canonical methods — canonical surface invariant violated")
	case len(s.errorCodes) == 0:
		panic("wiresurface: no canonical error codes — canonical surface invariant violated")
	case len(s.capabilities) == 0:
		panic("wiresurface: no canonical capabilities — canonical surface invariant violated")
	case len(s.typeNames) == 0:
		panic("wiresurface: no canonical wire types — canonical surface invariant violated")
	}
}

// serialize renders the surface to the canonical byte encoding: a
// format-version prefix line, then one labelled section per set — each label
// on its own line, then a member-count line, then the sorted members
// one-per-line. The count line makes the encoding collision-resistant by
// construction: a parser is bounded by the count, so a member value equal to
// a section label can never alias the label and let two distinct surfaces
// serialize to identical bytes. No map iteration, no struct formatting — the
// bytes are stable across Go versions and architectures.
func (s surface) serialize() []byte {
	var b strings.Builder
	b.WriteString(formatPrefix)
	b.WriteByte('\n')
	writeSection(&b, "protocol_version", []string{s.protocolVersion})
	writeSection(&b, "methods", s.methods)
	writeSection(&b, "errors", s.errorCodes)
	writeSection(&b, "capabilities", s.capabilities)
	writeSection(&b, "types", s.typeNames)
	return []byte(b.String())
}

// writeSection writes a labelled, count-prefixed, newline-delimited section.
// The count line bounds the member run so the encoding cannot be made
// ambiguous by a member that happens to equal a section label.
func writeSection(b *strings.Builder, label string, items []string) {
	b.WriteString(label)
	b.WriteByte('\n')
	b.WriteString(strconv.Itoa(len(items)))
	b.WriteByte('\n')
	for _, it := range items {
		b.WriteString(it)
		b.WriteByte('\n')
	}
}

// hashSurface computes the "sha256:"-prefixed lowercase-hex digest of a
// surface's canonical serialization.
func hashSurface(s surface) string {
	sum := sha256.Sum256(s.serialize())
	return "sha256:" + hex.EncodeToString(sum[:])
}

// String renders the surface's serialization for debugging. It is not part
// of the digest contract; callers compare digests, never serializations.
func (s surface) String() string {
	return fmt.Sprintf("wiresurface{methods:%d errors:%d caps:%d types:%d}",
		len(s.methods), len(s.errorCodes), len(s.capabilities), len(s.typeNames))
}
