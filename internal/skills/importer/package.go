// package.go — the complete-skill-package ingest surface.
//
// ImportPackage is the pure semantic ingest of a complete skill
// package archive (zip): it validates the archive through the
// canonical skillpkg primitives (traversal / collision / link-device /
// nested-archive / MIME / decompression / count-size-ratio rejections,
// plus the exactly-one-root-case-exact-SKILL.md invariant), parses the
// root SKILL.md through the SAME frontmatter + body state machine the
// file path uses (parseBodyLines — one canonical parse, two entry
// points), and produces the canonical package DTO plus its versioned
// PackageHash and resolver-neutral `skillpkg:` URI.
//
// ImportPackage is PURE: it performs no storage writes and no
// artifact uploads. The existing file-import surface (Import /
// Export / ImportAndStore) is untouched; this path is additive.

package importer

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"

	"github.com/hurtener/Harbor/internal/skills"
	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// ErrPackageSupportRefMissing — the SKILL.md body references a
// relative support path that is not present in the archive's
// validated support manifest. A package whose body points at a file
// the archive does not carry is broken; it fails loudly rather than
// materializing dangling references.
var ErrPackageSupportRefMissing = errors.New("importer: package support reference missing from archive")

// PackageSource carries the bytes of ONE complete skill package
// archive. PathHint is display metadata only (used for the
// slugified-name fallback when the root SKILL.md frontmatter lacks
// `name`).
type PackageSource struct {
	// Archive is the raw zip bytes of the complete skill package.
	Archive []byte
	// PathHint names the source (used for the name fallback). Empty
	// is acceptable.
	PathHint string
}

// PackageIngest is the outcome of ImportPackage: the parsed stored
// `skills.Skill` form (Origin=Pack), the canonical package DTO, the
// versioned PackageHash, and the resolver-neutral package URI.
type PackageIngest struct {
	// Skill is the parsed root SKILL.md in stored-skill form
	// (Origin=OriginPack, Scope defaulted like the file path).
	Skill skills.Skill
	// Package is the canonical complete-skill-package DTO (logical
	// skill content + ordered normalized support manifest).
	Package skillpkg.Package
	// Hash is the versioned package hash (v1:<64-hex>) — distinct
	// from Skill.ContentHash, which covers only the skill body.
	Hash string
	// URI is the bounded resolver-neutral package reference derived
	// from Hash (hash-before-URI-materialization).
	URI skillpkg.URI
}

// ImportPackage validates a complete skill package archive and parses
// its root SKILL.md. Pure: no storage writes, no artifact uploads.
// All archive / SKILL.md rejections surface as wrapped skillpkg
// sentinels; parse rejections surface as the existing importer
// sentinels (ErrMissingFrontmatter, ErrMalformedYAML,
// ErrMissingTrigger, ErrEmptySteps, ErrUnknownSection).
func (i *importerImpl) ImportPackage(ctx context.Context, src PackageSource) (PackageIngest, error) {
	if i.closed.Load() {
		return PackageIngest{}, ErrImporterClosed
	}
	if err := ctx.Err(); err != nil {
		return PackageIngest{}, err
	}
	return doImportPackage(ctx, src)
}

