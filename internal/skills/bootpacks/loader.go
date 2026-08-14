// loader.go — the eager, immutable boot-pack index constructor.
//
// [New] validates the normalized declaration list (bounds, duplicate
// keys, include shape) with zero I/O, then eagerly reads every
// declared package directory: Lstat/ReadDir/fstat strictness, per-file
// and aggregate byte bounds, the pure single-document package parse,
// and required-tools validation against the injected static catalog.
// Any rejection fails the whole load loud — the baseline never
// partially materializes. The frozen [Index] is returned only after
// every byte has been read and every entry validated.

package bootpacks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/skills/importer"
	skillpkg "github.com/hurtener/Harbor/internal/skills/package"
)

// loader carries the injected pure dependencies and the aggregate
// accounting of one [New] call. It is a per-call value, never shared.
type loader struct {
	parser  Parser
	catalog ToolCatalog
	limits  Limits
	// aggregateBytes is the running total of SKILL.md bytes read by
	// the current [New] call, enforced against
	// Limits.MaxAggregateBytes.
	aggregateBytes int64
}

// New eagerly builds the boot-pack index from the normalized
// `skills.boot_agent_packs` declarations. The returned *Index is
// immutable and safe for N concurrent lookups; after New returns the
// index never reads the filesystem again.
//
// The declaration list is assumed to have passed config validation
// (the boot agent packs config contract); the loader re-verifies the
// invariants it depends on — bounds, duplicate (tenant, agent) keys,
// include shape, and absolute (never CWD-resolved) directories — so a
// hand-built or bytes-loaded config fails loud here instead of
// misbehaving at boot.
func New(ctx context.Context, packs []config.BootAgentPackConfig, deps Deps) (*Index, error) {
	if deps.Parser == nil {
		return nil, fmt.Errorf("%w: Deps.Parser is required (the pure single-document package ingest)", ErrDepsIncomplete)
	}
	if deps.Catalog == nil {
		return nil, fmt.Errorf("%w: Deps.Catalog is required (the static-catalog compatibility reader)", ErrDepsIncomplete)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateDeclarations(packs); err != nil {
		return nil, err
	}

	l := &loader{
		parser:  deps.Parser,
		catalog: deps.Catalog,
		limits:  deps.Limits.Normalize(),
	}

	byKey := make(map[Key]*bucket, len(packs))
	for i := range packs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		decl := &packs[i]
		key := Key{TenantID: decl.TenantID, AgentID: decl.AgentID}
		b, err := l.loadBucket(ctx, decl)
		if err != nil {
			return nil, fmt.Errorf("bootpacks: %s: %w", key, err)
		}
		byKey[key] = b
	}
	return newIndex(byKey), nil
}

