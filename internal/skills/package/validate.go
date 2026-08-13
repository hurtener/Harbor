package skillpkg

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// validate.go — the canonical DTO validator. Every field is checked
// at the boundary so a malformed package never reaches a downstream
// consumer (the fail-loud posture: no lenient flag, no silent
// normalization of bad input — Normalize sorts and canonicalizes
// ORDER, it never repairs invalid VALUES).

// Validate checks the closed shape of the logical skill content.
// It is the package analogue of the storage envelope's validator:
// name / trigger / steps are mandatory, and every text field and
// slice is bounded. It does NOT check capability annotations against
// any live tool set — those are metadata, enforced at injection time.
func (s PackageSkill) Validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return fmt.Errorf("%w: Name empty", ErrInvalidSkillContent)
	}
	if CanonicalName(s.Name) == "" {
		return fmt.Errorf("%w: Name has no canonical form", ErrInvalidSkillContent)
	}
	if rl := len([]rune(s.Name)); rl > MaxPackageNameRunes {
		return fmt.Errorf("%w: Name exceeds %d runes (%d)", ErrInvalidSkillContent, MaxPackageNameRunes, rl)
	}
	if strings.TrimSpace(s.Trigger) == "" {
		return fmt.Errorf("%w: Trigger empty (planner match cue is mandatory)", ErrInvalidSkillContent)
	}
	if len(s.Steps) == 0 {
		return fmt.Errorf("%w: Steps empty (skills must declare >= 1 step)", ErrInvalidSkillContent)
	}
	if err := boundedText("title", s.Title); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidSkillContent, err)
	}
	if err := boundedText("description", s.Description); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidSkillContent, err)
	}
	if err := boundedText("trigger", s.Trigger); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidSkillContent, err)
	}
	if err := boundedText("task_type", s.TaskType); err != nil {
		return fmt.Errorf("%w: %w", ErrInvalidSkillContent, err)
	}
	if len(s.Steps) > MaxPackageSteps {
		return fmt.Errorf("%w: Steps exceed %d", ErrInvalidSkillContent, MaxPackageSteps)
	}
	if len(s.Preconditions) > MaxPackageSections {
		return fmt.Errorf("%w: Preconditions exceed %d", ErrInvalidSkillContent, MaxPackageSections)
	}
	if len(s.FailureModes) > MaxPackageSections {
		return fmt.Errorf("%w: FailureModes exceed %d", ErrInvalidSkillContent, MaxPackageSections)
	}
	for i, step := range s.Steps {
		if err := boundedText(fmt.Sprintf("steps[%d]", i), step); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidSkillContent, err)
		}
	}
	for field, list := range map[string][]string{
		"tags": s.Tags, "required_tools": s.RequiredTools,
		"required_ns": s.RequiredNS, "required_tags": s.RequiredTags,
	} {
		limit := MaxPackageAnnotations
		if field == "tags" {
			limit = MaxPackageTags
		}
		if len(list) > limit {
			return fmt.Errorf("%w: %s exceeds %d entries", ErrInvalidSkillContent, field, limit)
		}
		for _, entry := range list {
			if strings.TrimSpace(entry) == "" {
				return fmt.Errorf("%w: %s contains an empty entry", ErrInvalidSkillContent, field)
			}
			if err := boundedText(field+" entry", entry); err != nil {
				return fmt.Errorf("%w: %w", ErrInvalidSkillContent, err)
			}
		}
	}
	for _, item := range append(append([]string(nil), s.Preconditions...), s.FailureModes...) {
		if err := boundedText("text entry", item); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalidSkillContent, err)
		}
	}
	return nil
}

func boundedText(field, text string) error {
	if rl := len([]rune(text)); rl > MaxPackageTextRunes {
		return fmt.Errorf("%s exceeds %d runes (%d)", field, MaxPackageTextRunes, rl)
	}
	return nil
}

// Validate checks the closed shape of ONE support-manifest entry: the
// canonical path, a supported MIME, an exact non-negative size, and a
// 64-hex digest. When the entry carries materialized Data — INCLUDING
// a materialized empty `[]byte{}` (distinguished from a nil
// manifest-only `Data` by `Data != nil`) — the digest and size MUST
// match the bytes exactly (ErrSupportDigestMismatch /
// ErrSupportSizeMismatch) and the MIME must be satisfied by them; a
// manifest that lies about its own content fails loudly rather than
// hashing unverified claims. A nil `Data` is the manifest-only view
// and skips the byte checks.
func (f SupportFile) Validate() error {
	if _, err := canonicalizePath(f.Path); err != nil {
		return wrapSupportPathErr(err)
	}
	if f.Path == RootSkillFileName {
		return errSupportf("Path %q is the root skill document, not a support file", f.Path)
	}
	// URI representability: a support path Package.Validate accepts
	// must always be representable by the exact bounded skillpkg URI
	// constructor (NewURI enforces MaxURIRunes on the whole URI, so a
	// path longer than MaxPackageURIPathRunes would be accepted here
	// but rejected at materialization time — a dual-boundary gap).
	if rl := len([]rune(f.Path)); rl > MaxPackageURIPathRunes {
		return errSupportf("Path %q is %d runes; the %s:// URI carries at most %d path runes", f.Path, rl, URIScheme, MaxPackageURIPathRunes)
	}
	if !mimeSupported(f.Mime) {
		return fmt.Errorf("%w: %q (path %q)", ErrUnsupportedMime, f.Mime, f.Path)
	}
	if f.Size < 0 {
		return errSupportf("Size %d is negative", f.Size)
	}
	if !isHexDigest(f.Digest) {
		return errSupportf("Digest %q is not a 64-char lowercase hex sha256", f.Digest)
	}
	if f.Data != nil {
		if int64(len(f.Data)) != f.Size {
			return fmt.Errorf("%w: %q declares size %d but carries %d bytes", ErrSupportSizeMismatch, f.Path, f.Size, len(f.Data))
		}
		sum := sha256.Sum256(f.Data)
		if hex.EncodeToString(sum[:]) != f.Digest {
			return fmt.Errorf("%w: %q digest does not match its bytes", ErrSupportDigestMismatch, f.Path)
		}
		// MIME is content truth: when the entry carries materialized
		// bytes, the declared MIME must actually be satisfied by them.
		if err := ValidateMimeContent(f.Mime, f.Data); err != nil {
			return fmt.Errorf("%w: %q: %w", ErrMimeContentMismatch, f.Path, err)
		}
	}
	if f.Size > MaxPackageSupportBytes {
		return errSupportf("Size %d exceeds %d bytes", f.Size, MaxPackageSupportBytes)
	}
	return nil
}

