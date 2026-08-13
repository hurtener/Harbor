package skillpkg

import (
	"bytes"
	"encoding/json"
	"fmt"
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
// serialization produced by CanonicalBytes. The parsed DTO is
// validated before it is returned. The manifest `Data` bytes are not
// carried by the canonical form, so the reconstructed package is a
// manifest-only view.
func FromCanonicalBytes(b []byte) (Package, error) {
	var wire canonicalWire
	dec := json.NewDecoder(bytes.NewReader(b))
	if err := dec.Decode(&wire); err != nil {
		return Package{}, fmt.Errorf("%w: parse canonical form: %v", ErrInvalidPackage, err)
	}
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
	if err := p.Validate(); err != nil {
		return Package{}, err
	}
	return p, nil
}
