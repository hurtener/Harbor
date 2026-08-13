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
// PackageHash. Support files are referenced through the immutable
// `skillpkg://<PackageHash>/<encoded-canonical-support-path>` URI —
// one URI per support file, delivered by PackageIngest.SupportURI.
//
// ImportPackageMarkdown is the pure single-document sibling: it
// ingests ONE bounded UTF-8 SKILL.md document as a RESOURCE-FREE
// complete skill package (no support manifest) through the same
// parser / canonical DTO / hash. Any relative support-file reference
// in the body is rejected, because no support manifest exists to
// satisfy it.
//
// Both paths are PURE: they perform no storage writes and no artifact
// uploads. The existing file-import surface (Import / Export /
// ImportAndStore) is untouched; these paths are additive.

package importer

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"unicode/utf8"

	"github.com/hurtener/Harbor/internal/skills"
	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// ErrPackageSupportRefMissing — the SKILL.md body references a
// relative support path that is not present in the archive's
// validated support manifest (or, for the resource-free single-
// document ingest, any support reference at all). A package whose
// body points at a file the package does not carry is broken; it
// fails loudly rather than materializing dangling references.
var ErrPackageSupportRefMissing = errors.New("importer: package support reference missing from archive")

// ErrPackageMarkdownNotUTF8 — the single-document ingest received
// bytes that are not valid UTF-8. A SKILL.md document must be UTF-8.
var ErrPackageMarkdownNotUTF8 = errors.New("importer: package SKILL.md is not valid UTF-8")

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

// PackageMarkdownSource carries the bytes of ONE bounded UTF-8
// SKILL.md document for the pure single-document ingest path. The
// document IS the package's root skill document by construction
// (there is no archive, so the one-root-case-exact-SKILL.md invariant
// is vacuous). PathHint is display metadata only (name fallback).
type PackageMarkdownSource struct {
	// Markdown is the raw UTF-8 SKILL.md bytes. Bounded by
	// skillpkg.MaxPackageSkillMDBytes; must be valid UTF-8; must
	// satisfy the canonical SKILL.md gate (frontmatter, trigger,
	// steps).
	Markdown []byte
	// PathHint names the source (used for the name fallback). Empty
	// is acceptable.
	PathHint string
}

// PackageIngest is the outcome of the pure package ingest paths: the
// parsed stored `skills.Skill` form (Origin=Pack), the canonical
// package DTO, and the versioned PackageHash. Support-file references
// are derived on demand via SupportURI (the hash-before-URI
// materialization contract: the hash never depends on the URI form).
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
}

// SupportURI returns the immutable support URI of ONE support file of
// the package: `skillpkg://<Hash>/<encoded canonical path>`. The path
// must name an entry of the validated support manifest
// (ErrPackageSupportRefMissing otherwise). A resource-free package
// (the single-SKILL.md ingest) carries no support files, so
// SupportURI always fails for it.
func (pi PackageIngest) SupportURI(path string) (skillpkg.URI, error) {
	for _, f := range pi.Package.Supports {
		if f.Path == path {
			return skillpkg.NewURI(pi.Hash, path)
		}
	}
	return skillpkg.URI{}, fmt.Errorf("%w: %q is not in the package support manifest", ErrPackageSupportRefMissing, path)
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

// ImportPackageMarkdown ingests ONE bounded UTF-8 SKILL.md document as
// a resource-free complete skill package via the same parser /
// canonical DTO / hash as ImportPackage. Any relative support-file
// reference in the body is rejected because no support manifest
// exists. Pure: no storage writes, no artifact uploads.
func (i *importerImpl) ImportPackageMarkdown(ctx context.Context, src PackageMarkdownSource) (PackageIngest, error) {
	if i.closed.Load() {
		return PackageIngest{}, ErrImporterClosed
	}
	if err := ctx.Err(); err != nil {
		return PackageIngest{}, err
	}
	return doImportPackageMarkdown(ctx, src)
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
	manifest := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		if e.Path == skillpkg.RootSkillFileName {
			skillMD = append([]byte(nil), e.Data...)
			continue
		}
		manifest[e.Path] = struct{}{}
	}
	skill, pkg, err := parseSkillToPackage(ctx, skillMD, src.PathHint, entries, manifest)
	if err != nil {
		return PackageIngest{}, err
	}
	hash, err := skillpkg.PackageHash(pkg)
	if err != nil {
		return PackageIngest{}, fmt.Errorf("importer: package hash: %w", err)
	}
	return PackageIngest{Skill: skill, Package: pkg, Hash: hash}, nil
}

func doImportPackageMarkdown(ctx context.Context, src PackageMarkdownSource) (PackageIngest, error) {
	if !utf8.Valid(src.Markdown) {
		return PackageIngest{}, ErrPackageMarkdownNotUTF8
	}
	// A single document: no support manifest, so no support reference
	// can ever resolve — the empty manifest makes every relative body
	// reference fail loudly.
	skill, pkg, err := parseSkillToPackage(ctx, src.Markdown, src.PathHint, nil, map[string]struct{}{})
	if err != nil {
		return PackageIngest{}, err
	}
	hash, err := skillpkg.PackageHash(pkg)
	if err != nil {
		return PackageIngest{}, fmt.Errorf("importer: package hash: %w", err)
	}
	return PackageIngest{Skill: skill, Package: pkg, Hash: hash}, nil
}

// parseSkillToPackage is the shared canonical parse of ONE root
// SKILL.md document into the stored-skill form and the canonical
// package DTO. `supports` are the validated support entries of the
// package (nil for the resource-free single-document path); `manifest`
// is the support-path set body references are validated against
// (empty for the resource-free path, so any relative reference
// fails).
//
// The root SKILL.md is parsed through the shared canonical state
// machine (same frontmatter scan, same body classification as the
// file path; attachment substitution is intentionally absent — the
// body keeps its relative paths and the support manifest is the
// resolver's reference).
func parseSkillToPackage(ctx context.Context, skillMD []byte, pathHint string, supports []skillpkg.ArchiveEntry, manifest map[string]struct{}) (skills.Skill, skillpkg.Package, error) {
	if err := skillpkg.ValidateSkillMarkdown(skillMD, skillpkg.MarkdownLimits{}); err != nil {
		return skills.Skill{}, skillpkg.Package{}, fmt.Errorf("importer: package SKILL.md: %w", err)
	}
	rawFM, body, err := scanFrontmatter(skillMD)
	if err != nil {
		return skills.Skill{}, skillpkg.Package{}, err
	}
	fields, err := parseFrontmatter(rawFM.Bytes)
	if err != nil {
		return skills.Skill{}, skillpkg.Package{}, err
	}
	if strings.TrimSpace(fields.Trigger) == "" {
		return skills.Skill{}, skillpkg.Package{}, fmt.Errorf("%w: %w",
			ErrMissingTrigger, skills.ErrInvalidSkill)
	}
	description, sections, err := parseBodyLines(ctx, body)
	if err != nil {
		return skills.Skill{}, skillpkg.Package{}, err
	}
	steps := sections[sectionSteps]
	if len(steps) == 0 {
		return skills.Skill{}, skillpkg.Package{}, fmt.Errorf("%w: %w",
			ErrEmptySteps, skills.ErrInvalidSkill)
	}
	name := fields.Name
	if strings.TrimSpace(name) == "" {
		name = nameFallbackFromHint(pathHint)
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
		return skills.Skill{}, skillpkg.Package{}, err
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
	for _, e := range supports {
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
	if err := validatePackageSupportRefs(skill, manifest); err != nil {
		return skills.Skill{}, skillpkg.Package{}, err
	}
	return skill, pkg, nil
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
