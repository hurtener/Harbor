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
// The package frontmatter is CLOSED: the pure parser accepts only the
// canonical key set (name, title, trigger, task_type, tags,
// required_tools, required_namespaces, required_tags) and REJECTS
// authority-bearing fields (scope, origin, tenant, user, agent,
// authority, audience) and any other unknown key
// (ErrPackageFrontmatterDisallowed). The stored-skill envelope
// (Origin, Scope, Extra) is fixed by the parser, never by caller
// YAML, so the returned stored Skill is a deterministic function of
// the canonical Package — it cannot vary outside the Package/hash for
// caller-controlled content. The intended later user-import service
// applies authority outside this pure parser.
//
// Support references in the logical body are validated through the
// canonical ordinary-Markdown scanner (skillpkg.ScanSupportRefs):
// inline and reference-style links AND images must resolve to manifest
// entries; remote / absolute / fragment-only / ambiguous resource
// references are rejected (the package is self-contained), while
// ordinary remote navigation links and `#fragment` document anchors
// stay verbatim.
//
// ImportPackageMarkdown is the pure single-document sibling: it
// ingests ONE bounded UTF-8 SKILL.md document as a RESOURCE-FREE
// complete skill package (no support manifest) through the same
// parser / canonical DTO / hash. Any relative support-file reference
// in the body is rejected, because no support manifest exists to
// satisfy it.
//
// The pure export surface closes the round trip: MaterializePackageBody
// rewrites validated relative support refs to exact skillpkg URIs;
// ExportPackage dematerializes only the exact package's URIs back to
// relative paths (refusing foreign / malformed / dangling URIs) and
// produces the logical single-document form plus the ordered manifest;
// ReimportPackage rebuilds the canonical package from that form and
// verifies the hash.
//
// Both ingest paths are PURE: they perform no storage writes and no
// artifact uploads. The existing file-import surface (Import / Export /
// ImportAndStore) is untouched; these paths are additive.

package importer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-yaml"

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

// ErrPackageSupportRefRemote — the SKILL.md body references a remote
// (scheme-carrying) or absolute resource (an image, or a link to an
// absolute path). A complete skill package is self-contained: its
// images and resources must be carried by the package, so a remote or
// absolute resource reference is rejected rather than silently kept
// as a dangling claim. Ordinary remote navigation links and
// `#fragment` document anchors are NOT resource references and stay
// verbatim.
var ErrPackageSupportRefRemote = errors.New("importer: package support reference is remote or absolute")

// ErrPackageMarkdownNotUTF8 — the single-document ingest received
// bytes that are not valid UTF-8. A SKILL.md document must be UTF-8.
var ErrPackageMarkdownNotUTF8 = errors.New("importer: package SKILL.md is not valid UTF-8")