func wrapSupportPathErr(err error) error {
	var pe *pathErr
	if errors.As(err, &pe) {
		return fmt.Errorf("%w: %s", ErrInvalidSupport, pe.msg)
	}
	return fmt.Errorf("%w: %v", ErrInvalidSupport, err)
}

func isHexDigest(s string) bool {
	if len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
		case r >= 'a' && r <= 'f':
		default:
			return false
		}
	}
	return true
}

// Validate checks the closed shape of the whole package DTO: the
// name / version bounds, the logical skill content, and the support
// manifest (cardinality, per-entry validity, exact + case-folded path
// uniqueness, and the total byte bound). The manifest is validated as
// an ORDERED collection but the checks are order-insensitive; the
// canonical serializer enforces the ordering for identity purposes.
func (p Package) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return errInvalidf("Name empty")
	}
	if CanonicalName(p.Name) != p.Name {
		return errInvalidf("Name %q is not canonical (lowercase, trimmed)", p.Name)
	}
	if rl := len([]rune(p.Name)); rl > MaxPackageNameRunes {
		return errInvalidf("Name exceeds %d runes (%d)", MaxPackageNameRunes, rl)
	}
	if rl := len([]rune(p.Version)); rl > MaxPackageVersionRunes {
		return errInvalidf("Version exceeds %d runes (%d)", MaxPackageVersionRunes, rl)
	}
	if err := p.Skill.Validate(); err != nil {
		return fmt.Errorf("%w: skill: %w", ErrInvalidPackage, err)
	}
	// Dual-name closure: the package name must be the canonical form
	// of the skill name it carries. A package whose envelope name and
	// skill name disagree would let the same reviewed hash present two
	// different names to different consumers — the name pair is a
	// single identity and must agree under canonicalization.
	if p.Name != CanonicalName(p.Skill.Name) {
		return errInvalidf("Name %q is not the canonical form of the skill name %q", p.Name, p.Skill.Name)
	}
	if len(p.Supports) > MaxPackageSupports {
		return errInvalidf("supports exceed %d entries", MaxPackageSupports)
	}
	exact := make(map[string]struct{}, len(p.Supports))
	folded := make(map[string]struct{}, len(p.Supports))
	var total int64
	for _, f := range p.Supports {
		if err := f.Validate(); err != nil {
			return fmt.Errorf("%w: support %q: %w", ErrInvalidPackage, f.Path, err)
		}
		if _, dup := exact[f.Path]; dup {
			return errInvalidf("duplicate support path %q", f.Path)
		}
		if _, dup := folded[foldPath(f.Path)]; dup {
			return errInvalidf("case-colliding support paths under %q", foldPath(f.Path))
		}
		exact[f.Path] = struct{}{}
		folded[foldPath(f.Path)] = struct{}{}
		total += f.Size
		if total > MaxPackageTotalBytes {
			return errInvalidf("support total exceeds %d bytes", MaxPackageTotalBytes)
		}
	}
	return nil
}

// Normalize returns a defensive canonical copy of the package: the
// support manifest is sorted by canonical path and the unordered
// annotation slices (tags, required_tools, required_ns,
// required_tags) are sorted, so a caller-side ordering change cannot
// perturb the canonical serialization or the package hash. It never
// repairs invalid VALUES — the caller should Validate first (and the
// hash does).
func (p Package) Normalize() Package {
	out := p
	out.Skill = p.Skill.normalized()
	if len(p.Supports) > 0 {
		out.Supports = make([]SupportFile, len(p.Supports))
		copy(out.Supports, p.Supports)
		sort.Slice(out.Supports, func(i, j int) bool {
			return out.Supports[i].Path < out.Supports[j].Path
		})
	}
	return out
}

func (s PackageSkill) normalized() PackageSkill {
	out := s
	out.Tags = sortedStringsCopy(s.Tags)
	out.RequiredTools = sortedStringsCopy(s.RequiredTools)
	out.RequiredNS = sortedStringsCopy(s.RequiredNS)
	out.RequiredTags = sortedStringsCopy(s.RequiredTags)
	return out
}

func sortedStringsCopy(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	sort.Strings(out)
	return out
}
