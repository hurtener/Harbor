package skillpkg

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// serialize.go — the canonical deterministic serializer. The
// canonical serialization is the single identity-bearing text form of
// a package: it feeds PackageHash and round-trips through
// FromCanonicalBytes. It is deterministic by construction:
//
//   - fixed field order (struct marshaling order);
//   - unordered annotation slices (tags, required_tools, required_ns,
//     required_tags) are sorted;
//   - the support manifest is ordered by canonical path;
//   - `Data` bytes NEVER participate (the manifest's digest + size
//     do);
//   - empty slices serialize as absent (`omitempty`) so a nil vs
//     empty distinction cannot perturb identity.
//
// The serializer validates the package before emitting (fail loud:
// a structurally invalid package has no canonical form).

// canonicalWire is the private JSON shape. It exists so the canonical
// serialization is stable against DTO field additions: the shape is
// pinned here, and PackageHash's envelope version (`v1:`) is what
// changes when the shape must.
type canonicalWire struct {
	Name     string          `json:"name"`
	Version  string          `json:"version,omitempty"`
	Skill    canonicalSkill  `json:"skill"`
	Supports []canonicalFile `json:"supports,omitempty"`
}

type canonicalSkill struct {
	Name          string   `json:"name"`
	Title         string   `json:"title,omitempty"`
	Description   string   `json:"description,omitempty"`
	Trigger       string   `json:"trigger"`
	TaskType      string   `json:"task_type,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	Steps         []string `json:"steps"`
	Preconditions []string `json:"preconditions,omitempty"`
	FailureModes  []string `json:"failure_modes,omitempty"`
	RequiredTools []string `json:"required_tools,omitempty"`
	RequiredNS    []string `json:"required_ns,omitempty"`
	RequiredTags  []string `json:"required_tags,omitempty"`
}

type canonicalFile struct {
	Path   string `json:"path"`
	Mime   string `json:"mime"`
	Size   int64  `json:"size"`
	Digest string `json:"digest"`
}

// CanonicalBytes returns the canonical identity-bearing serialization
// of p. The input is validated and normalized first; the returned
// bytes are deterministic for a given logical package regardless of
// caller-side slice ordering.
func CanonicalBytes(p Package) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	p = p.Normalize()
	wire := canonicalWire{
		Name:    p.Name,
		Version: p.Version,
		Skill: canonicalSkill{
			Name:          p.Skill.Name,
			Title:         p.Skill.Title,
			Description:   p.Skill.Description,
			Trigger:       p.Skill.Trigger,
			TaskType:      p.Skill.TaskType,
			Tags:          p.Skill.Tags,
			Steps:         p.Skill.Steps,
			Preconditions: p.Skill.Preconditions,
			FailureModes:  p.Skill.FailureModes,
			RequiredTools: p.Skill.RequiredTools,
			RequiredNS:    p.Skill.RequiredNS,
			RequiredTags:  p.Skill.RequiredTags,
		},
	}
	if len(p.Supports) > 0 {
		wire.Supports = make([]canonicalFile, 0, len(p.Supports))
		for _, f := range p.Supports {
			wire.Supports = append(wire.Supports, canonicalFile{
				Path:   f.Path,
				Mime:   f.Mime,
				Size:   f.Size,
				Digest: f.Digest,
			})
		}
	}
	b, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("%w: marshal canonical form: %v", ErrInvalidPackage, err)
	}
	return b, nil
}

// FromCanonicalBytes reconstructs a Package from the canonical
// serialization produced by CanonicalBytes. The decoder is bounded,
// closed, and EXACT — it never accepts an alternate byte form of a
// package:
//
//   - the input is capped at MaxCanonicalBytes BEFORE decoding, so an
//     oversized or pathological document cannot trigger unbounded
//     allocation (the mandatory pre-validation bound);
//   - unknown fields are rejected at every object level (the
//     canonical wire is closed — an authority-looking field such as
//     `scope` / `origin` / `tenant` has no canonical slot);
//   - duplicate object keys are rejected at every object level,
//     including nested `skill` and `supports` entries;
//   - trailing non-whitespace values are rejected;
//   - the accepted bytes must be EXACTLY the canonical serialization:
//     the decoded package is validated, normalized, and re-encoded,
//     and the re-encoded form must byte-match the input, so every
//     non-canonical alternate form (key reordering, whitespace
//     differences, unsorted tags/supports, redundant escapes, `null`
//     where an empty list is canonically omitted, non-canonical
//     number/array shapes) is rejected.
//
// The parsed DTO is validated before it is returned. The manifest
// `Data` bytes are not carried by the canonical form, so the
// reconstructed package is a manifest-only view.
func FromCanonicalBytes(b []byte) (Package, error) {
	if len(b) > MaxCanonicalBytes {
		return Package{}, fmt.Errorf("%w: %d bytes exceeds %d", ErrInvalidPackage, len(b), MaxCanonicalBytes)
	}
	var wire canonicalWire
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&wire); err != nil {
		return Package{}, fmt.Errorf("%w: parse canonical form: %v", ErrInvalidPackage, err)
	}
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return Package{}, fmt.Errorf("%w: trailing value after the canonical form", ErrInvalidPackage)
		}
		return Package{}, fmt.Errorf("%w: trailing content after the canonical form: %v", ErrInvalidPackage, err)
	}
	p := packageFromWire(wire)
	if err := p.Validate(); err != nil {
		return Package{}, err
	}
	// Exactness gate: the accepted bytes are the canonical form only
	// if re-encoding the (validated, normalized) decoded package
	// reproduces them byte-for-byte.
	cb, err := CanonicalBytes(p)
	if err != nil {
		return Package{}, err
	}
	if !bytes.Equal(b, cb) {
		return Package{}, fmt.Errorf("%w: input is not the exact canonical serialization (non-canonical bytes)", ErrInvalidPackage)
	}
	return p, nil
}

// packageFromWire projects the strict-decoded wire shape onto the
// package DTO. `Data` is never carried by the canonical form.
func packageFromWire(wire canonicalWire) Package {
	p := Package{
		Name:    wire.Name,
		Version: wire.Version,
		Skill: PackageSkill{
			Name:          wire.Skill.Name,
			Title:         wire.Skill.Title,
			Description:   wire.Skill.Description,
			Trigger:       wire.Skill.Trigger,
			TaskType:      wire.Skill.TaskType,
			Tags:          wire.Skill.Tags,
			Steps:         wire.Skill.Steps,
			Preconditions: wire.Skill.Preconditions,
			FailureModes:  wire.Skill.FailureModes,
			RequiredTools: wire.Skill.RequiredTools,
			RequiredNS:    wire.Skill.RequiredNS,
			RequiredTags:  wire.Skill.RequiredTags,
		},
	}
	if len(wire.Supports) > 0 {
		p.Supports = make([]SupportFile, 0, len(wire.Supports))
		for _, f := range wire.Supports {
			p.Supports = append(p.Supports, SupportFile{
				Path:   f.Path,
				Mime:   f.Mime,
				Size:   f.Size,
				Digest: f.Digest,
			})
		}
	}
	return p
}

// UnmarshalJSON implements the strict object-level decode for the
// canonical wire: unknown fields are rejected (via
// DisallowUnknownFields on the alias shape) and duplicate object keys
// are rejected (via checkNoDuplicateKeys) at this object and every
// nested object. It is needed because a type that implements
// json.Unmarshaler is handed its raw object bytes, bypassing the outer
// decoder's DisallowUnknownFields — so each canonical shape enforces
// both rules itself.
func (w *canonicalWire) UnmarshalJSON(data []byte) error {
	type alias canonicalWire
	var a alias
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return err
	}
	if err := checkNoDuplicateKeys(data); err != nil {
		return err
	}
	*w = canonicalWire(a)
	return nil
}

func (s *canonicalSkill) UnmarshalJSON(data []byte) error {
	type alias canonicalSkill
	var a alias
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return err
	}
	if err := checkNoDuplicateKeys(data); err != nil {
		return err
	}
	*s = canonicalSkill(a)
	return nil
}

func (f *canonicalFile) UnmarshalJSON(data []byte) error {
	type alias canonicalFile
	var a alias
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&a); err != nil {
		return err
	}
	if err := checkNoDuplicateKeys(data); err != nil {
		return err
	}
	*f = canonicalFile(a)
	return nil
}

// checkNoDuplicateKeys walks one JSON value and rejects any object
// (at every nesting level) whose keys are not unique. It runs after
// the strict decode, so the input is already structurally valid JSON;
// the walk itself is linear and depth-bounded.
func checkNoDuplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	return walkNoDuplicateKeys(dec, 0)
}

func walkNoDuplicateKeys(dec *json.Decoder, depth int) error {
	if depth > 512 {
		return fmt.Errorf("canonical form nests deeper than 512 levels")
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil // scalar value
	}
	switch d {
	case '{':
		seen := make(map[string]struct{})
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return err
			}
			key, ok := keyTok.(string)
			if !ok {
				return fmt.Errorf("canonical form object key is not a string")
			}
			if _, dup := seen[key]; dup {
				return fmt.Errorf("canonical form repeats object key %q", key)
			}
			seen[key] = struct{}{}
			if err := walkNoDuplicateKeys(dec, depth+1); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim('}') {
			return fmt.Errorf("canonical form object is not closed")
		}
	case '[':
		for dec.More() {
			if err := walkNoDuplicateKeys(dec, depth+1); err != nil {
				return err
			}
		}
		end, err := dec.Token()
		if err != nil {
			return err
		}
		if end != json.Delim(']') {
			return fmt.Errorf("canonical form array is not closed")
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %v", d)
	}
	return nil
}