func doImportPackage(ctx context.Context, src PackageSource) (PackageIngest, error) {
	entries, err := skillpkg.ValidateArchive(src.Archive, skillpkg.ArchiveLimits{})
	if err != nil {
		return PackageIngest{}, fmt.Errorf("importer: package archive: %w", err)
	}
	if err := skillpkg.ValidateRootSkillEntries(entries); err != nil {
		return PackageIngest{}, err
	}

	var skillMD []byte
	for _, e := range entries {
		if e.Path == skillpkg.RootSkillFileName {
			skillMD = append([]byte(nil), e.Data...)
			break
		}
	}
	if err := skillpkg.ValidateSkillMarkdown(skillMD, skillpkg.MarkdownLimits{}); err != nil {
		return PackageIngest{}, fmt.Errorf("importer: package SKILL.md: %w", err)
	}

	// Parse the root SKILL.md through the shared canonical state
	// machine (same frontmatter scan, same body classification as the
	// file path; attachment substitution is intentionally absent —
	// the body keeps its relative paths and the support manifest is
	// the resolver's reference).
	rawFM, body, err := scanFrontmatter(skillMD)
	if err != nil {
		return PackageIngest{}, err
	}
	fields, err := parseFrontmatter(rawFM.Bytes)
	if err != nil {
		return PackageIngest{}, err
	}
	if strings.TrimSpace(fields.Trigger) == "" {
		return PackageIngest{}, fmt.Errorf("%w: %w",
			ErrMissingTrigger, skills.ErrInvalidSkill)
	}
	description, sections, err := parseBodyLines(ctx, body)
	if err != nil {
		return PackageIngest{}, err
	}
	steps := sections[sectionSteps]
	if len(steps) == 0 {
		return PackageIngest{}, fmt.Errorf("%w: %w",
			ErrEmptySteps, skills.ErrInvalidSkill)
	}
	name := fields.Name
	if strings.TrimSpace(name) == "" {
		name = nameFallbackFromHint(src.PathHint)
	}
	scope := skills.Scope(fields.Scope)
	if scope == "" {
		scope = skills.ScopeProject
	}
	skill := skills.Skill{
		Name:          name,
		Title:         fields.Title,
		Description:   description,
		Trigger:       fields.Trigger,
		TaskType:      fields.TaskType,
		Tags:          fields.Tags,
		Steps:         steps,
		Preconditions: sections[sectionPreconditions],
		FailureModes:  sections[sectionFailureModes],
		RequiredTools: fields.RequiredTools,
		RequiredNS:    fields.RequiredNamespaces,
		RequiredTags:  fields.RequiredTags,
		Origin:        skills.OriginPack,
		Scope:         scope,
		Extra: map[string]any{
			"_importer.frontmatter_raw": string(rawFM.Bytes),
			"_importer.source_sha256":   sourceHashHex(skillMD),
		},
	}
	skill.ContentHash = skills.CanonicalContentHash(skill)
	if err := skill.Validate(); err != nil {
		return PackageIngest{}, err
	}

	// Canonical package DTO: logical content + ordered normalized
	// support manifest (SKILL.md itself is the root document, not a
	// support file).
	pkg := skillpkg.Package{
		Name:    skillpkg.CanonicalName(skill.Name),
		Version: "",
		Skill: skillpkg.PackageSkill{
			Name:          skill.Name,
			Title:         skill.Title,
			Description:   skill.Description,
			Trigger:       skill.Trigger,
			TaskType:      skill.TaskType,
			Tags:          skill.Tags,
			Steps:         skill.Steps,
			Preconditions: skill.Preconditions,
			FailureModes:  skill.FailureModes,
			RequiredTools: skill.RequiredTools,
			RequiredNS:    skill.RequiredNS,
			RequiredTags:  skill.RequiredTags,
		},
	}
	for _, e := range entries {
		if e.Path == skillpkg.RootSkillFileName {
			continue
		}
		pkg.Supports = append(pkg.Supports, skillpkg.SupportFile{
			Path:   e.Path,
			Mime:   e.Mime,
			Size:   e.Size,
			Digest: e.Digest,
			Data:   e.Data,
		})
	}

	// Every relative body reference must name a manifest entry.
	manifest := make(map[string]struct{}, len(pkg.Supports))
	for _, f := range pkg.Supports {
		manifest[f.Path] = struct{}{}
	}
	if err := validatePackageSupportRefs(skill, manifest); err != nil {
		return PackageIngest{}, err
	}

	hash, err := skillpkg.PackageHash(pkg)
	if err != nil {
		return PackageIngest{}, fmt.Errorf("importer: package hash: %w", err)
	}
	uri, err := skillpkg.NewURI(hash, skillpkg.CanonicalName(skill.Name))
	if err != nil {
		return PackageIngest{}, fmt.Errorf("importer: package URI: %w", err)
	}
	return PackageIngest{Skill: skill, Package: pkg, Hash: hash, URI: uri}, nil
}

// validatePackageSupportRefs enforces that every relative inline
// image/attachment reference in the parsed skill body names an entry
// of the validated support manifest. Absolute paths and scheme URIs
// (http/https/data/artifact) stay verbatim, matching the file path's
// skip behavior; a relative reference with no manifest entry fails
// loudly.
func validatePackageSupportRefs(skill skills.Skill, manifest map[string]struct{}) error {
	check := func(text, where string) error {
		if !strings.Contains(text, "![") {
			return nil
		}
		for _, m := range imageRefRegexp.FindAllStringSubmatchIndex(text, -1) {
			ref := text[m[4]:m[5]]
			if hasSchemeOrAbs(ref) {
				continue
			}
			canonical, err := canonicalBodyRef(ref)
			if err != nil {
				return fmt.Errorf("%w: %q in %s: %v", ErrPackageSupportRefMissing, ref, where, err)
			}
			if _, ok := manifest[canonical]; !ok {
				return fmt.Errorf("%w: %q referenced in %s", ErrPackageSupportRefMissing, ref, where)
			}
		}
		return nil
	}
	for i, step := range skill.Steps {
		if err := check(step, fmt.Sprintf("steps[%d]", i)); err != nil {
			return err
		}
	}
	for _, pre := range skill.Preconditions {
		if err := check(pre, "preconditions"); err != nil {
			return err
		}
	}
	for _, fm := range skill.FailureModes {
		if err := check(fm, "failure modes"); err != nil {
			return err
		}
	}
	return check(skill.Description, "description")
}

// canonicalBodyRef normalizes a body-side relative reference into the
// canonical manifest path form: forward-slash, `./` stripped,
// `..` / absolute / backslash rejected.
func canonicalBodyRef(ref string) (string, error) {
	if ref == "" {
		return "", errors.New("empty reference")
	}
	if strings.HasPrefix(ref, "/") || strings.ContainsRune(ref, '\\') {
		return "", fmt.Errorf("not a root-relative forward-slash path")
	}
	cleaned := path.Clean(strings.TrimPrefix(ref, "./"))
	switch {
	case cleaned == ".":
		return "", errors.New("reference resolves to the package root")
	case cleaned == ".." || strings.HasPrefix(cleaned, "../"):
		return "", errors.New("reference escapes the package root")
	}
	return cleaned, nil
}