// ErrPackageFrontmatterDisallowed — the root SKILL.md frontmatter of a
// complete skill package carries a field outside the CLOSED package
// key set (name, title, trigger, task_type, tags, required_tools,
// required_namespaces, required_tags). Authority-bearing fields
// (scope, origin, tenant, user, agent, authority, audience) and any
// other unknown key are rejected: the pure package ingest must not
// let caller YAML set storage / authority envelope fields that the
// canonical Package/hash does not cover — otherwise the same reviewed
// PackageHash could present a different export-visible envelope. The
// intended later user-import service applies authority OUTSIDE this
// pure parser.
var ErrPackageFrontmatterDisallowed = errors.New("importer: package frontmatter carries a disallowed or unknown field")

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
// parsed stored `skills.Skill` form (Origin=OriginPack, Scope fixed to
// the project default — the pure parser rejects authority-bearing
// frontmatter, so caller YAML can never set the storage envelope),
// the canonical package DTO, and the versioned PackageHash.
// Support-file references are derived on demand via SupportURI (the
// hash-before-URI materialization contract: the hash never depends on
// the URI form).
type PackageIngest struct {
	// Skill is the parsed root SKILL.md in stored-skill form
	// (Origin=OriginPack, Scope=ScopeProject, empty Extra — the
	// storage envelope is a deterministic function of the package).
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

// packageFrontmatterFields is the CLOSED frontmatter key set the pure
// package ingest accepts. It deliberately EXCLUDES `scope` (and every
// other authority-bearing / storage-envelope key such as origin,
// tenant, user, agent, authority, audience): the canonical
// Package/hash does not carry those fields, so a package whose
// frontmatter set them could present the same reviewed hash with a
// different export-visible envelope. The later user-import service
// applies authority outside this pure parser.
type packageFrontmatterFields struct {
	Name               string   `yaml:"name"`
	Title              string   `yaml:"title"`
	Trigger            string   `yaml:"trigger"`
	TaskType           string   `yaml:"task_type"`
	Tags               []string `yaml:"tags"`
	RequiredTools      []string `yaml:"required_tools"`
	RequiredNamespaces []string `yaml:"required_namespaces"`
	RequiredTags       []string `yaml:"required_tags"`
}

// parsePackageFrontmatter parses the raw package frontmatter through
// goccy go-yaml with unknown-field rejection: any key outside the
// closed package key set — including authority-bearing fields like
// scope / origin / tenant / user / agent / authority / audience — is
// rejected (ErrPackageFrontmatterDisallowed wrapping the YAML error,
// which names the offending key). Duplicate keys are also rejected
// (goccy rejects them by default).
func parsePackageFrontmatter(raw []byte) (frontmatterFields, error) {
	var f packageFrontmatterFields
	if err := yaml.UnmarshalWithOptions(raw, &f, yaml.DisallowUnknownField()); err != nil {
		return frontmatterFields{}, fmt.Errorf("%w: %w", ErrPackageFrontmatterDisallowed, err)
	}
	return frontmatterFields{
		Name:               f.Name,
		Title:              f.Title,
		Trigger:            f.Trigger,
		TaskType:           f.TaskType,
		Tags:               f.Tags,
		RequiredTools:      f.RequiredTools,
		RequiredNamespaces: f.RequiredNamespaces,
		RequiredTags:       f.RequiredTags,
	}, nil
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
// resolver's reference). The frontmatter is parsed through the CLOSED
// package key set (parsePackageFrontmatter): authority-bearing and
// unknown keys are rejected, and the stored-skill envelope is fixed
// by the parser (Origin=OriginPack, Scope=ScopeProject, Extra empty),
// so the returned stored Skill is a deterministic function of the
// canonical Package.
func parseSkillToPackage(ctx context.Context, skillMD []byte, pathHint string, supports []skillpkg.ArchiveEntry, manifest map[string]struct{}) (skills.Skill, skillpkg.Package, error) {
	if err := skillpkg.ValidateSkillMarkdown(skillMD, skillpkg.MarkdownLimits{}); err != nil {
		return skills.Skill{}, skillpkg.Package{}, fmt.Errorf("importer: package SKILL.md: %w", err)
	}
	rawFM, body, err := scanFrontmatter(skillMD)
	if err != nil {
		return skills.Skill{}, skillpkg.Package{}, err
	}
	fields, err := parsePackageFrontmatter(rawFM.Bytes)
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
	// The pure package parser fixes the storage envelope: Scope is
	// always the project default (authority-bearing frontmatter such
	// as `scope` is rejected above, so caller YAML can never set it),
	// Origin is always OriginPack, and Extra stays empty. The returned
	// stored Skill is therefore a deterministic function of the
	// canonical Package — it cannot vary outside the Package/hash for
	// caller-controlled content. Export reproduces the document from
	// the canonical fields (synthesized frontmatter), never from
	// stashed caller bytes.
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
		Scope:         skills.ScopeProject,
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

// validatePackageSupportRefs enforces the package support-reference
// contract over the assembled logical body (description + sections)
// using the canonical ordinary-Markdown scanner
// (skillpkg.ScanSupportRefs — inline and reference-style links AND
// images, not a bespoke image-only syntax):
//
//   - an image reference must name a manifest entry — remote (scheme),
//     absolute, fragment-only, and ambiguous destinations are rejected
//     (the package is self-contained);
//   - a link reference to a relative path must name a manifest entry;
//     remote navigation links and `#fragment` document anchors are NOT
//     resource references and stay verbatim;
//   - an absolute or ambiguous link destination is rejected;
//   - every relative support path is canonicalized ONCE (the canonical
//     form is the manifest key) and must exist in the manifest.
func validatePackageSupportRefs(skill skills.Skill, manifest map[string]struct{}) error {
	body := assembleBody(skill)
	for _, r := range skillpkg.ScanSupportRefs(body) {
		kind, pathPart, _ := skillpkg.SplitDest(r.Dest)
		switch r.Kind {
		case skillpkg.SupportRefImage:
			switch kind {
			case skillpkg.DestScheme, skillpkg.DestAbsolute:
				return fmt.Errorf("%w: image reference %q (packages are self-contained)", ErrPackageSupportRefRemote, r.Dest)
			case skillpkg.DestFragment:
				return fmt.Errorf("%w: fragment-only image reference %q names no resource", ErrPackageSupportRefMissing, r.Dest)
			case skillpkg.DestAmbiguous:
				return fmt.Errorf("%w: ambiguous image reference %q", ErrPackageSupportRefMissing, r.Dest)
			case skillpkg.DestRelative:
				if err := requireManifestRef(pathPart, r.Dest, manifest); err != nil {
					return err
				}
			}
		case skillpkg.SupportRefLink:
			switch kind {
			case skillpkg.DestScheme, skillpkg.DestFragment:
				continue // remote navigation link / document anchor — retained
			case skillpkg.DestAbsolute:
				return fmt.Errorf("%w: absolute link reference %q", ErrPackageSupportRefRemote, r.Dest)
			case skillpkg.DestAmbiguous:
				return fmt.Errorf("%w: ambiguous link reference %q", ErrPackageSupportRefMissing, r.Dest)
			case skillpkg.DestRelative:
				if err := requireManifestRef(pathPart, r.Dest, manifest); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// requireManifestRef canonicalizes one relative destination path and
// requires it to name a manifest entry.
func requireManifestRef(pathPart, raw string, manifest map[string]struct{}) error {
	canonical, err := skillpkg.CanonicalizeSupportDest(pathPart)
	if err != nil {
		return fmt.Errorf("%w: %q: %v", ErrPackageSupportRefMissing, raw, err)
	}
	if _, ok := manifest[canonical]; !ok {
		return fmt.Errorf("%w: %q (canonical %q)", ErrPackageSupportRefMissing, raw, canonical)
	}
	return nil
}

// PackageExport is the logical single-document + manifest form of a
// complete skill package: the inverse of the package ingest, produced
// by ExportPackage and consumed by ReimportPackage.
type PackageExport struct {
	// Document is the full logical SKILL.md document (frontmatter +
	// body) with support references in their relative path form.
	Document []byte
	// Manifest is the ordered normalized support manifest (canonical
	// path, MIME, exact size, digest) — no materialized bytes.
	Manifest []skillpkg.SupportFile
	// Hash is the versioned package hash the document + manifest
	// belong to.
	Hash string
}

// MaterializePackageBody rewrites the logical body of an ingested
// package (description + sections) to its materialized form: every
// validated relative support reference becomes the exact
// `skillpkg://<PackageHash>/<encoded-canonical-support-path>` URI
// (skillpkg.MaterializeSupportRefs). Pure: no store, authority,
// lifecycle, or filesystem side effects.
func (i *importerImpl) MaterializePackageBody(ctx context.Context, ingest PackageIngest) (string, error) {
	if i.closed.Load() {
		return "", ErrImporterClosed
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return skillpkg.MaterializeSupportRefs(assembleBody(ingest.Skill), ingest.Package)
}

// ExportPackage reverses the package ingest for a materialized body:
// it dematerializes the package's support URIs back to their relative
// canonical paths — refusing foreign, malformed, and dangling URIs
// (skillpkg.DematerializeSupportRefs) — and assembles the logical
// single-document form plus the ordered normalized manifest. The
// package hash is verified against the ingest before any rewrite.
// Pure: no store, authority, lifecycle, or filesystem side effects.
func (i *importerImpl) ExportPackage(ctx context.Context, ingest PackageIngest, materializedBody string) (PackageExport, error) {
	if i.closed.Load() {
		return PackageExport{}, ErrImporterClosed
	}
	if err := ctx.Err(); err != nil {
		return PackageExport{}, err
	}
	if err := skillpkg.VerifyPackageHash(ingest.Package, ingest.Hash); err != nil {
		return PackageExport{}, err
	}
	logicalBody, err := skillpkg.DematerializeSupportRefs(materializedBody, ingest.Package)
	if err != nil {
		return PackageExport{}, err
	}
	return PackageExport{
		Document: assembleDocument(ingest, logicalBody),
		Manifest: manifestOnly(ingest.Package),
		Hash:     ingest.Hash,
	}, nil
}

// ReimportPackage rebuilds the canonical package from an exported
// logical document + manifest (the inverse of ExportPackage), parsing
// the document through the SAME canonical parse as the archive ingest
// and validating its support references against the manifest. The
// recomputed versioned package hash must equal the export's hash.
func (i *importerImpl) ReimportPackage(ctx context.Context, ex PackageExport) (PackageIngest, error) {
	if i.closed.Load() {
		return PackageIngest{}, ErrImporterClosed
	}
	if err := ctx.Err(); err != nil {
		return PackageIngest{}, err
	}
	entries := make([]skillpkg.ArchiveEntry, 0, len(ex.Manifest))
	manifest := make(map[string]struct{}, len(ex.Manifest))
	for _, f := range ex.Manifest {
		entries = append(entries, skillpkg.ArchiveEntry{
			Path:   f.Path,
			Mime:   f.Mime,
			Size:   f.Size,
			Digest: f.Digest,
		})
		manifest[f.Path] = struct{}{}
	}
	skill, pkg, err := parseSkillToPackage(ctx, ex.Document, "", entries, manifest)
	if err != nil {
		return PackageIngest{}, err
	}
	hash, err := skillpkg.PackageHash(pkg)
	if err != nil {
		return PackageIngest{}, fmt.Errorf("importer: package hash: %w", err)
	}
	if hash != ex.Hash {
		return PackageIngest{}, fmt.Errorf("importer: reimported package hash %q does not match export hash %q", hash, ex.Hash)
	}
	return PackageIngest{Skill: skill, Package: pkg, Hash: hash}, nil
}

// assembleBody emits the canonical logical body form of a parsed
// skill: the description followed by each non-empty canonical section
// in canonical order. It mirrors the file exporter's body emission so
// the package export round-trips byte-exactly.
func assembleBody(skill skills.Skill) string {
	var b strings.Builder
	if skill.Description != "" {
		b.WriteString(skill.Description)
		b.WriteByte('\n')
	}
	sections := []struct {
		section canonicalSection
		items   []string
	}{
		{sectionSteps, skill.Steps},
		{sectionPreconditions, skill.Preconditions},
		{sectionFailureModes, skill.FailureModes},
	}
	for _, sec := range sections {
		if len(sec.items) == 0 {
			continue
		}
		b.WriteByte('\n')
		b.WriteString(canonicalHeading(sec.section))
		b.WriteByte('\n')
		b.WriteByte('\n')
		for _, item := range sec.items {
			b.WriteString("- ")
			b.WriteString(item)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// assembleDocument rebuilds the full SKILL.md document: the
// deterministic frontmatter derived from the stored skill's canonical
// fields plus the canonical body. A package-ingested skill carries no
// stashed raw frontmatter (Extra is empty — the pure parser does not
// preserve caller bytes), so the frontmatter is always synthesized;
// the fallback to a stashed raw slot exists only for file-path skills
// that passed through Import.
func assembleDocument(ingest PackageIngest, body string) []byte {
	rawFM, ok := skillFrontmatterRaw(ingest.Skill)
	if !ok {
		rawFM = synthesiseFrontmatter(ingest.Skill)
	}
	var b bytes.Buffer
	b.WriteString(frontmatterFence)
	b.WriteByte('\n')
	b.Write(rawFM)
	b.WriteString(frontmatterFence)
	b.WriteByte('\n')
	b.WriteString(body)
	return b.Bytes()
}

// manifestOnly strips the materialized Data from a package's support
// manifest, leaving the ordered normalized manifest (path, MIME, size,
// digest).
func manifestOnly(pkg skillpkg.Package) []skillpkg.SupportFile {
	out := make([]skillpkg.SupportFile, 0, len(pkg.Supports))
	for _, f := range pkg.Supports {
		out = append(out, skillpkg.SupportFile{
			Path:   f.Path,
			Mime:   f.Mime,
			Size:   f.Size,
			Digest: f.Digest,
		})
	}
	return out
}