// validateDeclarations enforces the closed shape and bounds of the
// declaration list with zero filesystem I/O, so a malformed config
// fails before any directory is touched. The checks mirror the config
// contract (its constants are the single source of the ceilings) and
// defend a hand-built *Config that skipped validation.
func validateDeclarations(packs []config.BootAgentPackConfig) error {
	if len(packs) > config.MaxBootAgentPacks {
		return fmt.Errorf("%w: declaration count %d exceeds %d", ErrBoundExceeded, len(packs), config.MaxBootAgentPacks)
	}
	seen := make(map[Key]struct{}, len(packs))
	var includeTotal int
	for i := range packs {
		p := &packs[i]
		key := Key{TenantID: p.TenantID, AgentID: p.AgentID}
		if _, dup := seen[key]; dup {
			return fmt.Errorf("%w: %s: duplicate (tenant_id, agent_id) declaration", ErrBootPackInvalid, key)
		}
		seen[key] = struct{}{}

		if strings.TrimSpace(p.TenantID) == "" {
			return fmt.Errorf("%w: declaration %d: tenant_id is required", ErrBootPackInvalid, i)
		}
		if strings.TrimSpace(p.AgentID) == "" {
			return fmt.Errorf("%w: declaration %d: agent_id is required", ErrBootPackInvalid, i)
		}
		if r := len([]rune(p.TenantID)); r > config.MaxBootAgentPackFieldRunes {
			return fmt.Errorf("%w: declaration %d: tenant_id exceeds %d runes (%d)", ErrBoundExceeded, i, config.MaxBootAgentPackFieldRunes, r)
		}
		if r := len([]rune(p.AgentID)); r > config.MaxBootAgentPackFieldRunes {
			return fmt.Errorf("%w: declaration %d: agent_id exceeds %d runes (%d)", ErrBoundExceeded, i, config.MaxBootAgentPackFieldRunes, r)
		}
		if strings.TrimSpace(p.Directory) == "" {
			return fmt.Errorf("%w: declaration %d: directory is required", ErrBootPackInvalid, i)
		}
		if !filepath.IsAbs(p.Directory) {
			return fmt.Errorf("%w: %s: directory %q", ErrRelativeDirectory, key, p.Directory)
		}
		if r := len([]rune(p.Directory)); r > config.MaxBootAgentPackDirectoryRunes {
			return fmt.Errorf("%w: %s: directory exceeds %d runes (%d)", ErrBoundExceeded, key, config.MaxBootAgentPackDirectoryRunes, r)
		}
		if len(p.Include) == 0 {
			return fmt.Errorf("%w: %s: include must list at least one package-directory name", ErrBootPackInvalid, key)
		}
		if len(p.Include) > config.MaxBootAgentPackIncludes {
			return fmt.Errorf("%w: %s: include count %d exceeds %d", ErrBoundExceeded, key, len(p.Include), config.MaxBootAgentPackIncludes)
		}
		includeTotal += len(p.Include)
		if includeTotal > config.MaxBootAgentPackAggregateIncludes {
			return fmt.Errorf("%w: %s: aggregate include count %d exceeds %d", ErrBoundExceeded, key, includeTotal, config.MaxBootAgentPackAggregateIncludes)
		}
		for j, inc := range p.Include {
			if err := validateIncludeShape(key, j, inc); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateIncludeShape checks that ONE include is exactly one relative
// package-directory name — the shape that makes filepath.Join safe.
func validateIncludeShape(key Key, index int, inc string) error {
	var msg string
	switch {
	case inc == "":
		msg = "empty include name"
	case inc != strings.TrimSpace(inc):
		msg = fmt.Sprintf("%q has surrounding whitespace", inc)
	case inc == "." || inc == "..":
		msg = fmt.Sprintf("%q is not a package directory", inc)
	case strings.ContainsAny(inc, `/\`):
		msg = fmt.Sprintf("%q must be exactly one relative package-directory name — no path separators", inc)
	case strings.Contains(inc, ":"):
		msg = fmt.Sprintf("%q must be a single relative package-directory name — no drive/volume prefix or URI form", inc)
	case len([]rune(inc)) > config.MaxBootAgentPackFieldRunes:
		return fmt.Errorf("%w: %s: include[%d]: %q exceeds %d runes", ErrBoundExceeded, key, index, inc, config.MaxBootAgentPackFieldRunes)
	default:
		return nil
	}
	return fmt.Errorf("%w: %s: include[%d]: %s", ErrBootPackInvalid, key, index, msg)
}

// loadBucket eagerly loads and freezes ONE (tenant, agent) key's
// entries: every include is read, parsed, validated, and deduplicated
// before the bucket is returned.
func (l *loader) loadBucket(ctx context.Context, decl *config.BootAgentPackConfig) (*bucket, error) {
	entries := make([]Entry, 0, len(decl.Include))
	seenPaths := make(map[string]struct{}, len(decl.Include))
	seenNames := make(map[string]struct{}, len(decl.Include))
	for _, inc := range decl.Include {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		entry, err := l.loadInclude(ctx, decl, inc)
		if err != nil {
			return nil, err
		}
		normPath := filepath.Clean(entry.Source)
		if _, dup := seenPaths[normPath]; dup {
			return nil, fmt.Errorf("%w: %s: normalized include path %q", ErrDuplicateInclude, entry.Source, normPath)
		}
		seenPaths[normPath] = struct{}{}
		canonical := skillpkg.CanonicalName(entry.Skill.Name)
		if _, dup := seenNames[canonical]; dup {
			return nil, fmt.Errorf("%w: %s: canonical skill name %q", ErrDuplicateName, entry.Source, canonical)
		}
		seenNames[canonical] = struct{}{}
		entries = append(entries, entry)
	}
	return buildBucket(entries), nil
}

// loadInclude reads and validates ONE include directory of one
// declaration, returning the frozen entry.
func (l *loader) loadInclude(ctx context.Context, decl *config.BootAgentPackConfig, inc string) (Entry, error) {
	includeDir := filepath.Clean(filepath.Join(decl.Directory, inc))
	// Defense in depth: the validated single-segment include cannot
	// escape the declared directory, but the loader re-verifies the
	// lexical containment (CLAUDE.md §7 rule 5).
	if !pathWithin(includeDir, filepath.Clean(decl.Directory)) {
		return Entry{}, fmt.Errorf("%w: %s: include escapes the declared directory", ErrBootPackInvalid, includeDir)
	}

	data, err := readPackageDir(includeDir)
	if err != nil {
		return Entry{}, err
	}
	if err := l.accountAggregate(int64(len(data))); err != nil {
		return Entry{}, err
	}

	ingest, err := l.parser.ImportPackageMarkdown(ctx, importer.PackageMarkdownSource{
		Markdown: data,
		PathHint: includeDir,
	})
	if err != nil {
		return Entry{}, fmt.Errorf("bootpacks: %s: import: %w", includeDir, err)
	}
	for _, tool := range ingest.Skill.RequiredTools {
		if !l.catalog.Compatible(tool) {
			return Entry{}, fmt.Errorf("%w: %s: required_tools names %q which the static catalog cannot satisfy", ErrRequiredTool, includeDir, tool)
		}
	}
	return Entry{
		Skill:        deepCopySkill(ingest.Skill),
		PackageHash:  ingest.Hash,
		SemanticHash: ingest.Skill.ContentHash,
		Source:       includeDir,
	}, nil
}

// accountAggregate enforces the aggregate-byte bound over every
// SKILL.md read by one [New] call.
func (l *loader) accountAggregate(bytes int64) error {
	if l.aggregateBytes+bytes > l.limits.MaxAggregateBytes {
		return fmt.Errorf("%w: aggregate SKILL.md bytes %d would exceed %d", ErrBoundExceeded, l.aggregateBytes+bytes, l.limits.MaxAggregateBytes)
	}
	l.aggregateBytes += bytes
	return nil
}

// readPackageDir verifies that ONE include directory holds exactly one
// case-sensitive top-level regular SKILL.md and nothing else, and
// returns its bytes.
func readPackageDir(includeDir string) ([]byte, error) {
	di, err := os.Lstat(includeDir)
	if err != nil {
		return nil, fmt.Errorf("bootpacks: stat %s: %w", includeDir, err)
	}
	if err := checkDirInfo(di, includeDir); err != nil {
		return nil, err
	}

	entries, err := os.ReadDir(includeDir)
	if err != nil {
		return nil, fmt.Errorf("bootpacks: read %s: %w", includeDir, err)
	}
	if len(entries) != 1 || entries[0].Name() != skillpkg.RootSkillFileName {
		return nil, fmt.Errorf("%w: %s: exactly one top-level %q is required (found %d entries)", ErrSkillMDEntry, includeDir, skillpkg.RootSkillFileName, len(entries))
	}
	if entries[0].IsDir() {
		return nil, fmt.Errorf("%w: %s: %q is a directory, not the root skill document", ErrSkillMDEntry, includeDir, entries[0].Name())
	}

	skillPath := filepath.Join(includeDir, entries[0].Name())
	return readFileStrict(skillPath, skillpkg.MaxPackageSkillMDBytes)
}

// pathWithin reports whether p equals root or lies strictly under it
// (the canonical prefix check of CLAUDE.md §7 rule 5).
func pathWithin(p, root string) bool {
	if p == root {
		return true
	}
	return strings.HasPrefix(p, root+string(filepath.Separator))
}
